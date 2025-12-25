#!/system/bin/sh

logcat -b system -v time | while read line; do
    if echo "$line" | grep -q "setWifiEnabled: false"; then
        echo "Wi-Fi OFF detected at: $(date '+%Y-%m-%d %H:%M:%S')" >> /data/local/tmp/wifi_event.log
    fi
done
