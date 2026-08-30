# r9700_tune kernel module

Caps the maximum SCLK on an AMD Radeon AI PRO R9700 (Navi 48) while keeping
the automatic DPM range (min clock unchanged, max = sclk_max).

Background: on this card `pp_od_clk_voltage` is hidden (zeroed OD masks in the
SMU table) and pp_table uploads are ignored by the driver
(`smu_v14_0_2_get_pptable_from_pmfw` always re-reads the table from the SMU).
The sysfs/debugfs interfaces can only pin a constant clock. The driver's
internal `amdgpu_dpm_set_soft_freq_range(adev, PP_SCLK, 0, max)` sends only
`SetSoftMaxByFreq`, which is exactly the wanted behavior. This module calls it
via a kallsyms-resolved address and a kprobe-captured device pointer.

## Usage

```bash
insmod r9700_tune.ko sclk_max=2000
echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level
echo 2000 > /sys/module/r9700_tune/parameters/sclk_max
```

- Change at runtime: write MHz to `/sys/module/r9700_tune/parameters/sclk_max`
- Reset: `echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level`
  or reboot. Nothing persists.
- `apply.sh [MHz]` automates load + capture + apply.
- Overrides: `fn_addr=0x...` (address of amdgpu_dpm_set_soft_freq_range from
  /proc/kallsyms) and `adev_addr=0x...` (amdgpu_device pointer) for kernels
  where kallsyms/kprobes are restricted.

## Building

The module must be built against the exact running kernel (vermagic match).
It uses only core kernel symbols, so building against the matching mainline
version with the Unraid `CONFIG_LOCALVERSION` (and `CONFIG_MODVERSIONS` if the
Unraid kernel enables it) produces a loadable module:

```bash
# with kernel headers for the running kernel installed:
make -C /lib/modules/$(uname -r)/build M=$PWD modules
```

Prebuilt for Unraid 6.18.44-Unraid: see the GitHub release assets.
