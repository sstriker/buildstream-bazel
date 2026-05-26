// linux.c — Linux-only source. Picked up by the `if(LINUX)`
// arm when cmake configures for Linux; Tier 1 partitions it
// into the @platforms//os:linux arm of the emitted select().
int linux_platform_value(void) { return 1; }
