# Refresh every stored iCloud web session under state/accounts.
#
# Each account directory may hold icloud-web-session.json produced by the
# protocol login. This script calls bin/applekeepalive.exe once per account and
# writes a one-line summary per account. It never prints cookie or token values.
#
#   .\tools\keepalive-all.ps1                # refresh all
#   .\tools\keepalive-all.ps1 -Only us-04    # a single account directory
#   .\tools\keepalive-all.ps1 -WhatIf        # list targets only
#
# Schedule it with Task Scheduler, e.g. every 6 hours:
#   schtasks /create /tn "appleservices keepalive" /sc hourly /mo 6 ^
#     /tr "powershell -NoProfile -File D:\appleservices-family-private\tools\keepalive-all.ps1"

param(
    [string]$Root = (Join-Path $PSScriptRoot '..\state\accounts'),
    [string]$Only = '',
    [string]$LogDir = (Join-Path $PSScriptRoot '..\state\keepalive'),
    [switch]$WhatIf
)

$ErrorActionPreference = 'Stop'
$exe = Join-Path $PSScriptRoot '..\bin\applekeepalive.exe'
if (-not (Test-Path $exe)) { throw "applekeepalive.exe not found: $exe" }

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
$log = Join-Path $LogDir "keepalive-$stamp.log"

$targets = Get-ChildItem -Path $Root -Directory -ErrorAction SilentlyContinue |
    Where-Object { $Only -eq '' -or $_.Name -eq $Only } |
    ForEach-Object { Join-Path $_.FullName 'icloud-web-session.json' } |
    Where-Object { Test-Path $_ }

if (-not $targets) { Write-Host 'no sessions found'; exit 0 }

$ok = 0
$failed = 0
foreach ($session in $targets) {
    $account = Split-Path (Split-Path $session -Parent) -Leaf
    if ($WhatIf) { Write-Host "would refresh $account"; continue }
    # The CLI writes diagnostic lines to stderr on purpose; with
    # ErrorActionPreference=Stop PowerShell would treat that as a failure.
    $ErrorActionPreference = 'Continue'
    $line = & $exe -session $session 2>$null
    $code = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($code -eq 0) {
        $ok++
        $summary = "$stamp ok   $account $line"
    } else {
        $failed++
        $summary = "$stamp FAIL $account exit=$code"
    }
    Write-Host $summary
    Add-Content -Path $log -Value $summary -Encoding utf8
}

if (-not $WhatIf) {
    $tail = "$stamp summary ok=$ok failed=$failed total=$($targets.Count)"
    Write-Host $tail
    Add-Content -Path $log -Value $tail -Encoding utf8
}
if ($failed -gt 0) { exit 1 }
