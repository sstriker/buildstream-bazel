// base.c — unconditional source for the platform-partition-tier2
// fixture. Lives in flat srcs (no platform conditional);
// linux.c and win.c land under platform-specific arms via Tier 1
// + Tier 2 partition.
int base_value(void) { return 42; }
