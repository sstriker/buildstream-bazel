package toolchain

import (
	"reflect"
	"sort"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
)

// TestObserve_PartitionsCacheVarsByObservedAgreement: hand-build
// three variant probes whose cache disagrees on CMAKE_BUILD_TYPE
// and CMAKE_C_FLAGS_DEBUG/_RELEASE, but agrees on CMAKE_C_COMPILER
// and CMAKE_AR. Observe should:
//
//   - keep CMAKE_C_COMPILER + CMAKE_AR in baseline (same value
//     across every variant)
//   - put CMAKE_BUILD_TYPE + CMAKE_C_FLAGS_<x> entries in the
//     per-variant CacheVarOverrides (differed)
func TestObserve_PartitionsCacheVarsByObservedAgreement(t *testing.T) {
	results := []ProbeResult{
		probeFixture("baseline", map[string]string{
			"CMAKE_C_COMPILER":    "/usr/bin/cc",
			"CMAKE_AR":            "/usr/bin/ar",
			"CMAKE_BUILD_TYPE":    "",
			"CMAKE_C_FLAGS":       "-Wall",
			"CMAKE_C_FLAGS_DEBUG": "-O0 -g",
		}),
		probeFixture("debug", map[string]string{
			"CMAKE_C_COMPILER":    "/usr/bin/cc",
			"CMAKE_AR":            "/usr/bin/ar",
			"CMAKE_BUILD_TYPE":    "Debug",
			"CMAKE_C_FLAGS":       "-Wall",
			"CMAKE_C_FLAGS_DEBUG": "-O0 -g",
		}),
		probeFixture("release", map[string]string{
			"CMAKE_C_COMPILER":      "/usr/bin/cc",
			"CMAKE_AR":              "/usr/bin/ar",
			"CMAKE_BUILD_TYPE":      "Release",
			"CMAKE_C_FLAGS":         "-Wall",
			"CMAKE_C_FLAGS_RELEASE": "-O3 -DNDEBUG",
		}),
	}

	rt := Observe(results)
	if rt == nil {
		t.Fatal("Observe returned nil")
	}

	// Baseline: only entries that were identical across all three.
	// CMAKE_C_FLAGS is in all three with the same value, so its
	// tokens land in Base.Languages.C.BaseFlags.
	if got := rt.Base.Languages["C"].BaseFlags; !reflect.DeepEqual(got, []string{"-Wall"}) {
		t.Errorf("Base.C.BaseFlags = %v, want [-Wall]", got)
	}

	// CMAKE_BUILD_TYPE differed (empty / Debug / Release) → should
	// appear in each variant's CacheVarOverrides.
	for _, name := range []string{"baseline", "debug", "release"} {
		v, ok := rt.Variants[name]
		if !ok {
			t.Fatalf("Variants missing %q", name)
		}
		if _, present := v.CacheVarOverrides["CMAKE_BUILD_TYPE"]; !present {
			t.Errorf("variant %s: CMAKE_BUILD_TYPE not in overrides; %v",
				name, sortedKeys(v.CacheVarOverrides))
		}
	}

	// CMAKE_AR was identical across all three; it should NOT appear
	// in any variant's overrides.
	for _, name := range []string{"baseline", "debug", "release"} {
		if _, present := rt.Variants[name].CacheVarOverrides["CMAKE_AR"]; present {
			t.Errorf("variant %s: CMAKE_AR leaked into overrides; should be baseline-only", name)
		}
	}
}

// TestObserve_FlagDeltasFromBaseFlagsPlusBuildTypeFlags: Release's
// per-variant LanguageFlags delta should be only the additions
// over baseline — not -Wall (baseline shared) but -O3 + -DNDEBUG
// (Release-only).
func TestObserve_FlagDeltasFromBaseFlagsPlusBuildTypeFlags(t *testing.T) {
	results := []ProbeResult{
		probeFixture("baseline", map[string]string{
			"CMAKE_C_FLAGS": "-Wall",
		}),
		probeFixtureWithBuildType("release", "Release", map[string]string{
			"CMAKE_C_FLAGS":         "-Wall",
			"CMAKE_C_FLAGS_RELEASE": "-O3 -DNDEBUG",
		}),
	}
	rt := Observe(results)

	delta := rt.Variants["release"]
	if delta == nil {
		t.Fatal("Variants missing release")
	}
	got := delta.LanguageFlags["C"]
	want := []string{"-O3", "-DNDEBUG"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("release.C.LanguageFlags = %v, want %v", got, want)
	}
}

func TestObserve_EmptyResultsReturnsNil(t *testing.T) {
	if rt := Observe(nil); rt != nil {
		t.Errorf("Observe(nil) = %+v, want nil", rt)
	}
}

func TestObserve_SingleVariantStillReturnsResolved(t *testing.T) {
	results := []ProbeResult{
		probeFixture("only", map[string]string{
			"CMAKE_C_FLAGS": "-Wall",
		}),
	}
	rt := Observe(results)
	if rt == nil {
		t.Fatal("nil rt for single variant")
	}
	if _, ok := rt.Variants["only"]; !ok {
		t.Errorf("Variants[%q] missing", "only")
	}
	if got := rt.Base.Languages["C"].BaseFlags; !reflect.DeepEqual(got, []string{"-Wall"}) {
		t.Errorf("Base.C.BaseFlags = %v, want [-Wall]", got)
	}
}

func TestDefaultVariantMapping(t *testing.T) {
	cases := []struct {
		v    Variant
		want BazelFeature
	}{
		{Variant{Name: "baseline"}, BazelFeatureNone},
		{Variant{Name: "debug", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Debug"}}, BazelFeatureDbg},
		{Variant{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}}, BazelFeatureOpt},
		{Variant{Name: "minsize", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "MinSizeRel"}}, BazelFeatureOpt},
		{Variant{Name: "rdi", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "RelWithDebInfo"}}, BazelFeatureOpt},
		{Variant{Name: "asan", CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=address"}}, BazelFeatureAsan},
	}
	for _, tc := range cases {
		if got := DefaultVariantMapping(tc.v); got != tc.want {
			t.Errorf("DefaultVariantMapping(%v) = %q, want %q", VariantString(tc.v), got, tc.want)
		}
	}
}

// helpers

// probeFixture builds a ProbeResult whose Reply.Cache reflects the
// given map; Model is FromReply'd off it. CMAKE_BUILD_TYPE in
// CacheVars is treated as an empty/unset Variant by default.
func probeFixture(name string, cache map[string]string) ProbeResult {
	return probeFixtureWithBuildType(name, "", cache)
}

func probeFixtureWithBuildType(name, buildType string, cache map[string]string) ProbeResult {
	cv := map[string]string{}
	if buildType != "" {
		cv["CMAKE_BUILD_TYPE"] = buildType
		// Mirror cmake's behavior: CMAKE_BUILD_TYPE lands in the
		// cache so FromReply picks it up and reads the matching
		// CMAKE_<LANG>_FLAGS_<BUILD_TYPE> entries.
		cache = withEntry(cache, "CMAKE_BUILD_TYPE", buildType)
	}
	r := &fileapi.Reply{
		Cache: fileapi.Cache{
			Entries: cacheEntriesFromMap(cache),
		},
		Toolchains: fileapi.Toolchains{
			Toolchains: []fileapi.ToolchainEnt{
				{
					Language:             "C",
					SourceFileExtensions: []string{"c"},
					Compiler: fileapi.ToolchainCompiler{
						Id:   "GNU",
						Path: cache["CMAKE_C_COMPILER"],
					},
				},
			},
		},
	}
	m, _ := FromReply(r)
	return ProbeResult{
		Variant: Variant{Name: name, CacheVars: cv},
		Model:   m,
		Reply:   r,
	}
}

func cacheEntriesFromMap(m map[string]string) []fileapi.CacheEntry {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]fileapi.CacheEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, fileapi.CacheEntry{Name: k, Value: m[k]})
	}
	return out
}

// withEntry returns a copy of m with k=v added (or overwritten).
// Used by fixture builders that need to layer CMAKE_BUILD_TYPE
// on top of a base cache map without mutating it.
func withEntry(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
