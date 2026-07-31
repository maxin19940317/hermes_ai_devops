param(
    [Parameter(Mandatory = $true)]
    [string]$AdbPath
)

$ErrorActionPreference = "Stop"
$env:ANDROID_ADB_SERVER_PORT = "5137"

function Invoke-Adb {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    & $AdbPath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "adb failed (exit $LASTEXITCODE): $($Arguments -join ' ')"
    }
}

$deviceLines = @(& $AdbPath devices | Select-String "`tdevice$")
if ($deviceLines.Count -ne 1) {
    throw "Connect exactly one online target board while provisioning; found $($deviceLines.Count)."
}
$transport = (($deviceLines[0].ToString() -split "`t")[0]).Trim()
$logicalSerial = (& $AdbPath -s $transport shell /system/bin/getprop ro.serialno).Trim()
if ($LASTEXITCODE -ne 0 -or $logicalSerial -notmatch '^[A-Za-z0-9._-]+$' -or $logicalSerial -in @('?', 'unknown')) {
    throw "Invalid ro.serialno returned by device: '$logicalSerial'"
}
Write-Host "Provisioning $logicalSerial (ADB transport '$transport')" -ForegroundColor Cyan

Invoke-Adb -s $transport root
Start-Sleep -Seconds 2
Invoke-Adb -s $transport wait-for-device
Invoke-Adb -s $transport remount

$scriptDir = $PSScriptRoot
Invoke-Adb -s $transport push "$scriptDir\hermes-usb-serial.sh" /vendor/bin/hermes-usb-serial.sh
Invoke-Adb -s $transport push "$scriptDir\hermes-usb-serial.rc" /vendor/etc/init/hermes-usb-serial.rc
Invoke-Adb -s $transport shell chmod 0755 /vendor/bin/hermes-usb-serial.sh
Invoke-Adb -s $transport shell chmod 0644 /vendor/etc/init/hermes-usb-serial.rc

Write-Host "Installed persistent USB serial init files. Rebooting..." -ForegroundColor Green
Invoke-Adb -s $transport reboot
Write-Host "After reconnect, verify that 'adb devices -l' shows: $logicalSerial" -ForegroundColor Green
