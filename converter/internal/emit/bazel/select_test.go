package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/emit/bazel"
	"github.com/sstriker/cmake-to-bazel/converter/ir"
)

// TestEmit_PerPlatform_BaselinePlusSelect: a target with both
// shared baseline items and per-platform deltas renders as
// `<flat> + select({...})` with arms in sorted order plus a
// "//conditions:default": [] arm. Single-axis case (only os
// varying); multi-line listing for clarity.
func TestEmit_PerPlatform_BaselinePlusSelect(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "libfoo",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"common.c"},
			Hdrs:    []string{"include/foo.h"},
			Copts:   []string{"-Wall"},
			Defines: []string{"FOO=1"},
			PerPlatform: map[string]map[string][]string{
				"srcs": {
					"@platforms//os:darwin": {"darwin/foo.c"},
					"@platforms//os:linux":  {"linux/foo.c"},
				},
				"copts": {
					"@platforms//cpu:arm64": {"-mcpu=apple-m1"},
				},
			},
			Visibility: []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)

	// srcs: baseline ["common.c"] + select keyed on os, arms in
	// sorted order ("@platforms//os:darwin" before "...os:linux"),
	// closing with conditions:default. The arm contents are
	// indented at the 12-column rule-attribute level.
	wantSrcs := `srcs = ["common.c"] + select({
        "@platforms//os:darwin": [
            "darwin/foo.c",
        ],
        "@platforms//os:linux": [
            "linux/foo.c",
        ],
        "//conditions:default": [],
    }),`
	if !strings.Contains(gotStr, wantSrcs) {
		t.Errorf("srcs missing expected select() shape; got:\n%s\n\nwant substring:\n%s", gotStr, wantSrcs)
	}

	// copts: only one arm, so we still get the default arm.
	wantCopts := `copts = ["-Wall"] + select({
        "@platforms//cpu:arm64": [
            "-mcpu=apple-m1",
        ],
        "//conditions:default": [],
    }),`
	if !strings.Contains(gotStr, wantCopts) {
		t.Errorf("copts missing expected select() shape; got:\n%s\n\nwant substring:\n%s", gotStr, wantCopts)
	}

	// Untouched attributes (hdrs, defines) keep their flat shape.
	for _, want := range []string{
		`hdrs = ["include/foo.h"],`,
		`defines = ["FOO=1"],`,
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("expected %q in output; got:\n%s", want, gotStr)
		}
	}
}

// TestEmit_PerPlatform_OnlySelect: an attribute whose IR has an
// empty flat baseline but non-empty PerPlatform delta renders
// as a bare select() (no leading list + concatenation).
func TestEmit_PerPlatform_OnlySelect(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "libfoo",
			Kind: ir.KindCCLibrary,
			// No flat srcs; only platform-specific.
			PerPlatform: map[string]map[string][]string{
				"srcs": {
					"@platforms//os:darwin": {"darwin/only.c"},
				},
			},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	want := `srcs = select({
        "@platforms//os:darwin": [
            "darwin/only.c",
        ],
        "//conditions:default": [],
    }),`
	if !strings.Contains(gotStr, want) {
		t.Errorf("expected bare select() shape; got:\n%s\n\nwant substring:\n%s", gotStr, want)
	}
}

// TestEmit_PerPlatform_EmptyMapEqualsFlat: PerPlatform with all
// empty inner maps is treated identically to nil — the rendered
// BUILD.bazel matches the single-platform shape exactly. This
// is the "single-platform conversion is the N=1 case" contract:
// no fold output, no select() blocks, no diff vs the existing
// goldens.
func TestEmit_PerPlatform_EmptyMapEqualsFlat(t *testing.T) {
	flatTarget := ir.Target{
		Name:    "libbase",
		Kind:    ir.KindCCLibrary,
		Srcs:    []string{"a.c", "b.c"},
		Copts:   []string{"-O2"},
		Defines: []string{"X=1"},
	}
	emptySelectTarget := flatTarget
	emptySelectTarget.PerPlatform = map[string]map[string][]string{
		"srcs":  {},
		"copts": nil,
	}

	flatBytes, err := bazel.Emit(&ir.Package{Targets: []ir.Target{flatTarget}})
	if err != nil {
		t.Fatal(err)
	}
	emptyBytes, err := bazel.Emit(&ir.Package{Targets: []ir.Target{emptySelectTarget}})
	if err != nil {
		t.Fatal(err)
	}
	if string(flatBytes) != string(emptyBytes) {
		t.Errorf("empty PerPlatform must render identically to nil; flat:\n%s\nempty:\n%s",
			flatBytes, emptyBytes)
	}
}

// TestEmit_PerPlatform_CCBinaryFoldsHdrsIntoSrcs: cc_binary /
// cc_test don't accept hdrs; emit folds them into srcs. The same
// fold has to extend to per-platform hdrs deltas — they merge
// into the corresponding srcs delta arm so the binary still
// sees those headers as compilation inputs on the matching
// platform.
func TestEmit_PerPlatform_CCBinaryFoldsHdrsIntoSrcs(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "myapp",
			Kind: ir.KindCCBinary,
			Srcs: []string{"main.c"},
			PerPlatform: map[string]map[string][]string{
				"srcs": {
					"@platforms//os:darwin": {"darwin_main.c"},
				},
				"hdrs": {
					"@platforms//os:darwin": {"darwin_priv.h"},
				},
			},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	// srcs arm contains both the original srcs delta AND the
	// folded hdrs delta, sorted within the arm.
	wantArm := `"@platforms//os:darwin": [
            "darwin_main.c",
            "darwin_priv.h",
        ],`
	if !strings.Contains(gotStr, wantArm) {
		t.Errorf("expected darwin srcs arm to fold hdrs; got:\n%s\n\nwant substring:\n%s", gotStr, wantArm)
	}
	if strings.Contains(gotStr, "hdrs =") {
		t.Errorf("cc_binary should not emit hdrs attribute; got:\n%s", gotStr)
	}
}

// TestEmit_PerPlatformScalar_CCImportStaticLibrary: round-2
// fallback stubs whose install_tree.tar path diverges across
// platforms render cc_import.static_library as a select() keyed
// on the per-platform path with a trailing "//conditions:default":
// None arm. The default arm matters even when every in-matrix
// platform has a value: it makes out-of-matrix builds resolve to
// "attribute unset" instead of failing analysis on a missing arm.
// And for the partial-platform shape (one platform supplies
// static_library only, another supplies shared_library only) it's
// what stops the otherwise-absent platform from hitting a
// no-matching-condition error.
func TestEmit_PerPlatformScalar_CCImportStaticLibrary(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCImport,
			PerPlatformScalar: map[string]map[string]string{
				"static_library": {
					"@platforms//os:linux":  "lib/x86_64-linux-gnu/libfoo.a",
					"@platforms//os:darwin": "lib/libfoo.a",
				},
			},
			Visibility: []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	want := `static_library = select({
        "@platforms//os:darwin": "lib/libfoo.a",
        "@platforms//os:linux": "lib/x86_64-linux-gnu/libfoo.a",
        "//conditions:default": None,
    }),`
	if !strings.Contains(gotStr, want) {
		t.Errorf("cc_import.static_library missing expected select() shape; got:\n%s\n\nwant substring:\n%s", gotStr, want)
	}
	// No accidental shared_library emission when only static
	// is populated.
	if strings.Contains(gotStr, "shared_library") {
		t.Errorf("cc_import unexpectedly rendered shared_library; got:\n%s", gotStr)
	}
}

// TestEmit_PerPlatformScalar_CCImportPartialPlatformShape: cells
// disagree on which path attr to populate — one platform supplies
// static_library only, another supplies shared_library only. The
// select() for each attr lists only the platforms that contribute
// a value; the OTHER in-matrix platform resolves to None (attribute
// unset) via the "//conditions:default" arm. cc_import accepts
// missing static_library / shared_library, so each platform sees a
// half-populated cc_import — the right outcome: each platform's
// downstream uses the half that exists there.
func TestEmit_PerPlatformScalar_CCImportPartialPlatformShape(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCImport,
			PerPlatformScalar: map[string]map[string]string{
				"static_library": {"@platforms//os:linux": "lib/libfoo.a"},
				"shared_library": {"@platforms//os:darwin": "lib/libfoo.dylib"},
			},
			Visibility: []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	for _, want := range []string{
		`static_library = select({
        "@platforms//os:linux": "lib/libfoo.a",
        "//conditions:default": None,
    }),`,
		`shared_library = select({
        "@platforms//os:darwin": "lib/libfoo.dylib",
        "//conditions:default": None,
    }),`,
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("cc_import missing partial-platform select; got:\n%s\n\nwant substring:\n%s", gotStr, want)
		}
	}
}

// TestEmit_PerPlatform_ShBinarySrcsDiverge: sh_binary's srcs
// renders the same `<flat> + select()` or bare select() shape as
// cc_library/cc_binary, driven by PerPlatform["srcs"]. The round-
// 2 stub use case has arch-tagged installed binaries that don't
// share any path across platforms — bare select() with no flat
// baseline.
func TestEmit_PerPlatform_ShBinarySrcsDiverge(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "tool",
			Kind: ir.KindShBinary,
			PerPlatform: map[string]map[string][]string{
				"srcs": {
					"@platforms//os:linux":  {"bin/tool-linux"},
					"@platforms//os:darwin": {"bin/tool-darwin"},
				},
			},
			Visibility: []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	want := `srcs = select({
        "@platforms//os:darwin": [
            "bin/tool-darwin",
        ],
        "@platforms//os:linux": [
            "bin/tool-linux",
        ],
        "//conditions:default": [],
    }),`
	if !strings.Contains(gotStr, want) {
		t.Errorf("sh_binary srcs missing expected select() shape; got:\n%s\n\nwant substring:\n%s", gotStr, want)
	}
}

// TestEmit_PerPlatformScalar_EmptyPreservesLegacyShape: single-
// platform cc_import targets (no PerPlatformScalar) must render
// the historical `static_library = "path"` literal byte-
// identically. This is the "N=1 == legacy shape" contract that
// the fold's identity case depends on.
func TestEmit_PerPlatformScalar_EmptyPreservesLegacyShape(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:          "foo",
			Kind:          ir.KindCCImport,
			StaticLibrary: "lib/libfoo.a",
			Visibility:    []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, `static_library = "lib/libfoo.a",`) {
		t.Errorf("single-platform cc_import: want literal static_library line; got:\n%s", gotStr)
	}
	if strings.Contains(gotStr, "select(") {
		t.Errorf("single-platform cc_import should not render select(); got:\n%s", gotStr)
	}
}
