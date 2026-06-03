// Package bazeltoolchain renders a toolchain.Model to Bazel rules:
//
//   - BUILD.bazel: cc_toolchain + cc_toolchain_config + platform +
//     toolchain rules.
//   - cc_toolchain_config.bzl: a hand-rolled cc_toolchain_config rule
//     built on @bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl's
//     feature/flag_set/flag_group helpers. Hand-rolled rather than
//     wrapping unix_cc_toolchain_config because the latter's feature
//     list is sealed — no extension surface for sanitizers, coverage,
//     LTO, or other custom features. The rule emits one feature()
//     block per BazelFeature whose flag bundle is non-empty:
//     default_compile_flags + dbg + opt come from the standard probe
//     matrix; asan/tsan/msan/ubsan/coverage/lto come from the
//     FeatureVariants probe catalog when present.
//
// The output is a complete, drop-in repo subdirectory: a downstream
// `bazel build --extra_toolchains=//path:linux_x86_64_cc_toolchain
// --features=asan //:smoke` resolves the right cc_toolchain for
// compilation and activates the asan flag bundle.
//
// Pure rendering: takes a Model + Config (output knobs like names),
// returns bytes. Real-cmake invocation lives in toolchain.Probe.
package bazeltoolchain

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

// Config knobs the operator passes per emission. None are required;
// sensible defaults derived from the model are applied when unset.
type Config struct {
	// PackageName is the Bazel package the emitted rules live in.
	// Defaults to "toolchain". The toolchain identifier embeds it so
	// two emissions in the same workspace don't collide.
	PackageName string

	// TargetLibc names the libc on the target. Bazel's typical
	// values: "glibc", "musl", "macosx", "msvcrt". When empty we
	// fall back to "glibc" for Linux, "macosx" for Darwin, "" for
	// other OSes.
	TargetLibc string

	// ToolchainIdentifier is the cc_toolchain_config's
	// `toolchain_identifier`. Defaults to
	// "<target_os>_<target_cpu>_<compiler_id>" lowercased.
	ToolchainIdentifier string

	// VariantMapping classifies each Variant into a Bazel feature
	// slot at emit time. Nil falls back to
	// toolchain.DefaultVariantMapping (CMake build type → dbg /
	// opt). Operators with custom matrices (sanitizers, alt-
	// compilers) override.
	VariantMapping toolchain.VariantMapping

	// HardeningFeatures, when true, emits `fortify_source` and
	// `stack_protector` feature() blocks with default-enabled =
	// True and the standard distro-default flag bundles
	// (-D_FORTIFY_SOURCE=2, -fstack-protector-strong). Closes
	// the symbol-set delta the hardening probe in
	// internal/hardeningprobe surfaced: cmake's distro cc
	// applies these via the spec file by default; Bazel's
	// hermetic cc_toolchain doesn't, so converted-then-rebuilt
	// artifacts miss the __*_chk / __stack_chk_* references
	// the cmake-built artifacts carry.
	//
	// Off by default — opt-in keeps existing toolchain
	// behaviour unchanged. Operators who saw the
	// --probe-distro-hardening diagnostic enable this flag on
	// the next derive-toolchain run. Disable a single feature
	// per-build with `--features=-fortify_source` (Bazel's
	// per-feature opt-out shape).
	HardeningFeatures bool
}

// Bundle is the result of Emit: a map from filename (relative to the
// emitted package's directory) to file content. The caller writes
// each entry to disk.
type Bundle struct {
	Files map[string][]byte
}

// Emit renders one Model + Config to a Bundle ready to write into a
// Bazel package directory. Returns an error if the model lacks a
// minimum set of fields (no languages, no compiler path).
//
// For variant-aware emission (per-build-type compile/link flag sets
// drawn from a probe matrix), use EmitResolved with a
// toolchain.ResolvedToolchain — that path populates dbg_compile_flags
// / opt_compile_flags and the matching link-flag slots.
func Emit(m *toolchain.Model, cfg Config) (*Bundle, error) {
	if m == nil {
		return nil, fmt.Errorf("bazeltoolchain.Emit: nil model")
	}
	if len(m.Languages) == 0 {
		return nil, fmt.Errorf("bazeltoolchain.Emit: model has no languages")
	}
	return emit(m, nil, cfg)
}

// EmitResolved is the variant-aware entrypoint: rt.Base provides
// the always-on flag set, rt.Variants provides per-variant deltas.
// Bazel's compilation_mode toggles (dbg, opt) and other features
// are populated by routing each variant's delta through
// cfg.VariantMapping (defaults to DefaultVariantMapping if unset).
func EmitResolved(rt *toolchain.ResolvedToolchain, cfg Config) (*Bundle, error) {
	if rt == nil || rt.Base == nil {
		return nil, fmt.Errorf("bazeltoolchain.EmitResolved: nil ResolvedToolchain")
	}
	if len(rt.Base.Languages) == 0 {
		return nil, fmt.Errorf("bazeltoolchain.EmitResolved: base model has no languages")
	}
	return emit(rt.Base, rt, cfg)
}

func emit(m *toolchain.Model, rt *toolchain.ResolvedToolchain, cfg Config) (*Bundle, error) {
	cfg = cfg.withDefaults(m)

	build, err := emitBuildBazel(m, cfg)
	if err != nil {
		return nil, err
	}
	cfgBzl, err := emitConfigBzl(m, rt, cfg)
	if err != nil {
		return nil, err
	}
	return &Bundle{Files: map[string][]byte{
		"BUILD.bazel":             build,
		"cc_toolchain_config.bzl": cfgBzl,
	}}, nil
}

func (c Config) withDefaults(m *toolchain.Model) Config {
	if c.PackageName == "" {
		c.PackageName = "toolchain"
	}
	if c.ToolchainIdentifier == "" {
		c.ToolchainIdentifier = sanitizeID(strings.ToLower(
			fmt.Sprintf("%s_%s_%s",
				orDefault(m.TargetPlatform.OS, "unknown"),
				orDefault(m.TargetPlatform.CPU, "unknown"),
				lowestCompilerID(m),
			)))
	}
	if c.TargetLibc == "" {
		switch strings.ToLower(m.TargetPlatform.OS) {
		case "linux":
			c.TargetLibc = "glibc"
		case "darwin":
			c.TargetLibc = "macosx"
		}
	}
	return c
}

// emitBuildBazel renders the cc_toolchain + platform + toolchain
// rules. Handwritten string concatenation rather than text/template
// keeps the output stable and the rendering logic auditable.
func emitBuildBazel(m *toolchain.Model, cfg Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Generated by derive-toolchain. DO NOT EDIT.\n")
	buf.WriteString("\n")
	// Bazel 9 removed the native cc_toolchain rule; load it from rules_cc,
	// like the converter's cc_binary/cc_library emit already does.
	buf.WriteString("load(\"@rules_cc//cc:defs.bzl\", \"cc_toolchain\")\n")
	buf.WriteString("load(\":cc_toolchain_config.bzl\", \"cc_toolchain_config\")\n")
	buf.WriteString("\n")

	id := cfg.ToolchainIdentifier
	cpu := orDefault(m.TargetPlatform.CPU, "unknown")
	osName := strings.ToLower(orDefault(m.TargetPlatform.OS, "unknown"))

	// platform()
	fmt.Fprintf(&buf, "platform(\n    name = %q,\n    constraint_values = [\n", id+"_platform")
	if osName != "unknown" {
		fmt.Fprintf(&buf, "        %q,\n", "@platforms//os:"+osName)
	}
	if cpu != "unknown" {
		fmt.Fprintf(&buf, "        %q,\n", "@platforms//cpu:"+normalizeBazelCPU(cpu))
	}
	buf.WriteString("    ],\n)\n\n")

	// cc_toolchain_config()
	fmt.Fprintf(&buf, "cc_toolchain_config(\n    name = %q,\n)\n\n", id+"_config")

	// cc_toolchain()
	fmt.Fprintf(&buf, "cc_toolchain(\n")
	fmt.Fprintf(&buf, "    name = %q,\n", id+"_cc")
	fmt.Fprintf(&buf, "    toolchain_config = %q,\n", ":"+id+"_config")
	fmt.Fprintf(&buf, "    all_files = %q,\n", ":empty")
	fmt.Fprintf(&buf, "    compiler_files = %q,\n", ":empty")
	fmt.Fprintf(&buf, "    dwp_files = %q,\n", ":empty")
	fmt.Fprintf(&buf, "    linker_files = %q,\n", ":empty")
	fmt.Fprintf(&buf, "    objcopy_files = %q,\n", ":empty")
	fmt.Fprintf(&buf, "    strip_files = %q,\n", ":empty")
	fmt.Fprintf(&buf, "    supports_param_files = 1,\n")
	buf.WriteString(")\n\n")

	// toolchain() registration
	fmt.Fprintf(&buf, "toolchain(\n")
	fmt.Fprintf(&buf, "    name = %q,\n", id+"_toolchain")
	fmt.Fprintf(&buf, "    target_compatible_with = [\n")
	if osName != "unknown" {
		fmt.Fprintf(&buf, "        %q,\n", "@platforms//os:"+osName)
	}
	if cpu != "unknown" {
		fmt.Fprintf(&buf, "        %q,\n", "@platforms//cpu:"+normalizeBazelCPU(cpu))
	}
	buf.WriteString("    ],\n")
	fmt.Fprintf(&buf, "    toolchain = %q,\n", ":"+id+"_cc")
	fmt.Fprintf(&buf, "    toolchain_type = %q,\n", "@bazel_tools//tools/cpp:toolchain_type")
	buf.WriteString(")\n\n")

	// filegroup for cc_toolchain's *_files attrs.
	buf.WriteString("filegroup(name = \"empty\", srcs = [])\n")
	return buf.Bytes(), nil
}

// featureSlots is the ordered list of BazelFeatures the emitted
// cc_toolchain_config.bzl produces feature() blocks for. It's a copy of
// toolchain.GeneratedFeatures() — the single source of truth shared with
// toolchainfeature's flag-lift gate, so "what the toolchain backs" and
// "what the lift may rewrite to" can't drift. Order is stable (it
// controls the rendered order of constant blocks in the .bzl).
var featureSlots = toolchain.GeneratedFeatures()

// hardeningFeatureSlots is the ordered list of opt-in hardening
// features (emitted only when Config.HardeningFeatures is true).
// Unlike featureSlots, these carry fixed flag bundles instead of
// probe-derived ones — they mirror distro cc's spec-file defaults
// (-D_FORTIFY_SOURCE=2, -fstack-protector-strong) that cmake
// inherits silently but Bazel's hermetic toolchain doesn't.
//
// Both features land with enabled = True in the .bzl so they
// apply by default; opt out per-build with `--features=-<name>`.
var hardeningFeatureSlots = []struct {
	feature toolchain.BazelFeature
	compile []string
	link    []string
}{
	{
		feature: toolchain.BazelFeatureFortifySource,
		// -D_FORTIFY_SOURCE=2 requires optimization (-O1+);
		// distro spec wires it to act only when -O is set, so
		// putting it in the always-on compile flags is safe
		// in practice (cmake's Release default carries -O3
		// upstream and the converter preserves that via the
		// dbg/opt features). _FORTIFY_SOURCE=2 is the level
		// libc surfaces __sprintf_chk etc. from; level=1 is
		// less complete and level=3 is gcc-13+/glibc-2.34+ only.
		compile: []string{"-D_FORTIFY_SOURCE=2"},
	},
	{
		feature: toolchain.BazelFeatureStackProtector,
		// -fstack-protector-strong is the Debian/Ubuntu default
		// since ~14.04 (Ubuntu) / Buster (Debian). RHEL/Fedora
		// use the same setting. The older -fstack-protector-all
		// is more aggressive but slower; -fstack-protector (no
		// suffix) is the conservative original. -strong matches
		// the survey-host distro behaviour cmake mirrors.
		compile: []string{"-fstack-protector-strong"},
	},
}

// emitConfigBzl renders a hand-rolled cc_toolchain_config rule.
// The Starlark structure: module-level constants for every
// toolchain field (tool paths, builtin includes, per-feature flag
// lists), a private rule whose _impl reads those constants and
// returns CcToolchainConfigInfo, and a cc_toolchain_config(name)
// macro that instantiates the rule. BUILD.bazel callers see the
// same `cc_toolchain_config(name = "...")` interface they always
// did.
//
// Why hand-rolled: unix_cc_toolchain_config's feature list is
// sealed; we need feature("asan") / feature("tsan") / etc. that
// activate via `--features=<name>` from the FeatureVariants
// probe data, and the upstream rule offers no extension surface
// for that. Standard features (dbg, opt) are reproduced here in
// the same flag-set shape unix_cc_toolchain_config uses.
//
// When rt is nil, per-feature flag lists stay empty — the
// always-on default_compile_flags slot still gets the base
// CMAKE_<LANG>_FLAGS contribution either way.
func emitConfigBzl(m *toolchain.Model, rt *toolchain.ResolvedToolchain, cfg Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Generated by derive-toolchain. DO NOT EDIT.\n")
	buf.WriteString("\n")
	// Bazel 9 stripped the cc_common built-in global; load it from rules_cc so
	// create_cc_toolchain_config_info resolves.
	buf.WriteString(`load("@rules_cc//cc/common:cc_common.bzl", "cc_common")` + "\n")
	buf.WriteString(`load("@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl",` + "\n")
	buf.WriteString(`     "feature", "flag_group", "flag_set", "tool_path")` + "\n")
	buf.WriteString("\n")

	cMost := primaryLanguage(m)
	cxx := m.Languages["CXX"]
	tools := mergedTools(m)

	osName := strings.ToLower(orDefault(m.TargetPlatform.OS, "unknown"))
	cpu := normalizeBazelCPU(orDefault(m.TargetPlatform.CPU, "unknown"))
	targetSystemName := orDefault(cMost.Target, fmt.Sprintf("%s-%s", cpu, osName))

	// Identity constants.
	fmt.Fprintf(&buf, "_TOOLCHAIN_IDENTIFIER = %q\n", cfg.ToolchainIdentifier)
	fmt.Fprintf(&buf, "_HOST_SYSTEM_NAME = %q\n", targetSystemName)
	fmt.Fprintf(&buf, "_TARGET_SYSTEM_NAME = %q\n", targetSystemName)
	fmt.Fprintf(&buf, "_TARGET_CPU = %q\n", cpu)
	fmt.Fprintf(&buf, "_TARGET_LIBC = %q\n", orDefault(cfg.TargetLibc, "unknown"))
	fmt.Fprintf(&buf, "_COMPILER = %q\n", strings.ToLower(orDefault(cMost.CompilerID, "unknown")))
	fmt.Fprintf(&buf, "_ABI_VERSION = %q\n", "local")
	fmt.Fprintf(&buf, "_ABI_LIBC_VERSION = %q\n", "local")
	buf.WriteString("\n")

	// Builtin include dirs + tool paths.
	includes := unionStrings(cMost.BuiltinIncludeDirs, cxx.BuiltinIncludeDirs)
	emitStringListConst(&buf, "_CXX_BUILTIN_INCLUDE_DIRECTORIES", includes)
	emitStringMapConst(&buf, "_TOOL_PATHS", tools)
	buf.WriteString("\n")

	// Always-on flags (default_compile_flags).
	emitStringListConst(&buf, "_COMPILE_FLAGS", cMost.BaseFlags)
	emitStringListConst(&buf, "_CXX_FLAGS", cxx.BaseFlags)
	emitStringListConst(&buf, "_LINK_FLAGS", cMost.LinkFlags)
	buf.WriteString("\n")

	// Per-feature flag pairs (compile + link).
	for _, f := range featureSlots {
		compile, link := variantFlagsFor(rt, cfg.VariantMapping, f)
		emitStringListConst(&buf, "_"+strings.ToUpper(string(f))+"_COMPILE_FLAGS", compile)
		emitStringListConst(&buf, "_"+strings.ToUpper(string(f))+"_LINK_FLAGS", link)
	}
	// Hardening features (opt-in via Config.HardeningFeatures).
	// Constant flag bundles, not probe-derived. Empty slots when
	// the flag is off so the rendered .bzl byte-stable across the
	// off-by-default path.
	if cfg.HardeningFeatures {
		for _, hf := range hardeningFeatureSlots {
			emitStringListConst(&buf, "_"+strings.ToUpper(string(hf.feature))+"_COMPILE_FLAGS", hf.compile)
			emitStringListConst(&buf, "_"+strings.ToUpper(string(hf.feature))+"_LINK_FLAGS", hf.link)
		}
	}
	buf.WriteString("\n")

	// Action sets — copied from cc_toolchain_config_lib's standard
	// constants. Inlined here so the .bzl has no transitive load
	// dependency beyond cc_toolchain_config_lib itself.
	// _CXX_COMPILE_ACTIONS is the C++-only subset; we use it to
	// route _CXX_FLAGS at C++ compile actions specifically (cmake
	// puts -std=c++20 / -stdlib=... into CMAKE_CXX_FLAGS, not
	// CMAKE_C_FLAGS — the unified default_compile_flags feature
	// alone would drop them silently).
	buf.WriteString(`_ALL_COMPILE_ACTIONS = [
    "assemble",
    "preprocess-assemble",
    "c-compile",
    "c++-compile",
    "c++-header-parsing",
    "c++-module-compile",
    "c++-module-codegen",
    "lto-backend",
]

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
        _default_compile_flags_feature(_COMPILE_FLAGS, _CXX_FLAGS, _LINK_FLAGS),
`)
	for _, f := range featureSlots {
		fmt.Fprintf(&buf, "        _feature_with_flags(%q, False, _%s_COMPILE_FLAGS, _%s_LINK_FLAGS),\n",
			string(f),
			strings.ToUpper(string(f)),
			strings.ToUpper(string(f)))
	}
	// Hardening features: enabled = True (apply by default to
	// match cmake's distro-cc behaviour). Operators opt out per
	// build with `--features=-fortify_source` etc.
	if cfg.HardeningFeatures {
		for _, hf := range hardeningFeatureSlots {
			upper := strings.ToUpper(string(hf.feature))
			fmt.Fprintf(&buf, "        _feature_with_flags(%q, True, _%s_COMPILE_FLAGS, _%s_LINK_FLAGS),\n",
				string(hf.feature), upper, upper)
		}
	}
	buf.WriteString(`    ]
    return [cc_common.create_cc_toolchain_config_info(
        ctx = ctx,
        toolchain_identifier = _TOOLCHAIN_IDENTIFIER,
        host_system_name = _HOST_SYSTEM_NAME,
        target_system_name = _TARGET_SYSTEM_NAME,
        target_cpu = _TARGET_CPU,
        target_libc = _TARGET_LIBC,
        compiler = _COMPILER,
        abi_version = _ABI_VERSION,
        abi_libc_version = _ABI_LIBC_VERSION,
        tool_paths = [tool_path(name = name, path = path) for name, path in _TOOL_PATHS.items()],
        cxx_builtin_include_directories = _CXX_BUILTIN_INCLUDE_DIRECTORIES,
        features = features,
    )]

# provides is omitted deliberately: the rule returns CcToolchainConfigInfo via
# cc_common.create_cc_toolchain_config_info, but that provider is not a loadable
# global under bazel 9's Starlark autoloading.
_cc_toolchain_config_rule = rule(
    implementation = _impl,
    attrs = {},
)

def cc_toolchain_config(name):
    _cc_toolchain_config_rule(name = name)
`)
	return buf.Bytes(), nil
}

// variantFlagsFor walks the ResolvedToolchain's variants and
// folds every variant whose VariantMapping classification equals
// the requested feature ("dbg", "opt") into a merged compile +
// link delta. Returns nil/nil when rt is nil or no variants map.
//
// Multiple variants can fold to the same feature (e.g. Release +
// RelWithDebInfo + MinSizeRel → "opt"); we dedup-preserve-order
// rather than picking one. Operators who need finer control
// supply their own VariantMapping or post-process the .bzl.
func variantFlagsFor(rt *toolchain.ResolvedToolchain, mapping toolchain.VariantMapping, feature toolchain.BazelFeature) (compile []string, link []string) {
	if rt == nil {
		return nil, nil
	}
	if mapping == nil {
		mapping = toolchain.DefaultVariantMapping
	}
	for _, delta := range rt.Variants {
		if mapping(delta.Spec) != feature {
			continue
		}
		// LanguageFlags["C"] is the canonical shared set; CXX
		// duplicates most of it. Use C if present, fall back to
		// CXX, then any.
		if c, ok := delta.LanguageFlags["C"]; ok {
			compile = mergeOrdered(compile, c)
		} else if cxx, ok := delta.LanguageFlags["CXX"]; ok {
			compile = mergeOrdered(compile, cxx)
		} else {
			for _, fl := range delta.LanguageFlags {
				compile = mergeOrdered(compile, fl)
				break
			}
		}
		link = mergeOrdered(link, delta.LinkFlags)
	}
	return compile, link
}

// mergeOrdered appends src to dst, deduping while preserving order.
func mergeOrdered(dst, src []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, x := range dst {
		seen[x] = true
	}
	for _, x := range src {
		if !seen[x] {
			seen[x] = true
			dst = append(dst, x)
		}
	}
	return dst
}

// helpers

// emitStringListConst renders a module-level Starlark
// list constant: `_NAME = ["a", "b"]` on a single line for the
// empty case, multi-line otherwise. Used by the hand-rolled
// cc_toolchain_config emission.
func emitStringListConst(buf *bytes.Buffer, name string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(buf, "%s = []\n", name)
		return
	}
	fmt.Fprintf(buf, "%s = [\n", name)
	for _, it := range items {
		fmt.Fprintf(buf, "    %q,\n", it)
	}
	buf.WriteString("]\n")
}

// emitStringMapConst renders a module-level Starlark dict
// constant: `_NAME = {"k": "v", ...}` with keys in sorted order.
func emitStringMapConst(buf *bytes.Buffer, name string, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Fprintf(buf, "%s = {}\n", name)
		return
	}
	fmt.Fprintf(buf, "%s = {\n", name)
	for _, k := range keys {
		fmt.Fprintf(buf, "    %q: %q,\n", k, m[k])
	}
	buf.WriteString("}\n")
}

// primaryLanguage picks the model's "main" language for fields that
// are language-agnostic in Bazel (compiler ID, target triple). C is
// preferred over CXX since cmake reports identical compiler info for
// both when one driver (gcc / clang) handles both.
func primaryLanguage(m *toolchain.Model) toolchain.Language {
	if l, ok := m.Languages["C"]; ok {
		return l
	}
	for _, l := range m.Languages {
		return l
	}
	return toolchain.Language{}
}

// mergedTools fills in the standard Bazel tool_paths slots from the
// model's Tools struct, falling back to PATH names ("ar", "strip",
// ...) when cmake didn't set the variable. Compilers come from the
// per-language CompilerPath.
func mergedTools(m *toolchain.Model) map[string]string {
	c := primaryLanguage(m)
	cxx := m.Languages["CXX"]

	out := map[string]string{
		"ar":      orDefault(m.Tools.AR, "/usr/bin/ar"),
		"ld":      orDefault(m.Tools.Linker, "/usr/bin/ld"),
		"cpp":     orDefault(c.CompilerPath, "/usr/bin/cpp"),
		"gcc":     orDefault(c.CompilerPath, "/usr/bin/gcc"),
		"gcov":    "/usr/bin/gcov",
		"nm":      orDefault(m.Tools.NM, "/usr/bin/nm"),
		"objcopy": orDefault(m.Tools.Objcopy, "/usr/bin/objcopy"),
		"objdump": orDefault(m.Tools.Objdump, "/usr/bin/objdump"),
		"strip":   orDefault(m.Tools.Strip, "/usr/bin/strip"),
	}
	if cxx.CompilerPath != "" {
		// cc_toolchain_config doesn't have a separate g++ slot; the
		// gcc tool path is used for both. CXX flags differ at the
		// flag-set level, not the tool level.
		_ = cxx
	}
	return out
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func orDefault(v, dflt string) string {
	if v == "" {
		return dflt
	}
	return v
}

func lowestCompilerID(m *toolchain.Model) string {
	if l, ok := m.Languages["C"]; ok && l.CompilerID != "" {
		return strings.ToLower(l.CompilerID)
	}
	for _, l := range m.Languages {
		if l.CompilerID != "" {
			return strings.ToLower(l.CompilerID)
		}
	}
	return "unknown"
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// normalizeBazelCPU maps cmake's CMAKE_SYSTEM_PROCESSOR values to the
// names Bazel's @platforms//cpu:* uses. Common case: cmake reports
// "x86_64" while Bazel uses "x86_64"; aarch64 -> arm64. Operators
// can override at emit time via Config.ToolchainIdentifier if a
// platform names architectures differently.
func normalizeBazelCPU(cmakeCPU string) string {
	switch strings.ToLower(cmakeCPU) {
	case "amd64", "x86_64":
		return "x86_64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv7", "arm":
		return "arm"
	case "i386", "i486", "i586", "i686", "x86":
		return "x86_32"
	case "ppc64le", "powerpc64le":
		return "ppc"
	case "riscv64":
		return "riscv64"
	default:
		return cmakeCPU
	}
}
