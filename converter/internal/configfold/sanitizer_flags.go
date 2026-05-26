package configfold

import (
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// SanitizerFlagSet is the per-action flag set extracted from cmake's
// CMAKE_<LANG>_FLAGS_<CONFIG> / CMAKE_<TYPE>_LINKER_FLAGS_<CONFIG>
// cache entries for one sanitizer-shaped configuration.
//
// Per-language compile flags keep their cmake-language key
// (CompileFlags["C"], CompileFlags["CXX"]) so the emitter can route
// to the matching cc_toolchain action set. LinkFlags is a single
// merged slice because Bazel feature flag_set actions don't
// distinguish exe vs shared link in the way cmake does — most
// sanitizers pass the same -fsanitize=… for both.
type SanitizerFlagSet struct {
	// FeatureName is the Bazel feature name SanitizerFeature
	// returned for this config; emitted as the `feature(name = …)`
	// value.
	FeatureName string

	// CompileFlags is per-language; keys match CMake language names
	// (C, CXX, ASM). Empty values omit the language from the
	// generated feature's flag_set actions.
	CompileFlags map[string][]string

	// LinkFlags merges CMAKE_EXE_LINKER_FLAGS_<CONFIG> and
	// CMAKE_SHARED_LINKER_FLAGS_<CONFIG> into one slice (deduped,
	// order-preserving on first occurrence). Most sanitizers' link
	// flags are identical between the two; the union shape is the
	// safer default for the generated feature.
	LinkFlags []string
}

// ExtractSanitizerFlags pulls per-config sanitizer flag sets from
// the cmake cache for every configuration in configs whose name
// SanitizerFeature recognizes. Returns nil when no sanitizer-shaped
// config surfaces or when none of the recognized configs has any
// flag-cache entries.
//
// Reads (per config NAME — case-sensitive on the cmake side):
//
//   - CMAKE_C_FLAGS_<NAME>            → CompileFlags["C"]
//   - CMAKE_CXX_FLAGS_<NAME>          → CompileFlags["CXX"]
//   - CMAKE_ASM_FLAGS_<NAME>          → CompileFlags["ASM"]
//   - CMAKE_EXE_LINKER_FLAGS_<NAME>   → LinkFlags (merged)
//   - CMAKE_SHARED_LINKER_FLAGS_<NAME>→ LinkFlags (merged)
//
// cmake uppercases the config name for these variable lookups by
// convention (CMAKE_BUILD_TYPE=Debug → CMAKE_C_FLAGS_DEBUG); we
// match the convention.
//
// Output keys are the original cmake config names (preserving
// operator case so the emitter can show "from cmake config 'ASan'"
// in generated comments).
//
// Phase 5 of the generator-parity uplift (ROADMAP.md). Pairs with
// emit/sanitizerfeatures, which renders the extracted data as a
// .bzl file the operator drops into their cc_toolchain.
func ExtractSanitizerFlags(cache fileapi.Cache, configs []string) map[string]SanitizerFlagSet {
	out := map[string]SanitizerFlagSet{}
	for _, cfg := range configs {
		featureName, ok := SanitizerFeature(cfg)
		if !ok {
			continue
		}
		upper := strings.ToUpper(cfg)
		set := SanitizerFlagSet{
			FeatureName:  featureName,
			CompileFlags: map[string][]string{},
		}
		// Per-language compile flags.
		for _, lang := range []string{"C", "CXX", "ASM"} {
			key := "CMAKE_" + lang + "_FLAGS_" + upper
			if e := cache.Get(key); e != nil && e.Value != "" {
				if flags := tokenizeFlagString(e.Value); len(flags) > 0 {
					set.CompileFlags[lang] = flags
				}
			}
		}
		// Merge EXE + SHARED linker flags (deduped, order-
		// preserving on first occurrence).
		linkSeen := map[string]bool{}
		var linkFlags []string
		for _, key := range []string{
			"CMAKE_EXE_LINKER_FLAGS_" + upper,
			"CMAKE_SHARED_LINKER_FLAGS_" + upper,
		} {
			if e := cache.Get(key); e != nil && e.Value != "" {
				for _, f := range tokenizeFlagString(e.Value) {
					if !linkSeen[f] {
						linkSeen[f] = true
						linkFlags = append(linkFlags, f)
					}
				}
			}
		}
		set.LinkFlags = linkFlags

		// Drop the entry when cmake had no per-config flag
		// variables for the sanitizer — the operator may have
		// recognized the config name without populating flags
		// (cmake silently allows this; the generated feature
		// would be empty, surfacing as a no-op feature() rather
		// than the intended sanitizer).
		if len(set.CompileFlags) == 0 && len(set.LinkFlags) == 0 {
			continue
		}
		out[cfg] = set
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tokenizeFlagString splits a cmake-style flag string on whitespace,
// preserving quoted segments. cmake stores CMAKE_<LANG>_FLAGS_<CONFIG>
// as a single space-separated string; the emitter needs a slice.
//
// Quote handling is conservative: a "..." or '...' run starting at
// a token boundary collects content (with the quotes stripped) until
// the matching closer. Escapes inside quotes aren't processed
// (cmake's flag-string convention doesn't use them in practice).
func tokenizeFlagString(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
				continue
			}
			cur.WriteByte(c)
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if c == ' ' || c == '\t' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// SanitizerConfigOrder returns recognized configs from configs in
// the order SanitizerFlagSets should appear in deterministic output.
// Stable across runs: ordered by feature name (which is canonical;
// "asan" before "lto" before "tsan").
func SanitizerConfigOrder(sets map[string]SanitizerFlagSet) []string {
	names := make([]string, 0, len(sets))
	for n := range sets {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return sets[names[i]].FeatureName < sets[names[j]].FeatureName
	})
	return names
}
