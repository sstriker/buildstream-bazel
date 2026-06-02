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
// recordCodegenIncludeGlobs marks the source `-I` roots (that actually
// hold files of the codegen extension) on the genrule so split can emit a
// build-time glob() filegroup per owning package. It must NOT bake a file
// list into srcs (that would rot in the owned project B), must skip roots
// with no matching file on disk, and must leave non-codegen genrules
// (primary input NOT under an `-I` root) untouched.
func TestRecordCodegenIncludeGlobs(t *testing.T) {
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
	write("inc/Target.td")
	write("inc/sub/Sub.td")
	write("lib/Foo/Foo.td")
	write("empty/decoy.h") // an -I root with no .td → no glob recorded

	tablegen := ir.Target{
		Name: "gen_foo",
		Kind: ir.KindGenrule,
		Srcs: []string{"lib/Foo/Foo.td"},
		// Primary input lib/Foo/Foo.td sits under the `-I lib/Foo` root
		// (the include-resolving-codegen signal); `empty` holds no .td.
		GenruleCmd: "$(location //tools:tblgen) -I inc -I lib/Foo -I empty lib/Foo/Foo.td -o $(RULEDIR)/Foo.inc",
	}
	plain := ir.Target{
		Name:       "compile_thing",
		Kind:       ir.KindGenrule,
		Srcs:       []string{"src/thing.c"},
		GenruleCmd: "cc -I inc -c src/thing.c -o $(RULEDIR)/thing.o",
	}
	targets := []ir.Target{tablegen, plain}

	recordCodegenIncludeGlobs(targets, root)

	wantGlobs := []ir.CodegenIncludeGlob{
		{Root: "inc", Ext: ".td"},
		{Root: "lib/Foo", Ext: ".td"},
	}
	if got := targets[0].CodegenIncludeGlobs; !reflect.DeepEqual(got, wantGlobs) {
		t.Errorf("CodegenIncludeGlobs:\n  got:  %+v\n  want: %+v", got, wantGlobs)
	}
	// srcs must be untouched — no baked closure.
	if got := targets[0].Srcs; !reflect.DeepEqual(got, []string{"lib/Foo/Foo.td"}) {
		t.Errorf("tablegen srcs must not be baked, got %v", got)
	}
	if got := targets[1].CodegenIncludeGlobs; got != nil {
		t.Errorf("plain genrule must record no globs, got %+v", got)
	}
}

// Empty labelRoot (unit tests / offline replay with no source tree on
// disk) disables the pass entirely — no FS walk, nothing recorded.
func TestRecordCodegenIncludeGlobs_NoLabelRoot(t *testing.T) {
	targets := []ir.Target{{
		Name:       "gen_foo",
		Kind:       ir.KindGenrule,
		Srcs:       []string{"lib/Foo/Foo.td"},
		GenruleCmd: "tblgen -I lib/Foo lib/Foo/Foo.td -o $(RULEDIR)/Foo.inc",
	}}
	recordCodegenIncludeGlobs(targets, "")
	if got := targets[0].CodegenIncludeGlobs; got != nil {
		t.Errorf("empty labelRoot should record nothing, got %+v", got)
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
