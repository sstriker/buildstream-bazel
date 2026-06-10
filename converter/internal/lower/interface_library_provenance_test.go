package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerInterfaceLibraries_ProvenanceAndCallSite: a trace-synthesized
// INTERFACE library carries Provenance (the add_library call the trace
// recorded — the helper body for function-wrapped declarations) and CallSite
// (the user-level invocation from the trace frame stack), both reanchored to
// source-root-relative form. Comment recovery prefers CallSite, so the
// author comment above the `absl_cc_library(...)`-style call carries to the
// lib; the emit-side breadcrumb leads with it.
func TestLowerInterfaceLibraries_ProvenanceAndCallSite(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{{
			Name: "gizmo", Type: "INTERFACE",
			File: "/src/CMake/Helpers.cmake", Line: 30,
			CallFile: "/src/CMakeLists.txt", CallLine: 7, CallCmd: "add_iface_lib",
		}},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "", "/src", "/src", nil, nil,
		&codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib, got %v", got)
	}
	p := got[0].Provenance
	if p.File != "CMake/Helpers.cmake" || p.Line != 30 || p.Command != "add_library" {
		t.Errorf("Provenance = %+v; want CMake/Helpers.cmake:30 add_library", p)
	}
	c := got[0].CallSite
	if c.File != "CMakeLists.txt" || c.Line != 7 || c.Command != "add_iface_lib" {
		t.Errorf("CallSite = %+v; want CMakeLists.txt:7 add_iface_lib", c)
	}
}

// TestLowerInterfaceLibraries_NoCallSiteForDirectDeclaration: an INTERFACE
// library declared directly (no recovered invocation) gets Provenance only —
// zero CallSite, so comment recovery reads the declaration's own site.
func TestLowerInterfaceLibraries_NoCallSiteForDirectDeclaration(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{{
			Name: "plain", Type: "INTERFACE",
			File: "/src/CMakeLists.txt", Line: 12,
		}},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "", "/src", "/src", nil, nil,
		&codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib, got %v", got)
	}
	if p := got[0].Provenance; p.File != "CMakeLists.txt" || p.Line != 12 {
		t.Errorf("Provenance = %+v; want CMakeLists.txt:12", p)
	}
	if !got[0].CallSite.IsZero() {
		t.Errorf("CallSite = %+v; want zero for a direct declaration", got[0].CallSite)
	}
}
