package lower

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestStageGeneratedSourceRootIncludes reproduces OpenBLAS's
// GenerateNamedObjects shape: a generated wrapper source (write_file bake)
// `#include`s a real kernel by its source-root-ABSOLUTE path, and a cc_library
// compiles that wrapper. The pass must (1) rewrite the baked include to a
// workspace-relative path and (2) stage the kernel as a textual_hdr on the
// library so it's a declared input.
func TestStageGeneratedSourceRootIncludes(t *testing.T) {
	hostSrc := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(hostSrc, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The real kernel sources live in the tree; the wrapper is generated and is
	// NOT on disk (it's a write_file output).
	write("lapack/potrf/potrf_U_single.c", "int potrf(){return 0;}\n")
	write("kernel/arm/foo.c", "int foo(){return 0;}\n")

	absKernel := filepath.Join(hostSrc, "lapack/potrf/potrf_U_single.c")
	// A `..`-laden absolute include (OpenBLAS bakes kernel/x86_64/../arm/...).
	absFoo := filepath.Join(hostSrc, "kernel/x86_64/../arm/foo.c")

	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name: "lapack",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"lapack/CMakeFiles/spotrf_U_single.c", "kernel/CMakeFiles/foo_wrap.c"},
			},
			{
				Name:             "gen_spotrf",
				Kind:             ir.KindWriteFile,
				WriteFileOut:     "lapack/CMakeFiles/spotrf_U_single.c",
				WriteFileNewline: "unix",
				WriteFileContent: []string{
					"#define NAME spotrf",
					`#include "` + absKernel + `"`,
				},
				Tags: []string{"cmake-codegen-configure-file"},
			},
			{
				Name:             "gen_foo",
				Kind:             ir.KindWriteFile,
				WriteFileOut:     "kernel/CMakeFiles/foo_wrap.c",
				WriteFileNewline: "unix",
				WriteFileContent: []string{
					`#include "` + absFoo + `"`,
				},
				Tags: []string{"cmake-codegen-configure-file"},
			},
		},
		SubPackages: map[string]string{"lapack": "lapack", "gen_spotrf": "lapack", "gen_foo": "kernel"},
	}

	var warn bytes.Buffer
	stageGeneratedSourceRootIncludes(pkg, hostSrc, "elements/openblas", true, &warn)

	// (1) Includes rewritten to workspace-relative, `..` normalized.
	genS := findTarget(pkg, "gen_spotrf")
	if got := genS.WriteFileContent[1]; got != `#include "elements/openblas/lapack/potrf/potrf_U_single.c"` {
		t.Errorf("spotrf include not rewritten: %q", got)
	}
	genF := findTarget(pkg, "gen_foo")
	if got := genF.WriteFileContent[0]; got != `#include "elements/openblas/kernel/arm/foo.c"` {
		t.Errorf("foo include not rewritten/normalized: %q", got)
	}

	// (2) Both kernels staged as textual_hdrs on the compiling library.
	lib := findTarget(pkg, "lapack")
	want := []string{"kernel/arm/foo.c", "lapack/potrf/potrf_U_single.c"}
	if !reflect.DeepEqual(lib.TextualHdrs, want) {
		t.Errorf("lapack textual_hdrs = %v, want %v", lib.TextualHdrs, want)
	}
	if !strings.Contains(warn.String(), "GenerateNamedObjects") {
		t.Errorf("missing breadcrumb:\n%s", warn.String())
	}
}

// TestStageGeneratedSourceRootIncludes_Guards: includes outside the source
// tree, non-source extensions, missing files, and relative includes are all
// left verbatim; a cc_binary/cc_test (no textual_hdrs slot) gets a synth
// carrier; root package path means no workspace prefix.
func TestStageGeneratedSourceRootIncludes_Guards(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "k"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "k/real.c"), []byte("int r(){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := &ir.Package{
		Targets: []ir.Target{
			{Name: "tool", Kind: ir.KindCCBinary, Srcs: []string{"CMakeFiles/w.c"}},
			{
				Name:         "gen_w",
				Kind:         ir.KindWriteFile,
				WriteFileOut: "CMakeFiles/w.c",
				WriteFileContent: []string{
					`#include "/usr/include/stdio.h"`,                          // outside source tree — leave
					`#include "` + filepath.Join(hostSrc, "k/real.c") + `"`,    // in-tree — stage
					`#include "` + filepath.Join(hostSrc, "k/missing.c") + `"`, // missing — leave
					`#include "` + filepath.Join(hostSrc, "k/real.h") + `"`,    // header ext — leave (only created .c)
					`#include "rel/x.c"`,                                       // relative — leave
				},
			},
		},
		SubPackages: map[string]string{"tool": "", "gen_w": ""},
	}
	var warn bytes.Buffer
	// Root package path ("") → no workspace prefix on the rewrite.
	stageGeneratedSourceRootIncludes(pkg, hostSrc, "", true, &warn)

	gen := findTarget(pkg, "gen_w")
	want := []string{
		`#include "/usr/include/stdio.h"`,
		`#include "k/real.c"`,
		`#include "` + filepath.Join(hostSrc, "k/missing.c") + `"`,
		`#include "` + filepath.Join(hostSrc, "k/real.h") + `"`,
		`#include "rel/x.c"`,
	}
	if !reflect.DeepEqual(gen.WriteFileContent, want) {
		t.Errorf("content mismatch:\n got  %q\n want %q", gen.WriteFileContent, want)
	}

	// cc_binary has no textual_hdrs slot → a synth carrier lib carries k/real.c.
	tool := findTarget(pkg, "tool")
	carrier := findTarget(pkg, "tool_textual_srcs")
	if carrier == nil {
		t.Fatalf("expected synth carrier lib; targets: %+v", pkg.Targets)
	}
	if !reflect.DeepEqual(carrier.TextualHdrs, []string{"k/real.c"}) {
		t.Errorf("carrier textual_hdrs = %v, want [k/real.c]", carrier.TextualHdrs)
	}
	if !stringSliceContains(tool.Deps, ":tool_textual_srcs") {
		t.Errorf("tool deps missing carrier: %v", tool.Deps)
	}
	if !stringSliceContains(carrier.Tags, "cmake-codegen-generated-source-include") {
		t.Errorf("carrier missing audit tag: %v", carrier.Tags)
	}
}

// TestStageGeneratedSourceRootIncludes_NoOp: no write_file targets, or
// hostSrcOnDisk=false, is a clean no-op.
func TestStageGeneratedSourceRootIncludes_NoOp(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "lib", Kind: ir.KindCCLibrary, Srcs: []string{"a.c"}},
	}}
	stageGeneratedSourceRootIncludes(pkg, t.TempDir(), "p", true, nil)
	if len(pkg.Targets) != 1 || pkg.Targets[0].TextualHdrs != nil {
		t.Errorf("expected no-op, got %+v", pkg.Targets)
	}
	// hostSrcOnDisk=false short-circuits even with a candidate wrapper.
	pkg2 := &ir.Package{Targets: []ir.Target{
		{Name: "gen", Kind: ir.KindWriteFile, WriteFileOut: "w.c", WriteFileContent: []string{`#include "/x/y.c"`}},
	}}
	stageGeneratedSourceRootIncludes(pkg2, "/nonexistent", "p", false, nil)
	if pkg2.Targets[0].WriteFileContent[0] != `#include "/x/y.c"` {
		t.Errorf("hostSrcOnDisk=false should be a no-op")
	}
}
