$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'provider-environment.ps1')
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('apple-provider-test-' + [guid]::NewGuid())
$names = @('SMS_API_KEY', 'GRIZZLY_API_KEY', 'SMS_API_KEY_HERO', 'HERO_API_KEY', 'YESCAPTCHA_KEY')
$prior = @{}
function Assert-Equal($Actual, $Expected) {
    if ($Actual -cne $Expected) { throw 'Provider environment assertion failed (values withheld)' }
}
try {
    foreach ($name in $names) {
        $prior[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    New-Item -ItemType Directory -Path (Join-Path $testRoot '.cache\apple-os-ab') -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $testRoot '.cache\grizzly.key'), 'fixture-grizzly')
    [IO.File]::WriteAllText((Join-Path $testRoot '.cache\apple-os-ab\hero.key'), 'fixture-hero')
    $result = Get-AppleProviderEnvironment -Root $testRoot
    Assert-Equal $result.SMS_API_KEY 'fixture-grizzly'
    Assert-Equal $result.GRIZZLY_API_KEY $result.SMS_API_KEY
    Assert-Equal $result.SMS_API_KEY_HERO 'fixture-hero'
    Assert-Equal $result.HERO_API_KEY $result.SMS_API_KEY_HERO
    Assert-Equal ([Environment]::GetEnvironmentVariable('SMS_API_KEY', 'Process')) $null
    [Environment]::SetEnvironmentVariable('SMS_API_KEY', 'explicit-fixture', 'Process')
    $result = Get-AppleProviderEnvironment -Root $testRoot
    Assert-Equal $result.GRIZZLY_API_KEY 'explicit-fixture'
    [Environment]::SetEnvironmentVariable('SMS_API_KEY', $null, 'Process')
    [Environment]::SetEnvironmentVariable('GRIZZLY_API_KEY', 'legacy-fixture', 'Process')
    $result = Get-AppleProviderEnvironment -Root $testRoot
    Assert-Equal $result.SMS_API_KEY 'legacy-fixture'
    [Environment]::SetEnvironmentVariable('SMS_API_KEY', 'conflicting-fixture', 'Process')
    $rejected = $false
    try { Get-AppleProviderEnvironment -Root $testRoot | Out-Null } catch {
        $rejected = $_.Exception.Message -like 'Conflicting provider environment:*'
        if ($_.Exception.Message -match 'fixture') { throw 'Credential leaked in conflict error' }
    }
    Assert-Equal $rejected $true
    Write-Output 'PASS: file fallback, both aliases, explicit precedence, no mutation, conflict rejection/redaction'
} finally {
    foreach ($name in $prior.Keys) { [Environment]::SetEnvironmentVariable($name, $prior[$name], 'Process') }
    $resolvedTestRoot = [IO.Path]::GetFullPath($testRoot)
    $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\apple-provider-test-'
    if (-not $resolvedTestRoot.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unexpected test directory' }
    if (Test-Path -LiteralPath $resolvedTestRoot) { Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force }
}
