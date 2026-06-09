package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestApplyPrivateScopeToDefines_RoutesPrivateToLocalDefines(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Kind:    ir.KindCCLibrary,
			Defines: []string{"PUBLIC_MACRO=1", "PRIVATE_MACRO=2"},
		}},
	}
	calls := []shadow.TargetCompileCall{
		{
			Cmd:    "target_compile_definitions",
			Target: "foo",
			Groups: []shadow.TargetCompileGroup{
				{Visibility: "PUBLIC", Items: []string{"PUBLIC_MACRO=1"}},
				{Visibility: "PRIVATE", Items: []string{"PRIVATE_MACRO=2"}},
			},
		},
	}
	applyPrivateScopeToDefines(pkg, calls)
	got := pkg.Targets[0]
	if want := []string{"PUBLIC_MACRO=1"}; !reflect.DeepEqual(got.Defines, want) {
		t.Errorf("Defines = %v; want %v", got.Defines, want)
	}
	if want := []string{"PRIVATE_MACRO=2"}; !reflect.DeepEqual(got.LocalDefines, want) {
		t.Errorf("LocalDefines = %v; want %v", got.LocalDefines, want)
	}
}

func TestApplyPrivateScopeToDefines_EmptyVisibilityTreatedPrivate(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Kind:    ir.KindCCLibrary,
			Defines: []string{"LEGACY_FORM=1"},
		}},
	}
	calls := []shadow.TargetCompileCall{
		{
			Cmd:    "target_compile_definitions",
			Target: "foo",
			Groups: []shadow.TargetCompileGroup{
				// Legacy positional form: visibility = "".
				// cmake treats these as PRIVATE-equivalent for
				// the target's own compile.
				{Visibility: "", Items: []string{"LEGACY_FORM=1"}},
			},
		},
	}
	applyPrivateScopeToDefines(pkg, calls)
	got := pkg.Targets[0]
	if len(got.Defines) != 0 {
		t.Errorf("Defines should be empty after moving legacy form; got %v", got.Defines)
	}
	if want := []string{"LEGACY_FORM=1"}; !reflect.DeepEqual(got.LocalDefines, want) {
		t.Errorf("LocalDefines = %v; want %v", got.LocalDefines, want)
	}
}

func TestApplyPrivateScopeToDefines_LeavesDefinesNotInTraceUntouched(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Kind:    ir.KindCCLibrary,
			Defines: []string{"FROM_ADD_DEFINITIONS=1", "FROM_DIR_LEVEL=2"},
		}},
	}
	// Trace records nothing for foo — all defines came from
	// add_definitions / directory-level / CMAKE_*_FLAGS sources
	// that the trace doesn't tag per-target.
	calls := []shadow.TargetCompileCall{}
	applyPrivateScopeToDefines(pkg, calls)
	got := pkg.Targets[0]
	if !reflect.DeepEqual(got.Defines, []string{"FROM_ADD_DEFINITIONS=1", "FROM_DIR_LEVEL=2"}) {
		t.Errorf("Defines unchanged expected; got %v", got.Defines)
	}
	if got.LocalDefines != nil {
		t.Errorf("LocalDefines should stay nil; got %v", got.LocalDefines)
	}
}

func TestApplyPrivateScopeToDefines_KeepsPublicInDefines(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Defines: []string{"PUB=1", "IFC=2"},
		}},
	}
	calls := []shadow.TargetCompileCall{
		{
			Cmd:    "target_compile_definitions",
			Target: "foo",
			Groups: []shadow.TargetCompileGroup{
				{Visibility: "PUBLIC", Items: []string{"PUB=1"}},
				{Visibility: "INTERFACE", Items: []string{"IFC=2"}},
			},
		},
	}
	applyPrivateScopeToDefines(pkg, calls)
	got := pkg.Targets[0]
	if want := []string{"PUB=1", "IFC=2"}; !reflect.DeepEqual(got.Defines, want) {
		t.Errorf("PUBLIC/INTERFACE should stay in Defines; got %v", got.Defines)
	}
	if got.LocalDefines != nil {
		t.Errorf("LocalDefines should stay nil; got %v", got.LocalDefines)
	}
}

func TestApplyPrivateScopeToDefines_IgnoresCompileOptionsCmd(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Defines: []string{"FOO=1"},
		}},
	}
	calls := []shadow.TargetCompileCall{
		{
			// target_compile_OPTIONS, not target_compile_definitions.
			Cmd:    "target_compile_options",
			Target: "foo",
			Groups: []shadow.TargetCompileGroup{
				{Visibility: "PRIVATE", Items: []string{"FOO=1"}},
			},
		},
	}
	applyPrivateScopeToDefines(pkg, calls)
	got := pkg.Targets[0]
	if !reflect.DeepEqual(got.Defines, []string{"FOO=1"}) {
		t.Errorf("compile_options should not move defines; got %v", got.Defines)
	}
}

func TestApplyPrivateScopeToDefines_NilPkg(t *testing.T) {
	// Should not panic.
	applyPrivateScopeToDefines(nil, []shadow.TargetCompileCall{})
}

// add_definitions() defines are directory-scoped + PRIVATE: they must
// move to local_defines so they don't propagate to consumers (curl's
// BUILDING_LIBCURL on libcurl leaking to the curl tool).
func TestApplyAddDefinitionsScope_RoutesToLocalDefines(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "libcurl",
			Kind:    ir.KindCCLibrary,
			Defines: []string{"BUILDING_LIBCURL", "CURL_STATICLIB"},
		}},
	}
	addDefs := []shadow.AddDefinitionsCall{{Items: []string{"BUILDING_LIBCURL"}}}
	applyAddDefinitionsScope(pkg, addDefs, nil)
	got := pkg.Targets[0]
	if want := []string{"CURL_STATICLIB"}; !reflect.DeepEqual(got.Defines, want) {
		t.Errorf("Defines = %v; want %v", got.Defines, want)
	}
	if want := []string{"BUILDING_LIBCURL"}; !reflect.DeepEqual(got.LocalDefines, want) {
		t.Errorf("LocalDefines = %v; want %v", got.LocalDefines, want)
	}
}

// A define a target ALSO declares via PUBLIC target_compile_definitions
// genuinely propagates — it must stay in Defines even if an identical
// string appears in an add_definitions elsewhere in the project.
func TestApplyAddDefinitionsScope_PublicTcdWins(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Kind:    ir.KindCCLibrary,
			Defines: []string{"SHARED_MACRO"},
		}},
	}
	addDefs := []shadow.AddDefinitionsCall{{Items: []string{"SHARED_MACRO"}}}
	tcd := []shadow.TargetCompileCall{{
		Cmd:    "target_compile_definitions",
		Target: "foo",
		Groups: []shadow.TargetCompileGroup{{Visibility: "PUBLIC", Items: []string{"SHARED_MACRO"}}},
	}}
	applyAddDefinitionsScope(pkg, addDefs, tcd)
	got := pkg.Targets[0]
	if want := []string{"SHARED_MACRO"}; !reflect.DeepEqual(got.Defines, want) {
		t.Errorf("Defines = %v; want %v (PUBLIC tcd must stay transitive)", got.Defines, want)
	}
	if len(got.LocalDefines) != 0 {
		t.Errorf("LocalDefines = %v; want empty", got.LocalDefines)
	}
}

// No add_definitions recorded → byte-identical no-op (codemodel-only path).
func TestApplyAddDefinitionsScope_NoTraceNoOp(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Kind:    ir.KindCCLibrary,
			Defines: []string{"A", "B"},
		}},
	}
	applyAddDefinitionsScope(pkg, nil, nil)
	if want := []string{"A", "B"}; !reflect.DeepEqual(pkg.Targets[0].Defines, want) {
		t.Errorf("Defines = %v; want %v", pkg.Targets[0].Defines, want)
	}
	if len(pkg.Targets[0].LocalDefines) != 0 {
		t.Errorf("LocalDefines = %v; want empty", pkg.Targets[0].LocalDefines)
	}
	applyAddDefinitionsScope(nil, nil, nil) // must not panic
}

// TestApplyInterfaceScopeToDefines_KeepsExportedRoutesRest is the principled
// pass: a define stays transitive iff the owning cmake target exports it via
// INTERFACE_COMPILE_DEFINITIONS; everything else (private feature macros, the
// auto <target>_EXPORTS macro, NDEBUG globals) moves to local_defines —
// including for split sub-libraries mapped back to their owning target via
// subParent.
func TestApplyInterfaceScopeToDefines_KeepsExportedRoutesRest(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				// A normal target exporting PUB_API; FOO_INTERNAL + NDEBUG private.
				Name:    "vtksys",
				Kind:    ir.KindCCLibrary,
				Defines: []string{"PUB_API=1", "FOO_INTERNAL=2", "NDEBUG"},
			},
			{
				// A split sub-library: keyed under <parent>_CXX_4, carries the
				// real per-compile-group defines, owns nothing itself.
				Name:    "vtksys_CXX_4",
				Kind:    ir.KindCCLibrary,
				Defines: []string{"KWSYS_CXX_HAS_GETLOADAVG=1", "vtksys_EXPORTS", "SIZEOF_VOID_P=8"},
			},
			{
				// Absent from genexTargets (e.g. synthesized header lib) — left
				// untouched.
				Name:    "synth_headers",
				Kind:    ir.KindCCLibrary,
				Defines: []string{"NDEBUG"},
			},
		},
	}
	genexTargets := map[string]genexeval.TargetInfo{
		// vtksys exports only PUB_API; the sub's owner (vtksys) exports nothing
		// beyond that, so all the sub's feature/EXPORTS defines move local.
		"vtksys": {InterfaceCompileDefinitions: "PUB_API=1"},
	}
	subParent := map[string]string{"vtksys_CXX_4": "vtksys"}

	applyInterfaceScopeToDefines(pkg, genexTargets, subParent)

	// Normal target: PUB_API stays transitive; the rest go local.
	if want := []string{"PUB_API=1"}; !reflect.DeepEqual(pkg.Targets[0].Defines, want) {
		t.Errorf("vtksys.Defines = %v; want %v", pkg.Targets[0].Defines, want)
	}
	if want := []string{"FOO_INTERNAL=2", "NDEBUG"}; !reflect.DeepEqual(pkg.Targets[0].LocalDefines, want) {
		t.Errorf("vtksys.LocalDefines = %v; want %v", pkg.Targets[0].LocalDefines, want)
	}
	// Split sub: owner exports none of these → all move local (no leak).
	if len(pkg.Targets[1].Defines) != 0 {
		t.Errorf("vtksys_CXX_4.Defines = %v; want empty (no transitive leak)", pkg.Targets[1].Defines)
	}
	want := []string{"KWSYS_CXX_HAS_GETLOADAVG=1", "vtksys_EXPORTS", "SIZEOF_VOID_P=8"}
	if !reflect.DeepEqual(pkg.Targets[1].LocalDefines, want) {
		t.Errorf("vtksys_CXX_4.LocalDefines = %v; want %v", pkg.Targets[1].LocalDefines, want)
	}
	// Target absent from genexTargets: unchanged (conservative).
	if want := []string{"NDEBUG"}; !reflect.DeepEqual(pkg.Targets[2].Defines, want) {
		t.Errorf("synth_headers.Defines = %v; want %v (untouched)", pkg.Targets[2].Defines, want)
	}
	if len(pkg.Targets[2].LocalDefines) != 0 {
		t.Errorf("synth_headers.LocalDefines = %v; want empty", pkg.Targets[2].LocalDefines)
	}

	// Empty genexTargets is a no-op (no interface signal).
	pkg2 := &ir.Package{Targets: []ir.Target{{Name: "x", Defines: []string{"A"}}}}
	applyInterfaceScopeToDefines(pkg2, nil, nil)
	if want := []string{"A"}; !reflect.DeepEqual(pkg2.Targets[0].Defines, want) {
		t.Errorf("no-op: Defines = %v; want %v", pkg2.Targets[0].Defines, want)
	}
	applyInterfaceScopeToDefines(nil, genexTargets, subParent) // must not panic
}
