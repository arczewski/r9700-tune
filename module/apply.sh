#!/bin/bash
# apply.sh - load r9700_tune and set the SCLK soft max
#
# usage: bash apply.sh [MHz]        (default 2000)
#
# Safe to run at boot ("At First Array Start") or manually. The module and
# the cap are runtime-only: reboot without this script = stock GPU behavior.

set -e

MHZ="${1:-2000}"
GPU=/sys/class/drm/card0/device
MOD=/boot/r9700-tune/r9700_tune.ko
PARAM=/sys/module/r9700_tune/parameters/sclk_max
SYM="amdgpu_dpm_set_soft_freq_range"

[ "$(id -u)" = "0" ] || { echo "run as root"; exit 1; }
[ -f "$MOD" ] || { echo "$MOD not found - skipping"; exit 0; }

# Wait for amdgpu to be loaded (needed for the symbol lookup + capture probe)
TRIES=0
while ! grep -qw "$SYM" /proc/kallsyms; do
    TRIES=$((TRIES + 1))
    [ "$TRIES" -ge 30 ] && { echo "amdgpu not loaded after 30s - skipping"; exit 0; }
    sleep 2
done

[ -e "$GPU/pp_table" ] || { echo "R9700 not present - skipping"; exit 0; }

if ! grep -q r9700_tune /proc/modules; then
    ADDR=$(grep -w "$SYM" /proc/kallsyms | awk '{print $1}' | head -1)
    if [ -z "$ADDR" ] || [ "$ADDR" = "0000000000000000" ]; then
        # let the module try the kprobe-based kallsyms lookup itself
        insmod "$MOD" sclk_max="$MHZ" || { echo "insmod failed (kernel mismatch?) - skipping"; exit 0; }
    else
        insmod "$MOD" fn_addr="0x$ADDR" sclk_max="$MHZ" || { echo "insmod failed (kernel mismatch?) - skipping"; exit 0; }
    fi
    echo "r9700_tune loaded"
else
    echo "r9700_tune already loaded"
fi

# trigger the capture probe (safe re-apply of the auto performance level)
echo auto > "$GPU/power_dpm_force_performance_level" 2>/dev/null || true
sleep 1

# apply (again, in case the capture happened just now)
if echo "$MHZ" > "$PARAM" 2>/dev/null; then
    echo "SCLK soft max = $MHZ MHz"
else
    echo "warning: could not apply $MHZ MHz - check dmesg"
fi
