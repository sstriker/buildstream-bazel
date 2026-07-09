package coverage

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// fpResolver indexes a tiny imports manifest for the find_package tests.
func fpResolver(t *testing.T, exports ...*manifest.Export) *manifest.Resolver {
	t.Helper()
	res, err := manifest.Index(&manifest.Imports{
		Version:  1,
		Elements: []*manifest.Element{{Name: "e", Exports: exports}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestAuditLinkDeps_FlagsDroppedInCodebaseEdge: a target_link_libraries
// arm naming an in-codebase target that didn't land in any dep bucket is
// a silent dropped edge (the #302 class) and must be flagged.
func TestAuditLinkDeps_FlagsDroppedInCodebaseEdge(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "consumer", Kind: ir.KindCCLibrary, Deps: []string{":other"}},
		{Name: "dep_lib", Kind: ir.KindCCLibrary},
		{Name: "other", Kind: ir.KindCCLibrary},
	}}
	// consumer links dep_lib (in-codebase) + other (routed) + pthread (system).
	tll := map[string][]string{"consumer": {"dep_lib", "other", "pthread"}}

	got := AuditLinkDeps(pkg, tll, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(got), got)
	}
	if got[0].Target != "consumer" || got[0].Dep != "dep_lib" || got[0].Code != "dropped-link-dep" {
		t.Errorf("finding = %+v, want consumer/dep_lib/dropped-link-dep", got[0])
	}
}

// TestAuditLinkDeps_RoutedEdgesAndNonCodebaseClean: arms that landed in
// deps / implementation_deps / data, plus system libs and ::-namespaced
// (alias / imported) arms, must NOT be flagged.
func TestAuditLinkDeps_RoutedEdgesAndNonCodebaseClean(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{
			Name:               "consumer",
			Kind:               ir.KindCCLibrary,
			Deps:               []string{":pub"},
			ImplementationDeps: []string{":priv"},
			Data:               []string{":ordering"},
		},
		{Name: "pub", Kind: ir.KindCCLibrary},
		{Name: "priv", Kind: ir.KindCCLibrary},
		{Name: "ordering", Kind: ir.KindCCLibrary},
	}}
	tll := map[string][]string{"consumer": {
		"pub",         // routed to deps
		"priv",        // routed to implementation_deps
		"ordering",    // routed to data (add_dependencies edge)
		"m",           // system lib, not an in-codebase target
		"Boost::core", // namespaced alias / imported — skipped
		"consumer",    // self-reference — skipped
	}}
	if got := AuditLinkDeps(pkg, tll, nil); len(got) != 0 {
		t.Errorf("findings = %+v, want none", got)
	}
}

// TestAuditLinkDeps_InterfaceLibConsumer covers the #302 shape directly:
// a synthesized INTERFACE library (KindCCInterface) whose link arm to an
// in-codebase target was dropped is flagged just like a cc_library.
func TestAuditLinkDeps_InterfaceLibConsumer(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "boost_core", Kind: ir.KindCCInterface}, // dropped the arm
		{Name: "boost_assert", Kind: ir.KindCCInterface},
	}}
	tll := map[string][]string{"boost_core": {"boost_assert"}}
	got := AuditLinkDeps(pkg, tll, nil)
	if len(got) != 1 || got[0].Target != "boost_core" || got[0].Dep != "boost_assert" {
		t.Fatalf("findings = %+v, want boost_core->boost_assert", got)
	}
}

// TestAuditLinkDeps_AliasResolvedDepNotFlagged is the libevent false-
// positive guard: cmake's link arm names an interface-library / alias
// target (event_core), but the converter resolves it to that target's
// own concrete dep (:event_core_shared) and the consumer gets THAT in
// deps. The edge is present under the resolved label, so it must NOT be
// flagged. (Before the alias-resolution fix this produced 28 spurious
// findings on libevent's sample programs.)
func TestAuditLinkDeps_AliasResolvedDepNotFlagged(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		// Interface-library alias: event_core forwards to the concrete
		// shared lib.
		{Name: "event_core", Kind: ir.KindCCLibrary, Deps: []string{":event_core_shared"}},
		{Name: "event_core_shared", Kind: ir.KindCCLibrary},
		// Sample binary links `event_core` in cmake; converter gave it
		// the resolved :event_core_shared.
		{Name: "hello_world", Kind: ir.KindCCBinary, Deps: []string{":event_core_shared"}},
	}}
	tll := map[string][]string{"hello_world": {"event_core"}}
	if got := AuditLinkDeps(pkg, tll, nil); len(got) != 0 {
		t.Errorf("findings = %+v, want none (event_core resolved to :event_core_shared)", got)
	}
}

// TestAuditLinkDeps_FlagsDroppedFindPackageEdge: a directly-named `::`
// arm the imports manifest RESOLVES to a label that never landed in any
// dep bucket is a silent dropped find_package edge — the `::` analog of
// the in-codebase drop, previously invisible because every `::` arm was
// skipped unconditionally.
func TestAuditLinkDeps_FlagsDroppedFindPackageEdge(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "consumer", Kind: ir.KindCCLibrary, Deps: []string{"@zlib//:zlib"}},
	}}
	// consumer links Foo::foo (resolves to @foo//:foo, DROPPED) and
	// ZLIB::ZLIB (resolves to @zlib//:zlib, wired — must not flag).
	res := fpResolver(t,
		&manifest.Export{CMakeTarget: "Foo::foo", BazelLabel: "@foo//:foo"},
		&manifest.Export{CMakeTarget: "ZLIB::ZLIB", BazelLabel: "@zlib//:zlib"},
	)
	tll := map[string][]string{"consumer": {"Foo::foo", "ZLIB::ZLIB"}}
	got := AuditLinkDeps(pkg, tll, res)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1 (only Foo::foo dropped): %+v", len(got), got)
	}
	if got[0].Target != "consumer" || got[0].Dep != "Foo::foo" || got[0].Code != "dropped-find-package-dep" {
		t.Errorf("finding = %+v, want consumer/Foo::foo/dropped-find-package-dep", got[0])
	}
}

// TestAuditLinkDeps_FindPackageUnknownOrNilResolverClean: a `::` arm the
// manifest can't resolve (a system/out-of-tree import with no export) is
// NOT flagged — there's no label it could have dropped — and a nil
// resolver disables the `::` check entirely (pre-widening behavior).
func TestAuditLinkDeps_FindPackageUnknownOrNilResolverClean(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "consumer", Kind: ir.KindCCLibrary},
	}}
	tll := map[string][]string{"consumer": {"Unknown::thing"}}
	// Resolver knows a DIFFERENT target; Unknown::thing resolves to nil.
	res := fpResolver(t, &manifest.Export{CMakeTarget: "Known::known", BazelLabel: "@known//:known"})
	if got := AuditLinkDeps(pkg, tll, res); len(got) != 0 {
		t.Errorf("unknown :: arm must not flag (no label to drop): %+v", got)
	}
	if got := AuditLinkDeps(pkg, tll, nil); len(got) != 0 {
		t.Errorf("nil resolver must disable the :: check: %+v", got)
	}
}

// TestAuditLinkDeps_NoTraceIsNoOp: with no trace link data the check
// emits nothing (it's trace-derived, like the other recovery passes).
func TestAuditLinkDeps_NoTraceIsNoOp(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "consumer", Kind: ir.KindCCLibrary},
	}}
	if got := AuditLinkDeps(pkg, nil, nil); got != nil {
		t.Errorf("findings = %+v, want nil with no trace", got)
	}
}

// TestAuditLinkDeps_NonCCTargetsIgnored: genrules / tests aren't cc
// link targets and must be skipped both as consumers and as the
// in-codebase oracle.
func TestAuditLinkDeps_NonCCTargetsIgnored(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "gen", Kind: ir.KindGenrule},
		{Name: "consumer", Kind: ir.KindCCLibrary},
	}}
	// consumer "links" gen — but gen isn't a cc target, so it's not an
	// in-codebase link target (no false positive).
	tll := map[string][]string{"consumer": {"gen"}}
	if got := AuditLinkDeps(pkg, tll, nil); len(got) != 0 {
		t.Errorf("findings = %+v, want none (genrule is not a link target)", got)
	}
}
