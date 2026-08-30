# r9700-tune

CLI tool to analyze and patch the AMD Radeon AI PRO R9700 (Navi 48, SMU 14.0.2)
PowerPlay table on Linux/Unraid. Single static binary, no dependencies.

## Why this tool exists

On Radeon Pro / AI Pro SKUs, the classic overclocking sysfs node
`pp_od_clk_voltage` is missing. This is not a driver gap: kernel 6.18+ ships a
complete overdrive implementation for Navi 48, but the driver hides it when
`OverDriveLimitsBasicMin.FeatureCtrlMask` or
`OverDriveLimitsBasicMax.FeatureCtrlMask` in the VBIOS PowerPlay table is zero
(`smu_v14_0_2_check_powerplay_table()` sets `smu->od_enabled = false`).
Pro cards ship with those masks zeroed, so no kernel upgrade will expose the
interface.

However, writing a modified table to the `pp_table` sysfs node triggers an
SMU reset that re-initializes the card from the uploaded table. That gives us
safe, fully revertible (reboot = stock) control over:

- **power limit** - `CustomSkuTable.SocketPowerLimitAc/Dc` + `MsgLimits.Power`
- **max SCLK** - `SkuTable.DriverReportedClocks.GameClockAc` (the driver clamps
  the max DPM state to this value and pushes it as the SMU soft max)
- **power1_cap floor** - OD PPT percent trick
  (`BasicMax.FeatureCtrlMask` PPT bit + `BasicMin.Ppt`)
- **fan target temperature / acoustic RPM limit** - SMU fan control config

All offsets are verified against the kernel v6.18 headers
`smu_v14_0_2_pptable.h` and `smu14_driver_if_v14_0.h`.

## Install

Grab a release binary from the
[releases page](https://github.com/arczewski/r9700-tune/releases)
(`r9700-tune-linux-amd64` for Unraid), or build:

```bash
go build -o r9700-tune .
```

## Usage

### Analyze

```bash
cp /sys/class/drm/card0/device/pp_table /tmp/pp.bin
r9700-tune analyze /tmp/pp.bin
```

Prints every relevant field (power limits, DPM config, fan table, OD masks)
and a verdict explaining exactly why the OD interface is hidden on your card.

### Patch

```bash
# cap max SCLK at 2200 MHz and lower the SMU power limit to 170 W
r9700-tune patch /tmp/pp.bin -o patched.bin --max-sclk 2200 --power-limit 170

# apply live (root) - uploads to the SMU and resets it
r9700-tune patch /tmp/pp.bin --apply --max-sclk 2200 --power-limit 170

# lower the power1_cap sysfs floor, then use power1_cap directly
r9700-tune patch /tmp/pp.bin -o patched.bin --ppt-floor 150
cat patched.bin > /sys/class/drm/card0/device/pp_table
echo 150000000 > /sys/class/drm/card0/device/power1_cap
```

Flags: `--max-sclk`, `--power-limit`, `--ppt-floor`, `--fan-target-temp`,
`--acoustic-limit-rpm`, `--unlock-od` (VBIOS-flash prep), `--apply`, `--gpu`,
`--resize`, `-o`.

### Suggested starting values

- `--power-limit 170` alone first - the SMU downclocks automatically under
  load and the fan drops. Most effect for least risk.
- Still too loud: add `--max-sclk 2200` (or 2100).
- Prefer sysfs power control: `--ppt-floor 150`, then
  `echo 150000000 > power1_cap`.
- Fan tweaks only after the above (`--fan-target-temp 82`, watch hotspot).

### Verify under load

```bash
cat /sys/kernel/debug/dri/0000:c6:00.0/amdgpu_pm_info
cat /sys/class/drm/card0/device/pp_dpm_sclk
```

### Unraid persistence

See `contrib/r9700_tune.sh` - install as a user script at first array start.
It uploads the patched table, re-applies the performance level and power cap,
and logs to `/var/log/r9700_tune.log`.

## Safety

- Everything is runtime-only. Delete the patched file / disable the script and
  reboot to return to stock. Keep a dump of the original table.
- The upload triggers a brief SMU reset (a few seconds, no output on a
  headless card).
- The tool refuses writes whose `structuresize` header does not match the
  blob size, keeps it consistent automatically, and can extend a 4096-byte
  dump to the full 5812-byte layout (`--resize 5812` is the default).
- Do not modify `PFE_Settings.FeaturesToRun`, `DpmDescriptor`, `platform_caps`
  or the outer `overdrive_table` caps - this tool does not touch them.

## Why no `pp_od_clk_voltage` after patching?

The sysfs node is created only at driver load. A runtime pp_table upload can
flip `smu->od_enabled`, but the node does not appear until the driver is
reloaded, and reload loses the uploaded table. A permanent OD unlock requires
the modified FeatureCtrlMasks to be present in the VBIOS at boot
(`--unlock-od` prepares those bytes; flashing is out of scope). The table
patches above achieve the same quietness goals without OD.

## License

MIT
