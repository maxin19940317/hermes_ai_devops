# collect-diagnostics.ps1 — Hermes DevOps Agent 诊断信息收集(Windows)
# 用法: 在 agent 运行目录(agent-runs 的父目录)执行:
#   powershell -ExecutionPolicy Bypass -File .\collect-diagnostics.ps1
# 可选参数:
#   -RunsRoot <path>     结果根目录(默认 .\agent-runs)
#   -TaskFilter <str>    只显示任务目录名包含该字符串的任务(如 SNPE_2.21)
#   -QuatHost <ip>       q-uat 地址(默认 10.88.118.251)
param(
  [string]$RunsRoot = ".\agent-runs",
  [string]$TaskFilter = "",
  [string]$QuatHost = "10.88.118.251"
)

$ErrorActionPreference = "Continue"
function Section($t) { Write-Host "`n========== $t ==========" -ForegroundColor Cyan }

Section "1. 连通性"
foreach ($p in @(@{n="callbacks(18091)"; port=18091; path="/healthz"},
                @{n="minio(9000)";      port=9000;  path="/minio/health/live"},
                @{n="temporal-ui(18080, 应拒绝)"; port=18080; path=""})) {
  try {
    $r = Invoke-WebRequest -Uri "http://${QuatHost}:$($p.port)$($p.path)" -UseBasicParsing -TimeoutSec 5
    Write-Host ("{0} -> HTTP {1}" -f $p.n, $r.StatusCode)
  } catch {
    Write-Host ("{0} -> FAIL: {1}" -f $p.n, $_.Exception.Message)
  }
}
try {
  $r = Invoke-WebRequest -Uri "https://gitlab2.quectel.com/api/v4/projects/651" -UseBasicParsing -TimeoutSec 8
  Write-Host ("gitlab API(无 token, 期望 401) -> HTTP {0}" -f $r.StatusCode)
} catch {
  $code = $_.Exception.Response.StatusCode.value__
  Write-Host ("gitlab API(无 token, 期望 401) -> HTTP {0}" -f $code)
}

Section "2. 本地任务目录(最新 5 个)"
if (-not (Test-Path $RunsRoot)) {
  Write-Host "RunsRoot 不存在: $RunsRoot (在 agent 运行目录下执行本脚本)"
  exit 1
}
$dirs = Get-ChildItem $RunsRoot -Directory | Sort-Object LastWriteTime -Descending
if ($TaskFilter) { $dirs = $dirs | Where-Object { $_.Name -like "*$TaskFilter*" } }
$dirs = $dirs | Select-Object -First 5
foreach ($d in $dirs) { Write-Host ("{0}  {1}" -f $d.LastWriteTime, $d.Name) }

foreach ($d in $dirs) {
  Section "3. 任务: $($d.Name)"
  $summary = Join-Path $d.FullName "run-summary.json"
  if (Test-Path $summary) {
    Write-Host "--- run-summary.json ---"
    Get-Content $summary -Raw
  } else {
    Write-Host "(无 run-summary.json)"
  }
  foreach ($f in @("stdout.log", "stderr.log")) {
    $p = Join-Path $d.FullName $f
    if (Test-Path $p) {
      Write-Host "--- $f (最后 60 行) ---"
      Get-Content $p -Tail 60
    } else {
      Write-Host "(无 $f)"
    }
  }
  $devResult = Join-Path $d.FullName "device\results\result.json"
  if (Test-Path $devResult) {
    Write-Host "--- device\results\result.json ---"
    Get-Content $devResult -Raw
  }
  $logcat = Join-Path $d.FullName "logcat.txt"
  if (Test-Path $logcat) {
    Write-Host "--- logcat.txt (含 error/Fatal 的行, 最多 30 行) ---"
    Select-String -Path $logcat -Pattern "error|Fatal|exception" -SimpleMatch:$false |
      Select-Object -Last 30 | ForEach-Object { $_.Line }
  }
}

Section "4. 环境快照"
Write-Host ("adb: " + $env:AGENT_ADB_PATH)
Write-Host ("SOC aliases: " + $env:AGENT_SOC_ALIASES + " | caps: " + $env:AGENT_DEVICE_CAPABILITIES)
& $env:AGENT_ADB_PATH devices -l
Write-Host "`n收集完成。把以上全部输出发回即可。"
