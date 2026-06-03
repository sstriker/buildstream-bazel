package main

// Operator-facing mode dials. The dials collapse the forest of
// per-kind / per-binary converter flags into a small enum surface
// the operator picks at the CLI; write-a threads the resolved value
// verbatim into each converter's genrule cmd, and each converter
// decides what the dial means for its own refusal / baking /
// diagnostic shapes. The pass-through architecture lets per-kind
// converter behavior evolve without write-a touching the dial-
// derivation code.
//
//   --fidelity    strict | best-effort        (default: strict)
//   --bake-in     warn   | allow | reject     (default: warn)
//   --diagnostics                              (bool, default off)
//   --deployment  auto   | local | production (default: auto)
//
// fidelity decides what happens when a per-kind native converter
// refuses to lower an element. Threaded verbatim into every
// converter; each interprets "best-effort" against its own
// fallback shapes (cmake → execute_process placeholder; meson →
// install-plan-derived stubs; pyproject → write-a's
// pipeline-shape dispatch around the converter; trace-driven kinds
// have no native refusal shape so the dial is a no-op there).
//
// bake-in decides how the converter responds to convert-time-baked
// outputs (configure_file base64 captures, execute_process value
// hoists, cmake -P script bakes, ...). Threaded verbatim; only the
// cmake converter has bake sites worth gating today. Orthogonal to
// fidelity.
//
// diagnostics turns "first Tier-1 refusal aborts" into "collect
// every refusal, continue past each, write a structured report".
// Threaded verbatim; today only the cmake converter has the
// rejection-collection machinery wired (the flag is a no-op pass-
// through on meson / pyproject for CLI uniformity).
//
// deployment is the only dial that's NOT pass-through: it controls
// write-a's workspace-rendering decisions (round-1 vs round-2 trace
// shape, Project A converter-genrule fan-out for --platforms-json,
// Project B install genrule emission gated on best-effort kinds).
// It stays write-a-local.

import (
	"flag"
	"fmt"

	"github.com/sstriker/buildstream-bazel/internal/convmode"
)

const (
	// Re-export the shared converter enum values as untyped string
	// constants so write-a's CLI registers them via flag.String
	// without bringing the typed enums into the public flag surface
	// (CLI strings are easier to grep for; type conversion happens
	// in deriveModes where convmode.Parse* validates).
	fidelityStrict     = string(convmode.FidelityStrict)
	fidelityBestEffort = string(convmode.FidelityBestEffort)

	bakeInWarn   = string(convmode.BakeInWarn)
	bakeInAllow  = string(convmode.BakeInAllow)
	bakeInReject = string(convmode.BakeInReject)

	deploymentAuto       = "auto"
	deploymentLocal      = "local"
	deploymentProduction = "production"
)

// resolvedModes is the per-invocation outcome of mode derivation,
// consumed by main() to thread dial values into converter genrule
// cmds and to drive write-a's own workspace-rendering decisions.
type resolvedModes struct {
	fidelity    string
	bakeIn      string
	diagnostics bool
	deployment  string

	// traceRound1 is the effective value for the --trace-round1
	// flag derived from --deployment. main() writes it onto *round1
	// so the existing downstream guard at the install-genrule emit
	// site sees the mode-derived default.
	traceRound1 bool

	// downgrades is the list of human-readable lines printed in the
	// banner explaining where best-effort silently degraded to
	// strict (because tools weren't wired for that kind's fallback)
	// or where deployment=auto picked local (because publish/lookup
	// weren't wired). Empty when every requested mode could be
	// applied as-stated.
	downgrades []string
}

// modeFlags collects the raw values of every flag that participates
// in mode derivation OR in the startup banner's tools-list display.
// Splitting parsing from derivation keeps the validation testable
// without a process boundary.
type modeFlags struct {
	fidelity    string
	bakeIn      string
	diagnostics bool
	deployment  string

	// Tool-binary presence. Empty string ⇒ the corresponding
	// --*-bin flag wasn't set. Used to decide whether
	// --deployment=auto picks production, and to populate the
	// banner's wired / not-provided tools list.
	convertElementTrace string
	buildTracer         string
	tracePublish        string
	traceLookup         string
	convertElementMeson string
	pyprojectConverter  string

	// Per-kind / multi-platform escape hatches that operators may
	// still pass at the low level. Tracked here so the banner can
	// show them and the auto-deployment downgrade note can mention
	// when they imply --deployment=production but auto picked
	// local.
	cmakeRound2Fallback   bool
	mesonRound2Fallback   bool
	pyprojectFallback     bool
	traceRound1           bool
	platformsJSON         string
	cmakeConfigureFileBin string
	ccEmbedBin            string

	// useFuseSources is the experimental fuse-sources opt-in. It's
	// not a dial input per se, but the FUSE template for kind:cmake
	// doesn't thread --unsupported-execute-process-fallback through
	// (see handler_cmake's rejection on the explicit-fallback
	// path), so deriveModes uses it to avoid surfacing a best-
	// effort-engaged claim that wouldn't actually fire.
	useFuseSources bool

	// explicit is the set of flag names the operator passed on
	// the command line. Built from flag.Visit() after flag.Parse().
	explicit map[string]bool
}

// deriveModes validates the operator's dial choices and returns the
// effective decisions. It does NOT mutate any package-level config —
// callers consume resolvedModes and write its derived bools onto the
// corresponding *flag.Bool pointers to drive the rest of main().
//
// Pass-through philosophy: write-a doesn't second-guess what
// fidelity / bake-in / diagnostics MEAN per-kind. It just validates
// the enum, derives the deployment shape, and surfaces downgrade
// notes when a setting can't be honored (e.g. auto deployment with
// publish/lookup absent). Converter-side interpretation lives in
// each converter's own --fidelity / --bake-in / --diagnostics
// validation, which write-a's handler_*.go threads verbatim into
// the genrule cmd.
//
// Validation surface (returns an error rather than fatal-ing so
// tests can assert against the message):
//
//   - --fidelity ∈ {strict, best-effort}.
//   - --bake-in ∈ {warn, allow, reject}.
//   - --deployment ∈ {auto, local, production}.
//   - --deployment=production with --trace-round1=true is a
//     contradiction (round-1 IS local deployment).
//   - --deployment=local with --trace-round1=false is the symmetric
//     contradiction.
//   - --deployment=production with no publish+lookup binaries will
//     be rejected by the existing downstream checks in main() when
//     trace-driven kinds are active; this validator doesn't
//     duplicate them.
func deriveModes(m modeFlags) (resolvedModes, error) {
	fidelity, err := convmode.ParseFidelity(m.fidelity)
	if err != nil {
		return resolvedModes{}, err
	}
	bakeIn, err := convmode.ParseBakeIn(m.bakeIn)
	if err != nil {
		return resolvedModes{}, err
	}
	switch m.deployment {
	case deploymentAuto, deploymentLocal, deploymentProduction:
	default:
		return resolvedModes{}, fmt.Errorf("--deployment must be one of %q, %q, or %q (got %q)",
			deploymentAuto, deploymentLocal, deploymentProduction, m.deployment)
	}
	if m.deployment == deploymentProduction && m.explicit["trace-round1"] && m.traceRound1 {
		return resolvedModes{}, fmt.Errorf("--deployment=production is incompatible with --trace-round1=true (round-1 IS local deployment); set --deployment=local or drop --trace-round1")
	}
	if m.deployment == deploymentLocal && m.explicit["trace-round1"] && !m.traceRound1 {
		return resolvedModes{}, fmt.Errorf("--deployment=local is incompatible with --trace-round1=false (local deployment IS round-1); set --deployment=production or drop --trace-round1")
	}

	res := resolvedModes{
		fidelity:    string(fidelity),
		bakeIn:      string(bakeIn),
		diagnostics: m.diagnostics,
		deployment:  m.deployment,
	}

	// Deployment drives traceRound1. local ⇒ round-1; production ⇒
	// round-2; auto ⇒ round-2 if publish+lookup, else round-1.
	// Explicit --trace-round1 under auto picks the deployment label
	// from the value: true ⇒ local, false ⇒ production. (For
	// non-auto deployments, the symmetric contradiction checks
	// above already rejected mismatched explicit values.)
	res.traceRound1 = m.traceRound1
	switch m.deployment {
	case deploymentLocal:
		res.traceRound1 = true
	case deploymentProduction:
		res.traceRound1 = false
	case deploymentAuto:
		switch {
		case m.explicit["trace-round1"]:
			res.traceRound1 = m.traceRound1
			if res.traceRound1 {
				res.deployment = deploymentLocal
			} else {
				res.deployment = deploymentProduction
			}
		case m.tracePublish != "" && m.traceLookup != "":
			res.traceRound1 = false
			res.deployment = deploymentProduction
		default:
			res.traceRound1 = true
			res.deployment = deploymentLocal
			if m.convertElementTrace != "" || m.cmakeRound2Fallback || m.mesonRound2Fallback || m.platformsJSON != "" || fidelity == convmode.FidelityBestEffort {
				res.downgrades = append(res.downgrades,
					"--deployment=auto picked local (round-1) because --trace-publish-bin / --trace-lookup-bin weren't set; --platforms and the round-2 fallback shapes for kind:cmake / -meson require production")
			}
		}
	}

	// --bake-in=reject under --use-fuse-sources for kind:cmake
	// can't be threaded (the FUSE template doesn't pass --bake-in
	// today; see handler_cmake's bakeInFlag rendering). Surface a
	// note so the operator sees the gap.
	if bakeIn == convmode.BakeInReject && m.useFuseSources {
		res.downgrades = append(res.downgrades,
			"--bake-in=reject is not threaded into the kind:cmake FUSE template today; cmake-converter invocations under --use-fuse-sources will not enforce the reject policy")
	}

	return res, nil
}

// flagExplicit returns the set of flag names that were set on the
// command line (vs. left at their default value). Wrapper around
// flag.Visit so callers don't have to plumb the callback shape.
//
// Tests call deriveModes directly with a hand-built explicit map;
// the CLI entry point uses this helper after flag.Parse() returns.
func flagExplicit() map[string]bool {
	out := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { out[f.Name] = true })
	return out
}
