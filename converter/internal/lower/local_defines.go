package lower

import (
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// applyPrivateScopeToDefines routes PRIVATE-scoped
// target_compile_definitions trace events into the IR's
// non-transitive `local_defines` attribute instead of the
// transitive `defines`. Closes the cmake-to-Bazel scope-fidelity
// gap on PRIVATE compile_definitions:
//
//	target_compile_definitions(foo PRIVATE FOO_INTERNAL=1)
//	# cmake: applies only when compiling foo's own sources.
//	# Bazel `defines = ["FOO_INTERNAL=1"]` propagates the macro
//	# to every consumer linking against :foo — wrong scope.
//	# Bazel `local_defines = ["FOO_INTERNAL=1"]` matches cmake.
//
// Conservative behaviour: only moves defines the trace explicitly
// classifies as PRIVATE-scope. Defines that arrive from
// add_definitions / directory-level / CMAKE_*_FLAGS sources stay
// in the transitive Defines slice — the trace doesn't tag those,
// so the conservative default preserves the pre-existing emit.
//
// PUBLIC and INTERFACE scopes stay in Defines: Bazel's transitive
// defines is the right shape for those (the macro reaches every
// consumer). The empty-visibility legacy positional shape
// (`target_compile_definitions(foo FOO)` without a keyword) is
// PRIVATE-equivalent per cmake's own docs — also routed to
// LocalDefines.
//
// The pass walks pkg.Targets in-place. Targets without
// trace-recorded PRIVATE defines, or whose trace records no
// PRIVATE definitions, stay byte-identical to the pre-existing
// emit.
func applyPrivateScopeToDefines(pkg *ir.Package, calls []shadow.TargetCompileCall) {
	if pkg == nil || len(calls) == 0 {
		return
	}
	// Build (target → set of PRIVATE-scoped defines) from the
	// trace. Empty-visibility "" is PRIVATE-equivalent per cmake.
	privateByTarget := map[string]map[string]bool{}
	for _, call := range calls {
		if call.Cmd != "target_compile_definitions" {
			continue
		}
		for _, grp := range call.Groups {
			switch grp.Visibility {
			case "PRIVATE", "":
				// keep
			default:
				continue
			}
			set := privateByTarget[call.Target]
			if set == nil {
				set = map[string]bool{}
				privateByTarget[call.Target] = set
			}
			for _, item := range grp.Items {
				set[item] = true
			}
		}
	}
	if len(privateByTarget) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		privSet := privateByTarget[t.Name]
		if len(privSet) == 0 {
			continue
		}
		// Partition t.Defines into transitive (kept) + private (moved).
		kept := t.Defines[:0]
		for _, d := range t.Defines {
			if privSet[d] {
				if !stringSliceContains(t.LocalDefines, d) {
					t.LocalDefines = append(t.LocalDefines, d)
				}
				continue
			}
			kept = append(kept, d)
		}
		t.Defines = kept
	}
}
