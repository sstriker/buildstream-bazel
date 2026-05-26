package main

// Two-dial operator-facing mode flags. The dials collapse a forest of
// per-kind / per-binary flags into two enums an operator picks at the
// CLI; individual lower-level flags still work and override the
// derived defaults so existing scripts keep their semantics.
//
//   --fidelity   strict | best-effort        (default: strict)
//   --deployment auto   | local | production (default: auto)
//
// fidelity decides what happens when a per-kind native converter
// refuses to lower an element:
//
//   strict       refusal exits non-zero. The per-kind fallback flags
//                (--cmake-round2-fallback / --meson-round2-fallback /
//                --pyproject-fallback) default to off; explicit
//                opt-ins still work and override the default.
//   best-effort  refusal lands a placeholder shape + install_tree.tar
//                so downstream Bazel still resolves labels. The
//                per-kind fallback flags default to on when their
//                supporting tools are wired (publish + lookup +
//                tracer for cmake/meson; the converter for pyproject);
//                if the tools aren't wired, the fallback can't engage
//                and the banner surfaces a "best-effort downgrade"
//                note rather than erroring out.
//
// deployment decides whether the trace-driven kinds run in the
// monolithic round-1 shape (no REAPI AC rendezvous) or the split
// round-2 shape (publish/lookup via REAPI AC):
//
//   local        round-1 (today's --trace-round1). Project B's
//                install genrule is a single action; no AC needed.
//   production   round-2. Requires --trace-publish-bin and
//                --trace-lookup-bin to be on the command line. The
//                ONLY shape compatible with --platforms.
//   auto         production if publish + lookup binaries are wired,
//                else local. Default; matches the most common
//                "I gave write-a the tools I had" expectation.
//
// Explicit --trace-round1 sets deployment to local; passing both
// --trace-round1 and --deployment=production is rejected as a
// contradiction (rather than silently picking one).

import (
	"flag"
	"fmt"
)

const (
	fidelityStrict     = "strict"
	fidelityBestEffort = "best-effort"

	deploymentAuto       = "auto"
	deploymentLocal      = "local"
	deploymentProduction = "production"
)

// resolvedModes is the per-invocation outcome of mode derivation. It
// records both the operator's nominal request (the enum values
// printed in the startup banner) and the effective decisions write-a
// took for kinds whose tools weren't wired (the "downgrade" notes).
type resolvedModes struct {
	fidelity   string
	deployment string

	// downgrades is the list of human-readable lines printed in the
	// banner explaining where best-effort silently degraded to
	// strict (because tools weren't wired for that kind's fallback)
	// or where deployment=auto picked local (because publish/lookup
	// weren't wired). Empty when every requested mode could be
	// applied as-stated.
	downgrades []string
}

// modeFlags collects the raw values of every flag that participates
// in mode derivation. main() builds one of these from its flag
// pointers and hands it to deriveModes. Splitting parsing from
// derivation keeps the validation testable without a process
// boundary.
type modeFlags struct {
	fidelity   string
	deployment string

	// Tool-binary presence. Empty string ⇒ the corresponding
	// --bin flag wasn't set. Mode derivation reads these to decide
	// whether best-effort fallbacks can engage and whether
	// deployment=auto picks production.
	convertElementTrace string
	buildTracer         string
	tracePublish        string
	traceLookup         string
	convertElementMeson string
	pyprojectConverter  string

	// Per-kind fallback values as flag.Parse() saw them. These
	// carry the EXPLICIT setting only when explicit[<name>] is
	// true; otherwise they're zero-valued (the flag default) and
	// the derivation pass picks the value from fidelity.
	cmakeRound2Fallback   bool
	mesonRound2Fallback   bool
	pyprojectFallback     bool
	traceRound1           bool
	platformsJSON         string
	cmakeConfigureFileBin string

	// explicit is the set of flag names the operator passed on
	// the command line. Built from flag.Visit() after flag.Parse().
	explicit map[string]bool
}

// deriveModes validates the operator's mode choices and returns the
// effective fallback / deployment decisions. It does NOT mutate any
// package-level config — callers consume the returned struct and the
// resolved booleans alongside it to drive the rest of main().
//
// Validation surface (returns an error rather than fatal-ing so tests
// can assert against the message):
//
//   - --fidelity ∈ {strict, best-effort}.
//   - --deployment ∈ {auto, local, production}.
//   - --deployment=production with --trace-round1 set explicitly is
//     a contradiction.
//   - --deployment=production with no publish+lookup binaries and
//     trace-driven kinds active will be rejected by the existing
//     downstream checks in main(); this validator doesn't duplicate
//     them.
//   - --fidelity=strict with any of the per-kind fallback flags
//     explicitly set to true is allowed but surfaces in the banner
//     (operator wanted strict mode for most kinds but opted in to
//     one fallback explicitly — legitimate but worth showing).
func deriveModes(m modeFlags) (resolvedModes, bool, bool, bool, bool, error) {
	switch m.fidelity {
	case fidelityStrict, fidelityBestEffort:
	default:
		return resolvedModes{}, false, false, false, false,
			fmt.Errorf("--fidelity must be one of %q or %q (got %q)",
				fidelityStrict, fidelityBestEffort, m.fidelity)
	}
	switch m.deployment {
	case deploymentAuto, deploymentLocal, deploymentProduction:
	default:
		return resolvedModes{}, false, false, false, false,
			fmt.Errorf("--deployment must be one of %q, %q, or %q (got %q)",
				deploymentAuto, deploymentLocal, deploymentProduction, m.deployment)
	}
	if m.deployment == deploymentProduction && m.explicit["trace-round1"] && m.traceRound1 {
		return resolvedModes{}, false, false, false, false,
			fmt.Errorf("--deployment=production is incompatible with --trace-round1 (round-1 IS local deployment); set --deployment=local or drop --trace-round1")
	}

	res := resolvedModes{fidelity: m.fidelity, deployment: m.deployment}

	// Fidelity drives the per-kind fallback defaults. Each explicit
	// flag wins over the derived default. best-effort with no
	// supporting tools surfaces a banner note rather than erroring
	// out: the fallback simply can't engage for that kind, and the
	// kind keeps its strict refusal semantics.
	tracePipelineToolsReady := m.tracePublish != "" && m.traceLookup != "" && m.buildTracer != ""

	cmakeFB := m.cmakeRound2Fallback
	if !m.explicit["cmake-round2-fallback"] {
		cmakeFB = m.fidelity == fidelityBestEffort && tracePipelineToolsReady
		if m.fidelity == fidelityBestEffort && !tracePipelineToolsReady {
			res.downgrades = append(res.downgrades,
				"kind:cmake fallback unavailable (need --build-tracer-bin + --trace-publish-bin + --trace-lookup-bin); refusals will exit non-zero")
		}
	}
	mesonFB := m.mesonRound2Fallback
	if !m.explicit["meson-round2-fallback"] {
		mesonFB = m.fidelity == fidelityBestEffort && tracePipelineToolsReady && m.convertElementMeson != ""
		if m.fidelity == fidelityBestEffort && m.convertElementMeson != "" && !tracePipelineToolsReady {
			res.downgrades = append(res.downgrades,
				"kind:meson fallback unavailable (need --build-tracer-bin + --trace-publish-bin + --trace-lookup-bin); refusals will exit non-zero")
		}
	}
	pyprojectFB := m.pyprojectFallback
	if !m.explicit["pyproject-fallback"] {
		pyprojectFB = m.fidelity == fidelityBestEffort && m.pyprojectConverter != ""
	}

	// Deployment drives traceRound1. local ⇒ round-1; production ⇒
	// round-2; auto ⇒ round-2 if publish+lookup, else round-1.
	traceR1 := m.traceRound1
	switch m.deployment {
	case deploymentLocal:
		traceR1 = true
	case deploymentProduction:
		traceR1 = false
	case deploymentAuto:
		if m.explicit["trace-round1"] {
			// Operator explicitly chose round-1 inside auto.
			// Honor it; banner records the resolved deployment as
			// local for visibility.
			traceR1 = m.traceRound1
			if traceR1 {
				res.deployment = deploymentLocal
			} else {
				res.deployment = deploymentProduction
			}
		} else {
			if m.tracePublish != "" && m.traceLookup != "" {
				traceR1 = false
				res.deployment = deploymentProduction
			} else {
				traceR1 = true
				res.deployment = deploymentLocal
				if m.convertElementTrace != "" || m.cmakeRound2Fallback ||
					m.mesonRound2Fallback || m.platformsJSON != "" {
					res.downgrades = append(res.downgrades,
						"--deployment=auto picked local (round-1) because --trace-publish-bin / --trace-lookup-bin weren't set; --platforms requires production")
				}
			}
		}
	}

	return res, cmakeFB, mesonFB, pyprojectFB, traceR1, nil
}

// flagExplicit returns the set of flag names that were set on the
// command line (vs. left at their default value). Wrapper around
// flag.Visit so callers don't have to plumb the callback shape.
//
// Tests call deriveModes directly with a hand-built explicit map; the
// CLI entry point uses this helper after flag.Parse() returns.
func flagExplicit() map[string]bool {
	out := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { out[f.Name] = true })
	return out
}
