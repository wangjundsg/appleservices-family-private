<#
  薄控制台启动脚本（幂等：已在跑就只报地址，不重复起进程）

  用法：
    powershell -ExecutionPolicy Bypass -File tools\console\start.ps1
    powershell -ExecutionPolicy Bypass -File tools\console\start.ps1 -Stop

  端口：默认 8801（可用 -Port 覆盖）
  进程：Start-Process 后台常驻，PID 写入 .cache\console\server.pid
  日志：.cache\console\server.out.log / server.err.log
#>
[CmdletBinding()]
param(
  [int]$Port = 8801,
  [switch]$Stop
)

$ErrorActionPreference = 'Stop'

$consoleDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$root = (Resolve-Path (Join-Path $consoleDir '..\..')).Path
$cacheConsole = Join-Path $root '.cache\console'
New-Item -ItemType Directory -Force -Path $cacheConsole | Out-Null

$pidFile = Join-Path $cacheConsole 'server.pid'
$outLog = Join-Path $cacheConsole 'server.out.log'
$errLog = Join-Path $cacheConsole 'server.err.log'
$url = "http://127.0.0.1:$Port"

function Get-Health {
  try {
    $r = Invoke-WebRequest -Uri "$url/api/health" -TimeoutSec 3 -UseBasicParsing
    if ($r.StatusCode -eq 200) { return ($r.Content | ConvertFrom-Json) }
  } catch { return $null }
  return $null
}

function Get-PidFromFile {
  if (-not (Test-Path $pidFile)) { return $null }
  $raw = (Get-Content $pidFile -Raw).Trim()
  if (-not $raw) { return $null }
  $procId = 0
  if (-not [int]::TryParse($raw, [ref]$procId)) { return $null }
  $p = Get-Process -Id $procId -ErrorAction SilentlyContinue
  if ($p) { return $p.Id }
  return $null
}

if ($Stop) {
  $existing = Get-PidFromFile
  if (-not $existing) {
    Write-Host "[stop] 没有记录在案的 PID（$pidFile），可能已停止。" -ForegroundColor Yellow
    Remove-Item $pidFile -ErrorAction SilentlyContinue
    exit 0
  }
  Stop-Process -Id $existing -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 400
  Remove-Item $pidFile -ErrorAction SilentlyContinue
  Write-Host "[stop] 已停止控制台 PID=$existing" -ForegroundColor Green
  exit 0
}

# 1) 已经在跑？
$health = Get-Health
if ($health) {
  $existing = Get-PidFromFile
  Write-Host "[skip] 控制台已在运行 PID=$existing  $url  (tasks=$($health.tasks))" -ForegroundColor Cyan
  exit 0
}

# 2) 端口被别的进程占用？
$inUse = $null
try {
  $inUse = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction Stop |
    Select-Object -First 1 -ExpandProperty OwningProcess
} catch { $inUse = $null }
if ($inUse) {
  Write-Warning "[warn] 端口 $Port 已被 PID=$inUse 占用，且 /api/health 不通。请换端口：-Port 8802"
  exit 1
}

# 3) 找 node
$node = (Get-Command node -ErrorAction SilentlyContinue).Source
if (-not $node) { Write-Error '[err] 找不到 node，请先安装 Node.js'; exit 1 }

# 4) 后台启动
$env:CONSOLE_PORT = "$Port"
$proc = Start-Process -FilePath $node `
  -ArgumentList @('tools/console/server.cjs') `
  -WorkingDirectory $root `
  -WindowStyle Hidden `
  -RedirectStandardOutput $outLog `
  -RedirectStandardError $errLog `
  -PassThru
$proc.Id | Set-Content -Path $pidFile -Encoding ascii

Write-Host "[start] node PID=$($proc.Id)  等待 $url/api/health …" -ForegroundColor Cyan
$ok = $null
for ($i = 0; $i -lt 40; $i++) {
  Start-Sleep -Milliseconds 300
  $ok = Get-Health
  if ($ok) { break }
  if ($proc.HasExited) { break }
}

if (-not $ok) {
  Write-Warning "[fail] 启动后 /api/health 未通过，看日志：`n  $outLog`n  $errLog"
  if (Test-Path $errLog) { Get-Content $errLog -Tail 20 }
  exit 1
}

Write-Host ''
Write-Host "控制台已就绪： $url" -ForegroundColor Green
Write-Host "  PID      : $($proc.Id)  (记录在 $pidFile)"
Write-Host "  任务数   : $($ok.tasks)"
Write-Host "  真跑开关 : allowReal=$($ok.allowReal)  (默认 false，仅 dry-run)"
Write-Host "  停止     : powershell -ExecutionPolicy Bypass -File tools\console\start.ps1 -Stop"
Write-Host "  或       : Stop-Process -Id $($proc.Id) -Force"
Write-Host ''
