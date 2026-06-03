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

// backedFeatures is the set of feature NAMES the lift may safely rewrite a
// raw flag to: the generated cc_toolchain's declared vocabulary
// (toolchain.GeneratedFeatures) plus "pic" — a built-in feature every
// cc_toolchain, including bazel's autodetected default, defines.
//
// Sourcing it from toolchain.GeneratedFeatures (the same set bazeltoolchain
// emits feature() blocks for) rather than a hand-kept list is what makes the
// lift toolkit-aware for the generated toolkit: add a feature to the
// generated toolchain and the lift rewrites its flag automatically — the two
// sides can't drift. Names Feature() yields that AREN'T backed (the
// visibility presets, -fsanitize=leak) fall through to "" and stay raw, so
// the lift can never drop a flag onto a no-op feature.
var backedFeatures = func() map[string]bool {
	// pic is a built-in cc feature, not a generated feature() block.
	m := map[string]bool{"pic": true}
	for _, f := range toolchain.GeneratedFeatures() {
		m[string(f)] = true
	}
	return m
}()

// RewriteFeature returns the cc_toolchain feature name the lift may safely
// rewrite the given raw flag to, or "" to leave it as a raw copt/linkopt.
// It gates Feature() through backedFeatures so the lift only targets
// features the generated toolchain actually implements.
//
// IMPORTANT — this gates on the vocabulary of the cc_toolchain the converter
// SHIPS ALONGSIDE these BUILD files (toolchain + lifted features are a coupled
// matched set). It is NOT a universal truth about whatever toolchain
// ultimately resolves: an operator can hand-edit, extend, or swap the Bazel
// toolchain after conversion, and that's invisible here. The failure modes:
//   - operator keeps the generated toolchain → correct.
//   - operator ADDS features → the lift was conservative (flag stayed a raw
//     copt), so it still compiles, just unlifted. Safe.
//   - operator REMOVES/renames a targeted feature, or swaps in a toolchain
//     that doesn't back it → features=[…] is a no-op Bazel silently ignores
//     and the flag is dropped. The operator owns that divergence.
//
// Likewise the resolved toolchain is a build-time (--platforms) choice, so a
// per-kit lift would gate on the INTERSECTION of every targetable kit's
// vocabulary. Letting the operator pass in their real toolchain to drive this
// gate is a planned follow-up (separate PR); see ROADMAP.
func RewriteFeature(flag string) string {
	if f := Feature(flag); backedFeatures[f] {
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
