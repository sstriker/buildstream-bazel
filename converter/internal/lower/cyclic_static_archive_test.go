package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// TestDetectCyclicStaticArchives_InCodebaseAlwayslink: an in-codebase
// archive cmake repeats on the link line (cyclic SCC) gets alwayslink set
// on its emitted cc_library directly — the Bazel whole-archive equivalent —
// plus an informational todo so the auto-applied change is traceable.
func TestDetectCyclicStaticArchives_InCodebaseAlwayslink(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "foo", Kind: ir.KindCCLibrary},
		{Name: "app", Kind: ir.KindCCBinary},
	}}
	r := &fileapi.Reply{Targets: map[string]fileapi.Target{
		"foo::@": {Name: "foo", Type: "STATIC_LIBRARY", NameOnDisk: "libfoo.a"},
		"app::@": {Name: "app", Type: "EXECUTABLE", Link: &fileapi.TargetLink{
			CommandFragments: []fileapi.CommandFragment{
				{Role: "libraries", Fragment: "/b/libfoo.a"},
				{Role: "libraries", Fragment: "/b/libfoo.a"}, // repeated → cyclic SCC
			},
		}},
	}}
	tc := todos.New()
	detectCyclicStaticArchives(r, pkg, nil, "", tc)

	var foo *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "foo" {
			foo = &pkg.Targets[i]
		}
	}
	if foo == nil || !foo.Alwayslink {
		t.Fatalf("in-codebase repeated archive must set Alwayslink: %+v", foo)
	}
	got := tc.Report(todos.Preamble{}, "").Todos
	if len(got) != 1 || got[0].Kind != "cyclic-static-archive" || got[0].Disposition != todos.Informational {
		t.Errorf("want 1 informational cyclic-static-archive todo, got %+v", got)
	}
}

// TestDetectCyclicStaticArchives_PrebuiltTodo: a repeated PREBUILT archive
// (a manifest export, whose cc_import wrapper the converter can't touch)
// gets an actionable todo — unless the harvester already flagged it
// AlwaysLink, in which case it's silent (the wrapper is already
// whole-archive).
func TestDetectCyclicStaticArchives_PrebuiltTodo(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "app", Kind: ir.KindCCBinary}}}
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{Name: "pkg", Exports: []*manifest.Export{
			{CMakeTarget: "Pkg::a", BazelLabel: "//pkg:a", LinkPaths: []string{"/opt/prefix/lib/liba.a"}},
			{CMakeTarget: "Pkg::b", BazelLabel: "//pkg:b", AlwaysLink: true, LinkPaths: []string{"/opt/prefix/lib/libb.a"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{Targets: map[string]fileapi.Target{
		"app::@": {Name: "app", Type: "EXECUTABLE", Link: &fileapi.TargetLink{
			CommandFragments: []fileapi.CommandFragment{
				{Role: "libraries", Fragment: "/opt/prefix/lib/liba.a"},
				{Role: "libraries", Fragment: "/opt/prefix/lib/liba.a"}, // un-flagged prebuilt → todo
				{Role: "libraries", Fragment: "/opt/prefix/lib/libb.a"},
				{Role: "libraries", Fragment: "/opt/prefix/lib/libb.a"}, // already AlwaysLink → no todo
			},
		}},
	}}
	tc := todos.New()
	detectCyclicStaticArchives(r, pkg, res, "", tc)
	got := tc.Report(todos.Preamble{}, "").Todos
	if len(got) != 1 {
		t.Fatalf("want 1 todo (only the un-flagged prebuilt), got %d: %+v", len(got), got)
	}
	if got[0].Disposition != todos.Actionable || got[0].GroupKey != "Pkg::a" {
		t.Errorf("prebuilt todo = %+v, want actionable Pkg::a", got[0])
	}
}
