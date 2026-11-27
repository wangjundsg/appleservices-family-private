param(
    [string]$SourceDir = "",
    [string]$Repository = "https://github.com/wangshen233/sentinel-rs",
    [string]$ToolRoot = "D:\DevTools\sentinel-rs"
)

$ErrorActionPreference = "Stop"
$knownSource = "D:\appleservices-family-private\.cache\sentinel-rs-src"
if (-not $SourceDir) {
    $SourceDir = if (Test-Path -LiteralPath (Join-Path $knownSource ".git")) {
        $knownSource
    } else {
        Join-Path $PSScriptRoot ".cache\sentinel-rs-src"
    }
}
$SourceDir = [IO.Path]::GetFullPath($SourceDir)
$binDir = Join-Path $PSScriptRoot "bin"
$output = Join-Path $binDir "sentinel-rs.exe"

if (Test-Path -LiteralPath (Join-Path $SourceDir ".git")) {
    & git -C $SourceDir fetch --tags --prune origin
    if ($LASTEXITCODE) { throw "sentinel-rs fetch failed: $LASTEXITCODE" }
} else {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $SourceDir) | Out-Null
    & git clone $Repository $SourceDir
    if ($LASTEXITCODE) { throw "sentinel-rs clone failed: $LASTEXITCODE" }
}

$workbenchPatch = Join-Path $PSScriptRoot "workbench.patch"
if (-not (Test-Path -LiteralPath $workbenchPatch -PathType Leaf)) {
    throw "Missing workbench compatibility patch: $workbenchPatch"
}
& git -C $SourceDir apply --reverse --check $workbenchPatch 2>$null
if ($LASTEXITCODE) {
    & git -C $SourceDir apply --check $workbenchPatch
    if ($LASTEXITCODE) { throw "Workbench patch conflicts with source; deployment skipped" }
    & git -C $SourceDir apply $workbenchPatch
    if ($LASTEXITCODE) { throw "Workbench patch application failed: $LASTEXITCODE" }
}

$vcVars = Join-Path $ToolRoot "VS2022BuildTools\VC\Auxiliary\Build\vcvars64.bat"
$cargoHome = Join-Path $ToolRoot "cargo"
$rustupHome = Join-Path $ToolRoot "rustup"
$perl = Get-ChildItem -LiteralPath (Join-Path $ToolRoot "Strawberry") -Filter perl.exe -File -Recurse -ErrorAction SilentlyContinue |
    Where-Object { $_.DirectoryName -like "*\perl\bin" } |
    Select-Object -First 1
$libClang = Get-ChildItem -LiteralPath (Join-Path $ToolRoot "LLVM") -Filter libclang.dll -File -Recurse -ErrorAction SilentlyContinue |
    Where-Object { $_.DirectoryName -like "*\bin" } |
    Select-Object -First 1
if (-not (Test-Path -LiteralPath $vcVars -PathType Leaf)) { throw "Missing vcvars64.bat: $vcVars" }
if (-not (Test-Path -LiteralPath (Join-Path $cargoHome "bin\cargo.exe") -PathType Leaf)) { throw "Missing cargo under $cargoHome" }
if (-not $perl) { throw "Missing Strawberry Perl under $(Join-Path $ToolRoot 'Strawberry')" }
if (-not $libClang) { throw "Missing libclang.dll under $(Join-Path $ToolRoot 'LLVM')" }

$llvmDir = $libClang.DirectoryName
$env:CARGO_HOME = $cargoHome
$env:RUSTUP_HOME = $rustupHome
$env:LIBCLANG_PATH = $llvmDir
$env:PATH = (@(
    "D:\mingw64\bin",
    (Join-Path $cargoHome "bin"),
    $perl.DirectoryName,
    $llvmDir,
    $env:PATH
) -join ";")

# The upstream entry point currently hard-codes C: install paths. Run its exact
# contents from the source directory with only those two assignments adjusted.
$upstreamBuild = Join-Path $SourceDir "build.ps1"
$temporaryBuild = Join-Path $SourceDir (".protocol-b-build-{0}.ps1" -f [guid]::NewGuid().ToString("N"))
$script = Get-Content -LiteralPath $upstreamBuild -Raw
$script = $script.Replace(
    '$VcVars    = ''C:\Program Files\Microsoft Visual Studio\2022\Professional\VC\Auxiliary\Build\vcvars64.bat''',
    "`$VcVars    = '$vcVars'"
)
$script = $script.Replace(
    '$LlvmDir   = ''C:\Program Files\LLVM\bin''',
    "`$LlvmDir   = '$llvmDir'"
)
if (($script -notlike "*$vcVars*") -or ($script -notlike "*$llvmDir*")) {
    throw "Upstream build.ps1 install-path assignments were not found"
}
try {
    Set-Content -LiteralPath $temporaryBuild -Value $script -Encoding UTF8
    # Restored local patches may retain timestamps older than a cached library.
    # Rebuild this crate, keeping the expensive TLS dependencies cached.
    Push-Location -LiteralPath $SourceDir
    try {
        & cargo clean --release -p sentinel-rs
        if ($LASTEXITCODE) { throw "sentinel-rs cache invalidation failed: $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
    & $temporaryBuild -Test --locked --test fingerprint_tests --test protocol_b_loopback --test protocol_b_device_archive --test flow_tests --test flow_sentinel_tests
    if ($LASTEXITCODE) { throw "sentinel-rs compatibility tests failed: $LASTEXITCODE; deployment skipped" }
    & $temporaryBuild --locked --bin sentinel-rs
    if ($LASTEXITCODE) { throw "sentinel-rs upstream build failed: $LASTEXITCODE" }
} finally {
    Remove-Item -LiteralPath $temporaryBuild -Force -ErrorAction SilentlyContinue
}

$built = Join-Path $SourceDir "target\release\sentinel-rs.exe"
if (-not (Test-Path -LiteralPath $built -PathType Leaf)) {
    throw "Build output missing: $built"
}
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
Copy-Item -LiteralPath $built -Destination $output -Force
$hash = (Get-FileHash -LiteralPath $output -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath ($output + ".sha256") -Value $hash -Encoding ascii
Write-Output "sentinel-rs deployed: $output"
Write-Output "sha256: $hash"
