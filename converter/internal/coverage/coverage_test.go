package coverage

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

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

	got := AuditLinkDeps(pkg, tll)
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
	if got := AuditLinkDeps(pkg, tll); len(got) != 0 {
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
	got := AuditLinkDeps(pkg, tll)
	if len(got) != 1 || got[0].Target != "boost_core" || got[0].Dep != "boost_assert" {
		t.Fatalf("findings = %+v, want boost_core->boost_assert", got)
	}
}

// TestAuditLinkDeps_NoTraceIsNoOp: with no trace link data the check
// emits nothing (it's trace-derived, like the other recovery passes).
func TestAuditLinkDeps_NoTraceIsNoOp(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "consumer", Kind: ir.KindCCLibrary},
	}}
	if got := AuditLinkDeps(pkg, nil); got != nil {
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
	if got := AuditLinkDeps(pkg, tll); len(got) != 0 {
		t.Errorf("findings = %+v, want none (genrule is not a link target)", got)
	}
}
