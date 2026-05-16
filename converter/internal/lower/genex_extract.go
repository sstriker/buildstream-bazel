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
