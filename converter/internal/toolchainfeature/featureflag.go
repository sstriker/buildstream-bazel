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

import "strings"

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

// RewriteFeature returns the cc_toolchain feature name the lift may safely
// rewrite the given raw flag to, or "" to leave it as a raw copt/linkopt.
//
// It is a deliberate, explicit ALLOWLIST of the Feature() names a real
// cc_toolchain actually backs — NOT a denylist of the unbacked ones. The
// distinction is for forward-safety: a denylist lifts every *future* Feature()
// mapping by default, silently reintroducing the drop bug this guards against
// the moment someone maps a flag no toolchain implements. With an allowlist a
// new mapping stays a raw copt/linkopt until it's proven backed and added here.
//
// Backed set = the features the converter's generated toolchain emits
// (bazeltoolchain.featureSlots: asan, tsan, msan, ubsan, coverage, lto) that
// Feature() can actually name, plus `pic` — a built-in feature every
// cc_toolchain, including bazel's autodetected default, defines. Feature() has
// no mapping that yields `coverage`, so it never reaches this switch.
// Conversely the visibility presets and `-fsanitize=leak` are named by
// Feature() but not backed by the toolchains converted projects target by
// default — neither the generated one nor bazel's autodetected default — so
// they fall through to "" and stay raw. (No toolchain anywhere defines a
// visibility feature; the example sanitizer-features template DOES define an
// `lsan` feature, but it isn't a default target, so leak stays conservative
// here.) The planned toolkit-aware lift will replace this static allowlist
// with the toolkit's actual feature vocabulary.
func RewriteFeature(flag string) string {
	switch f := Feature(flag); f {
	case "pic", "lto", "asan", "tsan", "msan", "ubsan":
		return f
	default:
		return ""
	}
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
