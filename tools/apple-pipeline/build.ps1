$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..')).Path
Push-Location $repoRoot
try {
    New-Item -ItemType Directory -Force -Path (Join-Path $repoRoot 'bin') | Out-Null
    foreach ($name in @('login', 'appleweb', 'trusted-device')) {
        & go build -o (Join-Path $repoRoot "bin\$name.exe") "./cmd/$name"
        if ($LASTEXITCODE -ne 0) { throw "Build failed: $name" }
    }
} finally {
    Pop-Location
}
