package lower

import (
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchainfeature"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// liftRawFeatureFlags walks each ir.Target in the package and
// rewrites raw compile / link flags onto a cc_toolchain feature a real
// toolchain backs (`-fPIC`, `-flto`, `-fsanitize=address` / `thread` /
// `memory` / `undefined`) by removing them from `Copts` / `LinkOpts` and
// adding the matching feature name to `Features`. Flags whose feature the
// converter's generated toolchain and bazel's default don't back — the
// visibility presets and `-fsanitize=leak` — are left as raw copts/linkopts
// (see toolchainfeature.RewriteFeature); lifting them would silently drop the
// flag. (No toolchain anywhere defines a visibility feature; an `lsan` feature
// does exist in the example sanitizer-features template, but that isn't a
// toolchain converted projects target by default, so leak stays raw too. The
// walk processes both Copts and LinkOpts, so an unbacked flag stays wherever
// it started.)
//
// Rationale: cc_toolchain features are the canonical Bazel-idiomatic
// home for these flags. Per-rule emission via copts forks the
// toolchain shape per target, which both (1) defeats the operator's
// `--features=<name>` opt-in (it's already in the rule) and (2)
// produces ~785 `raw-toolchain-feature-flag` audit findings across
// the 9-project cross-project survey (see PR #247). Detection and this
// rewrite share the `converter/internal/toolchainfeature` table:
// detection reads the broad Feature / LooksLikeFeatureFlag set, the
// rewrite the toolchain-backed RewriteFeature subset — so they stay
// aligned without the lift dropping a flag.
//
// Sources of these flags the converter previously routed to copts:
//
//   - Per-target probe-genex Properties (CXX/C_VISIBILITY_PRESET,
//     VISIBILITY_INLINES_HIDDEN, POSITION_INDEPENDENT_CODE) —
//     applyProbeGenexProperties already routes POSITION_INDEPENDENT_CODE
//     to the `pic` feature; the visibility presets reach copts the same
//     way, but this pass deliberately leaves them there (RewriteFeature
//     declines to lift them — see the header).
//   - cmake's global CMAKE_<LANG>_FLAGS / CMAKE_POSITION_INDEPENDENT_CODE
//     applied across every target — those land in
//     CompileGroup.compileCommandFragments and end up in copts via the
//     codemodel path, bypassing the per-target probe-genex.
//   - cmake's `add_compile_options(-fPIC)` / `link_libraries(-flto)` —
//     same shape, different cmake API.
//
// The lift is a routing change; the resulting Bazel build behaves
// identically when the toolchain declares the matching feature (the
// SANITIZER_FEATURES shape in
// examples/sanitizer-features/toolchain/features.bzl, or the
// converter's generated toolchain). RewriteFeature is what keeps
// "identically" honest — it only rewrites flags a toolchain actually
// backs, so an unknown-feature no-op (Bazel ignores unknown features
// in user-supplied lists) can never silently drop a flag's effect.
//
// Per-target dedup: Features may already carry an entry the lift
// would add (e.g. POSITION_INDEPENDENT_CODE-set targets already
// have "pic" added by applyProbeGenexProperties). The lift
// merges, dedupes, and sorts so the final list is byte-stable.
func liftRawFeatureFlags(pkg *ir.Package) {
	if pkg == nil {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		t.Copts = extractFeatures(t.Copts, &t.Features)
		t.LinkOpts = extractFeatures(t.LinkOpts, &t.Features)
		// Dedup + sort Features so multiple sources (probe-genex
		// + this lift + LTO from codemodel) compose into a
		// stable list.
		t.Features = sortedDedupFeatures(t.Features)
	}
}

// extractFeatures walks `flags`, returning the subset that ISN'T a
// known toolchain-feature flag while appending the matching feature
// name (one per flag) to *features. The order of non-matching
// entries in flags is preserved; copts ordering matters to compilers.
func extractFeatures(flags []string, features *[]string) []string {
	if len(flags) == 0 {
		return flags
	}
	kept := make([]string, 0, len(flags))
	for _, f := range flags {
		if feat := toolchainfeature.RewriteFeature(f); feat != "" {
			*features = append(*features, feat)
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == len(flags) {
		// No rewrites; return the original slice so the
		// per-target audit-finding case (a target that carries
		// no liftable flags) doesn't allocate.
		return flags
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// sortedDedupFeatures returns a stable, deduped copy of in. Empty
// strings drop out so the lift can pass through nil-safe.
func sortedDedupFeatures(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
