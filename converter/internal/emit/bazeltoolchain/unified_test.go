package bazeltoolchain

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

func TestEmitUnified_RejectsBadInputs(t *testing.T) {
	if _, err := EmitUnified(nil, UnifiedConfig{}); err == nil {
		t.Error("nil platforms should error")
	}
	if _, err := EmitUnified([]PlatformToolchain{}, UnifiedConfig{}); err == nil {
		t.Error("empty platforms should error")
	}
	good := PlatformToolchain{
		Name:        "linux_x86_64",
		Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
		Resolved: &toolchain.ResolvedToolchain{
			Base: &toolchain.Model{
				HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				Languages: map[string]toolchain.Language{
					"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/gcc"},
				},
			},
		},
	}
	if _, err := EmitUnified([]PlatformToolchain{good}, UnifiedConfig{}); err != nil {
		t.Errorf("happy path failed: %v", err)
	}

	type tweak func(*PlatformToolchain)
	for name, tw := range map[string]tweak{
		"empty Name":        func(p *PlatformToolchain) { p.Name = "" },
		"no Constraints":    func(p *PlatformToolchain) { p.Constraints = nil },
		"nil Resolved":      func(p *PlatformToolchain) { p.Resolved = nil },
		"nil Resolved.Base": func(p *PlatformToolchain) { p.Resolved = &toolchain.ResolvedToolchain{} },
	} {
		t.Run(name, func(t *testing.T) {
			cp := good
			cp.Constraints = append([]string(nil), good.Constraints...)
			tw(&cp)
			if _, err := EmitUnified([]PlatformToolchain{cp}, UnifiedConfig{}); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestEmitUnified_TwoPlatformsFullShape(t *testing.T) {
	mk := func(name, cpu string) PlatformToolchain {
		return PlatformToolchain{
			Name: name,
			Constraints: []string{
				"@platforms//os:linux",
				"@platforms//cpu:" + cpu,
			},
			Resolved: &toolchain.ResolvedToolchain{
				Base: &toolchain.Model{
					HostPlatform:   toolchain.Platform{OS: "Linux", CPU: cpu},
					TargetPlatform: toolchain.Platform{OS: "Linux", CPU: cpu},
					Languages: map[string]toolchain.Language{
						"C": {
							CompilerID:         "GNU",
							CompilerPath:       "/usr/bin/" + cpu + "-linux-gnu-gcc",
							BuiltinIncludeDirs: []string{"/usr/include"},
							BaseFlags:          []string{"-Wall"},
						},
					},
				},
				Variants: map[string]*toolchain.VariantDelta{
					"asan": {
						Spec: toolchain.Variant{
							Name:      "asan",
							CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=address"},
						},
						LanguageFlags: map[string][]string{"C": {"-fsanitize=address"}},
						LinkFlags:     []string{"-fsanitize=address"},
					},
				},
			},
		}
	}
	plats := []PlatformToolchain{
		mk("linux_x86_64", "x86_64"),
		mk("linux_aarch64", "arm64"),
	}

	bundle, err := EmitUnified(plats, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified: %v", err)
	}

	expectFiles := []string{
		"platforms/BUILD.bazel",
		"toolchains/BUILD.bazel",
		"toolchains/cc_toolchain_config.bzl",
		".bazelrc",
	}
	for _, f := range expectFiles {
		if _, ok := bundle.Files[f]; !ok {
			t.Errorf("bundle missing %s", f)
		}
	}

	// platforms/BUILD.bazel: one platform() per platform.
	platsB := string(bundle.Files["platforms/BUILD.bazel"])
	for _, want := range []string{
		`platform(`,
		`name = "linux_x86_64"`,
		`name = "linux_aarch64"`,
		`@platforms//cpu:x86_64`,
		`@platforms//cpu:arm64`,
	} {
		if !strings.Contains(platsB, want) {
			t.Errorf("platforms/BUILD.bazel missing %q\n%s", want, platsB)
		}
	}

	// toolchains/BUILD.bazel: per-platform cc_toolchain_config + cc_toolchain
	// + toolchain() trio plus the aggregating filegroup.
	tcB := string(bundle.Files["toolchains/BUILD.bazel"])
	for _, want := range []string{
		`load("@rules_cc//cc:defs.bzl", "cc_toolchain")`,
		`load(":cc_toolchain_config.bzl", "cc_toolchain_config")`,
		`name = "linux_x86_64_config"`,
		`name = "linux_x86_64_cc"`,
		`name = "linux_x86_64_toolchain"`,
		`name = "linux_aarch64_toolchain"`,
		`target_compatible_with = [`,
		`@bazel_tools//tools/cpp:toolchain_type`,
		`name = "all"`,
		`":linux_x86_64_toolchain"`,
		`":linux_aarch64_toolchain"`,
		`asan_compile_flags = [`,
		`"-fsanitize=address"`,
	} {
		if !strings.Contains(tcB, want) {
			t.Errorf("toolchains/BUILD.bazel missing %q", want)
		}
	}

	// cc_toolchain_config.bzl: ONE rule definition; no module
	// constants; attrs cover the per-platform fields + every feature.
	cfg := string(bundle.Files["toolchains/cc_toolchain_config.bzl"])
	for _, want := range []string{
		`@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl`,
		`def _impl(ctx):`,
		`cc_toolchain_config = rule(`,
		`"cpu": attr.string(mandatory = True)`,
		`"tool_paths": attr.string_dict(default = {})`,
		`"asan_compile_flags": attr.string_list(default = [])`,
		`"asan_link_flags": attr.string_list(default = [])`,
		`"tsan_compile_flags": attr.string_list(default = [])`,
		`"coverage_compile_flags": attr.string_list(default = [])`,
		`load("@rules_cc//cc/common:cc_common.bzl", "cc_common")`,
		`_feature_with_flags("asan", False, ctx.attr.asan_compile_flags, ctx.attr.asan_link_flags)`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q", want)
		}
	}

	// .bazelrc: try-import for operator overrides + sanitizer feature
	// aliases + platform aliases.
	rc := string(bundle.Files[".bazelrc"])
	for _, want := range []string{
		`try-import %workspace%/user.bazelrc`,
		`build:asan --features=asan`,
		`build:tsan --features=tsan`,
		`build:linux_x86_64 --platforms=//platforms:linux_x86_64`,
		`build:linux_aarch64 --platforms=//platforms:linux_aarch64`,
	} {
		if !strings.Contains(rc, want) {
			t.Errorf(".bazelrc missing %q\n%s", want, rc)
		}
	}
	// dbg/opt are NOT --config aliases (they ride --compilation_mode).
	for _, unwant := range []string{
		`build:dbg --features=dbg`,
		`build:opt --features=opt`,
	} {
		if strings.Contains(rc, unwant) {
			t.Errorf(".bazelrc should not alias %q (Bazel handles it via --compilation_mode)", unwant)
		}
	}
}

// TestEmitUnified_CXXFlagsRouteToCXXOnlyActions: the unified
// cc_toolchain_config rule's _impl must call
// _default_compile_flags_feature with cxx_flags so language-
// specific flags (CMAKE_CXX_FLAGS) reach C++ compile actions
// instead of being silently dropped.
func TestEmitUnified_CXXFlagsRouteToCXXOnlyActions(t *testing.T) {
	plat := PlatformToolchain{
		Name:        "linux_x86_64",
		Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
		Resolved: &toolchain.ResolvedToolchain{
			Base: &toolchain.Model{
				HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				Languages: map[string]toolchain.Language{
					"C":   {CompilerID: "Clang", CompilerPath: "/usr/bin/clang", BaseFlags: []string{"-Wall"}},
					"CXX": {CompilerID: "Clang", CompilerPath: "/usr/bin/clang++", BaseFlags: []string{"-std=c++20"}},
				},
			},
		},
	}
	b, err := EmitUnified([]PlatformToolchain{plat}, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified: %v", err)
	}
	cfg := string(b.Files["toolchains/cc_toolchain_config.bzl"])
	for _, want := range []string{
		"_CXX_COMPILE_ACTIONS = [",
		`"c++-compile",`,
		"_default_compile_flags_feature(ctx.attr.compile_flags, ctx.attr.cxx_flags, ctx.attr.link_flags)",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q\n%s", want, cfg)
		}
	}

	// And the rule instance carries the CXX-only flag bytes.
	tcB := string(b.Files["toolchains/BUILD.bazel"])
	if !strings.Contains(tcB, "cxx_flags = [\n        \"-std=c++20\",") {
		t.Errorf("toolchains/BUILD.bazel missing cxx_flags = [\"-std=c++20\"]:\n%s", tcB)
	}
}

// TestEmitUnified_VariantMappingFlowsThrough: a UnifiedConfig
// with a custom VariantMapping must affect which feature slot
// each variant's flags land in. Without threading cfg.VariantMapping
// into emitToolchainsBuild, the per-platform cc_toolchain_config
// rule instances would silently fall back to DefaultVariantMapping
// regardless of what the operator passed.
func TestEmitUnified_VariantMappingFlowsThrough(t *testing.T) {
	plat := PlatformToolchain{
		Name:        "linux_x86_64",
		Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
		Resolved: &toolchain.ResolvedToolchain{
			Base: &toolchain.Model{
				HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				Languages:      map[string]toolchain.Language{"C": {CompilerPath: "/usr/bin/gcc"}},
			},
			Variants: map[string]*toolchain.VariantDelta{
				"asan": {
					Spec: toolchain.Variant{
						Name:      "asan",
						CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=address"},
					},
					LanguageFlags: map[string][]string{"C": {"-fsanitize=address"}},
				},
			},
		},
	}

	// Custom mapping: route everything to BazelFeatureNone.
	// Default routing would land asan flags in _ASAN_COMPILE_FLAGS;
	// the custom mapping should drop them.
	cfg := UnifiedConfig{
		VariantMapping: func(v toolchain.Variant) toolchain.BazelFeature {
			return toolchain.BazelFeatureNone
		},
	}
	b, err := EmitUnified([]PlatformToolchain{plat}, cfg)
	if err != nil {
		t.Fatalf("EmitUnified: %v", err)
	}
	tcB := string(b.Files["toolchains/BUILD.bazel"])
	if strings.Contains(tcB, `"-fsanitize=address"`) {
		t.Errorf("custom VariantMapping=None did not drop the asan flag\n%s", tcB)
	}
	// Sanity: same input under default mapping DOES route to asan slot.
	bDefault, err := EmitUnified([]PlatformToolchain{plat}, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified default: %v", err)
	}
	tcDef := string(bDefault.Files["toolchains/BUILD.bazel"])
	if !strings.Contains(tcDef, `"-fsanitize=address"`) {
		t.Errorf("default mapping should route asan flag into a feature slot\n%s", tcDef)
	}
}

// TestEmitUnified_Deterministic re-emits the same input twice and
// diffs every output file. Without this, regenerating the unified
// layout would churn cache keys.
func TestEmitUnified_Deterministic(t *testing.T) {
	mk := func(name, cpu string) PlatformToolchain {
		return PlatformToolchain{
			Name: name,
			Constraints: []string{
				"@platforms//os:linux",
				"@platforms//cpu:" + cpu,
			},
			Resolved: &toolchain.ResolvedToolchain{
				Base: &toolchain.Model{
					HostPlatform:   toolchain.Platform{OS: "Linux", CPU: cpu},
					TargetPlatform: toolchain.Platform{OS: "Linux", CPU: cpu},
					Languages: map[string]toolchain.Language{
						"C":   {CompilerID: "GNU", CompilerPath: "/usr/bin/gcc"},
						"CXX": {CompilerID: "GNU", CompilerPath: "/usr/bin/g++"},
					},
				},
			},
		}
	}
	plats := []PlatformToolchain{mk("linux_x86_64", "x86_64"), mk("linux_aarch64", "arm64")}

	a, err := EmitUnified(plats, UnifiedConfig{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	b, err := EmitUnified(plats, UnifiedConfig{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for path, body := range a.Files {
		if string(body) != string(b.Files[path]) {
			t.Errorf("%s not deterministic\n--- a ---\n%s\n--- b ---\n%s", path, body, b.Files[path])
		}
	}
}
