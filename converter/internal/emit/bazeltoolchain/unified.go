package bazeltoolchain

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

// PlatformToolchain pairs one platform with its observed
// ResolvedToolchain. Used as input to EmitUnified.
type PlatformToolchain struct {
	// Name is the operator-facing platform name ("linux_x86_64",
	// "linux_aarch64"). Combined with Kit it forms the slug that
	// becomes the platforms/BUILD.bazel rule name and the
	// toolchains/BUILD.bazel cc_toolchain prefix.
	Name string

	// Kit, when non-empty, is the compiler-kit name (from
	// cmake-kits.json) this toolchain was probed under. It makes the
	// (platform, kit) pair the unit of toolchain emission: each
	// distinct kit on a platform becomes its own cc_toolchain (a kit
	// pins a different compiler → different tool_paths) and is
	// disambiguated by a `kit` constraint_value at toolchain
	// resolution. Empty Kit = one toolchain per platform, with no kit
	// constraint dimension (the pre-kits layout, byte-for-byte).
	Kit string

	// Constraints are the constraint_value labels identifying this
	// platform. Become the platform()'s constraint_values and each
	// toolchain()'s target_compatible_with (plus the per-kit
	// constraint_value when Kit is set).
	Constraints []string

	// Resolved is the empirical fold of all probe cells for this
	// (platform, kit) (Observe() output across the build variants
	// probed under that compiler).
	Resolved *toolchain.ResolvedToolchain
}

// slug is the Bazel-target-safe identifier for this (platform, kit):
// the bare platform name when kit-less (preserving the pre-kits layout)
// or "<platform>_<kit>" when a kit pins the compiler axis.
func (p PlatformToolchain) slug() string {
	if p.Kit == "" {
		return p.Name
	}
	return p.Name + "_" + p.Kit
}

// kitConstraintSetting is the name of the generated constraint_setting
// the per-kit constraint_value targets belong to. It doubles as a
// reserved kit name: a kit named "kit" would emit
// constraint_value(name = "kit") next to constraint_setting(name = "kit")
// in the same package — a duplicate target. EmitUnified rejects that.
const kitConstraintSetting = "kit"

// kitLabel is the constraint_value label for a kit, emitted into
// platforms/BUILD.bazel and referenced from constraint_values /
// target_compatible_with.
func kitLabel(kit string) string { return "//platforms:" + kit }

// distinctKits returns the sorted set of non-empty kit names across
// plats — the kits that need a constraint_value emitted.
func distinctKits(plats []PlatformToolchain) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range plats {
		if p.Kit == "" || seen[p.Kit] {
			continue
		}
		seen[p.Kit] = true
		out = append(out, p.Kit)
	}
	sort.Strings(out)
	return out
}

// constraintValuesFor returns p's platform constraints plus its kit
// constraint_value (when Kit is set), sorted for golden stability.
func constraintValuesFor(p PlatformToolchain) []string {
	cvs := append([]string(nil), p.Constraints...)
	if p.Kit != "" {
		cvs = append(cvs, kitLabel(p.Kit))
	}
	sort.Strings(cvs)
	return cvs
}

// UnifiedConfig is the shared emit-time config (per-platform data
// lives in PlatformToolchain).
type UnifiedConfig struct {
	// VariantMapping classifies each Variant into a Bazel feature
	// slot. Nil falls back to toolchain.DefaultVariantMapping.
	VariantMapping toolchain.VariantMapping
}

// UnifiedBundle is the result of EmitUnified: the four files that
// land at the operator's repo root.
//
// File layout:
//
//	platforms/BUILD.bazel
//	toolchains/BUILD.bazel
//	toolchains/cc_toolchain_config.bzl
//	.bazelrc
//
// The unifier (cmd/unify-toolchains) writes each entry verbatim.
// MODULE.bazel is intentionally absent — the operator owns it; we
// only print a one-time setup banner if register_toolchains is
// missing.
type UnifiedBundle struct {
	Files map[string][]byte
}

// EmitUnified renders the standard multi-platform Bazel layout
// from N PlatformToolchain inputs. One Starlark rule in
// cc_toolchain_config.bzl is reused across every platform — the
// per-platform data flows in as rule instance attrs.
func EmitUnified(plats []PlatformToolchain, cfg UnifiedConfig) (*UnifiedBundle, error) {
	if len(plats) == 0 {
		return nil, fmt.Errorf("bazeltoolchain.EmitUnified: at least one platform required")
	}
	seenSlug := map[string]bool{}
	for i, p := range plats {
		if p.Name == "" {
			return nil, fmt.Errorf("bazeltoolchain.EmitUnified: plats[%d] has empty Name", i)
		}
		if len(p.Constraints) == 0 {
			return nil, fmt.Errorf("bazeltoolchain.EmitUnified: plats[%d] (%s) has no Constraints", i, p.Name)
		}
		if p.Resolved == nil || p.Resolved.Base == nil {
			return nil, fmt.Errorf("bazeltoolchain.EmitUnified: plats[%d] (%s) has nil Resolved/Base", i, p.Name)
		}
		s := p.slug()
		if seenSlug[s] {
			return nil, fmt.Errorf("bazeltoolchain.EmitUnified: duplicate platform/kit slug %q (plats[%d]); each (Name, Kit) must be unique", s, i)
		}
		seenSlug[s] = true
	}

	// platforms/BUILD.bazel namespace check. When kits are present, that
	// one package holds the `kit` constraint_setting, every kit
	// constraint_value, AND every platform slug — all in a single target
	// namespace. A kit named like a platform slug (common in a mixed
	// probe set where some cells are kit-less), a kit named "kit", or a
	// platform slug equal to a kit name all collide into a duplicate
	// target and an unparsable BUILD. Reject any such collision up front
	// with a message that names the offenders. (Kit-less runs emit no
	// constraint_setting / constraint_value, and platform slugs are the
	// already-unique platform names, so this is a no-op for them.)
	if kits := distinctKits(plats); len(kits) > 0 {
		owner := map[string]string{}
		claim := func(name, kind string) error {
			if prev, ok := owner[name]; ok {
				return fmt.Errorf("bazeltoolchain.EmitUnified: //platforms target %q is claimed by both %s and %s; rename the kit or platform so they don't collide", name, prev, kind)
			}
			owner[name] = kind
			return nil
		}
		if err := claim(kitConstraintSetting, "the kit constraint_setting"); err != nil {
			return nil, err
		}
		for _, k := range kits {
			if err := claim(k, fmt.Sprintf("kit constraint_value %q", k)); err != nil {
				return nil, err
			}
		}
		for _, p := range plats {
			if err := claim(p.slug(), fmt.Sprintf("platform %q", p.slug())); err != nil {
				return nil, err
			}
		}
	}

	// Stable platform order: input order. The emit functions
	// preserve it so two unifier runs over the same probe set
	// produce byte-identical output.

	platsBuild, err := emitPlatformsBuild(plats)
	if err != nil {
		return nil, err
	}
	tcBuild, err := emitToolchainsBuild(plats, cfg)
	if err != nil {
		return nil, err
	}
	tcConfigBzl, err := emitUnifiedConfigBzl(plats, cfg)
	if err != nil {
		return nil, err
	}
	bazelrc, err := emitBazelrc(plats)
	if err != nil {
		return nil, err
	}

	return &UnifiedBundle{
		Files: map[string][]byte{
			"platforms/BUILD.bazel":              platsBuild,
			"toolchains/BUILD.bazel":             tcBuild,
			"toolchains/cc_toolchain_config.bzl": tcConfigBzl,
			".bazelrc":                           bazelrc,
		},
	}, nil
}

// emitPlatformsBuild renders platforms/BUILD.bazel: one platform()
// rule per target. Constraint order is sorted for golden stability.
func emitPlatformsBuild(plats []PlatformToolchain) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Generated by unify-toolchains. DO NOT EDIT.\n")
	b.WriteString("# Hand-authored platform overrides should live in a separate package.\n\n")

	// Kit constraint dimension: one constraint_setting + a
	// constraint_value per distinct compiler kit. Each (platform, kit)
	// platform() below carries its kit's value, and the matching
	// toolchain()'s target_compatible_with requires it, so toolchain
	// resolution picks the right compiler. Emitted only when kits are
	// present — a kit-less run keeps the pre-kits platforms/ layout
	// byte-for-byte.
	kits := distinctKits(plats)
	if len(kits) > 0 {
		fmt.Fprintf(&b, "constraint_setting(name = %q)\n\n", kitConstraintSetting)
		for _, k := range kits {
			fmt.Fprintf(&b, "constraint_value(\n")
			fmt.Fprintf(&b, "    name = %q,\n", k)
			fmt.Fprintf(&b, "    constraint_setting = %q,\n", ":"+kitConstraintSetting)
			fmt.Fprintf(&b, "    visibility = [\"//visibility:public\"],\n")
			fmt.Fprintf(&b, ")\n\n")
		}
	}

	for _, p := range plats {
		fmt.Fprintf(&b, "platform(\n")
		fmt.Fprintf(&b, "    name = %q,\n", p.slug())
		fmt.Fprintf(&b, "    constraint_values = [\n")
		for _, c := range constraintValuesFor(p) {
			fmt.Fprintf(&b, "        %q,\n", c)
		}
		fmt.Fprintf(&b, "    ],\n")
		fmt.Fprintf(&b, "    visibility = [\"//visibility:public\"],\n")
		fmt.Fprintf(&b, ")\n\n")
	}
	return b.Bytes(), nil
}

// emitToolchainsBuild renders toolchains/BUILD.bazel: one
// cc_toolchain_config + cc_toolchain + toolchain() trio per
// platform. Operators activate them with
// register_toolchains("//toolchains:all"), where ":all" is Bazel's
// package wildcard — it registers every toolchain() in the package.
// We deliberately emit NO target literally named "all": such a target
// would shadow the wildcard, and register_toolchains would bind the
// label to it (a non-toolchain target with no DeclaredToolchainInfo)
// and fail analysis.
func emitToolchainsBuild(plats []PlatformToolchain, cfg UnifiedConfig) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Generated by unify-toolchains. DO NOT EDIT.\n\n")
	// Bazel 9 removed the native cc_toolchain rule; load it from rules_cc.
	b.WriteString(`load("@rules_cc//cc:defs.bzl", "cc_toolchain")` + "\n")
	b.WriteString(`load(":cc_toolchain_config.bzl", "cc_toolchain_config")` + "\n\n")

	for _, p := range plats {
		base := p.Resolved.Base
		cMost := primaryLanguage(base)
		cxx := base.Languages["CXX"]
		tools := mergedTools(base)
		osName := strings.ToLower(orDefault(base.TargetPlatform.OS, "unknown"))
		cpu := normalizeBazelCPU(orDefault(base.TargetPlatform.CPU, "unknown"))
		targetSystem := orDefault(cMost.Target, fmt.Sprintf("%s-%s", cpu, osName))
		compiler := strings.ToLower(orDefault(cMost.CompilerID, "unknown"))
		libc := defaultLibcFor(osName)

		slug := p.slug()
		configName := slug + "_config"
		ccName := slug + "_cc"
		toolchainName := slug + "_toolchain"

		fmt.Fprintf(&b, "cc_toolchain_config(\n")
		fmt.Fprintf(&b, "    name = %q,\n", configName)
		fmt.Fprintf(&b, "    cpu = %q,\n", cpu)
		fmt.Fprintf(&b, "    compiler = %q,\n", compiler)
		fmt.Fprintf(&b, "    toolchain_identifier = %q,\n", slug)
		fmt.Fprintf(&b, "    host_system_name = %q,\n", targetSystem)
		fmt.Fprintf(&b, "    target_system_name = %q,\n", targetSystem)
		fmt.Fprintf(&b, "    target_libc = %q,\n", libc)
		fmt.Fprintf(&b, "    abi_version = %q,\n", "local")
		fmt.Fprintf(&b, "    abi_libc_version = %q,\n", "local")
		emitListAttr(&b, "cxx_builtin_include_directories", unionStrings(cMost.BuiltinIncludeDirs, cxx.BuiltinIncludeDirs))
		emitDictAttr(&b, "tool_paths", tools)
		emitListAttr(&b, "compile_flags", cMost.BaseFlags)
		emitListAttr(&b, "cxx_flags", cxx.BaseFlags)
		emitListAttr(&b, "link_flags", cMost.LinkFlags)
		// Per-feature flag attrs.
		for _, f := range featureSlots {
			compile, link := variantFlagsFor(p.Resolved, cfg.VariantMapping, f)
			emitListAttr(&b, string(f)+"_compile_flags", compile)
			emitListAttr(&b, string(f)+"_link_flags", link)
		}
		fmt.Fprintf(&b, ")\n\n")

		fmt.Fprintf(&b, "cc_toolchain(\n")
		fmt.Fprintf(&b, "    name = %q,\n", ccName)
		fmt.Fprintf(&b, "    toolchain_config = %q,\n", ":"+configName)
		fmt.Fprintf(&b, "    all_files = %q,\n", ":empty")
		fmt.Fprintf(&b, "    compiler_files = %q,\n", ":empty")
		fmt.Fprintf(&b, "    dwp_files = %q,\n", ":empty")
		fmt.Fprintf(&b, "    linker_files = %q,\n", ":empty")
		fmt.Fprintf(&b, "    objcopy_files = %q,\n", ":empty")
		fmt.Fprintf(&b, "    strip_files = %q,\n", ":empty")
		fmt.Fprintf(&b, "    supports_param_files = 1,\n")
		fmt.Fprintf(&b, ")\n\n")

		fmt.Fprintf(&b, "toolchain(\n")
		fmt.Fprintf(&b, "    name = %q,\n", toolchainName)
		fmt.Fprintf(&b, "    target_compatible_with = [\n")
		for _, c := range constraintValuesFor(p) {
			fmt.Fprintf(&b, "        %q,\n", c)
		}
		fmt.Fprintf(&b, "    ],\n")
		fmt.Fprintf(&b, "    toolchain = %q,\n", ":"+ccName)
		fmt.Fprintf(&b, "    toolchain_type = %q,\n", "@bazel_tools//tools/cpp:toolchain_type")
		fmt.Fprintf(&b, ")\n\n")
	}

	// No "all" filegroup: register_toolchains("//toolchains:all") relies
	// on ":all" being the package wildcard, so a target named "all" would
	// shadow it and break registration (see emitToolchainsBuild doc).

	// :empty filegroup for cc_toolchain's *_files attrs.
	fmt.Fprintf(&b, "filegroup(name = \"empty\", srcs = [])\n")

	return b.Bytes(), nil
}

// emitUnifiedConfigBzl renders ONE attr-driven cc_toolchain_config
// rule definition reused across every platform. Per-platform data
// (tool paths, builtin includes, flag bundles) flows in as rule
// instance attrs from toolchains/BUILD.bazel — the .bzl file
// itself is platform-agnostic.
//
// Stage 2's emit had module-level constants (single platform, fast
// to write); Stage 5 generalizes by lifting those constants to
// rule attrs so one rule serves N platforms.
func emitUnifiedConfigBzl(plats []PlatformToolchain, cfg UnifiedConfig) ([]byte, error) {
	_ = plats // shape doesn't depend on platforms — they pass attrs at instantiation
	_ = cfg
	var b bytes.Buffer
	b.WriteString("# Generated by unify-toolchains. DO NOT EDIT.\n")
	b.WriteString("# One rule, all platforms, all features. Per-platform data flows in via attrs.\n")
	b.WriteString("\n")
	// Bazel 9 stripped the cc_common built-in global; load it from rules_cc.
	b.WriteString(`load("@rules_cc//cc/common:cc_common.bzl", "cc_common")` + "\n")
	b.WriteString(`load("@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl",` + "\n")
	b.WriteString(`     "feature", "flag_group", "flag_set", "tool_path")` + "\n\n")

	b.WriteString(`_ALL_COMPILE_ACTIONS = [
    "assemble",
    "preprocess-assemble",
    "c-compile",
    "c++-compile",
    "c++-header-parsing",
    "c++-module-compile",
    "c++-module-codegen",
    "lto-backend",
]

# C++-only subset, used to route ctx.attr.cxx_flags to C++ actions
# specifically. cmake puts -std=c++20 / -stdlib=... into
# CMAKE_CXX_FLAGS rather than CMAKE_C_FLAGS, so a single shared
# default_compile_flags slot would drop them silently.
_CXX_COMPILE_ACTIONS = [
    "c++-compile",
    "c++-header-parsing",
    "c++-module-compile",
    "c++-module-codegen",
]

_ALL_LINK_ACTIONS = [
    "c++-link-executable",
    "c++-link-dynamic-library",
    "c++-link-nodeps-dynamic-library",
]

def _feature_with_flags(name, enabled, compile_flags, link_flags):
    flag_sets = []
    if compile_flags:
        flag_sets.append(flag_set(
            actions = _ALL_COMPILE_ACTIONS,
            flag_groups = [flag_group(flags = compile_flags)],
        ))
    if link_flags:
        flag_sets.append(flag_set(
            actions = _ALL_LINK_ACTIONS,
            flag_groups = [flag_group(flags = link_flags)],
        ))
    return feature(name = name, enabled = enabled, flag_sets = flag_sets)

def _default_compile_flags_feature(compile_flags, cxx_flags, link_flags):
    flag_sets = []
    if compile_flags:
        flag_sets.append(flag_set(
            actions = _ALL_COMPILE_ACTIONS,
            flag_groups = [flag_group(flags = compile_flags)],
        ))
    if cxx_flags:
        flag_sets.append(flag_set(
            actions = _CXX_COMPILE_ACTIONS,
            flag_groups = [flag_group(flags = cxx_flags)],
        ))
    if link_flags:
        flag_sets.append(flag_set(
            actions = _ALL_LINK_ACTIONS,
            flag_groups = [flag_group(flags = link_flags)],
        ))
    return feature(name = "default_compile_flags", enabled = True, flag_sets = flag_sets)

def _impl(ctx):
    features = [
        _default_compile_flags_feature(ctx.attr.compile_flags, ctx.attr.cxx_flags, ctx.attr.link_flags),
`)
	for _, f := range featureSlots {
		fmt.Fprintf(&b, "        _feature_with_flags(%q, False, ctx.attr.%s_compile_flags, ctx.attr.%s_link_flags),\n",
			string(f), string(f), string(f))
	}
	b.WriteString(`    ]
    return [cc_common.create_cc_toolchain_config_info(
        ctx = ctx,
        toolchain_identifier = ctx.attr.toolchain_identifier,
        host_system_name = ctx.attr.host_system_name,
        target_system_name = ctx.attr.target_system_name,
        target_cpu = ctx.attr.cpu,
        target_libc = ctx.attr.target_libc,
        compiler = ctx.attr.compiler,
        abi_version = ctx.attr.abi_version,
        abi_libc_version = ctx.attr.abi_libc_version,
        tool_paths = [tool_path(name = name, path = path) for name, path in ctx.attr.tool_paths.items()],
        cxx_builtin_include_directories = ctx.attr.cxx_builtin_include_directories,
        features = features,
    )]

cc_toolchain_config = rule(
    implementation = _impl,
    attrs = {
        "cpu": attr.string(mandatory = True),
        "compiler": attr.string(mandatory = True),
        "toolchain_identifier": attr.string(mandatory = True),
        "host_system_name": attr.string(mandatory = True),
        "target_system_name": attr.string(mandatory = True),
        "target_libc": attr.string(default = "unknown"),
        "abi_version": attr.string(default = "local"),
        "abi_libc_version": attr.string(default = "local"),
        "cxx_builtin_include_directories": attr.string_list(default = []),
        "tool_paths": attr.string_dict(default = {}),
        "compile_flags": attr.string_list(default = []),
        "cxx_flags": attr.string_list(default = []),
        "link_flags": attr.string_list(default = []),
`)
	for _, f := range featureSlots {
		fmt.Fprintf(&b, "        \"%s_compile_flags\": attr.string_list(default = []),\n", string(f))
		fmt.Fprintf(&b, "        \"%s_link_flags\": attr.string_list(default = []),\n", string(f))
	}
	b.WriteString(`    },
    # provides intentionally omitted (CcToolchainConfigInfo is not a loadable
    # global under bazel 9; the rule still returns it via
    # cc_common.create_cc_toolchain_config_info).
)
`)
	return b.Bytes(), nil
}

// emitBazelrc renders the operator's .bazelrc. First line is the
// try-import for user.bazelrc so operator-authored overrides win
// (Bazel later-wins). Then --config aliases for sanitizers and
// platforms.
func emitBazelrc(plats []PlatformToolchain) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Generated by unify-toolchains. Operator-authored overrides go in user.bazelrc.\n")
	b.WriteString("# Bazel applies .bazelrc and user.bazelrc later-wins; user.bazelrc takes precedence.\n")
	b.WriteString("try-import %workspace%/user.bazelrc\n\n")

	b.WriteString("# Sanitizer / coverage / lto features. These ride --features=<name>;\n")
	b.WriteString("# the unifier emits matching feature() blocks in cc_toolchain_config.bzl.\n")
	for _, f := range featureSlots {
		// dbg + opt are driven by --compilation_mode, not --features,
		// so we don't alias them here.
		switch f {
		case toolchain.BazelFeatureDbg, toolchain.BazelFeatureOpt:
			continue
		}
		fmt.Fprintf(&b, "build:%s --features=%s\n", string(f), string(f))
	}
	b.WriteString("\n# Platform aliases — operator selects via --config=<platform>.\n")
	// Only when kits are in play — a kit-less .bazelrc must stay
	// byte-for-byte identical to the pre-kits output.
	if len(distinctKits(plats)) > 0 {
		b.WriteString("# With kits, the alias is per (platform, kit): --config=<platform>_<kit>.\n")
	}
	for _, p := range plats {
		fmt.Fprintf(&b, "build:%s --platforms=//platforms:%s\n", p.slug(), p.slug())
	}
	return b.Bytes(), nil
}

// emitListAttr / emitDictAttr render rule-instance attrs in the
// `name = [items],` form expected at the cc_toolchain_config call
// site. Empty lists collapse to `name = [],` to keep the diff
// minimal across runs that don't observe a particular feature.
func emitListAttr(b *bytes.Buffer, name string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(b, "    %s = [],\n", name)
		return
	}
	fmt.Fprintf(b, "    %s = [\n", name)
	for _, it := range items {
		fmt.Fprintf(b, "        %q,\n", it)
	}
	fmt.Fprintf(b, "    ],\n")
}

func emitDictAttr(b *bytes.Buffer, name string, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Fprintf(b, "    %s = {},\n", name)
		return
	}
	fmt.Fprintf(b, "    %s = {\n", name)
	for _, k := range keys {
		fmt.Fprintf(b, "        %q: %q,\n", k, m[k])
	}
	fmt.Fprintf(b, "    },\n")
}

// defaultLibcFor mirrors the heuristic in Config.withDefaults. Used
// when EmitUnified isn't given an explicit per-platform libc.
func defaultLibcFor(osName string) string {
	switch osName {
	case "linux":
		return "glibc"
	case "darwin":
		return "macosx"
	}
	return "unknown"
}
