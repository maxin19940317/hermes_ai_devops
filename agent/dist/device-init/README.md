# Persistent USB serial provisioning

This package makes an Android development board expose its `ro.serialno` as the
USB gadget iSerial after every boot.

## Requirements

- A userdebug/engineering build that supports `adb root` and `adb remount`.
- `/vendor/etc/init/*.rc` is loaded by Android init.
- Connect exactly one target board during provisioning, especially when its ADB
  transport serial is `?`.
- Each board must have a unique, non-empty `ro.serialno`.

## Install from Windows

Run from the directory containing these files:

```powershell
powershell -ExecutionPolicy Bypass -File .\install-device-init.ps1 `
  -AdbPath "D:\platform-tools_r33.0.2-windows\platform-tools\adb.exe"
```

The installer reads `ro.serialno`, remounts vendor, installs the shell script and
init service, then reboots the board. After USB reconnects, verify on private ADB
port 5137:

```powershell
$env:ANDROID_ADB_SERVER_PORT = "5137"
& "D:\platform-tools_r33.0.2-windows\platform-tools\adb.exe" devices -l
```

The transport serial should now equal the board's `ro.serialno`, rather than
`?`. Provision additional boards one at a time.

## Firmware integration

`adb remount` is suitable for development images. For production or read-only
vendor images, add `hermes-usb-serial.sh` and `hermes-usb-serial.rc` to the vendor
image build and provide the required SELinux policy. An enforcing image may deny
the init service until that policy is present; inspect `dmesg`/`logcat` for AVC
denials.
