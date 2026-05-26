// win.c — Windows-only source. Lives in the `elseif(WIN32)`
// arm of the if-block. On a Linux configure cmake doesn't
// execute the call that adds it, so the codemodel never sees
// it; the file's path stays on disk so Tier 2's CMakeLists
// parser can attribute it.
int win_platform_value(void) { return 2; }
