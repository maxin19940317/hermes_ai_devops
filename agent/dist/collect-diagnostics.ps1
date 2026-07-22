# collect-diagnostics.ps1 - Hermes DevOps Agent diagnostics collector (Windows)
# Run in the agent working directory (parent of agent-runs):
#   powershell -ExecutionPolicy Bypass -File .\collect-diagnostics.ps1
# Optional:
#   -RunsRoot <path>   run output root (default .\agent-runs)
#   -TaskFilter <str>  only show task dirs containing this string (e.g. SNPE_2.21)
#   -QuatHost <ip>     q-uat host (default 10.88.118.251)
param(
  [string]$RunsRoot = ".\agent-runs",
  [string]$TaskFilter = "",
  [string]$QuatHost = "10.88.118.251"
)

$ErrorActionPreference = "Continue"
function Section($t) { Write-Host "`n========== $t ==========" -ForegroundColor Cyan }

Section "1. Connectivity"
foreach ($p in @(@{n="callbacks(18091)"; port=18091; path="/healthz"},
                @{n="minio(9000)";      port=9000;  path="/minio/health/live"})) {
  try {
    $r = Invoke-WebRequest -Uri "http://${QuatHost}:$($p.port)$($p.path)" -UseBasicParsing -TimeoutSec 5
    Write-Host ("{0} -> HTTP {1}" -f $p.n, $r.StatusCode)
  } catch {
    Write-Host ("{0} -> FAIL: {1}" -f $p.n, $_.Exception.Message)
  }
}
try {
  $r = Invoke-WebRequest -Uri "https://gitlab2.quectel.com/api/v4/projects/651" -UseBasicParsing -TimeoutSec 8
  Write-Host ("gitlab API (no token, expect 401) -> HTTP {0}" -f $r.StatusCode)
} catch {
  $code = $_.Exception.Response.StatusCode.value__
  Write-Host ("gitlab API (no token, expect 401) -> HTTP {0}" -f $code)
}

Section "2. Latest 5 task dirs"
if (-not (Test-Path $RunsRoot)) {
  Write-Host "RunsRoot not found: $RunsRoot (run this script in the agent working dir)"
  exit 1
}
$dirs = Get-ChildItem $RunsRoot -Directory | Sort-Object LastWriteTime -Descending
if ($TaskFilter) { $dirs = $dirs | Where-Object { $_.Name -like "*$TaskFilter*" } }
$dirs = $dirs | Select-Object -First 5
foreach ($d in $dirs) { Write-Host ("{0}  {1}" -f $d.LastWriteTime, $d.Name) }

foreach ($d in $dirs) {
  Section "3. Task: $($d.Name)"
  $summary = Join-Path $d.FullName "run-summary.json"
  if (Test-Path $summary) {
    Write-Host "--- run-summary.json ---"
    Get-Content $summary -Raw
  } else {
    Write-Host "(no run-summary.json)"
  }
  foreach ($f in @("stdout.log", "stderr.log")) {
    $p = Join-Path $d.FullName $f
    if (Test-Path $p) {
      Write-Host "--- $f (last 60 lines) ---"
      Get-Content $p -Tail 60
    } else {
      Write-Host "(no $f)"
    }
  }
  $devResult = Join-Path $d.FullName "device\results\result.json"
  if (Test-Path $devResult) {
    Write-Host "--- device\results\result.json ---"
    Get-Content $devResult -Raw
  }
  $logcat = Join-Path $d.FullName "logcat.txt"
  if (Test-Path $logcat) {
    Write-Host "--- logcat.txt (error/Fatal lines, last 30) ---"
    Select-String -Path $logcat -Pattern "error|Fatal|exception" |
      Select-Object -Last 30 | ForEach-Object { $_.Line }
  }
}

Section "4. Environment snapshot"
Write-Host ("adb: " + $env:AGENT_ADB_PATH)
Write-Host ("soc aliases: " + $env:AGENT_SOC_ALIASES + " | caps: " + $env:AGENT_DEVICE_CAPABILITIES)
& $env:AGENT_ADB_PATH devices -l
Write-Host "`nDone. Paste all output above back for analysis."
