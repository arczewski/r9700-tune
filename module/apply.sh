#!/bin/bash
# apply.sh - load r9700_tune, set SCLK soft max and quiet fan settings
#
# usage: bash apply.sh [MHz]        (default 2000)
#
# Fan defaults (override via env): FAN_TARGET_TEMP FAN_ACOUSTIC_TARGET
# FAN_ACOUSTIC_LIMIT FAN_MIN_PWM FAN_CURVE_PWM
#
# Runtime-only: reboot without this script = stock GPU behavior.

set -e

MHZ="${1:-2000}"
FAN_TARGET_TEMP="${FAN_TARGET_TEMP:-85}"
FAN_ACOUSTIC_TARGET="${FAN_ACOUSTIC_TARGET:-1200}"
FAN_ACOUSTIC_LIMIT="${FAN_ACOUSTIC_LIMIT:-1600}"
FAN_MIN_PWM="${FAN_MIN_PWM:-15}"
FAN_CURVE_PWM="${FAN_CURVE_PWM:-15,20,28,38,50,65}"
PPT_OFFSET="${PPT_OFFSET:-0}"   # e.g. -50 = 150 W on a 300 W board; 0 = leave
POWER_CAP="${POWER_CAP:-210}"     # power1_cap in W (210 = firmware floor, 300 = max)
MCLK_MAX="${MCLK_MAX:-0}"         # max MCLK in MHz (stock 1258; e.g. 1058 = -200)
VDDGFX_OFFSET="${VDDGFX_OFFSET:-0}" # undervolt in mV, negative (e.g. -50; may be rejected)

GPU=/sys/class/drm/card0/device
MOD=/boot/r9700-tune/r9700_tune.ko
PARAM=/sys/module/r9700_tune/parameters
SYM="amdgpu_dpm_set_soft_freq_range"

[ "$(id -u)" = "0" ] || { echo "run as root"; exit 1; }
[ -f "$MOD" ] || { echo "$MOD not found - skipping"; exit 0; }

# Wait for amdgpu to be loaded (needed for symbol lookup + capture probes)
TRIES=0
while ! grep -qw "$SYM" /proc/kallsyms; do
    TRIES=$((TRIES + 1))
    [ "$TRIES" -ge 30 ] && { echo "amdgpu not loaded after 30s - skipping"; exit 0; }
    sleep 2
done

[ -e "$GPU/pp_table" ] || { echo "R9700 not present - skipping"; exit 0; }

if ! grep -q r9700_tune /proc/modules; then
    ADDR=$(grep -w "$SYM" /proc/kallsyms | awk '{print $1}' | head -1)
    ARGS="sclk_max=$MHZ mclk_max=$MCLK_MAX vddgfx_offset=$VDDGFX_OFFSET fan_target_temp=$FAN_TARGET_TEMP \
acoustic_target_rpm=$FAN_ACOUSTIC_TARGET acoustic_limit_rpm=$FAN_ACOUSTIC_LIMIT \
fan_min_pwm=$FAN_MIN_PWM fan_curve_pwm=$FAN_CURVE_PWM ppt_offset=$PPT_OFFSET"
    if [ -n "$ADDR" ] && [ "$ADDR" != "0000000000000000" ]; then
        insmod "$MOD" fn_addr="0x$ADDR" $ARGS || { echo "insmod failed (kernel mismatch?) - skipping"; exit 0; }
    else
        insmod "$MOD" $ARGS || { echo "insmod failed (kernel mismatch?) - skipping"; exit 0; }
    fi
    echo "r9700_tune loaded"
else
    echo "r9700_tune already loaded"
fi

# trigger the capture probes:
#  - adev: safe re-apply of the auto performance level
#  - smu:  read the pp_table sysfs node
echo auto > "$GPU/power_dpm_force_performance_level" 2>/dev/null || true
cat "$GPU/pp_table" > /dev/null 2>&1 || true
sleep 1

# update runtime params (harmless on a fresh load, needed for re-runs)
for kv in "fan_target_temp:$FAN_TARGET_TEMP" \
          "acoustic_target_rpm:$FAN_ACOUSTIC_TARGET" \
          "acoustic_limit_rpm:$FAN_ACOUSTIC_LIMIT" \
          "fan_min_pwm:$FAN_MIN_PWM" \
          "fan_curve_pwm:$FAN_CURVE_PWM" \
          "ppt_offset:$PPT_OFFSET" \
          "vddgfx_offset:$VDDGFX_OFFSET" \
          "mclk_max:$MCLK_MAX"; do
    name="${kv%%:*}"; val="${kv#*:}"
    echo "$val" > "$PARAM/$name" 2>/dev/null || true
done

# apply
if echo "$MHZ" > "$PARAM/sclk_max" 2>/dev/null; then
    echo "SCLK soft max = $MHZ MHz"
else
    echo "warning: could not apply SCLK cap - check dmesg"
fi

if echo 1 > "$PARAM/fan_apply" 2>/dev/null; then
    echo "OD settings applied (ppt ${PPT_OFFSET}%, fan target ${FAN_TARGET_TEMP}C, acoustic ${FAN_ACOUSTIC_TARGET}/${FAN_ACOUSTIC_LIMIT} RPM, min ${FAN_MIN_PWM}%)"
else
    echo "warning: could not apply OD settings - check dmesg"
fi

# power1_cap (210-300 W on the R9700; the 210 W floor is firmware-enforced)
if echo "$((POWER_CAP * 1000000))" > "$GPU/hwmon/hwmon4/power1_cap" 2>/dev/null; then
    echo "power1_cap set to $POWER_CAP W"
else
    echo "warning: could not set power1_cap - check the hwmon path"
fi

echo "Reset any time: echo auto > $GPU/power_dpm_force_performance_level (clocks); reboot resets fan"
