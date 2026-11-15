# One end-to-end Apple Account creation attempt over pure HTTP, with stop-loss.
#
# Buys a number, creates the account, walks the two verification steps and
# proves the account exists. Any failure before an SMS arrives refunds the
# number; a refund the provider refuses is parked instead of retried in place
# (docs/REGISTRAR-CONTRACT.md A-8).
#
# Output: one JSON receipt per attempt on stdout, appended to <OutDir>/receipts.jsonl.
#
#   powershell -File tools/signup/attempt.ps1 -Email a@b -GraphCfg .cache/graph-a.json `
#     -Provider grizzly -SmsCountry 12 -PhoneCC US -ExitPort 10001 -Locale en-US -Tz America/New_York
param(
  [Parameter(Mandatory = $true)][string]$Email,
  [Parameter(Mandatory = $true)][string]$GraphCfg,
  [ValidateSet('hero', 'grizzly')][string]$Provider = 'grizzly',
  [int]$SmsCountry = 12,
  [string]$PhoneCC = 'US',
  [double]$MaxPrice = 0.06,
  [string]$ExitPort = '10001',
  [string]$ExitCountry = 'us',
  [string]$Locale = 'en-US',
  [string]$Tz = 'America/New_York',
  [string]$CaptchaKey = '9f06f7c57b4d8fb2c8dd8e7731ad7a0a60d1b675147936',
  [int]$EmailWaitSeconds = 150,
  [int]$SmsWaitSeconds = 180,
  [string]$OutDir = '.cache/attempts',
  [string]$Binary = '.\bin\signup.exe'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$providers = @{
  hero    = @{ api = 'https://hero-sms.com/stubs/handler_api.php'; key = '8171b57bf42dd4d468d6A12221ff129c' }
  grizzly = @{ api = 'https://api.grizzlysms.com/stubs/handler_api.php'; key = '077656c37c8a02b8a02376f72a2a876b' }
}
$sms = $providers[$Provider]
$proxy = "socks5h://3ddce6da3add2226e7d2__cr.$ExitCountry;sessttl.30:2805d82cb2a633dc@gw.dataimpulse.com:$ExitPort"

$started = Get-Date
$receipt = [ordered]@{
  attempt  = (Get-Date -Format 'yyyyMMdd-HHmmss')
  email    = $Email
  provider = $Provider
  exit     = "$ExitCountry/$ExitPort"
  locale   = $Locale
  tz       = $Tz
  stage    = 'buy'
  ok       = $false
}

function Save-Receipt {
  param([hashtable]$Extra = @{})
  foreach ($k in $Extra.Keys) { $receipt[$k] = $Extra[$k] }
  $receipt['elapsed_ms'] = [int]((Get-Date) - $started).TotalMilliseconds
  $line = ($receipt | ConvertTo-Json -Compress -Depth 5)
  Add-Content -Path (Join-Path $OutDir 'receipts.jsonl') -Value $line
  Write-Output $line
}

function Sms-Call {
  param([string]$Query)
  try {
    return (Invoke-WebRequest -UseBasicParsing -Uri "$($sms.api)?api_key=$($sms.key)&$Query" -TimeoutSec 25).Content.Trim()
  } catch {
    $resp = $_.Exception.Response
    if ($resp) {
      $reader = New-Object IO.StreamReader($resp.GetResponseStream())
      return "HTTP_$($resp.StatusCode.value__):" + $reader.ReadToEnd().Trim()
    }
    return "ERR"
  }
}

function Refund {
  param([string]$Activation, [datetime]$BoughtAt)
  $status = Sms-Call "action=getStatus&id=$Activation"
  if ($status -like 'STATUS_OK*') {
    return 'sms_received_no_refund'
  }
  $cancel = Sms-Call "action=setStatus&status=8&id=$Activation"
  if ($cancel -like 'ACCESS_CANCEL*') { return 'refunded' }
  if ($cancel -like '*EARLY_CANCEL_DENIED*') {
    # Providers refuse a cancel inside the minimum activation period (120 s).
    # Waiting it out is cheaper than parking the number for a later sweep.
    $held = ((Get-Date) - $BoughtAt).TotalSeconds
    if ($held -lt 126) {
      Start-Sleep -Seconds ([int](128 - $held))
      $cancel = Sms-Call "action=setStatus&status=8&id=$Activation"
      if ($cancel -like 'ACCESS_CANCEL*') { return 'refunded_after_wait' }
    }
  }
  return "parked:$cancel"
}

function Run-Signup {
  param([string[]]$Arguments)
  $output = & $Binary @Arguments 2>&1 | Out-String
  $json = $null
  foreach ($line in ($output -split "`r?`n")) {
    if ($line.Trim().StartsWith('{')) { try { $json = $line | ConvertFrom-Json } catch { } }
  }
  return @{ raw = $output.Trim(); json = $json }
}

# ---- 1) buy -----------------------------------------------------------------
$buy = Sms-Call "action=getNumber&service=wx&country=$SmsCountry&maxPrice=$MaxPrice"
if ($buy -notlike 'ACCESS_NUMBER:*') {
  Save-Receipt @{ stage = 'buy'; error = $buy }
  exit 1
}
$parts = $buy.Split(':')
$activation = $parts[1]
$national = $parts[2]
$receipt['activation'] = $activation
$receipt['phone'] = $national

# ---- 2) create --------------------------------------------------------------
$receipt['stage'] = 'create'
$create = Run-Signup @(
  '-email', $Email, '-phone', $national, '-phonecc', $PhoneCC,
  '-tz', $Tz, '-locale', $Locale, '-proxy', $proxy, '-skip-landing',
  '-captcha-key', $CaptchaKey, '-stage', 'create', '-json'
)
if (-not $create.json -or $create.json.error) {
  $verdict = if ($create.json) { $create.json.error } else { $create.raw }
  Save-Receipt @{ stage = 'create'; error = $verdict; refund = (Refund $activation $started) }
  exit 1
}
$createdAt = (Get-Date).ToUniversalTime().ToString('o')
$receipt['created'] = $true

# ---- 3) email code ----------------------------------------------------------
$receipt['stage'] = 'email'
$sendEmail = Run-Signup @('-email', $Email, '-stage', 'email', '-proxy', $proxy, '-skip-landing', '-tz', $Tz, '-locale', $Locale, '-json')
$since = (Get-Date).AddMinutes(-2).ToUniversalTime().ToString('o')
$emailCode = ''
$deadline = (Get-Date).AddSeconds($EmailWaitSeconds)
while (-not $emailCode -and (Get-Date) -lt $deadline) {
  $poll = & node tools/capture/graph-code.cjs --file $GraphCfg --since $since 2>&1 | Out-String
  $hit = $null
  foreach ($line in ($poll -split "`r?`n")) { if ($line.Trim().StartsWith('{')) { try { $hit = $line | ConvertFrom-Json } catch { } } }
  if ($hit -and $hit.ok -and $hit.code) { $emailCode = $hit.code }
  else { Start-Sleep -Seconds 6 }
}
if (-not $emailCode) {
  Save-Receipt @{ stage = 'email'; error = 'no email code'; refund = (Refund $activation $started) }
  exit 1
}
$submitEmail = Run-Signup @('-email', $Email, '-stage', 'email', '-code', $emailCode, '-proxy', $proxy, '-skip-landing', '-tz', $Tz, '-locale', $Locale, '-json')
if (-not $submitEmail.json -or $submitEmail.json.error) {
  Save-Receipt @{ stage = 'email-submit'; error = $(if ($submitEmail.json) { $submitEmail.json.error } else { $submitEmail.raw }); refund = (Refund $activation $started) }
  exit 1
}
$receipt['email_verified'] = $true

# ---- 4) sms code ------------------------------------------------------------
$receipt['stage'] = 'phone'
$sendPhone = Run-Signup @('-email', $Email, '-phone', $national, '-phonecc', $PhoneCC, '-stage', 'phone', '-proxy', $proxy, '-skip-landing', '-tz', $Tz, '-locale', $Locale, '-json')
if (-not $sendPhone.json -or $sendPhone.json.error) {
  Save-Receipt @{ stage = 'phone-send'; error = $(if ($sendPhone.json) { $sendPhone.json.error } else { $sendPhone.raw }); refund = (Refund $activation $started) }
  exit 1
}
$smsCode = ''
$deadline = (Get-Date).AddSeconds($SmsWaitSeconds)
while (-not $smsCode -and (Get-Date) -lt $deadline) {
  $status = Sms-Call "action=getStatus&id=$activation"
  if ($status -like 'STATUS_OK:*') { $smsCode = $status.Split(':', 2)[1] }
  elseif ($status -like 'STATUS_WAIT*') { Start-Sleep -Seconds 5 }
  else { $receipt['sms_status'] = $status; Start-Sleep -Seconds 5 }
}
if (-not $smsCode) {
  Save-Receipt @{ stage = 'sms'; error = 'no sms code'; refund = (Refund $activation $started) }
  exit 1
}
$submitSms = Run-Signup @('-email', $Email, '-phone', $national, '-phonecc', $PhoneCC, '-stage', 'phone', '-code', $smsCode, '-proxy', $proxy, '-skip-landing', '-tz', $Tz, '-locale', $Locale, '-json')
if (-not $submitSms.json -or $submitSms.json.error) {
  Save-Receipt @{ stage = 'phone-submit'; error = $(if ($submitSms.json) { $submitSms.json.error } else { $submitSms.raw }) }
  exit 1
}

# ---- 5) prove it exists ----------------------------------------------------
$receipt['stage'] = 'verify'
$verify = Run-Signup @('-email', $Email, '-stage', 'verify', '-proxy', $proxy, '-skip-landing', '-json')
$exists = ($verify.json -and -not $verify.json.error -and $verify.json.accountExists)
Save-Receipt @{ stage = 'done'; ok = [bool]$exists; verify = $(if ($verify.json) { $verify.json } else { $verify.raw }) }
if ($exists) { exit 0 } else { exit 1 }
