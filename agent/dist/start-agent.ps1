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
$env:AGENT_RUNTIME_CALLBACK_URL = "https://${QUAT}:18091"
$env:AGENT_BASE_URL             = "http://${AGENT_BASE_IP}:8480"
$env:AGENT_ADB_PATH             = $ADB
# 2026-08-07:QCS Ubuntu 板设备树上报 idp(平台代号),约束用 QCS6490——
# alias 让 soc 上报为 QCS6490,Linux_Qualcomm_TFLite 的 soc 约束才能匹配。
# 注意:分隔符必须用逗号(与 Agent parseSOCAliases 一致),不能用分号。
$env:AGENT_SOC_ALIASES          = "trinket:QCM6125,idp:QCS6490"
# 方案 B(2026-08-06):设备能力表已迁移到 Runtime 统一管理(服务端权威,
# deploy/.env 的 DEVICE_CAPABILITIES_MAP)。新板接入只改服务端配置即可,
# 不再需要改本脚本。下面这行保留仅作 Agent 侧兜底(服务端未配置该板时
# 仍可上报能力);日常接入新板请勿在此追加,应更新服务端。
$env:AGENT_DEVICE_CAPABILITIES_MAP = '{"QCM6125":["hexagon"],"rk3568":["rknpu"],"rk3588":["rknpu"]}'
# Phase 3 mTLS:回调方向双向认证。已在系统/用户环境变量里设置过的优先;
# 未设置时默认取本脚本同目录下的 pem。
# ca-cert.pem 验服务端,client-*.pem 是客户端证书+私钥合体。
if (-not $env:AGENT_MTLS_CA_FILE)   { $env:AGENT_MTLS_CA_FILE   = "$PSScriptRoot\ca-cert.pem" }
if (-not $env:AGENT_MTLS_CERT_FILE) { $env:AGENT_MTLS_CERT_FILE = "$PSScriptRoot\client-windows-client-01.pem" }
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
$serial = (($usableLines | Select-Object -First 1) -split "`t")[0].Trim()
# 平台代号优先取 adb devices -l 的 product 字段——零设备侧调用,不会卡;
# 再做一次带超时的 getprop/uname 探测,adbd 异常时最多等 8s 跳过,不阻塞脚本。
$product = ""
foreach ($line in $usableLines) {
  $lineStr = $line.ToString()
  $parts = $lineStr -split "`t"
  if ($parts[0].Trim() -eq $serial) {
    if ($lineStr -match 'product:([^\s]+)') { $product = $Matches[1] }
    break
  }
}

# 带超时的 adb shell 执行(.NET Process;PS 5.1 的 & + ForEach-Object 管道在
# 设备返回无换行的二进制输出时会挂起,且没有超时手段)。超时/非零退出返回 $null。
# 注意:PS 5.1/.NET Framework 没有 ProcessStartInfo.ArgumentList,只能用
# Arguments 字符串(含空格的参数用双引号包裹)。
function Invoke-AdbShell {
  param([string[]]$Cmd, [int]$TimeoutSec = 8)
  $parts = @("-s", $serial, "shell") + $Cmd
  $argStr = (($parts | ForEach-Object {
    if ($_ -match '[\s"]') { "`"$_`"" } else { $_ }
  }) -join " ")
  $pinfo = New-Object System.Diagnostics.ProcessStartInfo
  $pinfo.FileName = $ADB
  $pinfo.Arguments = $argStr
  $pinfo.RedirectStandardOutput = $true
  $pinfo.RedirectStandardError = $true
  $pinfo.UseShellExecute = $false
  $p = New-Object System.Diagnostics.Process
  $p.StartInfo = $pinfo
  [void]$p.Start()
  if (-not $p.WaitForExit($TimeoutSec * 1000)) {
    try { $p.Kill(); $p.WaitForExit() } catch { }
    return $null
  }
  try {
    $err = $p.StandardError.ReadToEnd()
    $out = $p.StandardOutput.ReadToEnd()
  } catch { return $null }
  if ($p.ExitCode -ne 0) { return $null }
  return $out + $err
}

# Android 用 getprop;Linux 板(QCS Ubuntu / rk3568 等)没有 /system/bin/getprop,
# 输出是 shell 的 "No such file or directory" —— 此时走 Linux 分支,
# 与 Agent probe/precheckLinux 的判定同源,不要把这当错误。
$abiOut = Invoke-AdbShell @("/system/bin/getprop", "ro.product.cpu.abi")
$gotAndroid = ($null -ne $abiOut) -and (-not ($abiOut -match "No such file"))
if ($gotAndroid) {
  Write-Host "android abi = $($abiOut.Trim())  platform = $product"
} else {
  Write-Host "(non-Android / Linux board detected)" -ForegroundColor Yellow
  $uname = Invoke-AdbShell @("uname", "-m")
  $arch = if ($uname) { $uname.Trim() } else { "" }
  $abi = switch ($arch) {
    "aarch64" { "arm64-v8a" }
    "armv7l"  { "armeabi-v7a" }
    "x86_64"  { "x86_64" }
    "i686"    { "x86" }
    default   { $arch }
  }
  # soc:优先 device-tree compatible(与 Agent linuxSOC 同源),取第一个
  # "vendor,platform" 的 platform;探测失败/超时回退 adb devices -l 的 product。
  $soc = ""
  $compat = Invoke-AdbShell @("/bin/cat", "/proc/device-tree/compatible")
  if ($compat) {
    $candidates = ($compat -replace "`0", "`n") -split "`n" | Where-Object { $_ -match "," }
    if ($candidates) { $soc = ($candidates[0] -split ",")[-1] }
  }
  if (-not $soc) { $soc = $product }
  # AGENT_SOC_ALIASES(逗号分隔 key:value)命中则提示规范化名。
  $aliasHint = ""
  if ($env:AGENT_SOC_ALIASES -and $soc) {
    foreach ($pair in $env:AGENT_SOC_ALIASES.Split(',')) {
      $k, $v = $pair.Split(':')
      if ($k -and $k.Trim() -ieq $soc.Trim()) { $aliasHint = " -> $v"; break }
    }
  }
  Write-Host "linux   abi = $abi  soc = $soc$aliasHint"
}

Write-Host "`n==> 3/4 Server connectivity" -ForegroundColor Cyan
# mTLS 启用后 18091 只接受带客户端证书的 TLS 握手;PS 5.1 加载 pem 做
# 客户端证书很繁琐,这里降级为 TCP 连通性检查,TLS 握手由 agent 启动后
# 的首次心跳真实验证(失败会打 mTLS 相关错误日志)。
try {
  $tcp = New-Object Net.Sockets.TcpClient
  $tcp.Connect($QUAT, 18091)
  $tcp.Close()
  Write-Host "callbacks 18091 -> TCP ok (mTLS)"
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
