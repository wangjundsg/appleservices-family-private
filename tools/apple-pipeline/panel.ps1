param(
    [int]$Port = 18741,
    [string]$Python = 'python',
    [switch]$Build,
    [switch]$NoOpen,
    [switch]$AllowPurchase
)

$ErrorActionPreference = 'Stop'
$taskRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$taskState = Join-Path $taskRoot 'state'
$taskDashboard = Join-Path $taskRoot 'bin\dashboard.exe'
$taskURL = 'http://127.0.0.1:' + $Port + '/'
if (Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue) {
    Write-Host ('Panel already running: ' + $taskURL)
    if (-not $NoOpen) { Start-Process $taskURL }
    return
}

Push-Location $taskRoot
try {
    New-Item -ItemType Directory -Force -Path (Join-Path $taskRoot 'bin'), $taskState | Out-Null
    foreach ($taskName in @('login', 'appleweb', 'trusted-device', 'accountsetupcheck', 'nacexchange', 'dashboard')) {
        $taskBinary = Join-Path $taskRoot ('bin\' + $taskName + '.exe')
        if ($Build -or -not (Test-Path -LiteralPath $taskBinary)) {
            & go build -o $taskBinary ('./cmd/' + $taskName)
            if ($LASTEXITCODE -ne 0) { throw ('Build failed: ' + $taskName) }
        }
    }
    $taskWorker = Join-Path $taskRoot 'bin\trusted-device-gui.exe'
    if ($Build -or -not (Test-Path -LiteralPath $taskWorker)) {
        & go build -ldflags '-H=windowsgui -s -w' -o $taskWorker ./cmd/trusted-device
        if ($LASTEXITCODE -ne 0) { throw 'Build failed: dashboard account worker' }
    }
} finally { Pop-Location }

# Load existing private key files only into this child's server environment.
# No credentials enter arguments, console output or version control.
$taskKeys = @{
    GRIZZLY_API_KEY = '.cache\grizzly.key'
    HERO_API_KEY = '.cache\apple-os-ab\hero.key'
    YESCAPTCHA_KEY = '.cache\yescaptcha.key'
}
$taskPrevious = @{}
try {
    foreach ($taskKey in $taskKeys.Keys) {
        $taskPrevious[$taskKey] = [Environment]::GetEnvironmentVariable($taskKey, 'Process')
        $taskFile = Join-Path $taskRoot $taskKeys[$taskKey]
        if (-not $taskPrevious[$taskKey] -and (Test-Path -LiteralPath $taskFile)) {
            [Environment]::SetEnvironmentVariable($taskKey, [IO.File]::ReadAllText($taskFile).Trim(), 'Process')
        }
    }
    $taskArgs = @('-repo', ('"' + $taskRoot + '"'), '-dir', ('"' + $taskState + '"'), '-python', ('"' + $Python + '"'), '-port', $Port)
    if ($AllowPurchase) { $taskArgs += '--allow-purchase' }
    $taskProcess = Start-Process -FilePath $taskDashboard -ArgumentList $taskArgs -WorkingDirectory $taskRoot -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput (Join-Path $taskState 'dashboard.out.log') -RedirectStandardError (Join-Path $taskState 'dashboard.err.log')
} finally {
    foreach ($taskKey in $taskPrevious.Keys) { [Environment]::SetEnvironmentVariable($taskKey, $taskPrevious[$taskKey], 'Process') }
}

$taskDeadline = [DateTime]::UtcNow.AddSeconds(20)
do {
    if ($taskProcess.HasExited) { throw 'Panel exited. Check state/dashboard.err.log locally.' }
    try {
        $taskResponse = Invoke-WebRequest -Uri $taskURL -UseBasicParsing -TimeoutSec 2
        if ($taskResponse.Headers['X-AppleServices-UI'] -eq '1') {
            Write-Host ('Panel ready: ' + $taskURL)
            if (-not $NoOpen) { Start-Process $taskURL }
            return
        }
    } catch { }
    Start-Sleep -Milliseconds 200
} while ([DateTime]::UtcNow -lt $taskDeadline)
throw 'Panel did not become ready within 20 seconds.'
