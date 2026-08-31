# r9700_tune kernel module

See the main README at the repository root for the full story. This
directory contains the kernel module source, Makefile and the `apply.sh`
helper.

- `r9700_tune.c` - the module (SCLK soft max + OD fan/acoustic + PPT offset)
- `apply.sh` - load + trigger captures + apply (safe at boot)
- `r9700_tune.ko` - prebuilt for Unraid 6.18.44 (also a release asset)

Offsets baked into the module were computed against the exact Unraid
6.18.44 kernel tree (see the commit history for the verification probe).
