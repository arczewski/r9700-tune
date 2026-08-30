#!/bin/bash
# apply.sh - load r9700_tune and set the SCLK soft max
#
# usage: apply.sh [MHz]        (default 2000)
#
# Prerequisite: the r9700_tune.ko next to this script must be built for the
# running kernel (6.18.44-Unraid). The capture probe needs one write to
# power_dpm_force_performance_level, which this script performs.

set -e

MHZ="${1:-2000}"
GPU=/sys/class/drm/card0/device
MOD=/boot/r9700-tune/r9700_tune.ko
PARAM=/sys/module/r9700_tune/parameters/sclk_max

[ "$(id -u)" = "0" ] || { echo "run as root"; exit 1; }
[ -f "$MOD" ] || { echo "$MOD not found"; exit 1; }
[ -e "$GPU/pp_table" ] || { echo "R9700 not present"; exit 1; }

if ! grep -q r9700_tune /proc/modules; then
    ADDR=$(grep -w "amdgpu_dpm_set_soft_freq_range" /proc/kallsyms | awk '{print $1}' | head -1)
    if [ -z "$ADDR" ] || [ "$ADDR" = "0000000000000000" ]; then
        # let the module try the kprobe-based kallsyms lookup itself
        insmod "$MOD" sclk_max="$MHZ" || exit 1
    else
        insmod "$MOD" fn_addr="0x$ADDR" sclk_max="$MHZ" || exit 1
    fi
    echo "module loaded"
else
    echo "module already loaded"
fi

# trigger the capture probe (safe re-apply of the auto performance level)
echo auto > "$GPU/power_dpm_force_performance_level" || true
sleep 1

# apply (again, in case the capture happened just now)
echo "$MHZ" > "$PARAM"
echo "SCLK soft max = $MHZ MHz. Verify under load:"
echo "  cat /sys/kernel/debug/dri/0000:c6:00.0/amdgpu_pm_info"
echo "Reset any time:"
echo "  echo auto > $GPU/power_dpm_force_performance_level   (or reboot)"
