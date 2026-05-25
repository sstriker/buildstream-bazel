package configfold

import "strings"

// SanitizerFeature returns the cc_toolchain feature name a Bazel
// build would use for a cmake configuration name matching a known
// sanitizer / instrumentation pattern. Returns ("", false) when
// the name doesn't match a known pattern.
//
// Phase 5 of the generator-parity uplift (ROADMAP.md) uses this
// to route per-config compile/link fragment deltas onto cc_toolchain
// features rather than raw select() arms — matching Bazel
// convention for sanitizer / LTO / debug-info variants:
//
//   - cmake config "ASan" / "AddressSanitizer" → --features=asan
//   - cmake config "TSan" / "ThreadSanitizer"  → --features=tsan
//   - cmake config "MSan" / "MemorySanitizer"  → --features=msan
//   - cmake config "UBSan"                     → --features=ubsan
//   - cmake config "LSan"                      → --features=lsan
//   - cmake config "Coverage" / "Cov"          → --features=coverage
//   - cmake config "LTO" / "ReleaseLTO"        → --features=lto
//
// Match is case-insensitive on the config name; cmake itself
// doesn't normalize configuration names, but operator-side cmake
// convention varies (some projects write "ASan", others "Asan",
// others "ASAN"). The case-insensitive match keeps the lifter
// from chasing each variant.
//
// The standard four cmake configs (Debug, Release, RelWithDebInfo,
// MinSizeRel) return ("", false) — they're not feature variants
// in Bazel-toolchain terms; their flag deltas stay on raw selects.
func SanitizerFeature(config string) (string, bool) {
	lc := strings.ToLower(config)
	switch lc {
	case "asan", "addresssanitizer", "address_sanitizer":
		return "asan", true
	case "tsan", "threadsanitizer", "thread_sanitizer":
		return "tsan", true
	case "msan", "memorysanitizer", "memory_sanitizer":
		return "msan", true
	case "ubsan", "undefinedbehaviorsanitizer", "undefined_behavior_sanitizer", "undefined":
		return "ubsan", true
	case "lsan", "leaksanitizer", "leak_sanitizer":
		return "lsan", true
	case "coverage", "cov", "gcov":
		return "coverage", true
	case "lto":
		return "lto", true
	}
	// Tail-match LTO variants: "ReleaseLTO" / "MinSizeRelLTO" /
	// "RelWithDebInfoLTO" all carry the LTO suffix; treat as the
	// "lto" feature regardless of base config.
	if strings.HasSuffix(lc, "lto") && lc != "lto" {
		return "lto", true
	}
	return "", false
}
