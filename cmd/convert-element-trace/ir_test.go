package main

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestToIR_LibraryAndBinaryMapping verifies the trace-driven
// converter's CCRule slice flattens to the shared ir.Package shape
// the orchestrator's per-element multi-platform fold consumes.
// The mapping is one-to-one (one CCRule → one ir.Target), with
// Linkstatic=true on every cc_library to mirror the legacy
// emitBuild output, and Visibility=["//visibility:public"] on
// every target so the fold's cross-cell agreement check sees a
// consistent value.
func TestToIR_LibraryAndBinaryMapping(t *testing.T) {
	rules := []CCRule{
		{
			RuleKind: "cc_library",
			Name:     "foo",
			Srcs:     []string{"foo.c", "bar.c"},
			Copts:    []string{"-fstack-protector-strong"},
			Defines:  []string{"USE_FEATURE=1"},
		},
		{
			RuleKind: "cc_binary",
			Name:     "myapp",
			Srcs:     []string{"myapp.c"},
			Deps:     []string{":foo"},
		},
	}
	pkg := toIR(rules)
	if len(pkg.Targets) != 2 {
		t.Fatalf("toIR: want 2 targets, got %d", len(pkg.Targets))
	}
	lib := pkg.Targets[0]
	if lib.Kind != ir.KindCCLibrary {
		t.Errorf("toIR: cc_library mapped to %v, want %v", lib.Kind, ir.KindCCLibrary)
	}
	if !lib.Linkstatic {
		t.Errorf("toIR: cc_library missing linkstatic=true (legacy emitBuild adds it on every cc_library)")
	}
	if lib.Name != "foo" {
		t.Errorf("toIR: cc_library name %q, want foo", lib.Name)
	}
	wantSrcs := []string{"foo.c", "bar.c"}
	for i, s := range wantSrcs {
		if i >= len(lib.Srcs) || lib.Srcs[i] != s {
			t.Errorf("toIR: cc_library srcs[%d]=%q, want %q", i, lib.Srcs[i], s)
		}
	}
	if got := lib.Visibility; len(got) != 1 || got[0] != "//visibility:public" {
		t.Errorf("toIR: cc_library visibility %v, want [//visibility:public]", got)
	}
	bin := pkg.Targets[1]
	if bin.Kind != ir.KindCCBinary {
		t.Errorf("toIR: cc_binary mapped to %v, want %v", bin.Kind, ir.KindCCBinary)
	}
	if bin.Linkstatic {
		t.Errorf("toIR: cc_binary unexpectedly has linkstatic=true (only cc_library should)")
	}
	if len(bin.Deps) != 1 || bin.Deps[0] != ":foo" {
		t.Errorf("toIR: cc_binary deps %v, want [:foo]", bin.Deps)
	}
}

// TestToIR_Empty covers the round-2 boot phase: no rules recovered
// (no trace published yet). toIR returns an empty Package, which
// fold-element composes to empty without complaint — the same
// semantic the placeholder BUILD.bazel.out carries on the legacy
// rendering path.
func TestToIR_Empty(t *testing.T) {
	pkg := toIR(nil)
	if len(pkg.Targets) != 0 {
		t.Errorf("toIR(nil): want 0 targets, got %d", len(pkg.Targets))
	}
	if pkg.Name != "" {
		t.Errorf("toIR(nil): want empty Name (trace converter has no project() name), got %q", pkg.Name)
	}
}

// TestRecoveredRules_GeneratedHeadersAppliedUniformly mirrors
// emitBuild's internal logic: the generated-headers list lands as
// Hdrs on every rule (AC_CONFIG_HEADERS-style config.h is universal
// to every TU in an autotools build). The recoveredRules helper
// extracts the same post-fold ruleset for the --out-ir-json path
// so the JSON ir.Package matches the BUILD.bazel.out byte-stably.
func TestRecoveredRules_GeneratedHeadersAppliedUniformly(t *testing.T) {
	events := []Event{
		{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}},
		{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o"}},
		{Kind: EventLink, Out: "bar", Srcs: []string{"bar.c"}},
	}
	rules := recoveredRules(correlate(events), nil, nil, []string{"config.h"})
	if len(rules) != 2 {
		t.Fatalf("recoveredRules: want 2 rules, got %d", len(rules))
	}
	for _, r := range rules {
		if len(r.Hdrs) != 1 || r.Hdrs[0] != "config.h" {
			t.Errorf("recoveredRules: rule %q hdrs %v, want [config.h]", r.Name, r.Hdrs)
		}
	}
}
