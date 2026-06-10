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

// TestFindTextualSourceIncludes reproduces fmt's posix-mock-test shape: a test
// source quote-includes a library .cc (`../src/os.cc`) it does not itself
// compile. That file must be surfaced (so the caller can route it to a
// textual_hdrs slot) while in-srcs sources, header includes, escaping paths,
// and absent files are all excluded.
func TestFindTextualSourceIncludes(t *testing.T) {
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
	// test/posix-mock-test.cc textually includes ../src/os.cc (the idiom),
	// plus a header (must NOT be collected) and a sibling source it compiles.
	write("test/posix-mock-test.cc", "#include \"posix-mock.h\"\n#  include \"../src/os.cc\"\n#include <fmt/os.h>\n")
	write("src/os.cc", "int os(){return 0;}\n")
	write("src/format.cc", "int fmt(){return 0;}\n") // compiled by the target
	write("test/posix-mock.h", "#define M 1\n")
	// A source that includes a .cc which ESCAPES the element root — skipped.
	write("test/util.cc", "#include \"../../outside/x.cc\"\n")

	srcs := []string{"test/posix-mock-test.cc", "src/format.cc", "test/util.cc"}
	got, readers := findTextualSourceIncludes(hostSrc, srcs)
	want := []string{"src/os.cc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findTextualSourceIncludes includes = %v, want %v", got, want)
	}
	// readers = the INCLUDER source whose bytes drove the detection (the
	// declared source-byte read). The included os.cc is only Stat'd, not a
	// reader; util.cc's escaping include produced no hit, so it isn't a reader.
	wantReaders := []string{"test/posix-mock-test.cc"}
	if !reflect.DeepEqual(readers, wantReaders) {
		t.Errorf("findTextualSourceIncludes readers = %v, want %v", readers, wantReaders)
	}
}

// TestFindTextualSourceIncludes_NoneAndGuards: a target with only normal
// header includes (or no on-disk sources) yields nothing. A .cc include that
// names an already-compiled sibling src IS surfaced — cmake builds both
// shapes (VTK's bundled lz4: lz4.c compiles standalone AND lz4hc.c textually
// #include's it), and under Bazel a sibling src is not an input of the
// includer's compile action, so it must ALSO ride textual_hdrs (it stays in
// srcs; the attach never removes it).
func TestFindTextualSourceIncludes_NoneAndGuards(t *testing.T) {
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
	// a.cc includes a header (skip) and b.cc; b.cc is compiled by the target
	// AND textually included, so it must be surfaced (the lz4 fused shape).
	write("a.cc", "#include \"a.h\"\n#include \"b.cc\"\n")
	write("a.h", "#define A 1\n")
	write("b.cc", "int b(){return 0;}\n")
	got, readers := findTextualSourceIncludes(hostSrc, []string{"a.cc", "b.cc"})
	if len(got) != 1 || got[0] != "b.cc" {
		t.Errorf("compiled-and-textually-included sibling should be surfaced; got includes=%v", got)
	}
	if len(readers) != 1 || readers[0] != "a.cc" {
		t.Errorf("readers = %v, want [a.cc]", readers)
	}
	// A header-only includer surfaces nothing.
	if got, readers := findTextualSourceIncludes(hostSrc, []string{"b.cc"}); got != nil || readers != nil {
		t.Errorf("header-only target: got includes=%v readers=%v, want nil", got, readers)
	}
	// Empty inputs / missing host root are safe no-ops.
	if got, readers := findTextualSourceIncludes("", []string{"a.cc"}); got != nil || readers != nil {
		t.Errorf("empty hostSrc: got includes=%v readers=%v, want nil", got, readers)
	}
	if got, readers := findTextualSourceIncludes(hostSrc, nil); got != nil || readers != nil {
		t.Errorf("nil srcs: got includes=%v readers=%v, want nil", got, readers)
	}
}

// TestSynthesizeTextualSourceIncludeLibs: a cc_test that textually includes a
// .cc it doesn't compile gets a synthesized textual_hdrs cc_library carrying
// that file, plus a dep on it — declaring the input without compiling it
// standalone. Reproduces fmt's posix-mock-test. hostSrcOnDisk=false is a
// no-op (the scan reads source files).
func TestSynthesizeTextualSourceIncludeLibs(t *testing.T) {
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
	write("test/posix-mock-test.cc", "#include \"../src/os.cc\"\nint main(){return 0;}\n")
	write("src/os.cc", "int os(){return 0;}\n")
	write("src/format.cc", "int fmt(){return 0;}\n")

	mk := func() *ir.Package {
		return &ir.Package{
			Targets: []ir.Target{
				{Name: "posix-mock-test", Kind: ir.KindCCTest, Srcs: []string{"test/posix-mock-test.cc", "src/format.cc"}, Deps: []string{":gtest"}},
			},
			// The consumer lives in the test/ subpackage — the synth lib must
			// co-locate there so the dep stays same-package under split.
			SubPackages: map[string]string{"posix-mock-test": "test"},
		}
	}

	// hostSrcOnDisk=false → no-op.
	noop := mk()
	synthesizeTextualSourceIncludeLibs(noop, hostSrc, false, nil)
	if len(noop.Targets) != 1 {
		t.Fatalf("hostSrcOnDisk=false should be a no-op; got %d targets", len(noop.Targets))
	}
	if len(noop.SourceByteReads) != 0 {
		t.Errorf("no-op must publish no source reads; got %v", noop.SourceByteReads)
	}

	pkg := mk()
	var warn bytes.Buffer
	synthesizeTextualSourceIncludeLibs(pkg, hostSrc, true, &warn)

	if len(pkg.Targets) != 2 {
		t.Fatalf("expected original test + 1 synth lib, got %d targets", len(pkg.Targets))
	}
	test := findTarget(pkg, "posix-mock-test")
	lib := findTarget(pkg, "posix-mock-test_textual_srcs")
	if test == nil || lib == nil {
		t.Fatalf("expected posix-mock-test + synth lib posix-mock-test_textual_srcs; got %+v", pkg.Targets)
	}
	if lib.Kind != ir.KindCCLibrary {
		t.Errorf("synth lib Kind = %v, want cc_library", lib.Kind)
	}
	if !reflect.DeepEqual(lib.TextualHdrs, []string{"src/os.cc"}) {
		t.Errorf("synth lib TextualHdrs = %v, want [src/os.cc]", lib.TextualHdrs)
	}
	if !stringSliceContains(lib.Tags, "cmake-codegen-textual-source-include") {
		t.Errorf("synth lib missing audit tag: %v", lib.Tags)
	}
	if !stringSliceContains(test.Deps, ":posix-mock-test_textual_srcs") {
		t.Errorf("test deps missing synth lib: %v", test.Deps)
	}
	// The includer source whose bytes drove the detection is published as a
	// declared source-byte read (the narrowing-lens exception); the included
	// os.cc (only Stat'd) is NOT.
	if !reflect.DeepEqual(pkg.SourceByteReads, []string{"test/posix-mock-test.cc"}) {
		t.Errorf("SourceByteReads = %v, want [test/posix-mock-test.cc]", pkg.SourceByteReads)
	}
	if !stringSliceContains(test.Deps, ":gtest") {
		t.Errorf("test lost its original dep: %v", test.Deps)
	}
	// Co-located in the consumer's package (test/) so the dep is same-package
	// under split — a root-package private lib would be cross-package rejected.
	if got := pkg.SubPackages["posix-mock-test_textual_srcs"]; got != "test" {
		t.Errorf("synth lib SubPackages dir = %q, want \"test\" (co-located with consumer)", got)
	}
	if !strings.Contains(warn.String(), "posix-mock-test_textual_srcs") {
		t.Errorf("breadcrumb missing synth lib name:\n%s", warn.String())
	}
}

// TestFindTextualSourceIncludes_AbsoluteRejected: an absolute include
// (`#include "/src/os.cc"`) must be rejected outright. Without the guard,
// filepath.Join("test", "/src/os.cc") folds to "test/src/os.cc" — which we
// also create here — so it would be wrongly staged; the guard skips it.
func TestFindTextualSourceIncludes_AbsoluteRejected(t *testing.T) {
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
	write("test/a.cc", "#include \"/src/os.cc\"\n")
	write("test/src/os.cc", "int x(){return 0;}\n") // the would-be fold target
	if got, readers := findTextualSourceIncludes(hostSrc, []string{"test/a.cc"}); got != nil || readers != nil {
		t.Errorf("absolute include should be rejected; got includes=%v readers=%v", got, readers)
	}
}

// TestSynthesizeTextualSourceIncludeLibs_CCLibraryInline: a cc_library whose
// fused source textually #includes sibling .cc files (the gtest-all.cc /
// gmock-all.cc idiom) gets them in its OWN textual_hdrs — no synth lib (the
// library already has the slot). Also exercises ancestor-walk resolution: the
// fused source at gt/src/all.cc does `#include "src/gtest.cc"`, which resolves
// against the target's include root gt/ (an ancestor of the includer's gt/src/
// dir), not gt/src/ itself.
func TestSynthesizeTextualSourceIncludeLibs_CCLibraryInline(t *testing.T) {
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
	write("gt/src/all.cc", "#include \"src/gtest.cc\"\n#include \"src/port.cc\"\n")
	write("gt/src/gtest.cc", "int g(){return 0;}\n")
	write("gt/src/port.cc", "int p(){return 0;}\n")

	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "gtest", Kind: ir.KindCCLibrary, Srcs: []string{"gt/src/all.cc"}},
	}}
	synthesizeTextualSourceIncludeLibs(pkg, hostSrc, true, nil)
	// No synth lib for a cc_library — the includes go in its own textual_hdrs.
	if len(pkg.Targets) != 1 {
		t.Fatalf("expected no synth lib for cc_library; got %d targets: %+v", len(pkg.Targets), pkg.Targets)
	}
	want := []string{"gt/src/gtest.cc", "gt/src/port.cc"}
	if !reflect.DeepEqual(pkg.Targets[0].TextualHdrs, want) {
		t.Errorf("textual_hdrs = %v, want %v (ancestor-walk resolved against the include root gt/)", pkg.Targets[0].TextualHdrs, want)
	}
}
