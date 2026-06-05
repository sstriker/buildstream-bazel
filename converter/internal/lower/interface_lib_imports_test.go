package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerInterfaceLibraries_ImportsRouteNamespacedDep pins the abseil
// GTest::gmock fix: a trace-synth INTERFACE library that links a find_package
// IMPORTED target (`target_link_libraries(iface INTERFACE GTest::gmock)`) must
// route that dep through the imports manifest to its external Bazel label
// (`@googletest//:gtest`) — NOT fabricate a dangling in-package `:GTest_gmock`.
// This is the trace-synth counterpart of the codemodel dep path's
// imports.LookupCMakeTarget step.
func TestLowerInterfaceLibraries_ImportsRouteNamespacedDep(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{{Name: "iface", Type: "INTERFACE"}},
		Links: []shadow.TargetLinkCall{{
			Target: "iface",
			Groups: []shadow.TargetLinkGroup{
				{Visibility: "INTERFACE", Libs: []string{"GTest::gmock"}},
			},
		}},
	}
	rsv, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "googletest",
			Exports: []*manifest.Export{
				{CMakeTarget: "GTest::gmock", BazelLabel: "@googletest//:gtest"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("index imports: %v", err)
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "", "/src", "/src", nil, rsv,
		&codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib; got %d", len(got))
	}
	if want := []string{"@googletest//:gtest"}; !reflect.DeepEqual(got[0].Deps, want) {
		t.Errorf("iface.Deps = %v; want %v (find_package target routed via imports, not :GTest_gmock)", got[0].Deps, want)
	}
}

// TestLowerInterfaceLibraries_NoImportsKeepsSanitizedLocal pins the unchanged
// fallback: with no imports entry for a `::` name, the dep still sanitizes to a
// local `:Pkg_Comp` label (the alias-target rule resolves it). The imports
// routing is purely additive.
func TestLowerInterfaceLibraries_NoImportsKeepsSanitizedLocal(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{{Name: "iface", Type: "INTERFACE"}},
		Links: []shadow.TargetLinkCall{{
			Target: "iface",
			Groups: []shadow.TargetLinkGroup{
				{Visibility: "INTERFACE", Libs: []string{"Foo::bar"}},
			},
		}},
	}
	// nil resolver: LookupCMakeTarget is nil-safe and returns nil, so the
	// sanitize-to-local path stands.
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "", "/src", "/src", nil, nil,
		&codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib; got %d", len(got))
	}
	if want := []string{":Foo_bar"}; !reflect.DeepEqual(got[0].Deps, want) {
		t.Errorf("iface.Deps = %v; want %v (no imports entry → sanitized local label)", got[0].Deps, want)
	}
}

// TestPruneDanglingTraceInterfaceDeps pins the abseil
// absl::test_instance_tracker fix: a trace-synth INTERFACE library whose `:`-
// local dep names a target that was never emitted (a TESTONLY library the
// codemodel filtered out of a testing-off build) has that edge dropped, while a
// dep on an emitted sibling and an external (`@repo//:t`) label are kept.
func TestPruneDanglingTraceInterfaceDeps(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		// The trace-synth INTERFACE lib (abseil's heterogeneous_lookup_testing
		// shape: non-TESTONLY, but links a TESTONLY lib + a find_package target).
		{Name: "iface", Kind: ir.KindCCLibrary,
			Tags: []string{"cmake-codegen-interface-library-from-trace"},
			Deps: []string{
				":compare",                    // emitted sibling — keep
				":absl_test_instance_tracker", // TESTONLY, never emitted — drop
				"@googletest//:gtest",         // external import — keep
			},
			ImplementationDeps: []string{":also_missing"}, // not emitted — drop
		},
		// An emitted sibling the iface legitimately depends on.
		{Name: "compare", Kind: ir.KindCCLibrary, Srcs: []string{"compare.cc"}},
		// A non-interface target carrying a dangling local dep must be LEFT
		// ALONE — the prune only touches trace-synth INTERFACE libs.
		{Name: "regular", Kind: ir.KindCCLibrary, Srcs: []string{"r.cc"},
			Deps: []string{":absl_test_instance_tracker"}},
	}}
	if got := pruneDanglingTraceInterfaceDeps(pkg); got != 2 {
		t.Errorf("dropped count = %d; want 2 (iface's :absl_test_instance_tracker + :also_missing; regular's is NOT touched)", got)
	}

	if got, want := pkg.Targets[0].Deps, []string{":compare", "@googletest//:gtest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("iface.Deps = %v; want %v (dangling :absl_test_instance_tracker dropped, sibling + external kept)", got, want)
	}
	if got := pkg.Targets[0].ImplementationDeps; len(got) != 0 {
		t.Errorf("iface.ImplementationDeps = %v; want empty (dangling :also_missing dropped)", got)
	}
	if got, want := pkg.Targets[2].Deps, []string{":absl_test_instance_tracker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("regular.Deps = %v; want %v (non-interface target untouched)", got, want)
	}
}
