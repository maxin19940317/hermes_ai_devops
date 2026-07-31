#!/system/bin/sh

# USB unbind drops the originating adb shell. Ignore HUP so detached/manual
# execution can still write the serial and rebind the gadget.
trap '' HUP

TAG="hermes-usb-serial"
GADGET="/config/usb_gadget/g1"
SERIAL_FILE="${GADGET}/strings/0x409/serialnumber"
UDC_FILE="${GADGET}/UDC"

log_message() {
    /system/bin/log -t "${TAG}" "$1" 2>/dev/null || echo "${TAG}: $1"
}

serial="$(/system/bin/getprop ro.serialno)"
case "${serial}" in
    ""|"?"|"unknown"|*[!A-Za-z0-9._-]*)
        log_message "invalid ro.serialno: ${serial}"
        exit 1
        ;;
esac

attempt=0
while [ ! -f "${SERIAL_FILE}" ] && [ "${attempt}" -lt 30 ]; do
    /system/bin/sleep 1
    attempt=$((attempt + 1))
done
if [ ! -f "${SERIAL_FILE}" ] || [ ! -f "${UDC_FILE}" ]; then
    log_message "USB gadget ConfigFS is not ready"
    exit 1
fi

current="$(/system/bin/cat "${SERIAL_FILE}" 2>/dev/null)"
if [ "${current}" = "${serial}" ]; then
    log_message "USB serial already set to ${serial}"
    exit 0
fi

udc="$(/system/bin/cat "${UDC_FILE}" 2>/dev/null)"
if [ -n "${udc}" ]; then
    echo "" > "${UDC_FILE}" || exit 1
fi

if ! echo "${serial}" > "${SERIAL_FILE}"; then
    [ -n "${udc}" ] && echo "${udc}" > "${UDC_FILE}"
    log_message "failed to write USB serial"
    exit 1
fi

if [ -n "${udc}" ]; then
    echo "${udc}" > "${UDC_FILE}" || exit 1
fi
log_message "USB serial set to ${serial}"
