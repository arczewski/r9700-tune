// Command r9700-tune analyzes and patches AMD Navi 48 (SMU 14.0.2)
// PowerPlay tables (Radeon AI PRO R9700 / Radeon RX 9070 series) on Linux.
//
// All offsets were verified against the kernel v6.18 headers:
//
//	drivers/gpu/drm/amd/pm/swsmu/inc/smu_v14_0_2_pptable.h
//	drivers/gpu/drm/amd/pm/swsmu/inc/pmfw_if/smu14_driver_if_v14_0.h
//
// The patch path is runtime-only: writing a modified table to the pp_table
// sysfs node triggers an SMU reset that re-initializes the card from the
// uploaded table. A reboot restores the stock VBIOS table.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

var version = "dev"

// Verified absolute offsets inside the pp_table blob.
const (
	ppDefault = 1344 // smc_pptable offset inside the blob
	skuInPP   = 28   // PPTable_t.SkuTable
	cskuInPP  = 3580 // PPTable_t.CustomSkuTable
	fullSize  = 5812 // sizeof(struct smu_14_0_2_powerplay_table)

	odBitVfCurve  = 0
	odBitPpt      = 3
	odBitFanCurve = 4
	odBitGfxclk   = 8
	odBitUclk     = 9
)

var odFeatureNames = map[int]string{
	0:  "VDDGFX_VF_CURVE (voltage offset / undervolt)",
	1:  "GFX_VMAX",
	2:  "SOC_VMAX",
	3:  "PPT (power limit %)",
	4:  "FAN_CURVE (fan curve / acoustic limit / target temp)",
	5:  "FAN_LEGACY",
	6:  "FULL_CTRL",
	7:  "TDC",
	8:  "GFXCLK (SCLK min/max offset)",
	9:  "UCLK (MCLK min/max)",
	10: "FCLK",
	11: "ZERO_FAN",
	12: "TEMPERATURE",
	13: "EDC",
}

var swFeatureCaps = map[int]string{
	0: "AUTO_FAN_ACOUSTIC_LIMIT",
	1: "POWER_MODE",
	2: "AUTO_UV_ENGINE",
	3: "AUTO_OC_ENGINE",
	4: "AUTO_OC_MEMORY",
	5: "MEMORY_TIMING_TUNE",
	6: "MANUAL_AC_TIMING",
	7: "AUTO_VF_CURVE_OPTIMIZER",
	8: "AUTO_SOC_UV",
}

var pmSettingNames = []string{
	"POWER_LIMIT_QUIET", "POWER_LIMIT_BALANCE", "POWER_LIMIT_TURBO", "POWER_LIMIT_RAGE",
	"ACOUSTIC_TEMP_QUIET", "ACOUSTIC_TEMP_BALANCE", "ACOUSTIC_TEMP_TURBO", "ACOUSTIC_TEMP_RAGE",
	"ACOUSTIC_TARGET_RPM_QUIET", "ACOUSTIC_TARGET_RPM_BALANCE",
	"ACOUSTIC_TARGET_RPM_TURBO", "ACOUSTIC_TARGET_RPM_RAGE",
	"ACOUSTIC_LIMIT_RPM_QUIET", "ACOUSTIC_LIMIT_RPM_BALANCE",
	"ACOUSTIC_LIMIT_RPM_TURBO", "ACOUSTIC_LIMIT_RPM_RAGE",
}

var platformCapNames = map[int]string{
	0: "POWERPLAY", 1: "SBIOSPOWERSOURCE", 2: "HARDWAREDC", 3: "BACO",
	4: "MACO", 5: "SHADOWPSTATE", 6: "LEDSUPPORTED", 7: "MOBILEOVERDRIVE",
}

func u16(b []byte, o int) uint16 {
	if o+2 > len(b) {
		return 0
	}
	return binary.LittleEndian.Uint16(b[o : o+2])
}

func s16(b []byte, o int) int16 {
	return int16(u16(b, o))
}

func u32(b []byte, o int) uint32 {
	if o+4 > len(b) {
		return 0
	}
	return binary.LittleEndian.Uint32(b[o : o+4])
}

func putU16(b []byte, o int, v int) {
	binary.LittleEndian.PutUint16(b[o:o+2], uint16(v))
}

func putS16(b []byte, o int, v int) {
	putU16(b, o, v)
}

func putU32(b []byte, o int, v uint32) {
	binary.LittleEndian.PutUint32(b[o:o+4], v)
}

func has(b []byte, o, n int) bool { return o+n <= len(b) }

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", a...)
	os.Exit(1)
}

func fmtBits(val uint32, names map[int]string) string {
	if val == 0 {
		return fmt.Sprintf("  0x%08x  (NONE)", val)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "  0x%08x", val)
	bits := make([]int, 0, len(names))
	for bit := range names {
		bits = append(bits, bit)
	}
	sort.Ints(bits)
	for _, bit := range bits {
		if val&(1<<bit) != 0 {
			fmt.Fprintf(&sb, "\n      bit %2d: %s", bit, names[bit])
		}
	}
	return sb.String()
}

func dumpODLimits(b []byte, base int, label string) {
	fmt.Printf("  --- %s (blob offset 0x%04x) ---\n", label, base)
	if !has(b, base, 96) {
		fmt.Println("      NOT PRESENT IN THIS BLOB (blob smaller than struct)\n")
		return
	}
	fmt.Println("      FeatureCtrlMask:")
	fmt.Println(fmtBits(u32(b, base), odFeatureNames))
	if u32(b, base) != 0 {
		volt := make([]int, 6)
		for i := range volt {
			volt[i] = int(s16(b, base+4+2*i))
		}
		fmt.Printf("      VoltageOffsetPerZoneBoundary[0..5] (mV): %v\n", volt)
		fmt.Printf("      VddGfxVmax: %d mV   VddSocVmax: %d mV\n", u16(b, base+16), u16(b, base+18))
		fmt.Printf("      GfxclkFoffset: %d MHz\n", s16(b, base+20))
		fmt.Printf("      UclkFmin: %d MHz   UclkFmax: %d MHz\n", u16(b, base+24), u16(b, base+26))
		fmt.Printf("      FclkFmin: %d MHz   FclkFmax: %d MHz\n", u16(b, base+28), u16(b, base+30))
		fmt.Printf("      Ppt: %d%%   Tdc: %d%%\n", s16(b, base+32), s16(b, base+34))
		fmt.Printf("      FanLinearPwmPoints: %v\n", b[base+36:base+42])
		fmt.Printf("      FanLinearTempPoints (C): %v\n", b[base+42:base+48])
		fmt.Printf("      FanMinimumPwm: %d%%\n", u16(b, base+48))
		fmt.Printf("      AcousticTargetRpmThreshold: %d RPM\n", u16(b, base+50))
		fmt.Printf("      AcousticLimitRpmThreshold: %d RPM\n", u16(b, base+52))
		fmt.Printf("      FanTargetTemperature: %d C\n", u16(b, base+54))
		fmt.Printf("      FanZeroRpmEnable: %d   MaxOpTemp: %d C\n", b[base+56], b[base+57])
		fmt.Printf("      GfxclkFullCtrlMode: %d   UclkFullCtrlMode: %d\n", u16(b, base+64), u16(b, base+66))
	}
	fmt.Println()
}

func analyze(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) < 64 {
		return fmt.Errorf("blob too small to be a powerplay table (%d bytes)", len(b))
	}

	fmt.Printf("File: %s  size: %d bytes\n", path, len(b))
	fmt.Printf("header.structuresize = %d  (actual file size %d)\n", u16(b, 0), len(b))
	fmt.Printf("header.format_revision = %d   content_revision = %d\n", b[2], b[3])

	pp := int(u16(b, 6))
	if pp == 0 {
		pp = ppDefault
	}
	fmt.Printf("pmfw_pptable_start_offset = %d  (expected %d)\n", pp, ppDefault)
	fmt.Printf("pmfw_pptable_size = %d\n", u16(b, 8))

	caps := u32(b, 32)
	fmt.Printf("\nplatform_caps: 0x%08x\n", caps)
	capBits := []int{0, 1, 2, 3, 4, 5, 6, 7}
	for _, bit := range capBits {
		if caps&(1<<bit) != 0 {
			fmt.Printf("   + %s\n", platformCapNames[bit])
		}
	}
	fmt.Printf("thermal_controller_type: %d\n", b[36])
	fmt.Printf("small_power_limit1: %d W   small_power_limit2: %d W\n", u16(b, 37), u16(b, 39))
	fmt.Printf("boost_power_limit: %d W   software_shutdown_temp: %d C\n", u16(b, 41), u16(b, 43))

	odt := 188
	cap0 := uint32(0)
	cap1 := uint32(0)
	if has(b, odt+4, 64) {
		cap0 = binary.LittleEndian.Uint32(b[odt+4 : odt+8])
		cap1 = binary.LittleEndian.Uint32(b[odt+36 : odt+40])
	}
	fmt.Printf("\nouter overdrive_table @ 0x%04x:\n", odt)
	fmt.Println("  cap[0] (basic):", fmtBits(cap0, swFeatureCaps))
	fmt.Println("  cap[1] (advanced):", fmtBits(cap1, swFeatureCaps))
	fmt.Println("  pm_setting (power-mode presets, int16):")
	for i := 0; i < 16; i += 4 {
		var parts []string
		for j := 0; j < 4; j++ {
			name := strings.ReplaceAll(strings.ToTitle(pmSettingNames[i+j]), "_", " ")
			val := int(s16(b, odt+1092+2*(i+j)))
			parts = append(parts, fmt.Sprintf("%s: %d", name, val))
		}
		fmt.Println("     ", strings.Join(parts, "  "))
	}

	sku := pp + skuInPP
	csku := pp + cskuInPP

	fmt.Printf("\n=== PPTable_t @ 0x%04x (SkuTable @ 0x%04x, CustomSkuTable @ 0x%04x) ===\n", pp, sku, csku)
	fmt.Printf("\nSkuTable.Version: 0x%08x\n", u32(b, sku))

	drc := sku + 2576
	if has(b, drc, 16) {
		fmt.Println("\nDriverReportedClocks (driver's clock caps):")
		fmt.Printf("  BaseClockAc: %d MHz   GameClockAc: %d MHz   BoostClockAc: %d MHz\n",
			u16(b, drc), u16(b, drc+2), u16(b, drc+4))
		fmt.Printf("  BaseClockDc: %d MHz   GameClockDc: %d MHz   BoostClockDc: %d MHz\n",
			u16(b, drc+6), u16(b, drc+8), u16(b, drc+10))
		fmt.Printf("  MaxReportedClock: %d MHz\n", u16(b, drc+12))
		fmt.Println("  NOTE: the driver clamps the max SCLK DPM state to GameClockAc.")
		fmt.Println("        Patching GameClockAc is the safe way to cap the max GPU clock.")
	} else {
		fmt.Println("DriverReportedClocks NOT PRESENT in blob")
	}

	mlim := sku + 2604
	if has(b, mlim, 16) {
		fmt.Println("\nMsgLimits.Power (AC/DC, PPT0..PPT3) - caps power1_cap_max:")
		for t := 0; t < 4; t++ {
			fmt.Printf("  PPT%d: AC %d W   DC %d W\n", t, u16(b, mlim+4*t), u16(b, mlim+4*t+2))
		}
	} else {
		fmt.Println("MsgLimits NOT PRESENT in blob")
	}

	ft := sku + 568
	if has(b, ft, 32) {
		freqs := make([]int, 16)
		var nz []int
		for i := range freqs {
			freqs[i] = int(u16(b, ft+2*i))
			if freqs[i] != 0 {
				nz = append(nz, freqs[i])
			}
		}
		fmt.Printf("\nSkuTable.FreqTableGfx[16]: %v\n", freqs)
		fmt.Printf("  non-zero entries: %v\n", nz)
	} else {
		fmt.Println("FreqTableGfx NOT PRESENT in blob")
	}

	dd := sku + 216
	if has(b, dd, 11*32) {
		fmt.Println("\nDpmDescriptor[11] (SnapToDiscrete / NumDiscreteLevels):")
		for i := 0; i < 11; i++ {
			o := dd + 32*i
			fmt.Printf("  [%2d] snap=%d levels=%d\n", i, b[o+1], b[o+2])
		}
	} else {
		fmt.Println("DpmDescriptor NOT PRESENT in blob")
	}

	dumpODLimits(b, sku+2720, "SkuTable.OverDriveLimitsBasicMin")
	dumpODLimits(b, sku+2816, "SkuTable.OverDriveLimitsBasicMax")
	dumpODLimits(b, sku+2912, "SkuTable.OverDriveLimitsAdvancedMin")
	dumpODLimits(b, sku+3008, "SkuTable.OverDriveLimitsAdvancedMax")

	fmt.Println("=== CustomSkuTable (SMU power + fan config) ===")
	if !has(b, csku, 300) {
		fmt.Println("  CustomSkuTable NOT PRESENT in this blob (SMU fields beyond blob size).")
		fmt.Println("  If your pp_table file is 4096 bytes, all SMU power/fan fields below")
		fmt.Println("  are absent and read as zero by the driver.")
	} else {
		ac := make([]int, 4)
		dc := make([]int, 4)
		for i := 0; i < 4; i++ {
			ac[i] = int(u16(b, csku+2*i))
			dc[i] = int(u16(b, csku+276+2*i))
		}
		fmt.Printf("  SocketPowerLimitAc[0..3]: %v W\n", ac)
		fmt.Printf("  SocketPowerLimitDc[0..3]: %v W\n", dc)
		fmt.Printf("  VrTdcLimit: [%d, %d] A\n", u16(b, csku+8), u16(b, csku+10))
		tl := make([]int, 12)
		fanT := make([]int, 12)
		ctf := make([]int, 12)
		for i := 0; i < 12; i++ {
			tl[i] = int(u16(b, csku+20+2*i))
			fanT[i] = int(u16(b, csku+136+2*i))
			ctf[i] = int(u16(b, csku+168+2*i))
		}
		fmt.Printf("  TemperatureLimit[0..11]: %v\n", tl)
		fmt.Printf("  FanTargetTemperature[0..11]: %v\n", fanT)
		fmt.Printf("  FwCtfLimit[0..11]: %v\n", ctf)
		fmt.Printf("  FanPwmMin: %d%%\n", u16(b, csku+116))
		fmt.Printf("  AcousticTargetRpmThreshold: %d RPM\n", u16(b, csku+118))
		fmt.Printf("  AcousticLimitRpmThreshold: %d RPM\n", u16(b, csku+120))
		fmt.Printf("  FanMaximumRpm: %d RPM\n", u16(b, csku+122))
		fmt.Printf("  FanTargetGfxclk: %d MHz\n", u16(b, csku+126))
		fmt.Printf("  FanZeroRpmEnable: %d\n", b[csku+132])
		fmt.Printf("  PlatformTdcLimit: [%d, %d] A\n", u16(b, csku+272), u16(b, csku+274))
		fmt.Printf("  SocketPowerLimitSmartShift2: %d W\n", u16(b, csku+284))
	}

	fmt.Println("\n=== Verdict ===")
	bminMask := u32(b, sku+2720)
	bmaxMask := u32(b, sku+2816)
	if !has(b, sku+2720, 4) {
		fmt.Println("OverDriveLimitsBasicMin absent (blob < 4096 bytes). Driver reads 0.")
		fmt.Println("=> smu->od_enabled = false => pp_od_clk_voltage hidden. Confirmed cause.")
	} else if bminMask == 0 || bmaxMask == 0 {
		fmt.Printf("FeatureCtrlMask: BasicMin=0x%08x BasicMax=0x%08x\n", bminMask, bmaxMask)
		fmt.Println("=> At least one mask is zero, so the driver sets smu->od_enabled = false")
		fmt.Println("=> pp_od_clk_voltage is hidden. THIS is why the OD interface is missing.")
		fmt.Println("=> A newer kernel will NOT fix this; the gate reads VBIOS data.")
	} else {
		fmt.Println("Both OD FeatureCtrlMasks are non-zero - OD should be exposed.")
		fmt.Println("If pp_od_clk_voltage is still missing, the running kernel differs from")
		fmt.Println("mainline v6.18; recheck with uname -r / driver version.")
	}

	pptMin := 0
	if has(b, sku+2720+32, 2) {
		pptMin = int(s16(b, sku+2720+32))
	}
	if bmaxMask&(1<<odBitPpt) != 0 {
		fmt.Printf("\nPPT OD bit is set in BasicMax, BasicMin.Ppt = %d%%\n", pptMin)
		fmt.Printf("=> expected power1_cap_min = current_limit * (100%%%+d)/100\n", pptMin)
		fmt.Printf("   e.g. 300 W limit -> min %d W\n", 300*(100+pptMin)/100)
	} else {
		fmt.Println("\nPPT OD bit NOT set in BasicMax - power1_cap range is not OD-extended.")
	}

	if has(b, drc, 16) {
		g := int(u16(b, drc+2))
		fmt.Printf("\nEffective max SCLK the driver will set: %d MHz (GameClockAc).\n", g)
		fmt.Println("Patch this to 2200 (or 2100) to cap the automatic clock range.")
	}
	return nil
}

func clamp(v, lo, hi int, what string) int {
	if v < lo || v > hi {
		fatal("%s %d outside sane range [%d, %d]", what, v, lo, hi)
	}
	return v
}

func atoi(s, what string) int {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		fatal("invalid value for %s: %q", what, s)
	}
	return v
}

func patch(args []string) error {
	outName := ""
	apply := false
	unlockOD := false
	gpu := "/sys/class/drm/card0/device"
	maxSclk := 0
	powerLimit := 0
	pptFloor := 0
	fanTarget := 0
	acousticLimit := 0
	resize := 0
	input := ""

	nextVal := func(a, flagName string, i *int) string {
		if *i+1 >= len(args) {
			fatal("missing value for %s", flagName)
		}
		*i++
		return args[*i]
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "--out":
			outName = nextVal(a, a, &i)
		case strings.HasPrefix(a, "--out="):
			outName = strings.TrimPrefix(a, "--out=")
		case a == "-o=" || strings.HasPrefix(a, "-o="):
			outName = a[3:]
		case a == "--gpu":
			gpu = nextVal(a, a, &i)
		case strings.HasPrefix(a, "--gpu="):
			gpu = strings.TrimPrefix(a, "--gpu=")
		case a == "--max-sclk":
			maxSclk = atoi(nextVal(a, a, &i), a)
		case strings.HasPrefix(a, "--max-sclk="):
			maxSclk = atoi(strings.TrimPrefix(a, "--max-sclk="), "--max-sclk")
		case a == "--power-limit":
			powerLimit = atoi(nextVal(a, a, &i), a)
		case strings.HasPrefix(a, "--power-limit="):
			powerLimit = atoi(strings.TrimPrefix(a, "--power-limit="), "--power-limit")
		case a == "--ppt-floor":
			pptFloor = atoi(nextVal(a, a, &i), a)
		case strings.HasPrefix(a, "--ppt-floor="):
			pptFloor = atoi(strings.TrimPrefix(a, "--ppt-floor="), "--ppt-floor")
		case a == "--fan-target-temp":
			fanTarget = atoi(nextVal(a, a, &i), a)
		case strings.HasPrefix(a, "--fan-target-temp="):
			fanTarget = atoi(strings.TrimPrefix(a, "--fan-target-temp="), "--fan-target-temp")
		case a == "--acoustic-limit-rpm":
			acousticLimit = atoi(nextVal(a, a, &i), a)
		case strings.HasPrefix(a, "--acoustic-limit-rpm="):
			acousticLimit = atoi(strings.TrimPrefix(a, "--acoustic-limit-rpm="), "--acoustic-limit-rpm")
		case a == "--resize":
			resize = atoi(nextVal(a, a, &i), a)
		case strings.HasPrefix(a, "--resize="):
			resize = atoi(strings.TrimPrefix(a, "--resize="), "--resize")
		case a == "--apply":
			apply = true
		case a == "--unlock-od":
			unlockOD = true
		case a == "-h" || a == "--help":
			usage()
			os.Exit(0)
		case strings.HasPrefix(a, "-") && a != "-":
			fatal("unknown flag %s", a)
		default:
			if input != "" {
				fatal("multiple input files: %s and %s", input, a)
			}
			input = a
		}
	}

	if input == "" {
		return fmt.Errorf("usage: r9700-tune patch [flags] <pp_table.bin>")
	}

	if maxSclk == 0 && powerLimit == 0 && pptFloor == 0 && fanTarget == 0 &&
		acousticLimit == 0 && !unlockOD {
		return fmt.Errorf("nothing to do - use -h for flags")
	}

	b, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	origLen := len(b)
	structureSize := int(u16(b, 0))
	pp := int(u16(b, 6))
	if pp == 0 {
		pp = ppDefault
	}
	sku := pp + skuInPP
	csku := pp + cskuInPP
	fmt.Printf("input %s: %d bytes, structuresize=%d, pp@0x%x\n", input, origLen, structureSize, pp)

	// Highest offset any selected patch will touch.
	needed := 0
	if maxSclk != 0 {
		needed = max(needed, sku+2576+6) // BoostClockAc
	}
	if powerLimit != 0 {
		needed = max(needed, csku+284) // SocketPowerLimitSmartShift2
		needed = max(needed, sku+2604+16)
	}
	if pptFloor != 0 {
		needed = max(needed, sku+2816+4)
	}
	if fanTarget != 0 {
		needed = max(needed, csku+136+24)
	}
	if acousticLimit != 0 {
		needed = max(needed, csku+122)
	}
	if unlockOD {
		needed = max(needed, sku+2816+96)
	}

	if needed > len(b) {
		if resize > len(b) && resize >= needed {
			fmt.Printf("WARNING: patches need table offset %d but the blob is %d bytes.\n", needed, len(b))
			fmt.Printf("Extending blob to %d bytes. The SMU may reject an extended table on some\ncards; test carefully and check dmesg after applying.\n", resize)
			b = append(b, make([]byte, resize-len(b))...)
			putU16(b, 0, resize)
			fmt.Printf("structuresize updated %d -> %d\n", structureSize, resize)
			structureSize = resize
		} else {
			fatal("patches need table offset %d but the blob is only %d bytes.\n"+
				"Options:\n"+
				"  - apply only fields that exist in this dump (e.g. --max-sclk alone)\n"+
				"  - or pass --resize %d to attempt an extended-table upload", needed, len(b), fullSize)
		}
	}

	var changes []string
	need := func(off, n int, what string) {
		if off+n > len(b) {
			fatal("%s at offset %d is beyond the blob size (%d bytes). Re-dump the pp_table file and retry; if the dump really is this small, extend it first with --resize %d and re-run.", what, off, len(b), fullSize)
		}
	}

	if maxSclk != 0 {
		maxSclk = clamp(maxSclk, 500, 3000, "max-sclk")
		drc := sku + 2576
		game := drc + 2
		need(game, 2, "GameClockAc")
		old := int(u16(b, game))
		if old == 0 {
			fatal("GameClockAc is 0 in this table - cannot cap. Run analyze first.")
		}
		putU16(b, game, maxSclk)
		boost := drc + 4
		if int(u16(b, boost)) > maxSclk {
			putU16(b, boost, maxSclk)
		}
		changes = append(changes, fmt.Sprintf("max SCLK cap: %d -> %d MHz (GameClockAc)", old, maxSclk))
		ft := sku + 568
		if has(b, ft, 32) {
			var clamped []int
			for i := 0; i < 16; i++ {
				f := int(u16(b, ft+2*i))
				if f != 0 && f > maxSclk {
					putU16(b, ft+2*i, maxSclk)
					clamped = append(clamped, f)
				}
			}
			if len(clamped) > 0 {
				changes = append(changes, fmt.Sprintf("FreqTableGfx entries %v clamped to %d MHz", clamped, maxSclk))
			}
		}
	}

	if powerLimit != 0 {
		powerLimit = clamp(powerLimit, 100, 400, "power-limit")
		type lim struct {
			label string
			off   int
			cnt   int
		}
		for _, l := range []lim{
			{"SocketPowerLimitAc", csku + 0, 4},
			{"SocketPowerLimitDc", csku + 276, 4},
		} {
			need(l.off, 2*l.cnt, l.label)
			var old []int
			nonzero := false
			for i := 0; i < l.cnt; i++ {
				v := int(u16(b, l.off+2*i))
				old = append(old, v)
				if v != 0 {
					nonzero = true
				}
			}
			if nonzero {
				for i := 0; i < l.cnt; i++ {
					if old[i] != 0 {
						putU16(b, l.off+2*i, powerLimit)
					}
				}
				changes = append(changes, fmt.Sprintf("%s %v -> %d W", l.label, old, powerLimit))
			}
		}
		mlim := sku + 2604
		need(mlim, 16, "MsgLimits.Power")
		for t := 0; t < 4; t++ {
			for _, src := range []int{0, 2} {
				v := int(u16(b, mlim+4*t+src))
				if v != 0 && v > powerLimit {
					putU16(b, mlim+4*t+src, powerLimit)
					side := "AC"
					if src == 2 {
						side = "DC"
					}
					changes = append(changes, fmt.Sprintf("MsgLimits.Power[PPT%d] %s %d -> %d W", t, side, v, powerLimit))
				}
			}
		}
		for _, off := range []int{37, 39} { // small_power_limit1/2
			v := int(u16(b, off))
			if v != 0 && v > powerLimit {
				putU16(b, off, powerLimit)
				changes = append(changes, fmt.Sprintf("small_power_limit@%d %d -> %d W", off, v, powerLimit))
			}
		}
	}

	if pptFloor != 0 {
		pptFloor = clamp(pptFloor, 60, 400, "ppt-floor")
		cur := 0
		if has(b, csku, 2) {
			cur = int(u16(b, csku))
		}
		if cur == 0 && has(b, sku+2604, 2) {
			cur = int(u16(b, sku+2604))
		}
		if cur == 0 {
			cur = 300
			fmt.Printf("warning: could not determine current limit, assuming %d W\n", cur)
		}
		if pptFloor >= cur {
			fatal("ppt-floor (%d) must be below the board limit (%d W)", pptFloor, cur)
		}
		pct := int(float64(pptFloor-cur)/float64(cur)*100 + 0.5)
		if pptFloor-cur < 0 {
			pct = int(float64(pptFloor-cur)/float64(cur)*100 - 0.5)
		}
		bmax := sku + 2816
		bmin := sku + 2720
		need(bmax, 4, "OverDriveLimitsBasicMax.FeatureCtrlMask")
		need(bmin+32, 2, "OverDriveLimitsBasicMin.Ppt")
		putU32(b, bmax, u32(b, bmax)|(1<<odBitPpt))
		oldPct := int(s16(b, bmin+32))
		putS16(b, bmin+32, pct)
		changes = append(changes, fmt.Sprintf("PPT floor: BasicMin.Ppt %d%% -> %d%% (power1_cap floor becomes ~%d W with %d W limit)", oldPct, pct, pptFloor, cur))
		changes = append(changes, "BasicMax.FeatureCtrlMask |= PPT bit")
	}

	if fanTarget != 0 {
		fanTarget = clamp(fanTarget, 40, 100, "fan-target-temp")
		off := csku + 136
		need(off, 24, "FanTargetTemperature")
		var old []int
		nonzero := false
		for i := 0; i < 12; i++ {
			v := int(u16(b, off+2*i))
			old = append(old, v)
			if v != 0 {
				nonzero = true
			}
		}
		if !nonzero {
			fatal("FanTargetTemperature is all zeros in this table; skipping.")
		}
		set := map[int]bool{}
		for i := 0; i < 12; i++ {
			if old[i] != 0 {
				set[old[i]] = true
				putU16(b, off+2*i, fanTarget)
			}
		}
		keys := make([]int, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		changes = append(changes, fmt.Sprintf("FanTargetTemperature %v -> %d C", keys, fanTarget))
	}

	if acousticLimit != 0 {
		acousticLimit = clamp(acousticLimit, 500, 4000, "acoustic-limit-rpm")
		off := csku + 120
		need(off, 2, "AcousticLimitRpmThreshold")
		old := int(u16(b, off))
		if old == 0 {
			fatal("AcousticLimitRpmThreshold is 0 in this table; skipping.")
		}
		putU16(b, off, acousticLimit)
		changes = append(changes, fmt.Sprintf("AcousticLimitRpmThreshold %d -> %d RPM", old, acousticLimit))
	}

	if unlockOD {
		mask := uint32((1 << odBitVfCurve) | (1 << odBitPpt) | (1 << odBitFanCurve) |
			(1 << odBitGfxclk) | (1 << odBitUclk))
		bmin := sku + 2720
		bmax := sku + 2816
		need(bmax+95, 1, "OverDriveLimitsBasicMax")
		putU32(b, bmin, u32(b, bmin)|mask)
		putU32(b, bmax, u32(b, bmax)|mask)
		putS16(b, bmin+20, -600)
		putS16(b, bmax+20, 100)
		for i := 0; i < 6; i++ {
			putS16(b, bmin+4+2*i, -80)
			putS16(b, bmax+4+2*i, 0)
		}
		if int(s16(b, bmin+32)) > -50 {
			putS16(b, bmin+32, -50)
		}
		putS16(b, bmax+32, 0)
		putU16(b, bmin+24, 0)
		putU16(b, bmax+24, 0)
		putU16(b, bmin+26, 0)
		putU16(b, bmax+26, 2600)
		changes = append(changes, "OD unlocked: FeatureCtrlMask bits (VF_CURVE, PPT, FAN_CURVE, GFXCLK, UCLK) set in BasicMin+BasicMax with limits")
	}

	if len(changes) == 0 {
		return fmt.Errorf("no fields changed - check your inputs")
	}

	if outName == "" {
		outName = "patched_pp_table.bin"
	}
	if err := os.WriteFile(outName, b, 0o644); err != nil {
		return err
	}
	fmt.Println("\nChanges:")
	for _, c := range changes {
		fmt.Printf("  - %s\n", c)
	}
	fmt.Printf("\nWrote %s (%d bytes)\n", outName, len(b))

	if apply {
		if os.Geteuid() != 0 {
			fatal("--apply needs root")
		}
		sysfs := fmt.Sprintf("%s/pp_table", strings.TrimRight(gpu, "/"))
		if err := os.WriteFile(sysfs, b, 0o644); err != nil {
			return fmt.Errorf("upload to %s failed: %w", sysfs, err)
		}
		fmt.Printf("Uploaded to %s - SMU reset, then re-apply performance level:\n", sysfs)
		fmt.Printf("  echo auto > %s/power_dpm_force_performance_level\n", gpu)
		fmt.Printf("  echo 210000000 > %s/power1_cap   (or your chosen value)\n", gpu)
	} else {
		fmt.Println("Apply it with (as root):")
		fmt.Printf("  r9700-tune apply %s --power-cap 200\n", outName)
		fmt.Println("  (never use cat/echo redirection for the binary table - the kernel")
		fmt.Println("   needs the whole table in one write syscall)")
		fmt.Println("Revert any time by rebooting (runtime-only change).")
	}
	return nil
}

func applyTable(args []string) error {
	gpu := "/sys/class/drm/card0/device"
	powerCap := 0
	perfLevel := "auto"
	input := ""

	nextVal := func(a string, i *int) string {
		if *i+1 >= len(args) {
			fatal("missing value for %s", a)
		}
		*i++
		return args[*i]
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--gpu":
			gpu = nextVal(a, &i)
		case strings.HasPrefix(a, "--gpu="):
			gpu = strings.TrimPrefix(a, "--gpu=")
		case a == "--power-cap":
			powerCap = atoi(nextVal(a, &i), a)
		case strings.HasPrefix(a, "--power-cap="):
			powerCap = atoi(strings.TrimPrefix(a, "--power-cap="), "--power-cap")
		case a == "--perf-level":
			perfLevel = nextVal(a, &i)
		case strings.HasPrefix(a, "--perf-level="):
			perfLevel = strings.TrimPrefix(a, "--perf-level=")
		case a == "-h" || a == "--help":
			usage()
			os.Exit(0)
		case strings.HasPrefix(a, "-") && a != "-":
			fatal("unknown flag %s", a)
		default:
			if input != "" {
				fatal("multiple input files: %s and %s", input, a)
			}
			input = a
		}
	}

	if input == "" {
		return fmt.Errorf("usage: r9700-tune apply [flags] <pp_table.bin>")
	}

	b, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		fatal("apply needs root")
	}
	if len(b) < 2 || int(u16(b, 0)) != len(b) {
		fatal("refusing: structuresize header (%d) does not match file size (%d). The kernel would reject it with EIO.", u16(b, 0), len(b))
	}

	// Single write(2) syscall - the kernel validates the whole table in one
	// write, so never use cat/echo redirection for the binary table.
	sysfs := fmt.Sprintf("%s/pp_table", strings.TrimRight(gpu, "/"))
	if err := os.WriteFile(sysfs, b, 0o644); err != nil {
		return fmt.Errorf("upload failed: %w\ncheck dmesg: 'pp table size not matched' means the write was chunked; 'smu reset failed' means the SMU rejected the table", err)
	}
	fmt.Printf("Uploaded %s (%d bytes) - SMU reset done.\n", input, len(b))

	time.Sleep(2 * time.Second)

	if err := os.WriteFile(gpu+"/power_dpm_force_performance_level", []byte(perfLevel), 0o644); err != nil {
		fmt.Printf("warning: could not set performance level %q: %v\n", perfLevel, err)
	} else {
		fmt.Printf("Performance level set to %q.\n", perfLevel)
	}

	if powerCap > 0 {
		val := fmt.Sprintf("%d", powerCap*1000000)
		if err := os.WriteFile(gpu+"/power1_cap", []byte(val), 0o644); err != nil {
			fmt.Printf("warning: could not set power1_cap: %v\n", err)
		} else {
			fmt.Printf("power1_cap set to %d W.\n", powerCap)
		}
	}
	return nil
}

func usage() {
	fmt.Printf(`r9700-tune %s - AMD Radeon AI PRO R9700 (Navi 48 / SMU 14.0.2) PowerPlay table tool

Usage:
  r9700-tune analyze <pp_table.bin>          decode and explain a pp_table dump
  r9700-tune patch [flags] <pp_table.bin>    patch power/clock/fan values
  r9700-tune apply [flags] <pp_table.bin>    upload a table to the GPU (single write)
  r9700-tune version                         print version

Apply flags:
  --gpu <path>       GPU sysfs path (default /sys/class/drm/card0/device)
  --power-cap <W>    also set power1_cap after upload (e.g. 200)
  --perf-level <lvl> performance level to re-apply (default auto)

Patch flags:
  -o <file>                 output file (default: patched_pp_table.bin)
  --apply                   also upload to the GPU sysfs pp_table node (root)
  --gpu <path>              GPU sysfs path for --apply
  --max-sclk <MHz>          cap max SCLK via GameClockAc (e.g. 2200)
  --power-limit <W>         lower SMU board power limit (e.g. 170)
  --ppt-floor <W>           lower the power1_cap sysfs floor (e.g. 150)
  --fan-target-temp <C>     SMU fan target temperature (e.g. 82)
  --acoustic-limit-rpm <N>  SMU acoustic RPM limit (e.g. 1500)
  --unlock-od               set OD FeatureCtrlMasks (VBIOS-flash prep)
  --resize <N>              extend a short dump to N bytes before patching
                            (default: never extend; use 5812 to try a full table)

All patches are runtime-only (pp_table upload + SMU reset). Reboot = revert.
`, version, fullSize)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "analyze", "analyse":
		if len(os.Args) < 3 {
			fmt.Println("usage: r9700-tune analyze <pp_table.bin>")
			os.Exit(1)
		}
		err = analyze(os.Args[2])
	case "patch":
		err = patch(os.Args[2:])
	case "apply":
		err = applyTable(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("r9700-tune %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fatal("%v", err)
	}
}
