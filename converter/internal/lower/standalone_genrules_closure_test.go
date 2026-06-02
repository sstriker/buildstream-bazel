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
// augmentCodegenIncludeClosure recovers the rest by globbing the primary
// input's extension under each source `-I` root. This pins that the glob
// is recursive, spans every root, dedups against the existing src, and
// leaves non-codegen genrules (primary input NOT under an `-I` root)
// untouched.
func TestAugmentCodegenIncludeClosure(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("// td\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// include root (recursive) + the target's own dir + a decoy of a
	// different extension that must NOT be swept in.
	write("inc/Target.td")
	write("inc/sub/Sub.td")
	write("lib/Foo/Foo.td")
	write("inc/decoy.h")

	tablegen := ir.Target{
		Name:        "gen_foo",
		Kind:        ir.KindGenrule,
		Srcs:        []string{"lib/Foo/Foo.td"},
		GenruleOuts: []string{"Foo.inc"},
		// Primary input lib/Foo/Foo.td sits under the `-I lib/Foo` root,
		// the include-resolving-codegen signal.
		GenruleCmd: "$(location //tools:tblgen) -I inc -I lib/Foo lib/Foo/Foo.td -o $(RULEDIR)/Foo.inc",
	}
	// Decoy: a plain genrule that passes -I for a compiler but whose
	// input is NOT under any -I root — must be left alone.
	plain := ir.Target{
		Name:        "compile_thing",
		Kind:        ir.KindGenrule,
		Srcs:        []string{"src/thing.c"},
		GenruleOuts: []string{"thing.o"},
		GenruleCmd:  "cc -I inc -c src/thing.c -o $(RULEDIR)/thing.o",
	}
	targets := []ir.Target{tablegen, plain}

	augmentCodegenIncludeClosure(targets, root)

	wantTablegen := []string{
		"inc/Target.td",
		"inc/sub/Sub.td",
		"lib/Foo/Foo.td",
	}
	if got := targets[0].Srcs; !reflect.DeepEqual(got, wantTablegen) {
		t.Errorf("tablegen genrule srcs:\n  got:  %v\n  want: %v", got, wantTablegen)
	}
	if got := targets[1].Srcs; !reflect.DeepEqual(got, []string{"src/thing.c"}) {
		t.Errorf("plain genrule srcs should be untouched, got %v", got)
	}
}

// Empty labelRoot (unit tests / offline replay with no source tree on
// disk) disables the pass entirely — no FS walk, srcs unchanged.
func TestAugmentCodegenIncludeClosure_NoLabelRoot(t *testing.T) {
	targets := []ir.Target{{
		Name:       "gen_foo",
		Kind:       ir.KindGenrule,
		Srcs:       []string{"lib/Foo/Foo.td"},
		GenruleCmd: "tblgen -I lib/Foo lib/Foo/Foo.td -o $(RULEDIR)/Foo.inc",
	}}
	augmentCodegenIncludeClosure(targets, "")
	if got := targets[0].Srcs; !reflect.DeepEqual(got, []string{"lib/Foo/Foo.td"}) {
		t.Errorf("empty labelRoot should be a no-op, got %v", got)
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
