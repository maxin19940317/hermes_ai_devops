# start-agent.ps1 — Hermes DevOps Agent 一键启动(Windows)
# 新开的 PowerShell 里直接执行:
#   powershell -ExecutionPolicy Bypass -File .\start-agent.ps1
# 首次使用: 按实际环境修改下面两个变量(本机 LAN IP 和 adb 路径)。

# ===== 按环境修改(只需改这里) =====
$AGENT_BASE_IP = "10.88.118.51"   # 本机 LAN IP(ipconfig 查)
$ADB           = "D:\platform-tools_r33.0.2-windows\platform-tools\adb.exe"
# ==================================

$QUAT = "10.88.118.251"

$env:AGENT_CLIENT_ID            = "windows-client-01"
$env:AGENT_RUNTIME_CALLBACK_URL = "http://${QUAT}:18091"
$env:AGENT_BASE_URL             = "http://${AGENT_BASE_IP}:8480"
$env:AGENT_ADB_PATH             = $ADB
$env:AGENT_SOC_ALIASES          = "trinket:QCM6125"
$env:AGENT_DEVICE_CAPABILITIES  = "hexagon"
# 可选: $env:AGENT_LISTEN_ADDR / AGENT_VERSION / AGENT_RUNS_ROOT / AGENT_DB_PATH / AGENT_HEARTBEAT_INTERVAL

Write-Host "==> 1/4 准备私有 ADB Server (5137)" -ForegroundColor Cyan
Remove-Item Env:ANDROID_ADB_SERVER_PORT -ErrorAction SilentlyContinue
& $ADB kill-server 2>$null | Out-Null   # 停 5037
$env:ANDROID_ADB_SERVER_PORT = "5137"
& $ADB start-server 2>$null | Out-Null
& $ADB devices -l
$state = & $ADB devices | Select-String "`tdevice$" | Measure-Object
if ($state.Count -lt 1) {
  Write-Host "!! 5137 上没有可用设备,请先插好开发板/确认授权后重跑本脚本" -ForegroundColor Red
  exit 1
}

Write-Host "`n==> 2/4 设备属性自检" -ForegroundColor Cyan
$serial = (& $ADB devices | Select-String "`tdevice$" | Select-Object -First 1) -split "`t" | Select-Object -First 1
& $ADB -s $serial shell getprop ro.product.cpu.abi
& $ADB -s $serial shell getprop ro.board.platform

Write-Host "`n==> 3/4 服务端连通性" -ForegroundColor Cyan
try {
  $r = Invoke-WebRequest -Uri "http://${QUAT}:18091/healthz" -UseBasicParsing -TimeoutSec 5
  Write-Host "callbacks 18091 -> HTTP $($r.StatusCode) $($r.Content)"
} catch { Write-Host "callbacks 18091 -> FAIL: $($_.Exception.Message)" -ForegroundColor Red }
try {
  $r = Invoke-WebRequest -Uri "http://${QUAT}:9000/minio/health/live" -UseBasicParsing -TimeoutSec 5
  Write-Host "minio 9000 -> HTTP $($r.StatusCode)"
} catch { Write-Host "minio 9000 -> FAIL: $($_.Exception.Message)" -ForegroundColor Red }

Write-Host "`n==> 4/4 启动 Agent(前台, Ctrl+C 停止; 日志同时写 agent-console.log)" -ForegroundColor Cyan
.\agent.exe run 2>&1 | Tee-Object -FilePath agent-console.log
