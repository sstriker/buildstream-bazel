package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// stripDepOwnedHdrs walks each cc_library / cc_binary / cc_test
// in the package and removes hdrs entries already owned by a
// sibling target that's in the consumer's deps. Bazel propagates
// hdrs through deps, so re-listing a transitively-reachable
// header is redundant — and at LLVM/VTK/Boost scale it inflates
// per-target hdrs counts by 10-100x.
//
// Real-world example (boost 1.86.0): `boost_atomic_cxx_0` had
// 1 src and 981 hdrs, of which 980 were owned by sibling
// boost.assert / boost.align / boost.config targets already
// declared in its deps. The strip preserves compilability —
// Bazel's hdrs propagation gives the consumer the same header
// surface — while collapsing the per-target hdrs to just what
// the target actually owns.
//
// Conservative dependency-aware shape: a header H owned by
// sibling S is stripped from consumer C's hdrs ONLY when C
// declares S in its deps (or implementation_deps). If C uses H
// without depending on S today (a bug in the consumer's cmake
// declarations cmake silently let pass), the strip would
// surface as a missing-include at Bazel build time — which is
// the right outcome long-term but disruptive on the cutover.
// The depends-on guard avoids that disruption.
//
// Ownership: a header may be claimed by multiple in-package
// cc_libraries (cmake's discoverHeaders walks each target's
// include-dir tree, so two targets sharing `include/` both
// surface every header under it in their hdrs). The strip
// considers ALL owners — if ANY of them is in the consumer's
// deps, the consumer's listing is redundant. Per-platform
// hdrs (irt.PerPlatform["hdrs"][k]) are not considered for
// ownership today — keep the strip focused on the flat hdrs
// slot to avoid select-arm aliasing complexity.
func stripDepOwnedHdrs(pkg *ir.Package) {
	if pkg == nil || len(pkg.Targets) == 0 {
		return
	}
	// First pass: build hdr → set-of-owner-target-names index.
	// Multiple owners are the common case in cmake projects
	// where many targets share the same `include/` root.
	owners := make(map[string]map[string]bool, 128)
	for _, t := range pkg.Targets {
		if t.Kind != ir.KindCCLibrary && t.Kind != ir.KindCCInterface {
			continue
		}
		for _, h := range t.Hdrs {
			set, ok := owners[h]
			if !ok {
				set = map[string]bool{}
				owners[h] = set
			}
			set[t.Name] = true
		}
	}
	if len(owners) == 0 {
		return
	}
	// Second pass: per consumer, walk hdrs; drop entries
	// owned by ANY sibling in the consumer's deps /
	// implementation_deps. Only strip from cc_library /
	// cc_interface kinds — cc_binary / cc_test fold their hdrs
	// into srcs at emit time (Bazel 9 doesn't accept hdrs on
	// executables), and stripping a srcs-bound entry would lose
	// the file from the consumer's compile altogether.
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindCCLibrary && t.Kind != ir.KindCCInterface {
			continue
		}
		if len(t.Hdrs) == 0 {
			continue
		}
		depTargets := siblingDepSet(t)
		if len(depTargets) == 0 {
			continue
		}
		kept := t.Hdrs[:0:0]
		for _, h := range t.Hdrs {
			set := owners[h]
			// A target always "owns" its own listed headers;
			// the strip never removes a header for which the
			// consumer is the only claimant.
			drop := false
			for ownerName := range set {
				if ownerName == t.Name {
					continue
				}
				if depTargets[ownerName] {
					drop = true
					break
				}
			}
			if !drop {
				kept = append(kept, h)
			}
		}
		t.Hdrs = kept
	}
}

// siblingDepSet returns the set of in-package target names the
// consumer depends on. Strips Bazel-label syntax to the bare
// target name — `:foo` / `foo` both yield "foo". Cross-package
// labels (`//other:foo`) are silently dropped (no in-package
// owner can match). implementation_deps participate alongside
// deps because both propagate headers in the consumer's
// compile action.
func siblingDepSet(t *ir.Target) map[string]bool {
	if len(t.Deps) == 0 && len(t.ImplementationDeps) == 0 {
		return nil
	}
	out := make(map[string]bool, len(t.Deps)+len(t.ImplementationDeps))
	for _, d := range t.Deps {
		if name := siblingTargetName(d); name != "" {
			out[name] = true
		}
	}
	for _, d := range t.ImplementationDeps {
		if name := siblingTargetName(d); name != "" {
			out[name] = true
		}
	}
	return out
}

// siblingTargetName returns the bare target name for an
// in-package dep label, "" for cross-package labels. Recognized
// shapes: `foo`, `:foo`. Cross-package shapes like `//pkg:foo`
// or `@repo//pkg:foo` aren't in-package siblings — empty return.
func siblingTargetName(label string) string {
	if label == "" {
		return ""
	}
	if strings.HasPrefix(label, "//") || strings.HasPrefix(label, "@") {
		return ""
	}
	return strings.TrimPrefix(label, ":")
}
