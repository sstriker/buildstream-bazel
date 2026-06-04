package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
