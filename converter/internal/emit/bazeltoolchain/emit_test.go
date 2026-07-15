package bazeltoolchain

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

// TestEmit_HelloWorldFixture renders a Bundle from the recorded
// hello-world fileapi reply and asserts the structural invariants
// the cc_toolchain configuration requires. We don't do byte-for-byte
// golden matching here because the recorded compiler path / version
// drifts across hosts; structural assertions are the durable form.
func TestEmit_HelloWorldFixture(t *testing.T) {
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, err := toolchain.FromReply(r)
	if err != nil {
		t.Fatalf("FromReply: %v", err)
	}
	// hello-world fixture doesn't FORCE-cache CMAKE_HOST_SYSTEM_*;
	// fake them so the emitter has plausible defaults to render.
	m.HostPlatform = toolchain.Platform{OS: "Linux", CPU: "x86_64"}
	m.TargetPlatform = m.HostPlatform

	b, err := Emit(m, Config{PackageName: "toolchain"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	build := string(b.Files["BUILD.bazel"])
	cfg := string(b.Files["cc_toolchain_config.bzl"])

	if build == "" {
		t.Fatal("BUILD.bazel empty")
	}
	if cfg == "" {
		t.Fatal("cc_toolchain_config.bzl empty")
	}

	// Structural assertions on BUILD.bazel.
	for _, want := range []string{
		`load("@rules_cc//cc:defs.bzl", "cc_toolchain")`,
		`cc_toolchain(`,
		`platform(`,
		`toolchain(`,
		`@bazel_tools//tools/cpp:toolchain_type`,
		`@platforms//os:linux`,
		`@platforms//cpu:x86_64`,
	} {
		if !strings.Contains(build, want) {
			t.Errorf("BUILD.bazel missing %q\n%s", want, build)
		}
	}

	// Structural assertions on cc_toolchain_config.bzl. Stage 2
	// switched to a hand-rolled rule built on
	// cc_toolchain_config_lib.bzl primitives — the load + identity
	// constants + feature() blocks are the durable contract.
	for _, want := range []string{
		`load("@rules_cc//cc/common:cc_common.bzl", "cc_common")`,
		`@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl`,
		`"action_config", "feature", "flag_group", "flag_set", "tool", "tool_path"`,
		`_TARGET_CPU = "x86_64"`,
		`_COMPILER = "gnu"`,
		`_TOOL_PATHS = {`,
		`"ar":`,
		`"gcc":`,
		`_default_compile_flags_feature(_COMPILE_FLAGS, _CXX_FLAGS, _LINK_FLAGS)`,
		`_feature_with_flags("asan", False,`,
		`_feature_with_flags("tsan", False,`,
		`_feature_with_flags("ubsan", False,`,
		`_CXX_COMPILE_ACTIONS = [`,
		`"c++-compile",`,
		`def cc_toolchain_config(name):`,
		`_cc_toolchain_config_rule(name = name)`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q\n%s", want, cfg)
		}
	}
}

// TestEmit_PerLanguageCompileFlagRouting pins that the base compile flags
// route per source language — CMAKE_C_FLAGS (_COMPILE_FLAGS) at the C compile
// actions and CMAKE_CXX_FLAGS (_CXX_FLAGS) at the C++ compile actions — rather
// than lumping the C flags onto every compile action (which leaked a C-only
// flag onto C++ compiles).
func TestEmit_PerLanguageCompileFlagRouting(t *testing.T) {
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, err := toolchain.FromReply(r)
	if err != nil {
		t.Fatalf("FromReply: %v", err)
	}
	m.HostPlatform = toolchain.Platform{OS: "Linux", CPU: "x86_64"}
	m.TargetPlatform = m.HostPlatform
	b, err := Emit(m, Config{PackageName: "toolchain"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])

	// The C action set exists, includes c-compile, and excludes c++-compile.
	cBlock := between(t, cfg, "_C_COMPILE_ACTIONS = [", "]")
	if !strings.Contains(cBlock, `"c-compile"`) {
		t.Errorf("_C_COMPILE_ACTIONS must include c-compile:\n%s", cBlock)
	}
	if strings.Contains(cBlock, `"c++-compile"`) {
		t.Errorf("_C_COMPILE_ACTIONS must NOT include c++-compile (that leaks CMAKE_C_FLAGS onto C++):\n%s", cBlock)
	}
	// The base-flags feature routes compile_flags to the C actions and
	// cxx_flags to the C++ actions.
	feat := between(t, cfg, "def _default_compile_flags_feature(", "return feature(")
	if !strings.Contains(feat, "actions = _C_COMPILE_ACTIONS") {
		t.Errorf("default_compile_flags must route the C base flags to _C_COMPILE_ACTIONS:\n%s", feat)
	}
	if !strings.Contains(feat, "actions = _CXX_COMPILE_ACTIONS") {
		t.Errorf("default_compile_flags must route the C++ base flags to _CXX_COMPILE_ACTIONS:\n%s", feat)
	}
	if strings.Contains(feat, "actions = _ALL_COMPILE_ACTIONS") {
		t.Errorf("default_compile_flags must not route the base flags to _ALL_COMPILE_ACTIONS:\n%s", feat)
	}
}

// between returns the substring of s strictly between the first occurrence of
// open and the next occurrence of close after it; it fails the test if either
// marker is absent (guarding against index panics on a format change).
func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("marker %q not found:\n%s", open, s)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("closing marker %q not found after %q:\n%s", close, open, s)
	}
	return rest[:j]
}

// TestEmit_CXXCompilerRoutedToCXXActions pins that the C++ compiler driver
// (g++/clang++) is wired to the C++ compile+link actions via action_config,
// while the single tool_paths "gcc" slot stays the C compiler — so a C++
// binary links libstdc++ instead of failing on undefined std:: symbols.
func TestEmit_CXXCompilerRoutedToCXXActions(t *testing.T) {
	m := &toolchain.Model{
		HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
		TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
		Languages: map[string]toolchain.Language{
			"C":   {CompilerID: "GNU", CompilerPath: "/usr/bin/cc"},
			"CXX": {CompilerID: "GNU", CompilerPath: "/usr/bin/c++"},
		},
	}
	b, err := Emit(m, Config{PackageName: "toolchain"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])

	for _, want := range []string{
		`_CXX_COMPILER = "/usr/bin/c++"`,
		`"gcc": "/usr/bin/cc"`, // tool_paths C compiler slot
		`action_configs = _cxx_action_configs(_CXX_COMPILER)`,
		`def _cxx_action_configs(cxx_compiler):`,
		`action_config(action_name = name, enabled = True, tools = cxx_tools)`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q\n%s", want, cfg)
		}
	}
	// The C++ action set covers both compile and link.
	names := between(t, cfg, "_CXX_ACTION_NAMES = [", "]")
	for _, a := range []string{`"c++-compile"`, `"c++-link-executable"`, `"c++-link-dynamic-library"`} {
		if !strings.Contains(names, a) {
			t.Errorf("_CXX_ACTION_NAMES must include %s:\n%s", a, names)
		}
	}
}

// TestEmit_COnlyModelNoActionConfigs: a model with only the C language leaves
// _CXX_COMPILER empty, so _cxx_action_configs returns [] and the C++ actions
// fall back to the C driver (correct — no libstdc++ needed).
func TestEmit_COnlyModelNoActionConfigs(t *testing.T) {
	m := &toolchain.Model{
		HostPlatform:   toolchain.Platform{OS: "Linux", CPU: "x86_64"},
		TargetPlatform: toolchain.Platform{OS: "Linux", CPU: "x86_64"},
		Languages: map[string]toolchain.Language{
			"C": {CompilerID: "GNU", CompilerPath: "/usr/bin/cc"},
		},
	}
	b, err := Emit(m, Config{PackageName: "toolchain"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])
	if !strings.Contains(cfg, `_CXX_COMPILER = ""`) {
		t.Errorf("C-only toolchain must leave _CXX_COMPILER empty:\n%s", cfg)
	}
}

// TestEmit_HardeningFeaturesOff is the no-op baseline: with
// Config.HardeningFeatures = false (the default), the emitted
// cc_toolchain_config.bzl carries no fortify_source /
// stack_protector blocks.
func TestEmit_HardeningFeaturesOff(t *testing.T) {
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, err := toolchain.FromReply(r)
	if err != nil {
		t.Fatalf("FromReply: %v", err)
	}
	m.HostPlatform = toolchain.Platform{OS: "Linux", CPU: "x86_64"}
	m.TargetPlatform = m.HostPlatform

	b, err := Emit(m, Config{PackageName: "toolchain"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])
	for _, banned := range []string{
		`fortify_source`,
		`stack_protector`,
		`_FORTIFY_SOURCE=2`,
		`-fstack-protector`,
	} {
		if strings.Contains(cfg, banned) {
			t.Errorf("cc_toolchain_config.bzl carries %q with HardeningFeatures=false:\n%s", banned, cfg)
		}
	}
}

// TestEmit_HardeningFeaturesOn covers the opt-in path: with
// HardeningFeatures = true the emitted .bzl carries
// fortify_source + stack_protector feature() blocks with the
// distro-default flag bundles AND `enabled = True`.
func TestEmit_HardeningFeaturesOn(t *testing.T) {
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, err := toolchain.FromReply(r)
	if err != nil {
		t.Fatalf("FromReply: %v", err)
	}
	m.HostPlatform = toolchain.Platform{OS: "Linux", CPU: "x86_64"}
	m.TargetPlatform = m.HostPlatform

	b, err := Emit(m, Config{PackageName: "toolchain", HardeningFeatures: true})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cfg := string(b.Files["cc_toolchain_config.bzl"])

	for _, want := range []string{
		`_FORTIFY_SOURCE_COMPILE_FLAGS = [`,
		`"-D_FORTIFY_SOURCE=2"`,
		`_STACK_PROTECTOR_COMPILE_FLAGS = [`,
		`"-fstack-protector-strong"`,
		`_feature_with_flags("fortify_source", True, _FORTIFY_SOURCE_COMPILE_FLAGS, _FORTIFY_SOURCE_LINK_FLAGS)`,
		`_feature_with_flags("stack_protector", True, _STACK_PROTECTOR_COMPILE_FLAGS, _STACK_PROTECTOR_LINK_FLAGS)`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cc_toolchain_config.bzl missing %q\n%s", want, cfg)
		}
	}
}

func TestEmit_RejectsEmptyModel(t *testing.T) {
	if _, err := Emit(&toolchain.Model{}, Config{}); err == nil {
		t.Error("Emit on empty model should error")
	}
}

func TestNormalizeBazelCPU(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x86_64", "x86_64"},
		{"amd64", "x86_64"},
		{"aarch64", "arm64"},
		{"AArch64", "arm64"},
		{"unknown_arch", "unknown_arch"},
	}
	for _, tc := range cases {
		if got := normalizeBazelCPU(tc.in); got != tc.want {
			t.Errorf("normalizeBazelCPU(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
