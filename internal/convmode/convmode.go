// Package convmode declares the operator-facing dial enums shared by
// write-a and the per-kind converters.
//
// The intent: write-a's CLI takes the operator's dial choice
// (--fidelity / --bake-in / --diagnostics) and threads it VERBATIM
// into each converter's genrule cmd. Each converter accepts the same
// enum and decides what it means internally — what counts as a
// "best-effort fallback" for kind:cmake (execute_process placeholder)
// differs from kind:meson (install-plan-derived stubs) and kind:
// pyproject (probe-and-degrade-to-pipeline), but the operator-facing
// vocabulary is uniform.
//
// Keeping the enum string surface in one place lets future per-
// converter behavior changes ship without write-a touching the
// dial-derivation code. write-a's job becomes "validate the operator's
// choice; pass it through; render workspace-level shapes that depend
// on the dial value (e.g. Project B's install genrule emission for
// best-effort kind:cmake)".
//
// --deployment doesn't ship here because it controls write-a's
// workspace-rendering decisions (round-1 vs round-2 trace shape, the
// Project A converter genrule fan-out for --platforms-json), not
// any per-converter behavior. It stays write-a-local.
package convmode

import (
	"fmt"
	"strings"
)

// Fidelity decides what happens when a per-kind native converter
// refuses to lower an element:
//
//	Strict       refusal exits non-zero
//	BestEffort   refusal lowers to a placeholder shape so downstream
//	             Bazel still resolves labels (per-kind interpretation:
//	             cmake → cc_import / sh_binary stubs + install_tree
//	             .tar; meson → install-plan-derived stubs + install
//	             _tree.tar; pyproject → pipeline-shape coarse install)
type Fidelity string

const (
	FidelityStrict     Fidelity = "strict"
	FidelityBestEffort Fidelity = "best-effort"
)

// ParseFidelity validates a CLI string. Empty resolves to Strict so
// callers that don't pass --fidelity get today's behavior.
func ParseFidelity(s string) (Fidelity, error) {
	switch s {
	case "", string(FidelityStrict):
		return FidelityStrict, nil
	case string(FidelityBestEffort):
		return FidelityBestEffort, nil
	default:
		return FidelityStrict, fmt.Errorf("--fidelity must be one of %q or %q (got %q)",
			FidelityStrict, FidelityBestEffort, s)
	}
}

// BakeIn decides how the converter responds to convert-time-baked
// outputs (configure_file base64 captures, execute_process value
// hoists, cmake -P script bakes, ...):
//
//	Warn    emit the per-rule inventory on stderr; succeed
//	Allow   silent
//	Reject  exit non-zero with the inventory embedded
//
// Only the cmake converter has bake sites today; meson and pyproject
// honor the flag for CLI uniformity but resolve to a no-op.
type BakeIn string

const (
	BakeInWarn   BakeIn = "warn"
	BakeInAllow  BakeIn = "allow"
	BakeInReject BakeIn = "reject"
)

// ParseBakeIn validates a CLI string. Empty resolves to Warn so a
// caller leaving the flag default sees today's behavior.
func ParseBakeIn(s string) (BakeIn, error) {
	switch s {
	case "", string(BakeInWarn):
		return BakeInWarn, nil
	case string(BakeInAllow):
		return BakeInAllow, nil
	case string(BakeInReject):
		return BakeInReject, nil
	default:
		return BakeInWarn, fmt.Errorf("--bake-in must be one of %q, %q, or %q (got %q)",
			BakeInWarn, BakeInAllow, BakeInReject, s)
	}
}

// PerConfigBake decides whether the multi-config converter runs the
// per-build-type configure_file bake passes:
//
//	Auto   run them only when the trace shows project files reading
//	       CMAKE_BUILD_TYPE (zero overhead otherwise)
//	On     always run them
//	Off    never run them
type PerConfigBake string

const (
	PerConfigBakeAuto PerConfigBake = "auto"
	PerConfigBakeOn   PerConfigBake = "on"
	PerConfigBakeOff  PerConfigBake = "off"
)

// ParsePerConfigBake validates the --per-config-bake CLI string and
// canonicalizes its aliases. Empty resolves to Auto (the flag default).
// Accepts the forgiving aliases the consumer historically honored —
// on/force, off/false/0/no — and REJECTS anything else (so a typo fails
// loudly rather than silently falling through to auto). Case-insensitive.
func ParsePerConfigBake(s string) (PerConfigBake, error) {
	switch strings.ToLower(s) {
	case "", string(PerConfigBakeAuto):
		return PerConfigBakeAuto, nil
	case string(PerConfigBakeOn), "force":
		return PerConfigBakeOn, nil
	case string(PerConfigBakeOff), "false", "0", "no":
		return PerConfigBakeOff, nil
	default:
		return PerConfigBakeAuto, fmt.Errorf("--per-config-bake must be one of %q, %q, or %q (got %q)",
			PerConfigBakeAuto, PerConfigBakeOn, PerConfigBakeOff, s)
	}
}
