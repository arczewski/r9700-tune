#!/bin/bash
# r9700_tune.sh - Unraid user script: apply patched pp_table to R9700 at array start
#
# Install:
#   Settings -> User Scripts -> Add New Script (name it e.g. "r9700_tune")
#   paste this file, schedule "At First Array Start Only"
#   place the patched table at:  /boot/r9700/pp_table_patched.bin
#   keep the stock backup at:    /boot/r9700/pp_table_stock.bin
#
# Safe: the pp_table upload is runtime-only. Delete the patched file + reboot
# and the card returns to stock behavior.

GPU=/sys/class/drm/card0/device
TABLE=/boot/r9700/pp_table_patched.bin
STOCK=/boot/r9700/pp_table_stock.bin
LOG=/var/log/r9700_tune.log
POWER_CAP=210000000     # adjust: hwmon floor after --ppt-floor patch (e.g. 150000000)

exec >>"$LOG" 2>&1
echo "==== $(date) r9700_tune ===="

[ -e "$GPU/pp_table" ] || { echo "R9700 not present (no $GPU/pp_table) - exiting"; exit 0; }
[ -f "$TABLE" ]    || { echo "No patched table at $TABLE - exiting"; exit 0; }

# one-time stock backup if we do not have one yet
if [ ! -f "$STOCK" ]; then
    mkdir -p /boot/r9700
    cat "$GPU/pp_table" > "$STOCK" && echo "Stock pp_table backed up to $STOCK"
fi

SIZE=$(wc -c < "$TABLE")
echo "Uploading $TABLE ($SIZE bytes) ..."
if ! dd if="$TABLE" of="$GPU/pp_table" bs=1M status=none 2>/dev/null; then
    echo "ERROR: pp_table upload failed"
    exit 1
fi
sleep 3

echo "Re-applying performance level (auto) ..."
echo auto > "$GPU/power_dpm_force_performance_level" 2>/dev/null || true
sleep 1

echo "Setting power1_cap to $POWER_CAP ..."
echo "$POWER_CAP" > "$GPU/power1_cap" 2>/dev/null \
    && echo "power1_cap now: $(cat "$GPU/power1_cap")" \
    || echo "WARNING: could not set power1_cap (floor may have changed)"

echo "SCLK DPM:"
cat "$GPU/pp_dpm_sclk" 2>/dev/null
echo "Power / temp after tune:"
grep -E "Power|temp|SCLK" /sys/kernel/debug/dri/0000:c6:00.0/amdgpu_pm_info 2>/dev/null | head -8 || true
echo "done."
