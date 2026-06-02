package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// Tablegen-shaped genrules resolve `include "x.td"` against their `-I`
// roots, and cmake tracks that transitive closure only in a dynamic
// DEPFILE — so the lowered genrule lists just the explicit primary input.
// recordCodegenIncludeClosure replicates the depfile statically: it
// follows `include "..."` directives from the primary input, resolving
// each against the `-I` roots, and appends every reachable source file to
// srcs. It must add the precise closure (not a coarse glob), resolve
// transitively, and leave non-codegen genrules (primary input NOT under an
// `-I` root) untouched.
func TestRecordCodegenIncludeClosure(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Foo.td (primary) -> Target.td (via -I inc) -> sub/Sub.td (via -I inc).
	// Decoy.td is NOT included by anything, so it must NOT enter the
	// closure — that's the difference from a glob over-approximation.
	write("lib/Foo/Foo.td", "include \"Target.td\"\n")
	write("inc/Target.td", "  include \"sub/Sub.td\" // indented form\n")
	write("inc/sub/Sub.td", "// leaf, no includes\n")
	write("inc/Decoy.td", "// never included\n")

	tablegen := ir.Target{
		Name: "gen_foo",
		Kind: ir.KindGenrule,
		Srcs: []string{"lib/Foo/Foo.td"},
		// Primary input lib/Foo/Foo.td sits under the `-I lib/Foo` root
		// (the include-resolving-codegen signal); includes resolve via -I inc.
		GenruleCmd: "$(location //tools:tblgen) -I inc -I lib/Foo lib/Foo/Foo.td -o $(RULEDIR)/Foo.inc",
	}
	plain := ir.Target{
		Name:       "compile_thing",
		Kind:       ir.KindGenrule,
		Srcs:       []string{"src/thing.c"},
		GenruleCmd: "cc -I inc -c src/thing.c -o $(RULEDIR)/thing.o",
	}
	targets := []ir.Target{tablegen, plain}

	recordCodegenIncludeClosure(targets, root)

	// Primary stays first; closure additions appended sorted. Decoy.td is
	// excluded (not reachable via include directives).
	wantSrcs := []string{"lib/Foo/Foo.td", "inc/Target.td", "inc/sub/Sub.td"}
	if got := targets[0].Srcs; !reflect.DeepEqual(got, wantSrcs) {
		t.Errorf("tablegen srcs closure:\n  got:  %v\n  want: %v", got, wantSrcs)
	}
	if got := targets[1].Srcs; !reflect.DeepEqual(got, []string{"src/thing.c"}) {
		t.Errorf("plain genrule srcs must be untouched, got %v", got)
	}
}

// An include that doesn't resolve on the source FS (e.g. a generated .td)
// terminates that branch — no panic, no bogus src.
func TestRecordCodegenIncludeClosure_UnresolvedInclude(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "lib", "Foo.td")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("include \"Generated.td\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []ir.Target{{
		Name:       "gen_foo",
		Kind:       ir.KindGenrule,
		Srcs:       []string{"lib/Foo.td"},
		GenruleCmd: "tblgen -I lib lib/Foo.td -o $(RULEDIR)/Foo.inc",
	}}
	recordCodegenIncludeClosure(targets, root)
	if got := targets[0].Srcs; !reflect.DeepEqual(got, []string{"lib/Foo.td"}) {
		t.Errorf("unresolved include must add nothing, got %v", got)
	}
}

// Empty labelRoot (unit tests / offline replay with no source tree on
// disk) disables the pass entirely — no FS access, nothing added.
func TestRecordCodegenIncludeClosure_NoLabelRoot(t *testing.T) {
	targets := []ir.Target{{
		Name:       "gen_foo",
		Kind:       ir.KindGenrule,
		Srcs:       []string{"lib/Foo/Foo.td"},
		GenruleCmd: "tblgen -I lib/Foo lib/Foo/Foo.td -o $(RULEDIR)/Foo.inc",
	}}
	recordCodegenIncludeClosure(targets, "")
	if got := targets[0].Srcs; !reflect.DeepEqual(got, []string{"lib/Foo/Foo.td"}) {
		t.Errorf("empty labelRoot should add nothing, got %v", got)
	}
}

func TestGenruleIncludeRoots(t *testing.T) {
	cmd := "$(location //t:tg) -gen-x -I inc -Ilib/Foo/ -I inc -Iinclude in.td -o out"
	got := genruleIncludeRoots(cmd)
	want := []string{"inc", "lib/Foo", "include"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("genruleIncludeRoots:\n  got:  %v\n  want: %v", got, want)
	}
}
