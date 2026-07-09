// Package coverage holds the converter's lens-3 ("did we lose intent
// vs the CMakeLists?") audit. Unlike the rejection collector (Tier-1
// refusals the converter knows it made) and the bazelidiom audit
// (non-idiomatic shapes in the emitted BUILD), coverage findings are
// the losses the converter would otherwise NOT self-report: intent the
// codemodel/trace recorded that didn't make it into the IR.
//
// v1 implements the one deterministic, low-false-positive check
// identified in docs/survey-corpus.md's "three lenses" section:
// dependency coverage. A broad codemodel→BUILD differ is deliberately
// out of scope (its false-positive cost — object-library inlining,
// genrule re-wiring, header-library synthesis, the EXECUTABLE→cc_test
// rewrite — exceeds the signal; most other loss is already accounted by
// elision tags + unresolved-link-dep + the empty-* idioms).
package coverage

import (
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// Finding is one lens-3 coverage gap, keyed by the consuming target and
// the lost edge. Field names are capitalised to match the bazelidiom
// report's JSON shape (the survey counts both by `Code`).
type Finding struct {
	Rule    string `json:"Rule"`
	Target  string `json:"Target"`
	Dep     string `json:"Dep"`
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// Collector accumulates coverage findings. The zero value is usable;
// nil is a no-op sink so callers can pass it unconditionally.
type Collector struct {
	items []Finding
}

// New returns a fresh Collector.
func New() *Collector { return &Collector{} }

// Add records one finding (no-op on a nil collector).
func (c *Collector) Add(f Finding) {
	if c == nil {
		return
	}
	c.items = append(c.items, f)
}

// Reset clears the recorded findings. ToIR can run more than once against
// the same collector (two-pass genex / stamp / nested-cmake recovery);
// callers Reset before the final pass so the report reflects only that
// pass rather than accumulating duplicates. Mirrors todos.Collector.Reset.
func (c *Collector) Reset() {
	if c == nil {
		return
	}
	c.items = nil
}

// Items returns a copy of the recorded findings in insertion order.
func (c *Collector) Items() []Finding {
	if c == nil {
		return nil
	}
	out := make([]Finding, len(c.items))
	copy(out, c.items)
	return out
}

// AuditLinkDeps is the dependency-coverage check. For each emitted cc
// target it compares the trace-recorded target_link_libraries arms
// against what actually landed in deps / implementation_deps / data,
// and flags an arm that:
//
//   - names an in-codebase target (matches an emitted cc_library /
//     cc_binary / cc_library-interface name — the authoritative
//     in-codebase oracle), AND
//   - is absent from every dep bucket of that target.
//
// Such an arm is a silently dropped dependency edge — intent loss the
// converter didn't otherwise report. This is the exact class PR #302
// fixed (INTERFACE-library link arms not routed to deps); the check is
// a regression tripwire for it and a finder for new instances.
//
// A `::`-namespaced arm (alias / find_package-imported target) is
// handled through the imports manifest rather than the in-codebase
// oracle: when the manifest RESOLVES the arm to a Bazel label (so the
// lowering was expected to wire that label) but the label is absent
// from every dep bucket, it's a dropped find_package link edge —
// reported as "dropped-find-package-dep". An arm the manifest doesn't
// know (a truly external / system lib with no export) stays skipped:
// there's no label it could have dropped. This closes the historical
// blind spot where every `::` arm was skipped unconditionally. Because
// the check keys on the target's DIRECTLY-named arms, it never trips on
// the intentional transitive-only archive drop in lowerLinkFragments
// (that fires for arms a target does NOT name directly).
//
// Conservative by construction (biased to false negatives, not false
// positives), so a non-zero count is a real signal:
//   - `::` arms the imports manifest can't resolve are skipped (system
//     libraries, out-of-tree imports with no export).
//   - bare arms that don't match any emitted target name are skipped —
//     system libraries (pthread, m, …), which correctly never become an
//     in-codebase dep.
//   - a self-reference is skipped.
//
// imports is the same resolver the lowering wired deps from (may be
// nil — then the `::` check is a no-op, preserving the pre-widening
// behavior). traceLinkLibs maps a cmake target name to its ordered
// target_link_libraries lib names (all visibility arms); it is empty
// when no trace is available, in which case this check is a no-op.
func AuditLinkDeps(pkg *ir.Package, traceLinkLibs map[string][]string, imports *manifest.Resolver) []Finding {
	if pkg == nil || len(traceLinkLibs) == 0 {
		return nil
	}

	// The in-codebase oracle: every emitted cc target name -> its own
	// direct deps. The name set is the match oracle; the deps are used
	// to see through alias / interface-library indirection (below).
	inCodebase := make(map[string]bool, len(pkg.Targets))
	ownDeps := make(map[string][]string, len(pkg.Targets))
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if !isCCTarget(t.Kind) {
			continue
		}
		inCodebase[t.Name] = true
		d := make([]string, 0, len(t.Deps)+len(t.ImplementationDeps))
		d = append(d, t.Deps...)
		d = append(d, t.ImplementationDeps...)
		ownDeps[t.Name] = d
	}

	var findings []Finding
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if !isCCTarget(t.Kind) {
			continue
		}
		libs := traceLinkLibs[t.Name]
		if len(libs) == 0 {
			continue
		}
		emitted := make(map[string]bool, len(t.Deps)+len(t.ImplementationDeps)+len(t.Data))
		for _, d := range t.Deps {
			emitted[d] = true
		}
		for _, d := range t.ImplementationDeps {
			emitted[d] = true
		}
		for _, d := range t.Data {
			emitted[d] = true
		}

		seen := map[string]bool{}
		for _, lib := range libs {
			if lib == t.Name || seen[lib] {
				continue
			}
			seen[lib] = true
			if strings.Contains(lib, "::") {
				if f, ok := auditFindPackageArm(t, lib, imports, emitted); ok {
					findings = append(findings, f)
				}
				continue
			}
			if f, ok := auditInCodebaseArm(t, lib, inCodebase, ownDeps, emitted); ok {
				findings = append(findings, f)
			}
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Target != findings[j].Target {
			return findings[i].Target < findings[j].Target
		}
		return findings[i].Dep < findings[j].Dep
	})
	return findings
}

// auditFindPackageArm audits a `::` (find_package / namespaced) arm
// through the imports manifest: when the manifest resolves it to a Bazel
// label that never landed in a dep bucket, that's a dropped find_package
// edge. An arm the manifest can't resolve (a system / out-of-tree import
// with no export) is not flagged — there's no label it could have
// dropped — and a nil resolver disables the check entirely.
func auditFindPackageArm(t *ir.Target, lib string, imports *manifest.Resolver, emitted map[string]bool) (Finding, bool) {
	if imports == nil {
		return Finding{}, false
	}
	ex := imports.LookupCMakeTarget(lib)
	if ex == nil || ex.BazelLabel == "" || emitted[ex.BazelLabel] {
		return Finding{}, false
	}
	return Finding{
		Rule:   ruleName(t.Kind),
		Target: t.Name,
		Dep:    lib,
		Code:   "dropped-find-package-dep",
		Message: "target_link_libraries names find_package target " + lib +
			" (imports manifest resolves it to " + ex.BazelLabel +
			") but that label is absent from deps/implementation_deps/data — a silent dropped find_package link edge (lens-3 intent loss)",
	}, true
}

// auditInCodebaseArm audits a bare arm against the in-codebase oracle: an
// arm naming an emitted cc target that's absent from every dep bucket is
// a dropped edge (the #302 class), UNLESS it's reachable one hop through
// an alias/forwarder's own deps. cmake's link arm may name an
// interface-library / alias target (libevent's `event_core`) that the
// converter resolved to that target's concrete dep (`:event_core_shared`)
// and handed the consumer directly — the edge IS present under the
// resolved label, so accept when the consumer's deps include any of
// `lib`'s own direct deps. One hop covers the common forwarder shape;
// deeper chains stay conservative (a false negative, the safe direction).
func auditInCodebaseArm(t *ir.Target, lib string, inCodebase map[string]bool, ownDeps map[string][]string, emitted map[string]bool) (Finding, bool) {
	if !inCodebase[lib] || emitted[":"+lib] {
		return Finding{}, false
	}
	for _, rd := range ownDeps[lib] {
		if emitted[rd] {
			return Finding{}, false
		}
	}
	return Finding{
		Rule:   ruleName(t.Kind),
		Target: t.Name,
		Dep:    lib,
		Code:   "dropped-link-dep",
		Message: "target_link_libraries names in-codebase target " + lib +
			" but it is absent from deps/implementation_deps/data — a silent dropped dependency edge (lens-3 intent loss); check that the lowering routes this link arm (cf. #302, INTERFACE-library arms)",
	}, true
}

func isCCTarget(k ir.Kind) bool {
	switch k {
	case ir.KindCCLibrary, ir.KindCCBinary, ir.KindCCInterface:
		return true
	default:
		return false
	}
}

func ruleName(k ir.Kind) string {
	switch k {
	case ir.KindCCBinary:
		return "cc_binary"
	case ir.KindCCInterface:
		return "cc_library"
	default:
		return "cc_library"
	}
}
