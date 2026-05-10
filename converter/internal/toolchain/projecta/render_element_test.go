package projecta

import (
	"strings"
	"testing"
)

// TestRenderElementConvert_FullMatrix renders a 2-platform
// element-convert matrix and asserts the structural invariants:
// per-cell genrule names, per-cell outputs, exec_compatible_with
// constraints, the filegroup that aggregates every output, and
// the convert-element invocation shape.
func TestRenderElementConvert_FullMatrix(t *testing.T) {
	args := ElementConvertArgs{
		ElementName:         "libfoo",
		CmakeSourceLabel:    "//elements/libfoo:source",
		CmakeListsLabel:     "//elements/libfoo:CMakeLists.txt",
		ConvertElementLabel: "//tools:convert-element",
		Platforms: []Platform{
			{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
			{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}},
		},
	}
	body, err := RenderElementConvert(args)
	if err != nil {
		t.Fatalf("RenderElementConvert: %v", err)
	}
	got := string(body)

	for _, want := range []string{
		`name = "libfoo.linux_x86_64"`,
		`name = "libfoo.darwin_arm64"`,
		`"libfoo.linux_x86_64.BUILD.bazel"`,
		`"libfoo.linux_x86_64.ir.json"`,
		`"libfoo.darwin_arm64.BUILD.bazel"`,
		`"libfoo.darwin_arm64.ir.json"`,
		`@platforms//os:linux`,
		`@platforms//cpu:x86_64`,
		`@platforms//os:darwin`,
		`@platforms//cpu:arm64`,
		`tools = ["//tools:convert-element"]`,
		`$(location //tools:convert-element)`,
		`--source-root $$(dirname $(execpath //elements/libfoo:CMakeLists.txt))`,
		`--out-build $(location libfoo.linux_x86_64.BUILD.bazel)`,
		`--out-ir-json $(location libfoo.linux_x86_64.ir.json)`,
		`name = "all_cells"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestRenderElementConvert_OptionalArgsPlumbedConditionally:
// imports-manifest, prefix-dir, and toolchain-cmake-file are
// optional. When provided, they appear in srcs and the
// per-cell cmd; when omitted, neither.
func TestRenderElementConvert_OptionalArgsPlumbedConditionally(t *testing.T) {
	args := ElementConvertArgs{
		ElementName:             "libbar",
		CmakeSourceLabel:        "//elements/libbar:source",
		CmakeListsLabel:         "//elements/libbar:CMakeLists.txt",
		ConvertElementLabel:     "//tools:convert-element",
		ImportsManifestLabel:    "//elements/libbar:imports.json",
		PrefixDirLabel:          "//elements/libbar:prefix",
		ToolchainCMakeFileLabel: "//toolchains:linux_x86_64.cmake",
		Platforms: []Platform{
			{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux"}},
		},
	}
	body, err := RenderElementConvert(args)
	if err != nil {
		t.Fatalf("RenderElementConvert: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		`"//elements/libbar:imports.json"`,
		`"//elements/libbar:prefix"`,
		`"//toolchains:linux_x86_64.cmake"`,
		`--imports-manifest $(execpath //elements/libbar:imports.json)`,
		`--prefix-dir $$(dirname $(execpath //elements/libbar:prefix))`,
		`--toolchain-cmake-file $(execpath //toolchains:linux_x86_64.cmake)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestRenderElementConvert_RejectsMissingFields is the same
// shape RenderToolchainProbe's validation test uses: required
// fields error out cleanly with a clear message.
func TestRenderElementConvert_RejectsMissingFields(t *testing.T) {
	good := ElementConvertArgs{
		ElementName:         "libfoo",
		CmakeSourceLabel:    "//probe:source",
		CmakeListsLabel:     "//probe:CMakeLists.txt",
		ConvertElementLabel: "//tools:convert-element",
		Platforms: []Platform{
			{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux"}},
		},
	}
	if _, err := RenderElementConvert(good); err != nil {
		t.Fatalf("happy path failed: %v", err)
	}
	type tweak func(*ElementConvertArgs)
	cases := map[string]tweak{
		"empty ElementName":         func(a *ElementConvertArgs) { a.ElementName = "" },
		"unsafe ElementName":        func(a *ElementConvertArgs) { a.ElementName = "lib/foo" },
		"empty CmakeSourceLabel":    func(a *ElementConvertArgs) { a.CmakeSourceLabel = "" },
		"empty CmakeListsLabel":     func(a *ElementConvertArgs) { a.CmakeListsLabel = "" },
		"empty ConvertElementLabel": func(a *ElementConvertArgs) { a.ConvertElementLabel = "" },
		"no platforms":              func(a *ElementConvertArgs) { a.Platforms = nil },
		"platform with no name":     func(a *ElementConvertArgs) { a.Platforms[0].Name = "" },
		"platform with no constraints": func(a *ElementConvertArgs) {
			a.Platforms[0].Constraints = nil
		},
		"duplicate platform name": func(a *ElementConvertArgs) {
			a.Platforms = append(a.Platforms, Platform{
				Name: a.Platforms[0].Name, Constraints: a.Platforms[0].Constraints,
			})
		},
	}
	for label, tw := range cases {
		t.Run(label, func(t *testing.T) {
			args := good
			args.Platforms = append([]Platform(nil), good.Platforms...)
			tw(&args)
			if _, err := RenderElementConvert(args); err == nil {
				t.Errorf("expected error for %s", label)
			}
		})
	}
}
