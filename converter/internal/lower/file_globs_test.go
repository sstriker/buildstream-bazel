package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// threadFileGlobs folds a genrule's srcs into build-time glob() filegroups
// when (and only when) the genrule depends on the *whole* result set of a
// cmake file(GLOB). It must: distinguish GLOB (flat "*.x") from
// GLOB_RECURSE ("**/*.x"); leave a genrule that only partially overlaps a
// glob untouched (the subset guard); skip RELATIVE globs; and remove the
// folded files from srcs while keeping unrelated explicit srcs.
func TestThreadFileGlobs(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("data/a.txt")
	write("data/b.txt")
	write("src/x.in")
	write("src/sub/y.in") // only a GLOB_RECURSE matches this
	write("other/keep.c")

	globs := []shadow.FileGlobCall{
		// Relative pattern — anchored to the calling list file's dir (root).
		{File: filepath.Join(root, "CMakeLists.txt"), Var: "inputs", Patterns: []string{"data/*.txt"}, Recurse: false},
		// Absolute pattern (callFile irrelevant).
		{Var: "deep", Patterns: []string{filepath.Join(root, "src", "*.in")}, Recurse: true},
		// A RELATIVE option glob that WOULD match other/keep.c — must be
		// skipped, so keep.c survives in gen_full's srcs below.
		{File: filepath.Join(root, "CMakeLists.txt"), Var: "rel", Patterns: []string{"other/*.c"}, Recurse: false, Relative: true},
	}

	// gen_full depends on the entire result of both non-relative globs (plus
	// one unrelated explicit src) → both fold; keep.c stays.
	full := ir.Target{
		Name: "gen_full", Kind: ir.KindGenrule, GenruleCmd: "touch $@",
		Srcs: []string{"data/a.txt", "data/b.txt", "src/x.in", "src/sub/y.in", "other/keep.c"},
	}
	// gen_partial depends on only part of the data glob (a.txt, not b.txt) →
	// the subset guard leaves it untouched.
	partial := ir.Target{
		Name: "gen_partial", Kind: ir.KindGenrule, GenruleCmd: "touch $@",
		Srcs: []string{"data/a.txt"},
	}
	targets := []ir.Target{full, partial}

	threadFileGlobs(targets, globs, root)

	if got := targets[0].Srcs; !reflect.DeepEqual(got, []string{"other/keep.c"}) {
		t.Errorf("gen_full srcs after fold:\n  got:  %v\n  want: [other/keep.c]", got)
	}
	wantGroups := []ir.GlobSrcGroup{
		{Dir: "data", Pattern: "*.txt"},
		{Dir: "src", Pattern: "**/*.in"},
	}
	if got := targets[0].GlobSrcGroups; !reflect.DeepEqual(got, wantGroups) {
		t.Errorf("gen_full GlobSrcGroups:\n  got:  %+v\n  want: %+v", got, wantGroups)
	}
	if got := targets[1].Srcs; !reflect.DeepEqual(got, []string{"data/a.txt"}) {
		t.Errorf("gen_partial srcs must be untouched (subset guard), got %v", got)
	}
	if targets[1].GlobSrcGroups != nil {
		t.Errorf("gen_partial must record no glob groups, got %+v", targets[1].GlobSrcGroups)
	}
}

// A glob whose wildcard is in the directory portion ("data/*/*.txt") can't
// be rooted at a real package, so it's skipped — the genrule keeps its
// explicit srcs rather than getting a bogus glob filegroup.
func TestThreadFileGlobs_DirWildcardSkipped(t *testing.T) {
	root := t.TempDir()
	w := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("data/a/x.txt")
	w("data/b/y.txt")
	globs := []shadow.FileGlobCall{
		{Var: "v", Patterns: []string{filepath.Join(root, "data", "*", "*.txt")}, Recurse: false},
	}
	targets := []ir.Target{{
		Name: "gen", Kind: ir.KindGenrule, GenruleCmd: "touch $@",
		Srcs: []string{"data/a/x.txt", "data/b/y.txt"},
	}}
	threadFileGlobs(targets, globs, root)
	if targets[0].GlobSrcGroups != nil {
		t.Errorf("dir-wildcard glob must be skipped, got %+v", targets[0].GlobSrcGroups)
	}
	if got := targets[0].Srcs; len(got) != 2 {
		t.Errorf("srcs must be untouched when the glob is skipped, got %v", got)
	}
}

// Empty labelRoot (offline replay, no source tree) disables the pass.
func TestThreadFileGlobs_NoLabelRoot(t *testing.T) {
	targets := []ir.Target{{
		Name: "gen", Kind: ir.KindGenrule, GenruleCmd: "touch $@",
		Srcs: []string{"data/a.txt"},
	}}
	threadFileGlobs(targets, []shadow.FileGlobCall{{Var: "v", Patterns: []string{"/x/data/*.txt"}}}, "")
	if targets[0].GlobSrcGroups != nil {
		t.Errorf("empty labelRoot should fold nothing, got %+v", targets[0].GlobSrcGroups)
	}
}
