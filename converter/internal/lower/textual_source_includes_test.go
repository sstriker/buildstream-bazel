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
	got := findTextualSourceIncludes(hostSrc, srcs)
	want := []string{"src/os.cc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findTextualSourceIncludes = %v, want %v", got, want)
	}
}

// TestFindTextualSourceIncludes_NoneAndGuards: a target with only normal
// header includes (or no on-disk sources) yields nothing; a .cc include that
// names an already-compiled src is not double-counted.
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
	// a.cc includes a header (skip) and b.cc — but b.cc is compiled by the
	// target, so it must not be surfaced as a textual include.
	write("a.cc", "#include \"a.h\"\n#include \"b.cc\"\n")
	write("a.h", "#define A 1\n")
	write("b.cc", "int b(){return 0;}\n")
	if got := findTextualSourceIncludes(hostSrc, []string{"a.cc", "b.cc"}); got != nil {
		t.Errorf("expected nil (b.cc is compiled), got %v", got)
	}
	// Empty inputs / missing host root are safe no-ops.
	if got := findTextualSourceIncludes("", []string{"a.cc"}); got != nil {
		t.Errorf("empty hostSrc: got %v, want nil", got)
	}
	if got := findTextualSourceIncludes(hostSrc, nil); got != nil {
		t.Errorf("nil srcs: got %v, want nil", got)
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

	pkg := mk()
	var warn bytes.Buffer
	synthesizeTextualSourceIncludeLibs(pkg, hostSrc, true, &warn)

	if len(pkg.Targets) != 2 {
		t.Fatalf("expected original test + 1 synth lib, got %d targets", len(pkg.Targets))
	}
	test := findTarget(pkg, "posix-mock-test")
	lib := findTarget(pkg, "posix-mock-test_textual_srcs")
	if lib == nil {
		t.Fatalf("synth lib posix-mock-test_textual_srcs not found: %+v", pkg.Targets)
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
	if got := findTextualSourceIncludes(hostSrc, []string{"test/a.cc"}); got != nil {
		t.Errorf("absolute include should be rejected; got %v", got)
	}
}
