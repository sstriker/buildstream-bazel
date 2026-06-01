package lower

import (
	"bytes"
	"fmt"

	"github.com/sstriker/buildstream-bazel/internal/genexeval"
)

// extractGenexValues aligns a template against its cmake-
// rendered output to recover, per top-level genex, the bytes
// the cmake genex evaluator produced. The result maps each
// genex literal (e.g. `$<CONFIG:Release>`) to its rendered
// value (e.g. `1`).
//
// Algorithm: split the template at top-level genex boundaries,
// then walk template + rendered in lockstep:
//
//   - Static chunks (template text between/around genexes) must
//     match the corresponding prefix of remaining rendered bytes
//     verbatim.
//   - Each genex's resolved value = the rendered span between
//     the static chunk ending it and the static chunk starting
//     the next genex.
//
// This is the (b) "structured base64" lift's recovery primitive
// — sidesteps a genex evaluator entirely by trusting cmake's
// actual output and stitching it back together at Bazel time
// via literal substitution.
//
// Failure modes (all return a non-nil error and route the
// caller to the legacy bytes-embedded fallback):
//
//   - Template has no genex (extractor's contract requires at
//     least one).
//   - Static chunks don't align in rendered output (suggests
//     cmake applied a transform beyond genex evaluation —
//     `configure_file`-style substitution, an unknown encoding,
//     etc.).
//   - Same genex literal resolves to two different values in
//     the template (rare; would mean the genex is context-
//     dependent across positions, which v1's literal-replace
//     replay can't represent).
//   - Adjacent genexes with no static separator (the algorithm
//     needs the next static chunk as an anchor to know where
//     each genex's value ends).
//
// On any failure, the caller falls back to the legacy bytes-
// embedded shape — soundness is preserved at the cost of one
// more rendered-bytes-in-srckey entry.
func extractGenexValues(template, rendered []byte) (map[string]string, error) {
	ranges := genexeval.TopLevelGenexes(template)
	if len(ranges) == 0 {
		return nil, fmt.Errorf("template has no top-level genex")
	}
	values := map[string]string{}
	tplPos := 0
	renPos := 0
	for i, r := range ranges {
		// Static prefix between tplPos and the genex's `$<`.
		prefix := template[tplPos:r.Start]
		if !bytes.HasPrefix(rendered[renPos:], prefix) {
			return nil, fmt.Errorf("static chunk at template[%d:%d] (%q) does not match rendered[%d:] (%q...)",
				tplPos, r.Start, truncForErr(prefix), renPos, truncForErr(rendered[renPos:]))
		}
		renPos += len(prefix)

		literal := string(template[r.Start:r.End])

		// Determine the next anchor: the static text immediately
		// after this genex, up to the next genex (or template
		// end if this is the last).
		nextAnchorStart := len(template)
		if i+1 < len(ranges) {
			nextAnchorStart = ranges[i+1].Start
		}
		nextAnchor := template[r.End:nextAnchorStart]

		var valBytes []byte
		switch {
		case len(nextAnchor) == 0 && i+1 < len(ranges):
			// Adjacent genexes: can't disambiguate where the
			// first ends and the second begins by literal anchor.
			return nil, fmt.Errorf("genex %q is adjacent to the next genex with no static separator", literal)
		case len(nextAnchor) == 0:
			// Last genex, no trailing static: value extends to
			// the end of rendered.
			valBytes = rendered[renPos:]
			renPos = len(rendered)
		default:
			idx := bytes.Index(rendered[renPos:], nextAnchor)
			if idx < 0 {
				return nil, fmt.Errorf("post-genex anchor %q for %q does not appear in rendered[%d:]",
					truncForErr(nextAnchor), literal, renPos)
			}
			valBytes = rendered[renPos : renPos+idx]
			renPos += idx
		}

		if existing, ok := values[literal]; ok && existing != string(valBytes) {
			return nil, fmt.Errorf("genex %q resolves to two different values: %q and %q",
				literal, existing, valBytes)
		}
		values[literal] = string(valBytes)
		tplPos = r.End
	}

	// Tail: rendered bytes after the last genex's value must
	// equal the static chunk after the last genex (already
	// consumed as the last `nextAnchor` for non-trailing genexes,
	// but the last-genex case above sets renPos = len(rendered)
	// and leaves tplPos at the last genex's `>`; we still need
	// to verify the trailing static).
	tail := template[tplPos:]
	if !bytes.Equal(rendered[renPos:], tail) {
		return nil, fmt.Errorf("tail bytes after last genex don't match: rendered[%d:]=%q vs template[%d:]=%q",
			renPos, truncForErr(rendered[renPos:]), tplPos, truncForErr(tail))
	}

	return values, nil
}

// probeGenexValuesForBody resolves each top-level genex in a
// file(GENERATE) body individually via the two-pass literal probe,
// returning a literal→value map suitable for fileGenerateLiftedCmd's
// genexValues arg.
//
// This is tier (b′), between (b) extractGenexValues and the legacy
// bytes-embedded fallback. It fires only when (b) cannot recover the
// per-genex values by positional anchoring — adjacent genexes with
// no static separator, ambiguous static chunks. Where (b) trusts
// cmake's single rendered output and stitches it back together
// positionally, (b′) asks cmake to evaluate each genex literal in
// isolation (the warm second configure pass via cc.resolveLiteral),
// sidestepping the anchoring problem entirely. The acceptance metric
// is the shrinking UnsupportedError / legacy-fallback surface
// (ROADMAP.md Phase 3).
//
// Returns (nil, false) when:
//   - cc has no probe wiring (single-pass callers) or the body has no
//     top-level genex — nothing to do;
//   - pass 1 (sink recording): every literal is recorded as a probe
//     request and false is returned so the caller drops to legacy
//     this round; the orchestrator then runs the warm second pass and
//     re-lifts;
//   - any literal stays unresolved, or diverged per config, on pass 2
//     (the divergent case is a future select()-capable consumer — a
//     single literal-replace map can't represent it), or cmake's
//     resolved value itself still carries a genex (can't replay as a
//     literal substitution).
//
// On pass 1 it deliberately keeps recording the remaining literals
// after the first miss so the single warm second pass probes them all
// at once rather than one reconfigure per literal.
func probeGenexValuesForBody(cc *codegenContext, body []byte) (map[string]string, bool) {
	if cc == nil {
		return nil, false
	}
	ranges := genexeval.TopLevelGenexes(body)
	if len(ranges) == 0 {
		return nil, false
	}
	values := map[string]string{}
	// Dedupe by literal across BOTH passes. The values map only
	// populates on successful resolution (pass 2), so on pass 1 — where
	// resolveLiteral intentionally returns false — a values-keyed guard
	// wouldn't fire and repeated identical literals would re-call
	// Want() (harmless, the sink dedupes by hash, but wasteful locking).
	// A separate seen set attempts each distinct literal exactly once
	// regardless of pass.
	seen := map[string]bool{}
	allResolved := true
	for _, r := range ranges {
		literal := string(body[r.Start:r.End])
		if seen[literal] {
			continue
		}
		seen[literal] = true
		// file(GENERATE) genexes evaluate in project scope; any
		// target reference (e.g. $<TARGET_PROPERTY:app,P>) is
		// self-contained in the literal, which the probe hook
		// evaluates with full target knowledge.
		v, ok := cc.resolveLiteral(literal, "")
		if !ok {
			allResolved = false
			continue
		}
		if hasGenex([]byte(v)) {
			return nil, false
		}
		values[literal] = v
	}
	if !allResolved {
		return nil, false
	}
	return values, true
}

// truncForErr keeps error messages from dumping multi-KB
// templates wholesale. 40 bytes is enough to identify the
// offending span without overwhelming the diagnostic.
func truncForErr(b []byte) []byte {
	const cap = 40
	if len(b) <= cap {
		return b
	}
	return append(append([]byte{}, b[:cap]...), "..."...)
}
