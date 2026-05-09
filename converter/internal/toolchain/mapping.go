package toolchain

import "strings"

// VariantMapping classifies a Variant into a Bazel-side feature
// slot. The emit package consumes this to decide which
// cc_toolchain_config flag-set slot a variant's delta lands in
// (compile_flags, dbg_compile_flags, opt_compile_flags, or a
// custom feature for sanitizers / coverage / etc.).
//
// Returning "" (BazelFeatureNone) means "this variant's delta is
// not routed anywhere"; the variant was probed for observation
// but produces no Bazel-side flag-set contribution.
type VariantMapping func(v Variant) BazelFeature

// BazelFeature identifies a slot in cc_toolchain_config. The
// strings match Bazel's compilation_mode names where applicable
// ("dbg", "opt") and the conventional --features=<name> spelling
// for sanitizers / coverage / lto. Operators trigger them via
// `bazel build --features=asan ...` (typically aliased through
// .bazelrc as `--config=asan`).
type BazelFeature string

const (
	BazelFeatureNone     BazelFeature = ""
	BazelFeatureDbg      BazelFeature = "dbg"      // -> dbg_compile_flags / link
	BazelFeatureOpt      BazelFeature = "opt"      // -> opt_compile_flags / link
	BazelFeatureAsan     BazelFeature = "asan"     // -fsanitize=address
	BazelFeatureTsan     BazelFeature = "tsan"     // -fsanitize=thread
	BazelFeatureMsan     BazelFeature = "msan"     // -fsanitize=memory
	BazelFeatureUbsan    BazelFeature = "ubsan"    // -fsanitize=undefined
	BazelFeatureCoverage BazelFeature = "coverage" // --coverage / -fprofile-instr-generate
	BazelFeatureLto      BazelFeature = "lto"      // -flto
)

// SanitizerVariants is the canonical catalog of sanitizer probe
// variants. Each entry's CacheVars sets CMAKE_C_FLAGS / CMAKE_CXX_FLAGS
// to the sanitizer's expected flag bundle (CMAKE_BUILD_TYPE=Debug so
// the optimizer doesn't elide instrumentation). The catalog is the
// single source of truth for what flags Stage 2's emit layer maps
// back onto Bazel's `--features=<name>` slots.
//
// Coverage and LTO are intentionally separate from the four
// sanitizers — they're not mutually exclusive with sanitizers and
// frequently combined (asan + coverage is common).
//
// CMakePresets.json (Stage 3) carries the same catalog in JSON;
// TestSanitizerVariants_MatchesPresetsJSON keeps the two in sync.
var SanitizerVariants = []Variant{
	{
		Name: "asan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "-fsanitize=address -fno-omit-frame-pointer",
			"CMAKE_CXX_FLAGS":  "-fsanitize=address -fno-omit-frame-pointer",
		},
	},
	{
		Name: "tsan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "-fsanitize=thread",
			"CMAKE_CXX_FLAGS":  "-fsanitize=thread",
		},
	},
	{
		Name: "msan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "-fsanitize=memory -fno-omit-frame-pointer",
			"CMAKE_CXX_FLAGS":  "-fsanitize=memory -fno-omit-frame-pointer",
		},
	},
	{
		Name: "ubsan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "-fsanitize=undefined",
			"CMAKE_CXX_FLAGS":  "-fsanitize=undefined",
		},
	},
	{
		Name: "coverage",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "--coverage",
			"CMAKE_CXX_FLAGS":  "--coverage",
		},
	},
	{
		Name: "lto",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Release",
			"CMAKE_C_FLAGS":    "-flto",
			"CMAKE_CXX_FLAGS":  "-flto",
		},
	},
}

// DefaultVariantMapping is the standard variant classifier. Order
// of resolution:
//
//  1. CMAKE_C_FLAGS sanitizer / coverage / LTO substring → matching
//     BazelFeature. A sanitizer-flavoured cell is classified by its
//     flag content regardless of CMAKE_BUILD_TYPE so a "Debug + asan"
//     cell still routes to "asan", not "dbg" — the build-type signal
//     is the secondary axis there.
//  2. CMAKE_BUILD_TYPE → dbg / opt. Debug → dbg; Release / MinSizeRel
//     / RelWithDebInfo → opt.
//  3. Otherwise → BazelFeatureNone (variant observed but not routed).
//
// Operators with custom variants supply their own VariantMapping
// to the emit layer.
func DefaultVariantMapping(v Variant) BazelFeature {
	if f := classifyByFlagContent(v.CacheVars["CMAKE_C_FLAGS"]); f != BazelFeatureNone {
		return f
	}
	if f := classifyByFlagContent(v.CacheVars["CMAKE_CXX_FLAGS"]); f != BazelFeatureNone {
		return f
	}
	bt, ok := v.CacheVars["CMAKE_BUILD_TYPE"]
	if !ok {
		return BazelFeatureNone
	}
	switch strings.ToUpper(bt) {
	case "DEBUG":
		return BazelFeatureDbg
	case "RELEASE", "MINSIZEREL", "RELWITHDEBINFO":
		return BazelFeatureOpt
	default:
		return BazelFeatureNone
	}
}

// classifyByFlagContent matches the sanitizer / coverage / LTO
// substrings against a single flags string. Substring matching is
// chosen over exact-token matching so "-fsanitize=address,undefined"
// (combined sanitizers, valid in clang) routes to the first
// detected sanitizer. The order of checks is documented in the
// switch below; changing it changes which feature wins for a
// combined-flags cell.
func classifyByFlagContent(flags string) BazelFeature {
	if flags == "" {
		return BazelFeatureNone
	}
	switch {
	case strings.Contains(flags, "-fsanitize=address"):
		return BazelFeatureAsan
	case strings.Contains(flags, "-fsanitize=thread"):
		return BazelFeatureTsan
	case strings.Contains(flags, "-fsanitize=memory"):
		return BazelFeatureMsan
	case strings.Contains(flags, "-fsanitize=undefined"):
		return BazelFeatureUbsan
	case strings.Contains(flags, "--coverage") ||
		strings.Contains(flags, "-fprofile-instr-generate") ||
		strings.Contains(flags, "-fprofile-arcs"):
		return BazelFeatureCoverage
	case strings.Contains(flags, "-flto"):
		return BazelFeatureLto
	}
	return BazelFeatureNone
}
