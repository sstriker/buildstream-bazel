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
// Conservative by construction (biased to false negatives, not false
// positives), so a non-zero count is a real signal:
//   - `::`-namespaced arms (alias / find_package-imported targets) are
//     skipped — they resolve through the imports manifest / alias rules,
//     not an in-codebase ":name" dep.
//   - arms that don't match any emitted target name are skipped — system
//     libraries (pthread, m, …) and out-of-tree imports, which correctly
//     never become an in-codebase dep.
//   - a self-reference is skipped.
//
// traceLinkLibs maps a cmake target name to its ordered
// target_link_libraries lib names (all visibility arms); it is empty
// when no trace is available, in which case this check is a no-op.
func AuditLinkDeps(pkg *ir.Package, traceLinkLibs map[string][]string) []Finding {
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
			if lib == t.Name || strings.Contains(lib, "::") {
				continue
			}
			if !inCodebase[lib] || seen[lib] {
				continue
			}
			seen[lib] = true
			if emitted[":"+lib] {
				continue
			}
			// Alias / interface-library indirection: cmake's link arm
			// names `lib`, but the converter may resolve it to `lib`'s
			// own concrete target(s) — e.g. libevent's `event_core` is a
			// trace-synthesized interface cc_library whose only dep is
			// `:event_core_shared`, and consumers get
			// deps=[":event_core_shared"] directly. The edge IS present,
			// just under the resolved label, so don't flag it. Accept
			// when the consumer's deps include any of `lib`'s own direct
			// deps. One hop is enough for the common alias/forwarder
			// shape; deeper chains stay conservative (a false negative,
			// which is the safe direction for this audit).
			resolved := false
			for _, rd := range ownDeps[lib] {
				if emitted[rd] {
					resolved = true
					break
				}
			}
			if resolved {
				continue
			}
			findings = append(findings, Finding{
				Rule:   ruleName(t.Kind),
				Target: t.Name,
				Dep:    lib,
				Code:   "dropped-link-dep",
				Message: "target_link_libraries names in-codebase target " + lib +
					" but it is absent from deps/implementation_deps/data — a silent dropped dependency edge (lens-3 intent loss); check that the lowering routes this link arm (cf. #302, INTERFACE-library arms)",
			})
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
