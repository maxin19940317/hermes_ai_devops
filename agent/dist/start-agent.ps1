# start-agent.ps1 - Hermes DevOps Agent one-shot startup (Windows)
# Run in any new PowerShell:
#   powershell -ExecutionPolicy Bypass -File .\start-agent.ps1
# First time: edit the two variables below (LAN IP of THIS machine, adb path).

# 控制台切 UTF-8:agent 输出为 UTF-8,中文版 Windows 默认 GBK 解码会显示乱码。
# [Console]::OutputEncoding 同时决定 PowerShell 解码原生 stdout 与 Tee 落盘的编码。
chcp 65001 > $null
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

# ===== edit these two =====
$AGENT_BASE_IP = "10.88.118.51"   # LAN IP of this Windows machine (ipconfig)
$ADB           = "D:\platform-tools_r33.0.2-windows\platform-tools\adb.exe"
# ===========================

$QUAT = "10.88.118.251"

$env:AGENT_CLIENT_ID            = "windows-client-01"
$env:AGENT_RUNTIME_CALLBACK_URL = "http://${QUAT}:18091"
$env:AGENT_BASE_URL             = "http://${AGENT_BASE_IP}:8480"
$env:AGENT_ADB_PATH             = $ADB
$env:AGENT_SOC_ALIASES          = "trinket:QCM6125"
$env:AGENT_DEVICE_CAPABILITIES_MAP = '{"QCM6125":["hexagon"]}'
# optional: AGENT_LISTEN_ADDR / AGENT_VERSION / AGENT_RUNS_ROOT / AGENT_DB_PATH / AGENT_HEARTBEAT_INTERVAL

Write-Host "==> 1/4 Prepare private adb server (5137)" -ForegroundColor Cyan
Remove-Item Env:ANDROID_ADB_SERVER_PORT -ErrorAction SilentlyContinue
& $ADB kill-server 2>$null | Out-Null   # stop the system 5037 server
$env:ANDROID_ADB_SERVER_PORT = "5137"
& $ADB start-server 2>$null | Out-Null
& $ADB devices -l
$deviceLines = @(& $ADB devices | Select-String "`tdevice$")
$addressableLines = @($deviceLines | Where-Object { ($_ -split "`t")[0] -ne "?" })
$unknownLines = @($deviceLines | Where-Object { ($_ -split "`t")[0] -eq "?" })
$usableLines = @($addressableLines)
if ($unknownLines.Count -eq 1) {
  $usableLines += $unknownLines
  Write-Host "!! one device has transport serial '?'; agent will resolve its logical serial via ro.serialno" -ForegroundColor Yellow
} elseif ($unknownLines.Count -gt 1) {
  Write-Host "!! multiple devices have transport serial '?'; they are ambiguous and will be ignored" -ForegroundColor Yellow
}
if ($usableLines.Count -lt 1) {
  Write-Host "!! no usable device on 5137; plug in the board / accept authorization, then rerun" -ForegroundColor Red
  exit 1
}

Write-Host "`n==> 2/4 Device property self-check" -ForegroundColor Cyan
$serial = ($usableLines | Select-Object -First 1) -split "`t" | Select-Object -First 1
& $ADB -s $serial shell /system/bin/getprop ro.product.cpu.abi
& $ADB -s $serial shell /system/bin/getprop ro.board.platform

Write-Host "`n==> 3/4 Server connectivity" -ForegroundColor Cyan
try {
  $r = Invoke-WebRequest -Uri "http://${QUAT}:18091/healthz" -UseBasicParsing -TimeoutSec 5
  Write-Host "callbacks 18091 -> HTTP $($r.StatusCode) $($r.Content)"
} catch { Write-Host "callbacks 18091 -> FAIL: $($_.Exception.Message)" -ForegroundColor Red }
try {
  $r = Invoke-WebRequest -Uri "http://${QUAT}:9000/minio/health/live" -UseBasicParsing -TimeoutSec 5
  Write-Host "minio 9000 -> HTTP $($r.StatusCode)"
} catch { Write-Host "minio 9000 -> FAIL: $($_.Exception.Message)" -ForegroundColor Red }

Write-Host "`n==> 4/4 Start agent (foreground, Ctrl+C to stop; output tee'd to agent-console.log)" -ForegroundColor Cyan
# PS 5.1 quirks: native stderr lines become ErrorRecords; ToString() flattens them.
# Resolve the exe relative to THIS script, not the caller's working directory.
Set-Location $PSScriptRoot
& "$PSScriptRoot\agent.exe" run 2>&1 | ForEach-Object { $_.ToString() } | Tee-Object -FilePath agent-console.log
