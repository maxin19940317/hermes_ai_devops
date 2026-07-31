# Persistent USB serial provisioning

This package contains the files needed to make an Android development board
expose its `ro.serialno` as the USB gadget iSerial after every boot.

## Requirements

- A vendor image build, or a userdebug/engineering image where `adb remount`
  writes the real vendor filesystem rather than a late overlay.
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

The installer reads `ro.serialno`, remounts vendor, and refuses to continue if
`/vendor` is backed by overlayfs. A late overlay is visible after boot but its
new `.rc` files are not parsed by Android init. On a directly writable vendor it
installs the shell script and init service, then reboots the board.

After USB reconnects, verify on private ADB port 5137:

```powershell
$env:ANDROID_ADB_SERVER_PORT = "5137"
& "D:\platform-tools_r33.0.2-windows\platform-tools\adb.exe" devices -l
```

The transport serial should now equal the board's `ro.serialno`, rather than
`?`. Provision additional boards one at a time.

## Firmware integration

For production, read-only vendor, or overlay-backed development images, add
`hermes-usb-serial.sh` and `hermes-usb-serial.rc` to the vendor image build and
provide the required SELinux policy. An enforcing image may deny the init service
until that policy is present; inspect `dmesg`/`logcat` for AVC denials.

## Apply for the current boot only

This validates the shell logic but does not survive reboot:

```powershell
& $adb -s "?" push .\hermes-usb-serial.sh /data/local/tmp/hermes-usb-serial.sh
& $adb -s "?" shell chmod 0755 /data/local/tmp/hermes-usb-serial.sh
& $adb -s "?" shell "nohup /system/bin/sh /data/local/tmp/hermes-usb-serial.sh >/data/local/tmp/hermes-usb-serial.log 2>&1 </dev/null &"
```
