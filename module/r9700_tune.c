// SPDX-License-Identifier: GPL-2.0
/*
 * r9700_tune.c - SCLK soft-max cap + fan curve / acoustic control for the
 * AMD Radeon AI PRO R9700 (Navi 48, SMU 14.0.2).
 *
 * The card exposes no usable fan or clock-range controls from userspace:
 * OD is masked, pp_table uploads are ignored, and sysfs/debugfs can only
 * pin constant clocks. This module talks to the driver's internal
 * functions directly:
 *
 *  - amdgpu_dpm_set_soft_freq_range(adev, PP_SCLK, 0, max)
 *      -> SMU_MSG_SetSoftMaxByFreq: automatic range with a lower max
 *  - smu_v14_0_2_upload_overdrive_table(smu, od_table)
 *      -> fan curve points, fan target temperature, acoustic target/limit
 *         RPM, minimum PWM (FeatureCtrlMask bit 4 = FAN_CURVE)
 *
 * Non-exported functions are resolved via kallsyms (kprobe on
 * kallsyms_lookup_name). The amdgpu_device pointer is captured with a
 * kprobe on amdgpu_dpm_force_performance_level (arg0), the smu_context
 * pointer with a kprobe on smu_sys_get_pp_table (arg0).
 *
 * Struct offsets were computed against the Unraid 6.18.44 kernel tree:
 *   offsetof(struct smu_context, smu_table)                = 128
 *   offsetof(struct smu_table_context, overdrive_table)    = 2016
 *   OverDriveTable_t: FeatureCtrlMask 0, FanLinearPwmPoints 40,
 *     FanLinearTempPoints 46, FanMinimumPwm 52,
 *     AcousticTargetRpmThreshold 54, AcousticLimitRpmThreshold 56,
 *     FanTargetTemperature 58, FanMode 62, size 156
 *
 * Usage:
 *   insmod r9700_tune.ko sclk_max=2000 \
 *       fan_target_temp=85 acoustic_target_rpm=1200 \
 *       acoustic_limit_rpm=1500 fan_min_pwm=15 \
 *       fan_curve_pwm=15,20,28,38,50,65
 *   echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level
 *   cat /sys/class/drm/card0/device/pp_table > /dev/null
 *   echo 2000 > /sys/module/r9700_tune/parameters/sclk_max
 *   echo 1    > /sys/module/r9700_tune/parameters/fan_apply
 *
 * Reset: echo auto > power_dpm_force_performance_level (clocks) + reboot
 * (fan settings) - nothing here persists.
 */
#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/kprobes.h>
#include <linux/unaligned.h>

#define PP_SCLK 0 /* enum pp_clock_type (kgd_pp_interface.h) */
#define PP_MCLK 1

/* verified offsets (Unraid 6.18.44) */
#define SMU_TABLE_OFS       128
#define OVERDRIVE_TABLE_OFS 2016
#define OD_SIZE             156
#define OD_BIT_FAN_CURVE    4
#define OD_BIT_PPT          3
#define OD_BIT_VF_CURVE     0

static unsigned int sclk_max;
static unsigned int mclk_max;
static unsigned long long fn_addr;    /* amdgpu_dpm_set_soft_freq_range */
static unsigned long long adev_addr;  /* amdgpu_device */
static unsigned long long fan_fn_addr;/* smu_v14_0_2_upload_overdrive_table */
static void *adev_ptr;
static void *smu_ptr;

static unsigned int fan_target_temp;
static unsigned int acoustic_target_rpm;
static unsigned int acoustic_limit_rpm;
static unsigned int fan_min_pwm;
static char *fan_curve_pwm = "";
static int ppt_offset; /* percent, applied via the OD Ppt field */
static int vddgfx_offset; /* mV, applied via the OD VF curve offset */

typedef int (*set_soft_freq_range_fn)(void *adev, int type, u32 min, u32 max);
typedef int (*upload_overdrive_fn)(void *smu, void *od_table);

static set_soft_freq_range_fn sfr_fn;
static upload_overdrive_fn upload_fn;

/* kallsyms_lookup_name is no longer exported; call it through a kprobe. */
static unsigned long lookup_symbol(const char *name)
{
#ifdef CONFIG_KPROBES
	struct kprobe kp = { .symbol_name = "kallsyms_lookup_name" };
	unsigned long (*fn)(const char *name);
	unsigned long addr = 0;

	if (register_kprobe(&kp) < 0)
		return 0;
	if (kp.addr) {
		fn = (void *)kp.addr;
		addr = fn(name);
	}
	unregister_kprobe(&kp);
	return addr;
#else
	return 0;
#endif
}

#ifdef CONFIG_KPROBES
/* amdgpu_dpm_force_performance_level(adev, level): capture arg0 (RDI). */
static int cap_adev_pre(struct kprobe *p, struct pt_regs *regs);
static struct kprobe cap_adev_kp = {
	.symbol_name = "amdgpu_dpm_force_performance_level",
	.pre_handler = cap_adev_pre,
};

/* smu_sys_get_pp_table(handle, table): capture arg0 (RDI) = smu_context. */
static int cap_smu_pre(struct kprobe *p, struct pt_regs *regs);
static struct kprobe cap_smu_kp = {
	.symbol_name = "smu_sys_get_pp_table",
	.pre_handler = cap_smu_pre,
};

static bool cap_adev_armed, cap_smu_armed;

static int cap_adev_pre(struct kprobe *p, struct pt_regs *regs)
{
#if defined(CONFIG_X86_64)
	if (!adev_ptr)
		adev_ptr = (void *)regs->di;
#endif
	return 0;
}

static int cap_smu_pre(struct kprobe *p, struct pt_regs *regs)
{
#if defined(CONFIG_X86_64)
	if (!smu_ptr)
		smu_ptr = (void *)regs->di;
#endif
	return 0;
}
#endif

/* ------------------------------- clocks ------------------------------ */

static int apply_clk_range(unsigned int max_mhz, int type, const char *what)
{
	int ret;

	if (!max_mhz)
		return 0;
	if (!sfr_fn) {
		pr_err("r9700_tune: amdgpu_dpm_set_soft_freq_range not resolved\n");
		return -EIO;
	}
	if (!adev_ptr) {
		pr_info("r9700_tune: device not captured yet - write power_dpm_force_performance_level once (e.g. echo auto), then re-apply by writing %u to the %s parameter\n", max_mhz, what);
		return 0;
	}

	ret = sfr_fn(adev_ptr, type, 0, max_mhz);
	if (ret)
		pr_err("r9700_tune: set_soft_freq_range(min=0, max=%u MHz, %s) failed: %d\n", max_mhz, what, ret);
	else
		pr_info("r9700_tune: %s soft max set to %u MHz (automatic range below preserved)\n", what, max_mhz);

	return ret;
}

static int apply_sclk(unsigned int mhz)
{
	return apply_clk_range(mhz, PP_SCLK, "SCLK");
}

static int apply_mclk(unsigned int mhz)
{
	return apply_clk_range(mhz, PP_MCLK, "MCLK");
}

static int param_set_mclk_max(const char *val, const struct kernel_param *kp)
{
	unsigned int mhz;
	int ret;

	ret = kstrtouint(val, 0, &mhz);
	if (ret)
		return ret;
	if (mhz && (mhz < 100 || mhz > 3000))
		return -EINVAL;

	apply_mclk(mhz);
	mclk_max = mhz;
	return 0;
}

static const struct kernel_param_ops mclk_max_ops = {
	.set = param_set_mclk_max,
	.get = param_get_uint,
};

static int param_set_sclk_max(const char *val, const struct kernel_param *kp)
{
	unsigned int mhz;
	int ret;

	ret = kstrtouint(val, 0, &mhz);
	if (ret)
		return ret;
	if (mhz && (mhz < 200 || mhz > 4000))
		return -EINVAL;

	apply_sclk(mhz); /* errors logged; do not fail insmod before capture */
	sclk_max = mhz;
	return 0;
}

static const struct kernel_param_ops sclk_max_ops = {
	.set = param_set_sclk_max,
	.get = param_get_uint,
};

/* -------------------------------- fan ------------------------------- */

static int parse_curve(const char *s, u8 *out, int n)
{
	char *dup, *p, *tok;
	int i;

	dup = kstrdup(s, GFP_KERNEL);
	if (!dup)
		return -ENOMEM;
	p = dup;
	for (i = 0; i < n; i++) {
		tok = strsep(&p, ",");
		if (!tok || kstrtou8(tok, 0, &out[i])) {
			kfree(dup);
			return -EINVAL;
		}
	}
	kfree(dup);
	return 0;
}

static int apply_fan(void)
{
	u8 table[OD_SIZE];
	void **od_pp;
	u32 mask;
	int ret;

	if (!upload_fn) {
		pr_err("r9700_tune: smu_v14_0_2_upload_overdrive_table not resolved\n");
		return -EIO;
	}
	if (!smu_ptr) {
		pr_info("r9700_tune: smu not captured yet - read the pp_table sysfs once (e.g. cat /sys/class/drm/card0/device/pp_table > /dev/null), then re-apply by writing 1 to /sys/module/r9700_tune/parameters/fan_apply\n");
		return 0;
	}

	od_pp = (void **)((u8 *)smu_ptr + SMU_TABLE_OFS + OVERDRIVE_TABLE_OFS);
	if (!*od_pp) {
		pr_err("r9700_tune: overdrive_table buffer is NULL\n");
		return -EIO;
	}
	memcpy(table, *od_pp, OD_SIZE);
	mask = get_unaligned_le32(table + 0);

	if (!fan_target_temp && !acoustic_target_rpm && !acoustic_limit_rpm &&
	    !fan_min_pwm && !fan_curve_pwm[0] && !ppt_offset && !vddgfx_offset) {
		pr_info("r9700_tune: no OD settings configured - nothing to do\n");
		return 0;
	}

	if (fan_target_temp || acoustic_target_rpm || acoustic_limit_rpm ||
	    fan_min_pwm || fan_curve_pwm[0])
		mask |= 1U << OD_BIT_FAN_CURVE;

	if (vddgfx_offset) {
		if (vddgfx_offset < -150 || vddgfx_offset > 0) {
			pr_err("r9700_tune: vddgfx_offset %d out of range [-150, 0]\n", vddgfx_offset);
			return -EINVAL;
		}
		mask |= 1U << OD_BIT_VF_CURVE;
		for (int i = 0; i < 6; i++)
			put_unaligned_le16((u16)(s16)vddgfx_offset, table + 4 + 2 * i);
	}

	if (ppt_offset) {
		if (ppt_offset < -60 || ppt_offset > 20) {
			pr_err("r9700_tune: ppt_offset %d out of range [-60, +20]\n", ppt_offset);
			return -EINVAL;
		}
		mask |= 1U << OD_BIT_PPT;
		put_unaligned_le16((u16)(s16)ppt_offset, table + 36);
	}

	if (fan_target_temp)
		put_unaligned_le16(fan_target_temp, table + 58);
	if (acoustic_target_rpm)
		put_unaligned_le16(acoustic_target_rpm, table + 54);
	if (acoustic_limit_rpm)
		put_unaligned_le16(acoustic_limit_rpm, table + 56);
	if (fan_min_pwm)
		put_unaligned_le16(fan_min_pwm, table + 52);
	if (fan_curve_pwm[0]) {
		u8 pwm[6];
		static const u8 temps[6] = { 40, 50, 60, 70, 80, 90 };

		if (parse_curve(fan_curve_pwm, pwm, 6)) {
			pr_err("r9700_tune: bad fan_curve_pwm - expected 6 comma-separated values\n");
			return -EINVAL;
		}
		memcpy(table + 40, pwm, 6);
		memcpy(table + 46, temps, 6);
	}

	put_unaligned_le32(mask, table + 0);

	ret = upload_fn(smu_ptr, table);
	if (ret && (mask & (1U << OD_BIT_PPT))) {
		pr_warn("r9700_tune: OD upload with PPT failed: %d - retrying without it\n", ret);
		mask &= ~(1U << OD_BIT_PPT);
		put_unaligned_le32(mask, table);
		ret = upload_fn(smu_ptr, table);
	}
	if (ret && (mask & (1U << OD_BIT_VF_CURVE))) {
		pr_warn("r9700_tune: OD upload with voltage offset failed: %d - retrying without it\n", ret);
		mask &= ~(1U << OD_BIT_VF_CURVE);
		put_unaligned_le32(mask, table);
		ret = upload_fn(smu_ptr, table);
	}
	if (ret)
		pr_err("r9700_tune: overdrive upload failed: %d\n", ret);
	else if (mask & (1U << OD_BIT_VF_CURVE))
		pr_info("r9700_tune: OD settings applied (vddgfx %d mV, fan target %u C, acoustic %u/%u RPM, min pwm %u%%)\n",
			vddgfx_offset, fan_target_temp, acoustic_target_rpm, acoustic_limit_rpm, fan_min_pwm);
	else
		pr_info("r9700_tune: fan settings applied (fan target %u C, acoustic %u/%u RPM, min pwm %u%%)\n",
			fan_target_temp, acoustic_target_rpm, acoustic_limit_rpm, fan_min_pwm);

	return ret;
}

static int param_set_fan_apply(const char *val, const struct kernel_param *kp)
{
	apply_fan(); /* errors logged */
	return 0;
}

static const struct kernel_param_ops fan_apply_ops = {
	.set = param_set_fan_apply,
	.get = param_get_uint,
};

static unsigned int fan_apply;

/* ------------------------------- module ------------------------------ */

module_param_cb(sclk_max, &sclk_max_ops, &sclk_max, 0644);
MODULE_PARM_DESC(sclk_max, "max SCLK in MHz, 0 = no change (default 0)");

module_param_cb(mclk_max, &mclk_max_ops, &mclk_max, 0644);
MODULE_PARM_DESC(mclk_max, "max MCLK in MHz, 0 = no change (default 0; stock is 1258)");

module_param_cb(fan_apply, &fan_apply_ops, &fan_apply, 0644);
MODULE_PARM_DESC(fan_apply, "write anything to apply the fan settings");

module_param(fan_target_temp, uint, 0644);
MODULE_PARM_DESC(fan_target_temp, "fan target temperature in C (0 = leave)");
module_param(acoustic_target_rpm, uint, 0644);
MODULE_PARM_DESC(acoustic_target_rpm, "acoustic target RPM (0 = leave)");
module_param(acoustic_limit_rpm, uint, 0644);
MODULE_PARM_DESC(acoustic_limit_rpm, "acoustic limit RPM (0 = leave)");
module_param(fan_min_pwm, uint, 0644);
MODULE_PARM_DESC(fan_min_pwm, "minimum fan PWM percent (0 = leave)");
module_param(fan_curve_pwm, charp, 0644);
MODULE_PARM_DESC(fan_curve_pwm, "fan curve PWM: p0,p1,p2,p3,p4,p5 at 40..90C (empty = leave)");
module_param(ppt_offset, int, 0644);
MODULE_PARM_DESC(ppt_offset, "power limit offset in percent (e.g. -50 = 150 W on a 300 W board; 0 = leave)");
module_param(vddgfx_offset, int, 0644);
MODULE_PARM_DESC(vddgfx_offset, "VDDGFX offset in mV, negative = undervolt (e.g. -50; 0 = leave). May be rejected on Pro boards.");

module_param(fn_addr, ullong, 0444);
MODULE_PARM_DESC(fn_addr, "override address of amdgpu_dpm_set_soft_freq_range");
module_param(fan_fn_addr, ullong, 0444);
MODULE_PARM_DESC(fan_fn_addr, "override address of smu_v14_0_2_upload_overdrive_table");
module_param(adev_addr, ullong, 0444);
MODULE_PARM_DESC(adev_addr, "override amdgpu_device pointer");

static int __init r9700_tune_init(void)
{
	unsigned long addr;

	addr = fn_addr ? (unsigned long)fn_addr
		       : lookup_symbol("amdgpu_dpm_set_soft_freq_range");
	if (!addr) {
		pr_err("r9700_tune: cannot resolve amdgpu_dpm_set_soft_freq_range (kallsyms hidden or kprobes disabled); pass fn_addr=0x<addr>\n");
		return -EINVAL;
	}
	sfr_fn = (set_soft_freq_range_fn)(uintptr_t)addr;
	pr_info("r9700_tune: amdgpu_dpm_set_soft_freq_range @ 0x%lx\n", addr);

	addr = fan_fn_addr ? (unsigned long)fan_fn_addr
			   : lookup_symbol("smu_v14_0_2_upload_overdrive_table");
	if (addr)
		upload_fn = (upload_overdrive_fn)(uintptr_t)addr;
	else
		pr_warn("r9700_tune: cannot resolve smu_v14_0_2_upload_overdrive_table - fan control disabled (pass fan_fn_addr=0x<addr> if needed)\n");

	if (adev_addr)
		adev_ptr = (void *)(uintptr_t)adev_addr;

#ifdef CONFIG_KPROBES
	if (!adev_ptr && register_kprobe(&cap_adev_kp) == 0)
		cap_adev_armed = true;
	if (register_kprobe(&cap_smu_kp) == 0)
		cap_smu_armed = true;
	if (!cap_adev_armed && !adev_ptr) {
		pr_err("r9700_tune: cannot arm capture probes; pass adev_addr=0x<addr> manually\n");
		unregister_kprobe(&cap_smu_kp);
		cap_smu_armed = false;
		return -EINVAL;
	}
#else
	if (!adev_ptr) {
		pr_err("r9700_tune: kernel built without kprobes; pass adev_addr=0x<addr> manually\n");
		return -EINVAL;
	}
#endif

	pr_info("r9700_tune: capture probes armed - trigger with: echo auto > power_dpm_force_performance_level AND cat pp_table > /dev/null\n");

	if (sclk_max)
		apply_sclk(sclk_max);
	if (mclk_max)
		apply_mclk(mclk_max);

	return 0;
}

static void __exit r9700_tune_exit(void)
{
#ifdef CONFIG_KPROBES
	if (cap_adev_armed)
		unregister_kprobe(&cap_adev_kp);
	if (cap_smu_armed)
		unregister_kprobe(&cap_smu_kp);
#endif
	pr_info("r9700_tune: unloaded. Reset clocks with: echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level (fan settings reset on reboot)\n");
}

module_init(r9700_tune_init);
module_exit(r9700_tune_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("SCLK soft-max cap and fan/acoustic control for AMD Radeon AI PRO R9700 (Navi 48)");
MODULE_VERSION("0.5");
