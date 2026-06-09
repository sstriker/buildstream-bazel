package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
)

// applyInterfaceLinkScopeToDeps routes a target's PRIVATE link dependencies
// from the transitive `deps` to the non-transitive `implementation_deps`,
// using cmake's own private-link marker.
//
// Why this is needed beyond the trace. cmake records each target's
// INTERFACE_LINK_LIBRARIES with PRIVATE link deps wrapped in `$<LINK_ONLY:Dep>`
// and PUBLIC deps bare. The genex probe captures that property verbatim (genex
// intact), so `genexTargets[t].InterfaceLinkLibraries` carries the exact
// public/private split. The existing trace-driven routing
// (depScopeIsPrivate / traceLinkScope) only sees `target_link_libraries(...
// PRIVATE ...)` calls — but build systems that wire links through property
// machinery instead of target_link_libraries (VTK's vtk_module_link sets
// LINK_LIBRARIES via set_property, never emitting a TLL the trace can read)
// leave that routing blind, so every such PRIVATE dep lands in the transitive
// `deps`. Its PUBLIC compile definitions and include dirs then over-propagate
// to every consumer: VTK's `VTK::ParallelDIY` (PRIVATE_DEPENDS of
// FiltersExtraction) leaked `DIY_NO_THREADS` to ~478 TUs cmake never gave it
// to (34 vs 512), and OpenBLAS leaked a privately-linked dep's
// `lapack-netlib/LAPACKE/include` to ~1700.
//
// The marker is unambiguous, so this is precise — it moves ONLY deps cmake
// itself marked `$<LINK_ONLY:>` (and not also listed bare). Targets without a
// probe-captured interface (no LINK_ONLY markers — the trace-aggregate path,
// or no probe at all) yield an empty private set and the pass is a no-op, so
// non-probe converts stay byte-identical and the existing trace routing still
// owns PRIVATE classification there.
func applyInterfaceLinkScopeToDeps(pkg *ir.Package, genexTargets map[string]genexeval.TargetInfo, subParent map[string]string) {
	if pkg == nil || len(genexTargets) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if len(t.Deps) == 0 || !kindAllowsImplementationDeps(t.Kind) {
			continue
		}
		// Split subs are keyed under their synthesized name; the link
		// interface belongs to the owning cmake target.
		owner := t.Name
		if p, ok := subParent[t.Name]; ok {
			owner = p
		}
		ti, ok := genexTargets[owner]
		if !ok || ti.InterfaceLinkLibraries == "" {
			continue
		}
		priv, pub := parseInterfaceLinkScope(ti.InterfaceLinkLibraries)
		if len(priv) == 0 {
			continue
		}
		kept := t.Deps[:0]
		for _, dep := range t.Deps {
			name := localDepName(dep)
			// Only move an in-codebase `:dep` cmake marked LINK_ONLY-private
			// (and never bare-public). External/import labels and any dep that
			// also appears bare stay transitive.
			if name != "" && priv[name] && !pub[name] {
				if !stringSliceContains(t.ImplementationDeps, dep) {
					t.ImplementationDeps = append(t.ImplementationDeps, dep)
				}
				continue
			}
			kept = append(kept, dep)
		}
		t.Deps = kept
	}
}

// parseInterfaceLinkScope splits a raw (genex-intact) INTERFACE_LINK_LIBRARIES
// list into the namespace-stripped bare target names cmake marks PRIVATE
// (wrapped in `$<LINK_ONLY:...>`) versus PUBLIC (bare). Entries carrying any
// other unresolved genex are skipped defensively — a literal `$<...>` must
// never be treated as a target name.
func parseInterfaceLinkScope(joined string) (private, public map[string]bool) {
	private = map[string]bool{}
	public = map[string]bool{}
	for _, raw := range strings.Split(joined, ";") {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if inner, ok := stripLinkOnly(e); ok {
			if !strings.Contains(inner, "$<") {
				if n := bareTargetName(inner); n != "" {
					private[n] = true
				}
			}
			continue
		}
		if strings.Contains(e, "$<") {
			continue
		}
		if n := bareTargetName(e); n != "" {
			public[n] = true
		}
	}
	return private, public
}

// stripLinkOnly unwraps a `$<LINK_ONLY:X>` genex to X, reporting whether it
// matched. cmake emits exactly this wrapper for a PRIVATE link dependency in
// INTERFACE_LINK_LIBRARIES.
func stripLinkOnly(e string) (string, bool) {
	const p = "$<LINK_ONLY:"
	if strings.HasPrefix(e, p) && strings.HasSuffix(e, ">") {
		return e[len(p) : len(e)-1], true
	}
	return "", false
}

// bareTargetName strips a cmake namespace alias (`VTK::Foo` -> `Foo`) to the
// bare target name the codemodel / idToName registry uses. INTERFACE_LINK_-
// LIBRARIES references the `::`-aliased export name; the converter's dep labels
// carry the bare name.
func bareTargetName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	return s
}

// localDepName returns the bare target name of an in-codebase `:dep` label
// (pre-split, where intra-element deps are `:name`), or "" for an external /
// already-relabeled dep the link-scope pass must not touch.
func localDepName(label string) string {
	if strings.HasPrefix(label, ":") && !strings.Contains(label, "/") {
		return label[1:]
	}
	return ""
}
