package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerInterfaceLibraries_RootIncludeDiscoversHeaders pins the glm
// glm-header-only fix (part 1): an INTERFACE lib whose declared include path
// is the package root — `target_include_directories(lib INTERFACE
// $<BUILD_INTERFACE:${CMAKE_SOURCE_DIR}>)`, which relativizes to "" — must
// walk the root and OWN its headers rather than emit empty. Bazel rejects
// `includes = [""]`, so the root is not emitted as an include attr.
func TestLowerInterfaceLibraries_RootIncludeDiscoversHeaders(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "foo.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{{Name: "rootlib", Type: "INTERFACE"}},
		Includes: []shadow.TargetIncludeCall{{
			Target: "rootlib",
			Groups: []shadow.TargetIncludeGroup{
				{Visibility: "INTERFACE", Dirs: []string{root}}, // absolute root → rel ""
			},
		}},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, root, root, root, nil,
		&codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib; got %d", len(got))
	}
	if len(got[0].Includes) != 0 {
		t.Errorf("Includes = %v; want empty (the package root is never an includes attr)", got[0].Includes)
	}
	found := false
	for _, h := range got[0].Hdrs {
		// discoverHeaders normalizes with filepath.ToSlash, so the result is
		// always forward-slashed regardless of host OS.
		if h == "pkg/foo.h" {
			found = true
		}
	}
	if !found {
		t.Errorf("Hdrs = %v; want to contain pkg/foo.h (root-walk discovery)", got[0].Hdrs)
	}
}

// TestRouteTraceInterfaceLibDeps pins part 2: a trace target_link_libraries
// arm naming an in-codebase trace-synth INTERFACE lib is routed into the
// consumer's deps (glm → glm-header-only). Arms naming a non-interface
// target, an unknown name, or a system lib are left alone.
func TestRouteTraceInterfaceLibDeps(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "glm", Kind: ir.KindCCLibrary, Srcs: []string{"glm.cpp"}},
		{Name: "glm-header-only", Kind: ir.KindCCLibrary,
			Tags: []string{"cmake-codegen-interface-library-from-trace"}},
		{Name: "other", Kind: ir.KindCCLibrary}, // in-codebase but NOT interface-from-trace
	}}
	traceLinkLibs := map[string][]string{
		"glm": {"glm-header-only", "other", "pthread"},
	}
	scope := map[string]map[string]string{"glm": {"glm-header-only": "PUBLIC"}}
	routeTraceInterfaceLibDeps(pkg, traceLinkLibs, scope)
	if got := pkg.Targets[0].Deps; !reflect.DeepEqual(got, []string{":glm-header-only"}) {
		t.Errorf("glm.Deps = %v; want [:glm-header-only] (PUBLIC arm, only the trace-synth interface)", got)
	}
}

// TestRouteTraceInterfaceLibDeps_PrivateScope honours the trace arm's
// visibility: a PRIVATE link on a cc_library routes to implementation_deps
// (no re-export), while a binary/test — which has no implementation_deps
// bucket — takes it in deps regardless.
func TestRouteTraceInterfaceLibDeps_PrivateScope(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "lib", Kind: ir.KindCCLibrary, Srcs: []string{"a.cpp"}},
		{Name: "iface", Kind: ir.KindCCLibrary,
			Tags: []string{"cmake-codegen-interface-library-from-trace"}},
		{Name: "app", Kind: ir.KindCCBinary, Srcs: []string{"main.cpp"}},
		// A cc_interface consumer also emits as cc_library, so a PRIVATE arm
		// must land in implementation_deps just like cc_library.
		{Name: "ifacecons", Kind: ir.KindCCInterface},
	}}
	traceLinkLibs := map[string][]string{"lib": {"iface"}, "app": {"iface"}, "ifacecons": {"iface"}}
	scope := map[string]map[string]string{
		"lib":       {"iface": "PRIVATE"},
		"app":       {"iface": "PRIVATE"},
		"ifacecons": {"iface": "PRIVATE"},
	}
	routeTraceInterfaceLibDeps(pkg, traceLinkLibs, scope)
	if got := pkg.Targets[0].ImplementationDeps; !reflect.DeepEqual(got, []string{":iface"}) {
		t.Errorf("lib.ImplementationDeps = %v; want [:iface] (PRIVATE on cc_library)", got)
	}
	if len(pkg.Targets[0].Deps) != 0 {
		t.Errorf("lib.Deps = %v; want empty (PRIVATE routed to implementation_deps)", pkg.Targets[0].Deps)
	}
	if got := pkg.Targets[2].Deps; !reflect.DeepEqual(got, []string{":iface"}) {
		t.Errorf("app.Deps = %v; want [:iface] (binary has no implementation_deps bucket)", got)
	}
	if got := pkg.Targets[3].ImplementationDeps; !reflect.DeepEqual(got, []string{":iface"}) {
		t.Errorf("ifacecons.ImplementationDeps = %v; want [:iface] (PRIVATE on cc_interface)", got)
	}
}

// TestRouteTraceInterfaceLibDeps_NoDuplicate doesn't re-add an edge already
// present in deps or implementation_deps.
func TestRouteTraceInterfaceLibDeps_NoDuplicate(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "app", Kind: ir.KindCCLibrary, ImplementationDeps: []string{":iface"}},
		{Name: "iface", Kind: ir.KindCCLibrary,
			Tags: []string{"cmake-codegen-interface-library-from-trace"}},
	}}
	routeTraceInterfaceLibDeps(pkg, map[string][]string{"app": {"iface"}}, nil)
	if got := pkg.Targets[0].Deps; len(got) != 0 {
		t.Errorf("app.Deps = %v; want empty (edge already in implementation_deps, no duplicate)", got)
	}
}
