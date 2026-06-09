package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
)

func TestParseInterfaceLinkScope(t *testing.T) {
	// VTK FiltersExtraction shape: bare = PUBLIC, $<LINK_ONLY:...> = PRIVATE,
	// namespace stripped to the bare target name.
	in := "VTK::CommonCore;VTK::FiltersGeneral;$<LINK_ONLY:VTK::CommonDataModel>;" +
		"$<LINK_ONLY:VTK::ParallelDIY>;$<LINK_ONLY:VTK::vtkbuild>"
	priv, pub := parseInterfaceLinkScope(in)
	wantPriv := map[string]bool{"CommonDataModel": true, "ParallelDIY": true, "vtkbuild": true}
	wantPub := map[string]bool{"CommonCore": true, "FiltersGeneral": true}
	if !reflect.DeepEqual(priv, wantPriv) {
		t.Errorf("private = %v; want %v", priv, wantPriv)
	}
	if !reflect.DeepEqual(pub, wantPub) {
		t.Errorf("public = %v; want %v", pub, wantPub)
	}
}

func TestParseInterfaceLinkScope_SkipsOtherGenex(t *testing.T) {
	// A non-LINK_ONLY unresolved genex must never be treated as a target name.
	priv, pub := parseInterfaceLinkScope("Foo;$<TARGET_NAME_IF_EXISTS:Bar>;$<LINK_ONLY:Baz>")
	if pub["Foo"] != true || len(pub) != 1 {
		t.Errorf("public = %v; want only Foo", pub)
	}
	if priv["Baz"] != true || len(priv) != 1 {
		t.Errorf("private = %v; want only Baz", priv)
	}
}

// TestApplyInterfaceLinkScopeToDeps moves a LINK_ONLY-marked in-codebase dep to
// implementation_deps while keeping the bare PUBLIC dep transitive; an external
// label and a target without probe data are untouched.
func TestApplyInterfaceLinkScopeToDeps(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name: "FiltersExtraction",
				Kind: ir.KindCCLibrary,
				Deps: []string{":CommonCore", ":ParallelDIY", "//external:zlib"},
			},
			{
				// Split sub: owner is FiltersExtraction via subParent.
				Name: "FiltersExtraction_CXX_0",
				Kind: ir.KindCCLibrary,
				Deps: []string{":ParallelDIY"},
			},
			{
				// No probe signal (absent from genexTargets) — untouched.
				Name: "NoProbe",
				Kind: ir.KindCCLibrary,
				Deps: []string{":ParallelDIY"},
			},
		},
	}
	genexTargets := map[string]genexeval.TargetInfo{
		"FiltersExtraction": {InterfaceLinkLibraries: "VTK::CommonCore;$<LINK_ONLY:VTK::ParallelDIY>"},
	}
	subParent := map[string]string{"FiltersExtraction_CXX_0": "FiltersExtraction"}
	applyInterfaceLinkScopeToDeps(pkg, genexTargets, subParent)

	mod := pkg.Targets[0]
	if want := []string{":CommonCore", "//external:zlib"}; !reflect.DeepEqual(mod.Deps, want) {
		t.Errorf("Deps = %v; want %v (ParallelDIY private, external kept)", mod.Deps, want)
	}
	if want := []string{":ParallelDIY"}; !reflect.DeepEqual(mod.ImplementationDeps, want) {
		t.Errorf("ImplementationDeps = %v; want %v", mod.ImplementationDeps, want)
	}
	// Split sub inherits the owner's interface → its ParallelDIY moves too.
	sub := pkg.Targets[1]
	if len(sub.Deps) != 0 || !reflect.DeepEqual(sub.ImplementationDeps, []string{":ParallelDIY"}) {
		t.Errorf("sub Deps=%v ImplDeps=%v; want private move", sub.Deps, sub.ImplementationDeps)
	}
	// No probe data → untouched.
	if want := []string{":ParallelDIY"}; !reflect.DeepEqual(pkg.Targets[2].Deps, want) {
		t.Errorf("NoProbe Deps = %v; want unchanged %v", pkg.Targets[2].Deps, want)
	}

	// No LINK_ONLY markers (trace-aggregate / non-probe shape) → no-op.
	pkg2 := &ir.Package{Targets: []ir.Target{{Name: "x", Kind: ir.KindCCLibrary, Deps: []string{":a", ":b"}}}}
	applyInterfaceLinkScopeToDeps(pkg2, map[string]genexeval.TargetInfo{"x": {InterfaceLinkLibraries: "VTK::a;VTK::b"}}, nil)
	if want := []string{":a", ":b"}; !reflect.DeepEqual(pkg2.Targets[0].Deps, want) {
		t.Errorf("no-LINK_ONLY: Deps = %v; want unchanged %v", pkg2.Targets[0].Deps, want)
	}
	applyInterfaceLinkScopeToDeps(nil, genexTargets, subParent) // must not panic
}
