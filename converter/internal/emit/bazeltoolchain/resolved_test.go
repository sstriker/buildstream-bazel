package bazeltoolchain

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
)

// TestEmitResolved_RoutesPerVariantFlagsViaMapping: a hand-built
// ResolvedToolchain with -O3 on a Release-tagged variant and -O0
// on a Debug-tagged variant must land -O3 in opt_compile_flags,
// -O0 in dbg_compile_flags, via cfg.VariantMapping (the default
// CMake build-type classifier).
func TestEmitResolved_RoutesPerVariantFlagsViaMapping(t *testing.T) {
	rt := &toolchain.ResolvedToolchain{
		Base: &toolchain.Model{
			HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
			TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
			Languages: map[string]toolchain.Language{
				"C": {
					CompilerID:           "GNU",
					CompilerPath:         "/usr/bin/gcc",
					BuiltinIncludeDirs:   []string{"/usr/include"},
					BaseFlags:            []string{"-Wall"},
					SourceFileExtensions: []string{"c"},
				},
			},
		},
		Variants: map[string]*toolchain.VariantDelta{
			"release": {
				Spec:          toolchain.Variant{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}},
				LanguageFlags: map[string][]string{"C": {"-O3"}},
			},
			"debug": {
				Spec:          toolchain.Variant{Name: "debug", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Debug"}},
				LanguageFlags: map[string][]string{"C": {"-O0", "-g"}},
			},
		},
	}
	b, err := EmitResolved(rt, Config{})
	if err != nil {
		t.Fatalf("EmitResolved: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])
	// Stage 2 emits flag lists as module-level Starlark constants
	// (_COMPILE_FLAGS, _OPT_COMPILE_FLAGS, _DBG_COMPILE_FLAGS).
	for _, want := range []string{
		"_COMPILE_FLAGS = [\n    \"-Wall\",",
		"_OPT_COMPILE_FLAGS = [\n    \"-O3\",",
		"_DBG_COMPILE_FLAGS = [\n    \"-O0\",\n    \"-g\",",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q\n%s", want, cfg)
		}
	}
}

// TestEmitResolved_OptModeMergesAcrossCMakeBuildTypes: Release
// adds -O3, MinSizeRel adds -Os; both fold to "opt" via the
// default mapping. The merged opt_compile_flags must dedup-
// preserve order.
func TestEmitResolved_OptModeMergesAcrossCMakeBuildTypes(t *testing.T) {
	rt := &toolchain.ResolvedToolchain{
		Base: &toolchain.Model{
			Languages: map[string]toolchain.Language{
				"C": {CompilerPath: "/usr/bin/gcc"},
			},
		},
		Variants: map[string]*toolchain.VariantDelta{
			"release": {
				Spec:          toolchain.Variant{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}},
				LanguageFlags: map[string][]string{"C": {"-O3", "-DNDEBUG"}},
			},
			"minsizerel": {
				Spec:          toolchain.Variant{Name: "minsizerel", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "MinSizeRel"}},
				LanguageFlags: map[string][]string{"C": {"-Os", "-DNDEBUG"}},
			},
		},
	}
	b, _ := EmitResolved(rt, Config{})
	cfg := string(b.Files["cc_toolchain_config.bzl"])
	for _, want := range []string{`-O3`, `-Os`, `-DNDEBUG`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("opt slot missing %q\n%s", want, cfg)
		}
	}
	// -DNDEBUG appears in both; dedup means it's emitted once.
	count := strings.Count(cfg, `"-DNDEBUG"`)
	if count != 1 {
		t.Errorf("-DNDEBUG appears %d times, want 1 (dedup)", count)
	}
}

// TestEmitResolved_CustomMapping: a custom VariantMapping routes
// a sanitizer variant into BazelFeatureNone, dropping it; the dbg
// and opt slots stay empty.
func TestEmitResolved_CustomMapping(t *testing.T) {
	rt := &toolchain.ResolvedToolchain{
		Base: &toolchain.Model{
			Languages: map[string]toolchain.Language{
				"C": {CompilerPath: "/usr/bin/gcc"},
			},
		},
		Variants: map[string]*toolchain.VariantDelta{
			"asan": {
				Spec:          toolchain.Variant{Name: "asan", CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=address"}},
				LanguageFlags: map[string][]string{"C": {"-fsanitize=address"}},
			},
		},
	}
	cfg := Config{
		// Custom mapping: route nothing.
		VariantMapping: func(v toolchain.Variant) toolchain.BazelFeature {
			return toolchain.BazelFeatureNone
		},
	}
	b, err := EmitResolved(rt, cfg)
	if err != nil {
		t.Fatalf("EmitResolved: %v", err)
	}
	body := string(b.Files["cc_toolchain_config.bzl"])
	if strings.Contains(body, "-fsanitize=address") {
		t.Errorf("variant flag should not have been routed when mapping returns None\n%s", body)
	}
}

func TestEmitResolved_NilRejected(t *testing.T) {
	if _, err := EmitResolved(nil, Config{}); err == nil {
		t.Error("EmitResolved(nil) should error")
	}
	if _, err := EmitResolved(&toolchain.ResolvedToolchain{}, Config{}); err == nil {
		t.Error("EmitResolved with nil Base should error")
	}
}
