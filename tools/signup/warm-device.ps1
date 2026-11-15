# Warm one device profile with read-only visits.
#
# Every run does exactly what a returning visitor does: fetch the portal, the
# create configuration, the widget page and a couple of its bundles, then ask the
# read-only "is this address free" question. No number is bought, no captcha is
# solved, no account is created.
#
# What accumulates is the whole point: cookies, visit count, first-seen time
# (which is what ageDays is computed from), and the frozen fingerprint profile.
#
#   powershell -File tools/signup/warm-device.ps1 -Email a@b.c -Device shao-xing-home
#
# Appends one JSON line per run to <DeviceDir>/<Device>.visits.jsonl so a review
# can see the growth curve. Exits non-zero only when the device record itself is
# unusable; an Apple-side failure is recorded and does not stop the schedule.
param(
  [Parameter(Mandatory = $true)][string]$Email,
  [Parameter(Mandatory = $true)][string]$Device,
  [string]$DeviceDir = '.cache/devices',
  [string]$StateDir = '.cache/signup/warm',
  [string]$EgressCountry = 'CN',
  [string]$EgressSource = 'verified-egress',
  [string]$Telemetry = '.cache/jslog/capture-1.json',
  [string]$Profile = 'auto',
  [string]$Locale = 'en-US',
  [string]$Tz = 'America/New_York',
  [string]$Binary = '.\bin\signup.exe'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root

if (-not (Test-Path $Binary)) {
  Write-Output ('{"ok":false,"error":"binary_missing"}')
  exit 2
}
New-Item -ItemType Directory -Force -Path $DeviceDir, $StateDir | Out-Null

$statePath = Join-Path $StateDir ("warm-{0}.json" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
$args = @(
  '-email', $Email, '-stage', 'preflight', '-json',
  '-device', $Device, '-device-dir', $DeviceDir,
  '-profile-mode', 'new', '-profile', $Profile,
  '-egress-country', $EgressCountry, '-egress-source', $EgressSource,
  '-locale', $Locale, '-tz', $Tz,
  '-human', '-pacing-min', '1500ms', '-pacing-max', '9000ms', '-warmup-assets', '2',
  '-state', $statePath
)
if ($Telemetry -and (Test-Path $Telemetry)) { $args += @('-telemetry', $Telemetry) }

$output = & $Binary @args 2>&1 | Out-String
$exit = $LASTEXITCODE
$receipt = $null
foreach ($line in ($output -split "`r?`n")) {
  if ($line.Trim().StartsWith('{')) {
    try { $receipt = $line | ConvertFrom-Json } catch { }
  }
}

$dev = $receipt.device
$record = [ordered]@{
  at       = (Get-Date).ToUniversalTime().ToString('o')
  device   = $Device
  email    = $Email
  exit     = $exit
  ok       = [bool]($receipt -and $dev)
  visits   = if ($dev) { $dev.visits } else { $null }
  ageDays  = if ($dev) { $dev.ageDays } else { $null }
  cookies  = if ($dev) { $dev.cookies } else { $null }
  loaded   = if ($dev) { $dev.cookiesLoaded } else { $null }
  reused   = if ($dev) { $dev.profileReused } else { $null }
  profile  = if ($dev) { $dev.profile } else { $null }
  stage    = if ($receipt) { $receipt.stage } else { $null }
  reason   = if ($receipt) { $receipt.reason } else { 'no_receipt' }
}
$logPath = Join-Path $DeviceDir ("{0}.visits.jsonl" -f $Device)
Add-Content -Path $logPath -Value ($record | ConvertTo-Json -Compress)

# Keep the per-run task files out of the way: the device record is the durable
# part, the task file only carries this visit's session.
Remove-Item $statePath -ErrorAction SilentlyContinue

Write-Output ($record | ConvertTo-Json -Compress)
if (-not $record.ok) { exit 1 }
exit 0
