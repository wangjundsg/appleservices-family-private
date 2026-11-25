# Resolves values only; the caller owns temporary process environment changes.
function Get-AppleProviderEnvironment {
    param([Parameter(Mandatory = $true)][string]$Root)
    $providers = @(
        @{ Names = @('SMS_API_KEY', 'GRIZZLY_API_KEY'); File = '.cache\grizzly.key' },
        @{ Names = @('SMS_API_KEY_HERO', 'HERO_API_KEY'); File = '.cache\apple-os-ab\hero.key' },
        @{ Names = @('YESCAPTCHA_KEY'); File = '.cache\yescaptcha.key' }
    )
    $resolved = @{}
    foreach ($provider in $providers) {
        $explicit = @($provider.Names | ForEach-Object {
            $value = [Environment]::GetEnvironmentVariable($_, 'Process')
            if (-not [string]::IsNullOrWhiteSpace($value)) { $value.Trim() }
        } | Select-Object -Unique)
        if ($explicit.Count -gt 1) { throw ('Conflicting provider environment: ' + ($provider.Names -join ', ')) }
        $value = if ($explicit.Count) { $explicit[0] } else { $null }
        $file = Join-Path $Root $provider.File
        if (-not $value -and (Test-Path -LiteralPath $file)) { $value = [IO.File]::ReadAllText($file).Trim() }
        foreach ($name in $provider.Names) { $resolved[$name] = $value }
    }
    return $resolved
}
