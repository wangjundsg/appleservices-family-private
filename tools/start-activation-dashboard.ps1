[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$StateDir,
    [Parameter(Mandatory)][string]$NetworkConfig,
    [Parameter(Mandatory)][string]$DashboardBinary,
    [Parameter(Mandatory)][string]$WorkerBinary,
    [Parameter(Mandatory)][string]$Library,
    [string]$Python = 'python',
    [string]$RepoRoot = (Split-Path $PSScriptRoot -Parent),
    [int]$Port = 18741,
    [string]$RegisterConfig = '',
    [switch]$AllowPurchase,
    [switch]$PreflightOnly,
    [switch]$ValidateOnly
)
$ErrorActionPreference = 'Stop'
if ($PSVersionTable.PSVersion -lt [version]'7.4') { throw 'PowerShell 7.4 or newer is required for explicit child environment.' }
$taskConfigPath = (Resolve-Path -LiteralPath $NetworkConfig).Path
try { $taskConfig = Get-Content -LiteralPath $taskConfigPath -Raw | ConvertFrom-Json -AsHashtable } catch { throw 'Network configuration is not valid JSON.' }
$taskKeys = @('native_http_proxy', 'http_proxy', 'https_proxy', 'all_proxy', 'socks_proxy', 'no_proxy')
if ($taskConfig.Count -ne $taskKeys.Count) { throw 'Network configuration must specify exactly the six documented routing fields.' }
foreach ($taskKey in $taskKeys) {
    if (!$taskConfig.ContainsKey($taskKey) -or $taskConfig[$taskKey] -isnot [string] -or $taskConfig[$taskKey] -match '[\r\n\x00]') { throw "Invalid network configuration field: $taskKey" }
    $taskConfig[$taskKey] = $taskConfig[$taskKey].Trim()
    if (($taskKey -eq 'no_proxy' -and $taskConfig[$taskKey] -ne '*') -or ($taskKey -ne 'no_proxy' -and $taskConfig[$taskKey] -ne '')) { throw 'Local activation is fixed to direct routing: all proxy fields must be empty and no_proxy must be *.' }
}
$taskDashboard = (Resolve-Path -LiteralPath $DashboardBinary).Path
$taskWorker = (Resolve-Path -LiteralPath $WorkerBinary).Path
$taskLibrary = (Resolve-Path -LiteralPath $Library).Path
$taskRepo = (Resolve-Path -LiteralPath $RepoRoot).Path
$taskState = (Resolve-Path -LiteralPath $StateDir).Path
$taskEnvironment = @{
    APPLESERVICES_APPLEID = ''; APPLESERVICES_PASSWORD = ''
    APPLESERVICES_NATIVE_HTTP_PROXY = ''
    HTTP_PROXY = ''; HTTPS_PROXY = ''
    ALL_PROXY = ''; NO_PROXY = '*'
    APPLESERVICES_SOCKS_PROXY = ''
    APPLESERVICES_REMOTE_ANISETTE = ''
    APPLESERVICES_NETWORK_CONFIG_SOURCE = $taskConfigPath
    APPLESERVICES_NETWORK_CONFIG_SHA256 = (Get-FileHash -LiteralPath $taskConfigPath -Algorithm SHA256).Hash.ToLowerInvariant()
}
if ($AllowPurchase -and !$RegisterConfig) { throw 'Purchasing requires an explicit registration configuration.' }
if ($RegisterConfig) {
    $taskRegisterConfig = (Resolve-Path -LiteralPath $RegisterConfig).Path
    try { $null = Get-Content -LiteralPath $taskRegisterConfig -Raw | ConvertFrom-Json } catch { throw 'Registration configuration is not valid JSON.' }
    . (Join-Path $PSScriptRoot 'apple-pipeline/provider-environment.ps1')
    $taskProviderEnvironment = Get-AppleProviderEnvironment -Root $taskRepo
    foreach ($taskProviderName in $taskProviderEnvironment.Keys) { $taskEnvironment[$taskProviderName] = $taskProviderEnvironment[$taskProviderName] }
    $taskEnvironment.APPLESERVICES_REGISTER_CONFIG = $taskRegisterConfig
}
$taskReceipt = [ordered]@{
    dashboard_sha256 = (Get-FileHash -LiteralPath $taskDashboard -Algorithm SHA256).Hash.ToLowerInvariant()
    worker_sha256 = (Get-FileHash -LiteralPath $taskWorker -Algorithm SHA256).Hash.ToLowerInvariant()
    library_sha256 = (Get-FileHash -LiteralPath $taskLibrary -Algorithm SHA256).Hash.ToLowerInvariant()
    config_source = $taskConfigPath; config_sha256 = $taskEnvironment.APPLESERVICES_NETWORK_CONFIG_SHA256
    manual_start = $true; port = $Port; activation_route = 'direct'
    registration_configured = [bool]$RegisterConfig; purchase_enabled = [bool]$AllowPurchase
}
if ($ValidateOnly) { $taskReceipt | ConvertTo-Json; return }
$taskArgs = @('-dir', $taskState, '-repo', $taskRepo, '-worker', $taskWorker, '-adi-dll', $taskLibrary, '-python', $Python, '-port', "$Port", '-manual-start')
if ($AllowPurchase) { $taskArgs += '-allow-purchase' }
if ($PreflightOnly) { $taskArgs += '-network-preflight' }
# Start-Process joins ArgumentList; quote each path rather than relying on spaces.
$taskQuotedArgs = foreach ($taskArg in $taskArgs) {
    if ($taskArg.Contains('"') -or $taskArg -match '[\r\n]') { throw 'Invalid process argument.' }
    '"' + ($taskArg -replace '(\\+)$', '$1$1') + '"'
}
if (!$PreflightOnly -and (Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue)) { throw 'Dashboard port is occupied; no process was stopped. Verify the existing deployment before replacing it.' }
$taskOutputRoot = Split-Path $taskConfigPath -Parent
$taskRun = [Guid]::NewGuid().ToString('N')
$taskOut = Join-Path $taskOutputRoot "dashboard-$taskRun.out.log"
$taskErr = Join-Path $taskOutputRoot "dashboard-$taskRun.err.log"
$taskProcess = Start-Process -FilePath $taskDashboard -ArgumentList $taskQuotedArgs -WorkingDirectory $taskRepo -Environment $taskEnvironment -WindowStyle Hidden -PassThru -RedirectStandardOutput $taskOut -RedirectStandardError $taskErr
if ($PreflightOnly) {
    $taskProcess.WaitForExit()
    # This mode outputs only the allowlisted NetworkCheck schema, never stderr.
    Get-Content -LiteralPath $taskOut -Raw
    if ($taskProcess.ExitCode -ne 0) { throw 'Activation network preflight failed; no account was opened.' }
    return
}
$taskReady = $false
for ($taskAttempt = 0; $taskAttempt -lt 20; $taskAttempt++) {
    if ($taskProcess.HasExited) { throw 'Dashboard exited before listening; inspect private diagnostics locally.' }
    $taskListener = Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue | Where-Object OwningProcess -eq $taskProcess.Id
    if ($taskListener) { $taskReady = $true; break }
    Start-Sleep -Milliseconds 500
}
if (!$taskReady) { throw 'Dashboard did not become ready; no worker was stopped.' }
$taskRunning = Get-Process -Id $taskProcess.Id
if ($taskRunning.Path -ne $taskDashboard -or (Get-FileHash -LiteralPath $taskRunning.Path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $taskReceipt.dashboard_sha256) { throw 'Running dashboard image differs from the requested binary.' }
$taskReceipt.pid = $taskProcess.Id
$taskReceipt | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $taskOutputRoot 'deployment-receipt.json') -Encoding utf8
$taskReceipt | ConvertTo-Json
