// SPDX-License-Identifier: GPL-2.0
/*
 * r9700_tune.c - cap the maximum SCLK on an AMD Radeon AI PRO R9700
 * (Navi 48, SMU 14.0.2) while keeping the automatic DPM range.
 *
 * Why this exists:
 *  - pp_od_clk_voltage is hidden on this card (OD masks are zero in the
 *    SMU's PowerPlay table), so the normal overdrive path is unavailable.
 *  - pp_table upload is a no-op on SMU 14.0.2 (the driver always re-reads
 *    the table from the SMU and ignores the uploaded copy).
 *  - amdgpu's sysfs/debugfs only allow pinning min=max (constant clock).
 *
 * amdgpu internally has amdgpu_dpm_set_soft_freq_range(adev, PP_SCLK,
 * min, max) which sends SMU_MSG_SetSoftMaxByFreq. With min=0 only the
 * soft MAX is changed, so the SMU keeps the automatic range
 * (500 MHz .. sclk_max). The function is not exported, so this module
 * resolves it via kallsyms (kprobe on kallsyms_lookup_name) and captures
 * the amdgpu_device pointer with a kprobe on
 * amdgpu_dpm_force_performance_level (first argument).
 *
 * Usage:
 *   insmod r9700_tune.ko sclk_max=2000
 *   echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level
 *   echo 2000 > /sys/module/r9700_tune/parameters/sclk_max
 *
 * Change at runtime: write MHz to /sys/module/r9700_tune/parameters/sclk_max
 * Reset: echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level
 *        (or reboot). Nothing here is persistent.
 */
#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/kprobes.h>

#define PP_SCLK 0 /* enum pp_clock_type (kgd_pp_interface.h) */

static unsigned int sclk_max;
static unsigned long long fn_addr;    /* override: amdgpu_dpm_set_soft_freq_range */
static unsigned long long adev_addr;  /* override: amdgpu_device pointer */
static void *adev_ptr;

typedef int (*set_soft_freq_range_fn)(void *adev, int type, u32 min, u32 max);

static set_soft_freq_range_fn sfr_fn;

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
static int cap_pre(struct kprobe *p, struct pt_regs *regs);
static struct kprobe cap_kp = {
	.symbol_name = "amdgpu_dpm_force_performance_level",
	.pre_handler = cap_pre,
};
static bool cap_armed;

static int cap_pre(struct kprobe *p, struct pt_regs *regs)
{
#if defined(CONFIG_X86_64)
	if (!adev_ptr)
		adev_ptr = (void *)regs->di;
#endif
	return 0;
}
#endif

static int apply(unsigned int mhz)
{
	int ret;

	if (!mhz)
		return 0;
	if (!sfr_fn) {
		pr_err("r9700_tune: amdgpu_dpm_set_soft_freq_range not resolved\n");
		return -EIO;
	}
	if (!adev_ptr) {
		pr_info("r9700_tune: device not captured yet - write power_dpm_force_performance_level once (e.g. echo auto), then re-apply by writing %u to /sys/module/r9700_tune/parameters/sclk_max\n", mhz);
		return 0;
	}

	ret = sfr_fn(adev_ptr, PP_SCLK, 0, mhz);
	if (ret)
		pr_err("r9700_tune: set_soft_freq_range(min=0, max=%u MHz) failed: %d\n", mhz, ret);
	else
		pr_info("r9700_tune: SCLK soft max set to %u MHz (automatic range below preserved)\n", mhz);

	return ret;
}

static int param_set_sclk_max(const char *val, const struct kernel_param *kp)
{
	unsigned int mhz;
	int ret;

	ret = kstrtouint(val, 0, &mhz);
	if (ret)
		return ret;
	if (mhz && (mhz < 200 || mhz > 4000))
		return -EINVAL;

	apply(mhz); /* errors are logged; do not fail insmod before capture */
	sclk_max = mhz;
	return 0;
}

static const struct kernel_param_ops sclk_max_ops = {
	.set = param_set_sclk_max,
	.get = param_get_uint,
};

module_param_cb(sclk_max, &sclk_max_ops, &sclk_max, 0644);
MODULE_PARM_DESC(sclk_max, "max SCLK in MHz, 0 = no change (default 0)");
module_param(fn_addr, ullong, 0444);
MODULE_PARM_DESC(fn_addr, "override address of amdgpu_dpm_set_soft_freq_range");
module_param(adev_addr, ullong, 0444);
MODULE_PARM_DESC(adev_addr, "override amdgpu_device pointer");

static int __init r9700_tune_init(void)
{
	unsigned long addr = 0;

	addr = fn_addr ? (unsigned long)fn_addr
		       : lookup_symbol("amdgpu_dpm_set_soft_freq_range");
	if (!addr) {
		pr_err("r9700_tune: cannot resolve amdgpu_dpm_set_soft_freq_range (kallsyms hidden or kprobes disabled); pass fn_addr=0x<addr> from /proc/kallsyms\n");
		return -EINVAL;
	}
	sfr_fn = (set_soft_freq_range_fn)(uintptr_t)addr;
	pr_info("r9700_tune: amdgpu_dpm_set_soft_freq_range @ 0x%lx\n", addr);

	if (adev_addr) {
		adev_ptr = (void *)(uintptr_t)adev_addr;
		pr_info("r9700_tune: using adev from parameter: %px\n", adev_ptr);
	}

#ifdef CONFIG_KPROBES
	if (!adev_ptr) {
		if (register_kprobe(&cap_kp) == 0) {
			cap_armed = true;
			pr_info("r9700_tune: capture probe armed - write power_dpm_force_performance_level once (e.g. echo auto) to capture the device\n");
		} else {
			pr_err("r9700_tune: cannot arm capture probe; pass adev_addr=0x<addr> manually\n");
			return -EINVAL;
		}
	}
#else
	if (!adev_ptr) {
		pr_err("r9700_tune: kernel built without kprobes; pass adev_addr=0x<addr> manually\n");
		return -EINVAL;
	}
#endif

	if (sclk_max)
		apply(sclk_max);

	return 0;
}

static void __exit r9700_tune_exit(void)
{
#ifdef CONFIG_KPROBES
	if (cap_armed)
		unregister_kprobe(&cap_kp);
#endif
	pr_info("r9700_tune: unloaded. Reset the cap with: echo auto > /sys/class/drm/card0/device/power_dpm_force_performance_level\n");
}

module_init(r9700_tune_init);
module_exit(r9700_tune_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("Cap max SCLK on AMD Radeon AI PRO R9700 (Navi 48)");
MODULE_VERSION("0.1");
