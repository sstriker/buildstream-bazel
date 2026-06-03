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
	// + toolchain() trio.
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
		`asan_compile_flags = [`,
		`"-fsanitize=address"`,
	} {
		if !strings.Contains(tcB, want) {
			t.Errorf("toolchains/BUILD.bazel missing %q", want)
		}
	}
	// No target literally named "all" — it would shadow the
	// register_toolchains("//toolchains:all") package wildcard.
	if strings.Contains(tcB, `name = "all"`) {
		t.Errorf("toolchains/BUILD.bazel must not define a target named \"all\" (shadows the :all wildcard)\n%s", tcB)
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
	// Kit-less .bazelrc must not carry the kit-alias comment — it would
	// break byte-for-byte parity with the pre-kits output.
	if strings.Contains(rc, "# With kits") {
		t.Errorf("kit-less .bazelrc must not carry the kit-alias comment line\n%s", rc)
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

func TestEmitUnified_KitDimension(t *testing.T) {
	// Two kits on one platform → two toolchains disambiguated by a
	// `kit` constraint_value, named with the <platform>_<kit> slug.
	mk := func(kit, cc string) PlatformToolchain {
		return PlatformToolchain{
			Name: "linux_x86_64",
			Kit:  kit,
			Constraints: []string{
				"@platforms//os:linux",
				"@platforms//cpu:x86_64",
			},
			Resolved: &toolchain.ResolvedToolchain{
				Base: &toolchain.Model{
					HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
					TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
					Languages: map[string]toolchain.Language{
						"C": {
							CompilerID:         "GNU",
							CompilerPath:       cc,
							BuiltinIncludeDirs: []string{"/usr/include"},
						},
					},
				},
				Variants: map[string]*toolchain.VariantDelta{},
			},
		}
	}
	plats := []PlatformToolchain{mk("gcc-13", "/usr/bin/gcc-13"), mk("clang-15", "/usr/bin/clang-15")}

	bundle, err := EmitUnified(plats, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified: %v", err)
	}

	platsB := string(bundle.Files["platforms/BUILD.bazel"])
	for _, want := range []string{
		`constraint_setting(name = "kit")`,
		`constraint_value(`,
		`name = "gcc-13"`,
		`name = "clang-15"`,
		`constraint_setting = ":kit"`,
		`name = "linux_x86_64_gcc-13"`,   // platform() slug
		`name = "linux_x86_64_clang-15"`, // platform() slug
		`"//platforms:gcc-13"`,           // kit constraint in constraint_values
		`"//platforms:clang-15"`,
	} {
		if !strings.Contains(platsB, want) {
			t.Errorf("platforms/BUILD.bazel missing %q\n%s", want, platsB)
		}
	}

	tcB := string(bundle.Files["toolchains/BUILD.bazel"])
	for _, want := range []string{
		`name = "linux_x86_64_gcc-13_config"`,
		`name = "linux_x86_64_gcc-13_cc"`,
		`name = "linux_x86_64_gcc-13_toolchain"`,
		`name = "linux_x86_64_clang-15_toolchain"`,
		`toolchain_identifier = "linux_x86_64_gcc-13"`,
		`"//platforms:gcc-13"`, // kit constraint in target_compatible_with
		`"//platforms:clang-15"`,
	} {
		if !strings.Contains(tcB, want) {
			t.Errorf("toolchains/BUILD.bazel missing %q\n%s", want, tcB)
		}
	}

	rc := string(bundle.Files[".bazelrc"])
	for _, want := range []string{
		"# With kits, the alias is per (platform, kit)",
		"build:linux_x86_64_gcc-13 --platforms=//platforms:linux_x86_64_gcc-13",
		"build:linux_x86_64_clang-15 --platforms=//platforms:linux_x86_64_clang-15",
	} {
		if !strings.Contains(rc, want) {
			t.Errorf(".bazelrc missing %q\n%s", want, rc)
		}
	}
}

func TestEmitUnified_NoKitOmitsConstraintSetting(t *testing.T) {
	// Backward-compat: a kit-less platform emits NO kit constraint
	// dimension (the pre-kits layout).
	p := PlatformToolchain{
		Name:        "linux_x86_64",
		Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
		Resolved: &toolchain.ResolvedToolchain{
			Base: &toolchain.Model{
				TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				Languages:      map[string]toolchain.Language{"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/gcc"}},
			},
			Variants: map[string]*toolchain.VariantDelta{},
		},
	}
	bundle, err := EmitUnified([]PlatformToolchain{p}, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified: %v", err)
	}
	platsB := string(bundle.Files["platforms/BUILD.bazel"])
	// Check the rule CALLS (constraint_setting(...) / constraint_value(...)),
	// not the always-present `constraint_values = [` platform() attr.
	if strings.Contains(platsB, "constraint_setting(") || strings.Contains(platsB, "constraint_value(") {
		t.Errorf("kit-less run emitted a kit constraint dimension:\n%s", platsB)
	}
	if !strings.Contains(platsB, `name = "linux_x86_64"`) {
		t.Errorf("kit-less platform should keep its bare slug name\n%s", platsB)
	}
}

func TestEmitUnified_RejectsDuplicateSlug(t *testing.T) {
	mk := func() PlatformToolchain {
		return PlatformToolchain{
			Name:        "linux_x86_64",
			Kit:         "gcc-13",
			Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
			Resolved: &toolchain.ResolvedToolchain{
				Base: &toolchain.Model{
					TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
					Languages:      map[string]toolchain.Language{"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/gcc-13"}},
				},
				Variants: map[string]*toolchain.VariantDelta{},
			},
		}
	}
	if _, err := EmitUnified([]PlatformToolchain{mk(), mk()}, UnifiedConfig{}); err == nil {
		t.Fatal("EmitUnified accepted duplicate (Name, Kit) slug; want error")
	}
}

func TestEmitUnified_RejectsReservedKitName(t *testing.T) {
	// A kit literally named "kit" collides with the generated
	// constraint_setting(name = "kit") in platforms/BUILD.bazel.
	p := PlatformToolchain{
		Name:        "linux_x86_64",
		Kit:         "kit",
		Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
		Resolved: &toolchain.ResolvedToolchain{
			Base: &toolchain.Model{
				TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
				Languages:      map[string]toolchain.Language{"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/gcc"}},
			},
			Variants: map[string]*toolchain.VariantDelta{},
		},
	}
	if _, err := EmitUnified([]PlatformToolchain{p}, UnifiedConfig{}); err == nil {
		t.Fatal(`EmitUnified accepted reserved kit name "kit"; want error`)
	}
}

func TestEmitUnified_RejectsKitVsPlatformNameCollision(t *testing.T) {
	// Mixed probe set: one kit-less platform named "linux_x86_64" and a
	// second platform carrying a kit *named* "linux_x86_64". In
	// platforms/BUILD.bazel the kit-less platform() and the kit's
	// constraint_value() would both claim the target name "linux_x86_64".
	mk := func(name, kit string) PlatformToolchain {
		return PlatformToolchain{
			Name:        name,
			Kit:         kit,
			Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
			Resolved: &toolchain.ResolvedToolchain{
				Base: &toolchain.Model{
					TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
					Languages:      map[string]toolchain.Language{"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/gcc"}},
				},
				Variants: map[string]*toolchain.VariantDelta{},
			},
		}
	}
	plats := []PlatformToolchain{
		mk("linux_x86_64", ""),    // kit-less → platform slug "linux_x86_64"
		mk("foo", "linux_x86_64"), // kit constraint_value "linux_x86_64"
	}
	if _, err := EmitUnified(plats, UnifiedConfig{}); err == nil {
		t.Fatal("EmitUnified accepted a kit name equal to a platform name; want collision error")
	}
}

// TestEmitUnified_BuiltinSysroot: a probed CMAKE_SYSROOT becomes the
// cc_toolchain_config builtin_sysroot instance attr (so Bazel passes
// --sysroot=), the rule definition declares the attr + threads it
// (empty → None), and a host build (no sysroot) emits no instance attr
// so its output stays byte-identical to the pre-sysroot layout.
func TestEmitUnified_BuiltinSysroot(t *testing.T) {
	mk := func(sysroot string) PlatformToolchain {
		return PlatformToolchain{
			Name:        "linux_aarch64",
			Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"},
			Resolved: &toolchain.ResolvedToolchain{
				Base: &toolchain.Model{
					TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "aarch64"},
					Sysroot:        sysroot,
					Languages: map[string]toolchain.Language{
						"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/aarch64-linux-gnu-gcc"},
					},
				},
				Variants: map[string]*toolchain.VariantDelta{},
			},
		}
	}

	withSR, err := EmitUnified([]PlatformToolchain{mk("/opt/aarch64-sysroot")}, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified (sysroot): %v", err)
	}
	tcB := string(withSR.Files["toolchains/BUILD.bazel"])
	if !strings.Contains(tcB, `builtin_sysroot = "/opt/aarch64-sysroot"`) {
		t.Errorf("toolchains/BUILD.bazel missing builtin_sysroot instance attr\n%s", tcB)
	}
	cfg := string(withSR.Files["toolchains/cc_toolchain_config.bzl"])
	for _, want := range []string{
		`"builtin_sysroot": attr.string(default = "")`,
		`builtin_sysroot = ctx.attr.builtin_sysroot or None`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q", want)
		}
	}

	hostBundle, err := EmitUnified([]PlatformToolchain{mk("")}, UnifiedConfig{})
	if err != nil {
		t.Fatalf("EmitUnified (host): %v", err)
	}
	if hostB := string(hostBundle.Files["toolchains/BUILD.bazel"]); strings.Contains(hostB, "builtin_sysroot =") {
		t.Errorf("host build must not emit a builtin_sysroot instance attr\n%s", hostB)
	}
}
