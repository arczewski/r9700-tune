# r9700-tune

Tools to make an AMD Radeon AI PRO R9700 (Navi 48, SMU 14.0.2) quiet on
Linux/Unraid: cap the maximum GPU clock while keeping the automatic DPM
range, lower the power limit below the 210 W floor, and apply a custom fan
curve with acoustic RPM limits.

Two components:

1. **`r9700-tune`** - a static Go CLI to analyze and patch PowerPlay
   tables (`analyze`, `patch`, `extract-vbios`, `extract-pptable`,
   `apply`).
2. **`r9700_tune.ko`** - a small kernel module that does what actually
   works on Navi 48: sets the SCLK soft max, uploads fan/acoustic settings
   and a percentage power limit offset through the SMU's overdrive table.

---

## Background: why this card is hard to tune

The R9700 (Radeon AI PRO, Navi 48) ships with every normal Linux tuning
interface disabled or non-functional:

| Interface | Status on the R9700 |
|---|---|
| `pp_od_clk_voltage` (overdrive sysfs) | **Missing.** The driver hides it when `FeatureCtrlMask` of `OverDriveLimitsBasicMin`/`BasicMax` in the SMU PowerPlay table is zero (`smu_v14_0_2_check_powerplay_table()` sets `smu->od_enabled = false`). Pro/AI SKUs ship with those masks zeroed, so no kernel version will expose it - the gate reads data, not code. |
| `pp_table` sysfs upload | **A no-op on Navi 48.** `smu_v14_0_2_get_pptable_from_pmfw()` always re-fetches the table from the SMU (`TransferTableSmu2Dram`) and never consults the uploaded copy, unlike RDNA1-3. A successful upload changes nothing. |
| VBIOS PowerPlay table | **Absent.** The 58880-byte VBIOS contains no `powerplayinfo` atom table (master list index 15 is empty), and `smu_14_0_2.bin` has no soft pptable either - the table lives inside the SMU firmware itself. |
| `power1_cap` | Works (SMU `SetPptLimit`) but enforces a 210 W floor on this board. |
| `amdgpu_force_sclk` (debugfs) | Calls `SetSoftMinByFreq` + `SetSoftMaxByFreq` with min=max, i.e. pins a constant clock. No range. Rejected with `EINVAL` on this card. |
| Fan curve / acoustic controls | Exist in the SMU (the same OD fan table Windows Adrenalin uses) but are not reachable from userspace. |

What the SMU does honor, and what this project uses:

- `SetSoftMaxByFreq` - a soft maximum clock; the automatic range below it
  is preserved (idle still drops to ~500 MHz). This is what
  `amdgpu_dpm_set_soft_freq_range(adev, PP_SCLK, 0, max)` does internally.
- The **OD overdrive table upload** (`smu_v14_0_2_upload_overdrive_table`,
  `TransferTableDram2Smu` with `SMU_TABLE_OVERDRIVE`) - carries the fan
  curve, fan target temperature, acoustic target/limit RPM, minimum PWM
  (feature bit 4) and a **percentage power limit offset** `Ppt`
  (feature bit 3), applied to the board limit - this bypasses the 210 W
  `power1_cap` floor (e.g. `-50` = 150 W on a 300 W board).

All offsets and behavior were verified against the kernel v6.18 source
(`drivers/gpu/drm/amd/pm/swsmu/smu14/smu_v14_0_2_ppt.c`,
`swsmu/inc/smu_v14_0_2_pptable.h`,
`swsmu/inc/pmfw_if/smu14_driver_if_v14_0.h`, `pm/amdgpu_dpm.c`,
`pm/amdgpu_pm.c`, `pm/swsmu/amdgpu_smu.c`) and compiled against the exact
Unraid 6.18.44 kernel tree.

---

## The kernel module (the part that works)

`module/r9700_tune.ko` is built for Unraid kernel `6.18.44-Unraid`
(vermagic `6.18.44-Unraid SMP preempt mod_unload`). It is a runtime-only
module: nothing persists across reboot, the VBIOS is never touched.

How it works:

- Resolves the non-exported driver functions via kallsyms
  (`amdgpu_dpm_set_soft_freq_range`, `smu_v14_0_2_upload_overdrive_table`).
- Captures the `amdgpu_device` pointer with a kprobe on
  `amdgpu_dpm_force_performance_level` and the `smu_context` pointer with a
  kprobe on `smu_sys_get_pp_table`.
- Calls the soft-max function with `min=0` (only the maximum changes) and
  uploads an OD table with the fan/power fields.

### Install (Unraid)

```bash
mkdir -p /boot/r9700-tune && cd /boot/r9700-tune
wget -O r9700_tune.ko \
  https://github.com/arczewski/r9700-tune/releases/download/v1.0.7/r9700_tune-6.18.44-Unraid.ko
wget -O apply.sh \
  https://raw.githubusercontent.com/arczewski/r9700-tune/main/module/apply.sh
```

### Usage

```bash
# quiet default: 2000 MHz cap + relaxed fan curve
bash /boot/r9700-tune/apply.sh 2000

# or fully customized (env vars override the defaults)
PPT_OFFSET=-50 \
FAN_TARGET_TEMP=90 \
FAN_ACOUSTIC_TARGET=1000 \
FAN_ACOUSTIC_LIMIT=1300 \
FAN_MIN_PWM=10 \
FAN_CURVE_PWM=10,13,16,22,30,42 \
bash /boot/r9700-tune/apply.sh 1200
```

| Setting | Env var | Meaning |
|---|---|---|
| Max SCLK | positional arg | soft max in MHz (automatic range below it) |
| Power limit offset | `PPT_OFFSET` | percent on the board limit: `-30`=210 W, `-40`=180 W, `-50`=150 W, `-60`=120 W (0 = leave) |
| Fan target temperature | `FAN_TARGET_TEMP` | °C the fan controller aims for (default 85) |
| Acoustic target RPM | `FAN_ACOUSTIC_TARGET` | quiet-mode target RPM (default 1200) |
| Acoustic limit RPM | `FAN_ACOUSTIC_LIMIT` | upper RPM bound for the fan (default 1600) |
| Minimum PWM | `FAN_MIN_PWM` | floor for fan duty, % (default 15) |
| Fan curve | `FAN_CURVE_PWM` | 6 PWM % values at 40/50/60/70/80/90 °C (default `15,20,28,38,50,65`) |

Runtime adjustment without reloading:

```bash
echo 1800 > /sys/module/r9700_tune/parameters/sclk_max   # change clock cap
echo 1    > /sys/module/r9700_tune/parameters/fan_apply  # re-apply OD settings
```

Module parameters: `sclk_max`, `fan_apply` (trigger), `fan_target_temp`,
`acoustic_target_rpm`, `acoustic_limit_rpm`, `fan_min_pwm`,
`fan_curve_pwm`, `ppt_offset`, plus overrides `fn_addr`, `fan_fn_addr`,
`adev_addr` for restricted kernels.

### Reset / safety

```bash
# remove the clock cap (fan/ppt settings remain until reboot)
echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level

# everything back to stock
rmmod r9700_tune && reboot
```

Nothing is persistent. Watch temperatures the first hour after a fan
change - the card is safe up to ~105 °C, keep the hotspot below ~95 °C for
long-term comfort. If it runs hotter than you like, raise the curve.

### Boot persistence (Unraid)

User Scripts -> Add New Script, schedule "At First Array Start":

```bash
#!/bin/bash
PPT_OFFSET=-50 FAN_TARGET_TEMP=90 FAN_ACOUSTIC_TARGET=1000 FAN_ACOUSTIC_LIMIT=1300 FAN_MIN_PWM=10 FAN_CURVE_PWM=10,13,16,22,30,42 bash /boot/r9700-tune/apply.sh 1200
```

The script waits for amdgpu, skips cleanly if the card or module file is
missing, and fails safe on kernel mismatches (stock behavior).

### Kernel updates

The `.ko` is tied to `6.18.44-Unraid`; after an Unraid kernel update it
will refuse to load (safe - card runs stock). Rebuild:

```bash
# build against the matching prebuilt Unraid kernel tree
# (get the URL from https://github.com/ich777/unraid_kernel/releases)
cd module
make -C <extracted-unraid-kernel-tree> M=$PWD modules
```

---

## The CLI (`r9700-tune`)

Static Go binary, no dependencies. Useful for analyzing/repairing
PowerPlay tables on cards where that path works (RDNA1-3, consumer
RDNA4) and for extracting tables from VBIOS/firmware images.

```bash
r9700-tune analyze <pp_table.bin>       # decode a pp_table dump
r9700-tune patch  <pp_table.bin> [flags]  # patch power/clock/fan fields
r9700-tune apply  <pp_table.bin> [flags]  # single-write upload to the GPU
r9700-tune extract-vbios <rom|--pci>      # pull the pp_table from a VBIOS
r9700-tune extract-pptable <fw.bin>       # pull a soft pptable from firmware
```

Important caveats discovered while building it:

- The `pp_table` sysfs read is clamped to `PAGE_SIZE-1` (4095 bytes), so
  dumps of 5812-byte tables are always truncated.
- `cat file > pp_table` fails with `EIO`: the kernel accepts the table
  only as a single write whose length matches the `structuresize` header.
  Use `r9700-tune apply` (or `dd bs=1M`).
- **On Navi 48 the upload is accepted but has no effect** (see table
  above). Use the kernel module for the R9700.

Release assets include `r9700-tune-linux-amd64` and
`r9700-tune-linux-arm64`.

---

## Results on the R9700 (reference numbers)

| Setting | SCLK under load | Power | Temp |
|---|---|---|---|
| stock (210 W cap) | 2350 MHz | ~209 W | 70-75 °C |
| 2000 MHz cap | ~1950 MHz | ~165 W | 38-46 °C (old fan curve) |
| 1200 MHz cap + 150 W limit | ~1200 MHz | ~100-120 W | depends on fan curve |

---

## License

MIT. The kernel module is GPL-2.0 (as required for a kernel module).
