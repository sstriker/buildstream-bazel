package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
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
