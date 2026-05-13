package projecta

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

func TestRenderToolchainProbe_RejectsMissingFields(t *testing.T) {
	mustGood := ToolchainProbeArgs{
		CmakeSourceLabel: "//probe:source",
		CmakeListsLabel:  "//probe:CMakeLists.txt",
		ProbeCellLabel:   "//tools:probe-cell",
		Variants: []toolchain.Variant{
			{Name: "baseline"},
		},
		Platforms: []Platform{
			{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux"}},
		},
	}
	if _, err := RenderToolchainProbe(mustGood); err != nil {
		t.Fatalf("happy path failed: %v", err)
	}

	type tweak func(*ToolchainProbeArgs)
	cases := map[string]tweak{
		"empty CmakeSourceLabel": func(a *ToolchainProbeArgs) { a.CmakeSourceLabel = "" },
		"empty CmakeListsLabel":  func(a *ToolchainProbeArgs) { a.CmakeListsLabel = "" },
		"empty ProbeCellLabel":   func(a *ToolchainProbeArgs) { a.ProbeCellLabel = "" },
		"no variants":            func(a *ToolchainProbeArgs) { a.Variants = nil },
		"no platforms":           func(a *ToolchainProbeArgs) { a.Platforms = nil },
		"platform with no name":  func(a *ToolchainProbeArgs) { a.Platforms[0].Name = "" },
		"platform with no constraints": func(a *ToolchainProbeArgs) {
			a.Platforms[0].Constraints = nil
		},
		"platform name with dot": func(a *ToolchainProbeArgs) {
			a.Platforms[0].Name = "linux.x86_64"
		},
		"platform name with slash": func(a *ToolchainProbeArgs) {
			a.Platforms[0].Name = "linux/x86_64"
		},
		"platform name with colon": func(a *ToolchainProbeArgs) {
			a.Platforms[0].Name = "linux:x86_64"
		},
		"platform name with space": func(a *ToolchainProbeArgs) {
			a.Platforms[0].Name = "linux x86_64"
		},
		"variant with empty name": func(a *ToolchainProbeArgs) {
			a.Variants[0].Name = ""
		},
		"variant name with slash": func(a *ToolchainProbeArgs) {
			a.Variants[0].Name = "asan/extra"
		},
		"duplicate platform name": func(a *ToolchainProbeArgs) {
			a.Platforms = append(a.Platforms, Platform{
				Name:        a.Platforms[0].Name,
				Constraints: a.Platforms[0].Constraints,
			})
		},
		"duplicate variant name": func(a *ToolchainProbeArgs) {
			a.Variants = append(a.Variants, toolchain.Variant{Name: a.Variants[0].Name})
		},
	}
	for name, tw := range cases {
		t.Run(name, func(t *testing.T) {
			args := mustGood
			args.Platforms = append([]Platform(nil), mustGood.Platforms...)
			args.Variants = append([]toolchain.Variant(nil), mustGood.Variants...)
			tw(&args)
			if _, err := RenderToolchainProbe(args); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestRenderToolchainProbe_FullMatrix(t *testing.T) {
	args := ToolchainProbeArgs{
		CmakeSourceLabel: "//probe:source",
		CmakeListsLabel:  "//probe:CMakeLists.txt",
		ProbeCellLabel:   "//tools:probe-cell",
		Variants: []toolchain.Variant{
			{Name: "baseline"},
			{
				Name: "asan",
				CacheVars: map[string]string{
					"CMAKE_BUILD_TYPE": "Debug",
					"CMAKE_C_FLAGS":    "-fsanitize=address -fno-omit-frame-pointer",
				},
			},
		},
		Platforms: []Platform{
			{Name: "linux_x86_64", Constraints: []string{
				"@platforms//os:linux",
				"@platforms//cpu:x86_64",
			}},
			{Name: "linux_aarch64", Constraints: []string{
				"@platforms//os:linux",
				"@platforms//cpu:arm64",
			}},
		},
	}
	body, err := RenderToolchainProbe(args)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(body)

	// One genrule per (platform, variant) — 2 platforms * 2 variants = 4.
	for _, name := range []string{
		`name = "linux_x86_64.baseline"`,
		`name = "linux_x86_64.asan"`,
		`name = "linux_aarch64.baseline"`,
		`name = "linux_aarch64.asan"`,
	} {
		if !strings.Contains(got, name) {
			t.Errorf("missing genrule %s", name)
		}
	}

	// Each cell's exec_compatible_with carries the right constraints.
	if !strings.Contains(got, `"@platforms//cpu:x86_64",`) {
		t.Errorf("missing x86_64 constraint")
	}
	if !strings.Contains(got, `"@platforms//cpu:arm64",`) {
		t.Errorf("missing arm64 constraint")
	}

	// CacheVars rendered in lex order via sortedCacheVarKeys.
	wantPiece := `--cache-var 'CMAKE_BUILD_TYPE=Debug' \
            --cache-var 'CMAKE_C_FLAGS=-fsanitize=address -fno-omit-frame-pointer' \`
	if !strings.Contains(got, wantPiece) {
		t.Errorf("CacheVars not in expected order/format. Got:\n%s", got)
	}

	// The aggregating filegroup names every cell's output.
	for _, out := range []string{
		"linux_x86_64.baseline.probe.json",
		"linux_x86_64.asan.probe.json",
		"linux_aarch64.baseline.probe.json",
		"linux_aarch64.asan.probe.json",
	} {
		if !strings.Contains(got, out) {
			t.Errorf("filegroup missing %s", out)
		}
	}
	if !strings.Contains(got, `name = "all_probes"`) {
		t.Errorf("aggregating filegroup name missing")
	}
}

// TestRenderToolchainProbe_Deterministic confirms the renderer's
// output is byte-stable across runs (no map iteration leaking).
// Without this, project A's BUILD.bazel would churn the operator's
// Bazel cache key on every regeneration.
func TestRenderToolchainProbe_Deterministic(t *testing.T) {
	args := ToolchainProbeArgs{
		CmakeSourceLabel: "//probe:source",
		CmakeListsLabel:  "//probe:CMakeLists.txt",
		ProbeCellLabel:   "//tools:probe-cell",
		Variants: []toolchain.Variant{
			{Name: "asan", CacheVars: map[string]string{
				"CMAKE_C_FLAGS":        "-fsanitize=address",
				"CMAKE_CXX_FLAGS":      "-fsanitize=address",
				"CMAKE_TOOLCHAIN_HOOK": "1",
			}},
		},
		Platforms: []Platform{
			{Name: "linux", Constraints: []string{
				"@platforms//os:linux",
				"@platforms//cpu:x86_64",
			}},
		},
	}
	a, err := RenderToolchainProbe(args)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b, err := RenderToolchainProbe(args)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("non-deterministic output\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}
