package genexeval

import "fmt"

// Range is a half-open byte range [Start, End) into a template,
// covering one top-level `$<...>` block including the `$<` and
// the matching `>`. Returned by TopLevelGenexes; consumed by
// ApplyValues and by the lifter's per-genex byte alignment in
// extractGenexValues.
type Range struct {
	Start, End int
}

// TopLevelGenexes returns the byte ranges of each top-level
// `$<...>` block in s, ordered by appearance. A "top-level"
// genex is one whose `$<` opener doesn't sit inside another
// `$<...>`; cmake's generator-expression grammar allows
// arbitrary nesting (e.g. `$<IF:$<CONFIG:Release>,a,b>`), and
// the lift's structured-base64 extractor only needs the outermost
// boundaries — the resolved bytes for a nested expression are
// already collapsed into the parent's resolved value at
// cmake-render time.
//
// Unbalanced `$<` (no matching `>`) is treated as literal text
// and skipped. The function is byte-faithful: it doesn't decode
// utf-8 or trim whitespace.
//
// Shared between the convert-time lifter (recovering per-genex
// rendered bytes from cmake's output) and the Bazel-time
// cmake-configure-file tool (replaying values dict against the
// rendered template). Single source of truth — diverging edge-
// case handling (e.g. how `$\<` escaping should work, if ever
// added) lands here, not in two places.
func TopLevelGenexes(s []byte) []Range {
	var ranges []Range
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '<' {
			start := i
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				switch {
				case j+1 < len(s) && s[j] == '$' && s[j+1] == '<':
					depth++
					j += 2
				case s[j] == '>':
					depth--
					j++
				default:
					j++
				}
			}
			if depth == 0 {
				ranges = append(ranges, Range{Start: start, End: j})
				i = j
				continue
			}
			// Unbalanced — treat as literal text from $ onward.
			i++
			continue
		}
		i++
	}
	return ranges
}

// ApplyValues replaces each top-level genex literal in template
// with its mapped resolved value. The replacement is literal —
// no syntax parsing, no recursive evaluation. The caller's
// invariant: values was produced by the lifter's
// extractGenexValues (or constructed to match its shape), so
// every top-level genex in template has a matching key.
//
// Returns an error if template contains a top-level genex not
// present in values (would land a literal `$<...>` in the
// output — wrong bytes a Bazel consumer would notice). Genex
// values containing further `$<...>` text are NOT recursively
// re-substituted; cmake fully evaluates at generate-time so the
// recovered values never carry literal genex syntax in practice.
//
// Single source of truth for the lift's (b)-shape: both the
// converter's lift-time consistency check and the
// cmake-configure-file tool's Bazel-time replay call this.
func ApplyValues(template []byte, values map[string]string) ([]byte, error) {
	ranges := TopLevelGenexes(template)
	if len(ranges) == 0 {
		return template, nil
	}
	out := make([]byte, 0, len(template))
	pos := 0
	for _, r := range ranges {
		out = append(out, template[pos:r.Start]...)
		literal := string(template[r.Start:r.End])
		val, ok := values[literal]
		if !ok {
			return nil, fmt.Errorf("no value for genex %q", literal)
		}
		out = append(out, val...)
		pos = r.End
	}
	out = append(out, template[pos:]...)
	return out, nil
}
