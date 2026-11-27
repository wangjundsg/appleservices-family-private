$ErrorActionPreference = 'Stop'
$taskTestRoot = Join-Path ([IO.Path]::GetTempPath()) ('activation-launcher-test-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $taskTestRoot | Out-Null
try {
    $taskFixture = Join-Path $taskTestRoot 'fixture with spaces.exe'
    'not executable' | Set-Content -LiteralPath $taskFixture
    $taskConfigPath = Join-Path $taskTestRoot 'network.json'
    $taskConfig = @{native_http_proxy=''; http_proxy=''; https_proxy=''; all_proxy=''; socks_proxy=''; no_proxy='*'}
    $taskArgs = @{StateDir=$taskTestRoot; NetworkConfig=$taskConfigPath; DashboardBinary=$taskFixture; WorkerBinary=$taskFixture; Library=$taskFixture; ValidateOnly=$true}
    $taskConfig | ConvertTo-Json | Set-Content -LiteralPath $taskConfigPath
    $taskReceipt = (& (Join-Path $PSScriptRoot 'start-activation-dashboard.ps1') @taskArgs) | ConvertFrom-Json
    if ($taskReceipt.activation_route -ne 'direct' -or !$taskReceipt.manual_start -or $taskReceipt.config_source -ne $taskConfigPath -or $taskReceipt.config_sha256 -ne (Get-FileHash -LiteralPath $taskConfigPath).Hash.ToLowerInvariant()) { throw 'Configuration provenance was not preserved.' }
    $taskRejected = $false
    try { & (Join-Path $PSScriptRoot 'start-activation-dashboard.ps1') @taskArgs -AllowPurchase | Out-Null } catch { $taskRejected = $true }
    if (!$taskRejected) { throw 'Purchasing accepted without registration configuration.' }
    $taskRegisterFixture = Join-Path $taskTestRoot 'registration.json'
    '{}' | Set-Content -LiteralPath $taskRegisterFixture
    $taskRegistrationReceipt = (& (Join-Path $PSScriptRoot 'start-activation-dashboard.ps1') @taskArgs -RegisterConfig $taskRegisterFixture -AllowPurchase) | ConvertFrom-Json
    if (!$taskRegistrationReceipt.registration_configured -or !$taskRegistrationReceipt.purchase_enabled) { throw 'Registration configuration was not retained.' }
    foreach ($taskInvalid in @('http://127.0.0.1:12345', 'socks5://127.0.0.1:12345', 'http://user:private-marker@localhost:12345', 'http://localhost', "http://localhost:12345`n")) {
        $taskConfig.native_http_proxy = $taskInvalid
        $taskConfig | ConvertTo-Json | Set-Content -LiteralPath $taskConfigPath
        $taskRejected = $false
        try { & (Join-Path $PSScriptRoot 'start-activation-dashboard.ps1') @taskArgs | Out-Null } catch {
            $taskRejected = $true
            if ($_.Exception.Message.Contains('private-marker')) { throw 'Proxy credential escaped in diagnostic.' }
        }
        if (!$taskRejected) { throw 'Unsafe configuration accepted.' }
    }
    $taskConfig.Remove('no_proxy')
    $taskConfig | ConvertTo-Json | Set-Content -LiteralPath $taskConfigPath
    $taskRejected = $false
    try { & (Join-Path $PSScriptRoot 'start-activation-dashboard.ps1') @taskArgs | Out-Null } catch { $taskRejected = $true }
    if (!$taskRejected) { throw 'Partial configuration could inherit an old route.' }
    'Activation launcher checks passed.'
} finally {
    $taskResolved = [IO.Path]::GetFullPath($taskTestRoot)
    $taskTempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if (!$taskResolved.StartsWith($taskTempRoot, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path $taskResolved -Leaf) -notlike 'activation-launcher-test-*') { throw 'Unsafe test cleanup path.' }
    Remove-Item -LiteralPath $taskResolved -Recurse -Force
}
