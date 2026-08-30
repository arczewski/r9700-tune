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
    ARGS="sclk_max=$MHZ fan_target_temp=$FAN_TARGET_TEMP \
acoustic_target_rpm=$FAN_ACOUSTIC_TARGET acoustic_limit_rpm=$FAN_ACOUSTIC_LIMIT \
fan_min_pwm=$FAN_MIN_PWM fan_curve_pwm=$FAN_CURVE_PWM"
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

# apply
if echo "$MHZ" > "$PARAM/sclk_max" 2>/dev/null; then
    echo "SCLK soft max = $MHZ MHz"
else
    echo "warning: could not apply SCLK cap - check dmesg"
fi

if echo 1 > "$PARAM/fan_apply" 2>/dev/null; then
    echo "fan settings applied (target ${FAN_TARGET_TEMP}C, acoustic ${FAN_ACOUSTIC_TARGET}/${FAN_ACOUSTIC_LIMIT} RPM, min ${FAN_MIN_PWM}%)"
else
    echo "warning: could not apply fan settings - check dmesg"
fi

echo "Reset any time: echo auto > $GPU/power_dpm_force_performance_level (clocks); reboot resets fan"
