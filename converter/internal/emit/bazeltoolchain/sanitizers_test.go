package bazeltoolchain

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
)

// TestEmitResolved_SanitizerVariantsRoutedToFeatures: a probe matrix
// containing asan / tsan / ubsan / coverage cells must produce
// non-empty _ASAN_COMPILE_FLAGS / _TSAN_COMPILE_FLAGS / etc.
// constants in the emitted .bzl. This is the Stage 2 contract:
// `--features=asan` reaches the right flag bundle without the
// operator authoring per-feature plumbing by hand.
func TestEmitResolved_SanitizerVariantsRoutedToFeatures(t *testing.T) {
	rt := &toolchain.ResolvedToolchain{
		Base: &toolchain.Model{
			HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
			TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
			Languages: map[string]toolchain.Language{
				"C": {
					CompilerID:           "Clang",
					CompilerPath:         "/usr/bin/clang-15",
					BuiltinIncludeDirs:   []string{"/usr/include"},
					BaseFlags:            []string{"-Wall"},
					SourceFileExtensions: []string{"c"},
				},
			},
		},
		Variants: map[string]*toolchain.VariantDelta{
			"asan": {
				Spec: toolchain.Variant{
					Name: "asan",
					CacheVars: map[string]string{
						"CMAKE_BUILD_TYPE": "Debug",
						"CMAKE_C_FLAGS":    "-fsanitize=address -fno-omit-frame-pointer",
					},
				},
				LanguageFlags: map[string][]string{"C": {"-fsanitize=address", "-fno-omit-frame-pointer"}},
				LinkFlags:     []string{"-fsanitize=address"},
			},
			"tsan": {
				Spec: toolchain.Variant{
					Name:      "tsan",
					CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=thread"},
				},
				LanguageFlags: map[string][]string{"C": {"-fsanitize=thread"}},
				LinkFlags:     []string{"-fsanitize=thread"},
			},
			"ubsan": {
				Spec: toolchain.Variant{
					Name:      "ubsan",
					CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=undefined"},
				},
				LanguageFlags: map[string][]string{"C": {"-fsanitize=undefined"}},
			},
			"coverage": {
				Spec: toolchain.Variant{
					Name:      "coverage",
					CacheVars: map[string]string{"CMAKE_C_FLAGS": "--coverage"},
				},
				LanguageFlags: map[string][]string{"C": {"--coverage"}},
			},
		},
	}
	b, err := EmitResolved(rt, Config{})
	if err != nil {
		t.Fatalf("EmitResolved: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])

	// Sanitizer cells routed via DefaultVariantMapping into the
	// matching feature constants.
	for _, want := range []string{
		"_ASAN_COMPILE_FLAGS = [\n    \"-fsanitize=address\",",
		"_ASAN_LINK_FLAGS = [\n    \"-fsanitize=address\",",
		"_TSAN_COMPILE_FLAGS = [\n    \"-fsanitize=thread\",",
		"_TSAN_LINK_FLAGS = [\n    \"-fsanitize=thread\",",
		"_UBSAN_COMPILE_FLAGS = [\n    \"-fsanitize=undefined\",",
		"_COVERAGE_COMPILE_FLAGS = [\n    \"--coverage\",",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q in:\n%s", want, cfg)
		}
	}

	// Untouched feature slots stay empty (msan, lto not in the
	// fixture's variant set).
	for _, want := range []string{
		"_MSAN_COMPILE_FLAGS = []",
		"_MSAN_LINK_FLAGS = []",
		"_LTO_COMPILE_FLAGS = []",
		"_LTO_LINK_FLAGS = []",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("expected empty slot %q; not found in:\n%s", want, cfg)
		}
	}
}

// TestEmitResolved_FeatureSlotsAlwaysPresent locks in the
// invariant that even with an empty ResolvedToolchain (no
// variants), all per-feature flag constants are emitted as empty
// lists. The Starlark _impl references each constant by name; a
// missing constant would crash the rule load.
func TestEmitResolved_FeatureSlotsAlwaysPresent(t *testing.T) {
	rt := &toolchain.ResolvedToolchain{
		Base: &toolchain.Model{
			Languages: map[string]toolchain.Language{
				"C": {CompilerPath: "/usr/bin/cc"},
			},
		},
	}
	b, err := EmitResolved(rt, Config{})
	if err != nil {
		t.Fatalf("EmitResolved: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])
	for _, name := range []string{
		"_DBG_COMPILE_FLAGS",
		"_OPT_COMPILE_FLAGS",
		"_ASAN_COMPILE_FLAGS",
		"_TSAN_COMPILE_FLAGS",
		"_MSAN_COMPILE_FLAGS",
		"_UBSAN_COMPILE_FLAGS",
		"_COVERAGE_COMPILE_FLAGS",
		"_LTO_COMPILE_FLAGS",
		"_DBG_LINK_FLAGS",
		"_OPT_LINK_FLAGS",
		"_ASAN_LINK_FLAGS",
		"_TSAN_LINK_FLAGS",
		"_MSAN_LINK_FLAGS",
		"_UBSAN_LINK_FLAGS",
		"_COVERAGE_LINK_FLAGS",
		"_LTO_LINK_FLAGS",
	} {
		if !strings.Contains(cfg, name+" =") {
			t.Errorf("missing constant %s in:\n%s", name, cfg)
		}
	}
}
