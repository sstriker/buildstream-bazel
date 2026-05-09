package bazeltoolchain

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
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
		`@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl`,
		`"feature", "flag_group", "flag_set", "tool_path"`,
		`_TARGET_CPU = "x86_64"`,
		`_COMPILER = "gnu"`,
		`_TOOL_PATHS = {`,
		`"ar":`,
		`"gcc":`,
		`_feature_with_flags("default_compile_flags", True,`,
		`_feature_with_flags("asan", False,`,
		`_feature_with_flags("tsan", False,`,
		`_feature_with_flags("ubsan", False,`,
		`def cc_toolchain_config(name):`,
		`_cc_toolchain_config_rule(name = name)`,
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
