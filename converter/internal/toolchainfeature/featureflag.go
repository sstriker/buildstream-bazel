// Package toolchainfeature is the shared mapping from raw compile/link
// flags to cc_toolchain feature names. The same table drives:
//
//   - bazelidiom.Audit's `raw-toolchain-feature-flag` finding (detection
//     side: "this flag is in copts/linkopts but a feature would be the
//     Bazel-idiomatic home for it").
//
//   - lower.liftRawFeatureFlags (rewrite side: walks each target's
//     Copts/Linkopts, removes matching flags, and adds the feature
//     name to ir.Target.Features so the cc_toolchain owns the flag
//     set uniformly). The rewrite gates through RewriteFeature — a
//     subset that only rewrites flags a cc_toolchain actually backs —
//     so the lift can never silently drop a flag onto a feature no
//     toolchain implements. The detection side keeps the broader set.
//
// Keeping one source of truth means adding a new flag → feature
// mapping is a one-line change visible to both consumers
// simultaneously.
package toolchainfeature

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

// Feature returns the cc_toolchain feature name that owns the
// given raw compile/link flag, or "" when the flag has no
// first-class feature equivalent. The mapping mirrors the
// SANITIZER_FEATURES convention in
// examples/sanitizer-features/toolchain/features.bzl plus the
// position-independent-code / visibility / link-time-optimization
// features the converter has been emitting as per-rule copts.
func Feature(flag string) string {
	switch flag {
	case "-fPIC", "-fpic":
		return "pic"
	case "-flto":
		return "lto"
	case "-fvisibility=hidden":
		return "visibility_hidden"
	case "-fvisibility-inlines-hidden":
		return "visibility_inlines_hidden"
	case "-fsanitize=address":
		return "asan"
	case "-fsanitize=thread":
		return "tsan"
	case "-fsanitize=memory":
		return "msan"
	case "-fsanitize=undefined":
		return "ubsan"
	case "-fsanitize=leak":
		return "lsan"
	}
	return ""
}

// BackedFeatures builds a lift gate set from a toolchain's declared feature
// vocabulary: the given feature names plus the built-in `pic` (which every
// cc_toolchain, including bazel's autodetected default, defines — so it's
// always liftable even when a toolchain's feature() list doesn't name it).
//
// Operators get their toolchain's vocabulary from toolchainscan.ParseDeclared
// (parsing their cc_toolchain_config.bzl) and pass it here; the converter's own
// generated vocabulary feeds defaultBackedFeatures below.
func BackedFeatures(featureNames []string) map[string]bool {
	m := map[string]bool{"pic": true}
	for _, n := range featureNames {
		m[n] = true
	}
	return m
}

// defaultBackedFeatures gates the lift when the operator doesn't point it at
// their own toolchain: the converter's generated cc_toolchain vocabulary
// (toolchain.GeneratedFeatures — the same set bazeltoolchain emits feature()
// blocks for, so the lift and the emitted toolchain can't drift) plus pic.
var defaultBackedFeatures = BackedFeatures(generatedFeatureNames())

func generatedFeatureNames() []string {
	g := toolchain.GeneratedFeatures()
	out := make([]string, len(g))
	for i, f := range g {
		out[i] = string(f)
	}
	return out
}

// RewriteFeature returns the feature name the lift may safely rewrite the given
// raw flag to under the converter's GENERATED toolchain vocabulary, or "" to
// leave it a raw copt/linkopt. Equivalent to RewriteFeatureWith(flag, nil).
func RewriteFeature(flag string) string {
	return RewriteFeatureWith(flag, nil)
}

// RewriteFeatureWith is RewriteFeature gated on a caller-supplied backed-feature
// set (build it with BackedFeatures) — typically the operator's REAL toolchain
// vocabulary, enumerated from their Starlark by toolchainscan. A nil set falls
// back to defaultBackedFeatures (the generated toolchain).
//
// Why a gate at all: a target's `features` attr is static, and Bazel silently
// ignores a feature its resolved cc_toolchain doesn't define — so rewriting a
// flag onto an unbacked feature drops it. Gating on the actual toolchain's
// vocabulary keeps the rewrite faithful. (Across multiple targetable
// toolchains the safe set is their INTERSECTION, since any of them may resolve;
// the caller is responsible for intersecting before calling.)
func RewriteFeatureWith(flag string, backed map[string]bool) string {
	if backed == nil {
		backed = defaultBackedFeatures
	}
	if f := Feature(flag); backed[f] {
		return f
	}
	return ""
}

// IsFeatureFlag reports whether the flag has a registered feature
// equivalent. Equivalent to `Feature(flag) != ""` but kept as a
// separate name so call sites read clearly.
func IsFeatureFlag(flag string) bool {
	return Feature(flag) != ""
}

// LooksLikeFeatureFlag is the audit-side predicate — the same as
// IsFeatureFlag today, but kept separate so a future audit-only
// pattern (e.g. a `-fsanitize=*` prefix match for forward-compat
// detection without committing to a specific feature name) can
// diverge without disturbing the rewrite-side path.
func LooksLikeFeatureFlag(flag string) bool {
	if IsFeatureFlag(flag) {
		return true
	}
	// Forward-compat: any `-fsanitize=<x>` shape is plausibly a
	// future feature even when we don't have an explicit mapping
	// (e.g. `-fsanitize=cfi`, `-fsanitize=safe-stack`). The audit
	// surfaces these as "could be a feature"; the rewrite side
	// declines until a name is registered (Feature returns "").
	return strings.HasPrefix(flag, "-fsanitize=")
}
