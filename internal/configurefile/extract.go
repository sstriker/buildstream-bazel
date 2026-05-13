package configurefile

import (
	"bytes"
	"fmt"
	"strings"
)

// Extract reverse-engineers the values dict that cmake used at
// configure time, given the template (.h.in) and the rendered
// output cmake produced. The lift in convert-element-cmake calls this
// to capture values without re-running cmake or parsing
// CMakeCache.txt — cmake already did the substitution; we just
// recover the inputs from the inputs+output pair.
//
// Verification: after extraction Substitute(template, values, opts)
// is run and the result compared byte-for-byte against the
// rendered input. Mismatches return an error so callers can
// fall back to the legacy base64-embedding shape rather than
// emit a genrule that produces a different config.h than the
// project expects. The verify-pass is also the soundness gate
// for Options the extractor doesn't model line-shape-explicitly
// (e.g. EscapeQuotes' literal-quote-passthrough behavior, or
// NewlineStyle's terminator choice): if the recovered values
// don't reproduce the rendered output through Substitute, we
// know the option's effect can't be cleanly recovered and
// the caller should fall back to legacy.
//
// Supported template shapes (cmake's documented configure_file
// directives):
//
//   - @VAR@ markers on plain lines.
//   - ${VAR} markers on plain lines (unless opts.AtOnly).
//   - #cmakedefine FOO              → recovers FOO truthiness.
//   - #cmakedefine FOO <content>    → recovers FOO truthiness +
//     any @VAR@ in content.
//   - #cmakedefine01 FOO            → recovers FOO 0/1.
//   - CopyOnly: trivial — values dict is empty, verify-pass
//     ensures rendered matches the template (modulo the
//     newline rewrite COPYONLY may apply).
//   - EscapeQuotes: when extracting a marker's value out of the
//     rendered span, undo `\"` and `\\` escapes (mirrors the
//     escape Substitute would re-apply on the verify pass).
//   - NewlineStyle: line splitting honors the configured style
//     so CRLF rendered output aligns 1:1 with LF templates.
func Extract(template, rendered []byte, opts Options) (map[string]string, error) {
	values := map[string]string{}
	tplLines := splitLines(template, NewlineLF)
	outLines := splitLines(rendered, lineTerminatorFor(opts.NewlineStyle, rendered))
	if len(tplLines) != len(outLines) {
		return nil, fmt.Errorf("template/rendered line count differs (%d vs %d); can't align", len(tplLines), len(outLines))
	}
	if opts.CopyOnly {
		// COPYONLY: no values to extract. The verify-pass
		// below catches a template/rendered mismatch
		// (e.g. NewlineStyle drift).
		verify := Substitute(template, values, opts)
		if !bytes.Equal(verify, rendered) {
			return nil, fmt.Errorf("COPYONLY template differs from rendered output; can't recover")
		}
		return values, nil
	}
	for i := range tplLines {
		// Strip trailing \r so CRLF templates don't carry the
		// \r through to the per-line extractor (which expects
		// LF-terminated content).
		tpl := strings.TrimSuffix(tplLines[i], "\r")
		out := strings.TrimSuffix(outLines[i], "\r")
		if err := extractLine(tpl, out, values, opts); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	verify := Substitute(template, values, opts)
	if !bytes.Equal(verify, rendered) {
		return nil, fmt.Errorf("extracted values don't reproduce the rendered output; ambiguous template or unmodeled option")
	}
	return values, nil
}

// splitLines splits b on the given line terminator, preserving
// the empty trailing element when b ends with a terminator (so
// '\n'-terminated and unterminated templates have different
// line counts). Mirrors what Substitute's line walker does.
func splitLines(b []byte, term NewlineStyle) []string {
	if len(b) == 0 {
		return nil
	}
	switch term {
	case NewlineCRLF:
		return strings.Split(string(b), "\r\n")
	default:
		return strings.Split(string(b), "\n")
	}
}

// lineTerminatorFor picks the NewlineStyle to use for splitting
// a buffer: explicit if opts forces it, else autodetected from
// the bytes (CRLF if the first '\n' is preceded by '\r', LF
// otherwise — same heuristic Substitute uses).
func lineTerminatorFor(style NewlineStyle, b []byte) NewlineStyle {
	if style != NewlineDefault {
		return style
	}
	if idx := bytes.IndexByte(b, '\n'); idx > 0 && b[idx-1] == '\r' {
		return NewlineCRLF
	}
	return NewlineLF
}

// extractLine populates values from a single template/rendered
// line pair. Returns nil on success (including when no markers
// were present), an error when the line shape is recognized
// but alignment fails.
func extractLine(tpl, out string, values map[string]string, opts Options) error {
	// #cmakedefine01 FOO → "#define FOO 0|1"
	if name, ok := matchCmakedefine01(tpl); ok {
		return extract01(name, tpl, out, values)
	}
	// #cmakedefine FOO [<content>] → "#define FOO [<expanded>]" or "/* #undef FOO */"
	if name, content, ok := matchCmakedefine(tpl); ok {
		return extractDefine(name, content, tpl, out, values, opts)
	}
	// Plain line: align literal pieces around @VAR@ / ${VAR}.
	return extractPlain(tpl, out, values, opts)
}

// matchCmakedefine01 returns (name, true) if line is
// `<leading>#cmakedefine01 NAME`. Trailing whitespace tolerated.
func matchCmakedefine01(line string) (string, bool) {
	body, ok := matchDirective(line, "#cmakedefine01")
	if !ok {
		return "", false
	}
	name := strings.TrimSpace(body)
	if name == "" {
		return "", false
	}
	return name, true
}

// matchCmakedefine returns (name, content, true) for a
// `<leading>#cmakedefine NAME [content...]` line. content is
// "" when only NAME is present.
func matchCmakedefine(line string) (string, string, bool) {
	body, ok := matchDirective(line, "#cmakedefine")
	if !ok {
		return "", "", false
	}
	body = strings.TrimLeft(body, " \t")
	if body == "" {
		return "", "", false
	}
	name, rest := splitFirstField(body)
	if name == "" {
		return "", "", false
	}
	return name, rest, true
}

func extract01(name, tpl, out string, values map[string]string) error {
	leading := cmakeDefineLeading(tpl)
	defineLine := leading + "#define " + name + " "
	if !strings.HasPrefix(out, defineLine) {
		return fmt.Errorf("#cmakedefine01 %s: rendered line %q doesn't have the expected `#define %s X` shape", name, out, name)
	}
	digit := strings.TrimSpace(out[len(defineLine):])
	switch digit {
	case "0", "1":
		return setValue(values, name, digit)
	}
	return fmt.Errorf("#cmakedefine01 %s: trailing token %q not 0/1", name, digit)
}

func extractDefine(name, content, tpl, out string, values map[string]string, opts Options) error {
	leading := cmakeDefineLeading(tpl)
	undefLine := leading + "/* #undef " + name + " */"
	if out == undefLine {
		return setValue(values, name, "")
	}
	defineHead := leading + "#define " + name
	if !strings.HasPrefix(out, defineHead) {
		return fmt.Errorf("#cmakedefine %s: rendered line %q matches neither `#define %s ...` nor `/* #undef %s */`", name, out, name, name)
	}
	// Truthy. Mark with a stable truthy sentinel ("1") unless
	// the variable was already extracted with a different
	// value.
	if err := setValue(values, name, "1"); err != nil {
		return err
	}
	if content == "" {
		// `#cmakedefine FOO` → rendered is exactly defineHead.
		if out != defineHead {
			return fmt.Errorf("#cmakedefine %s: expected rendered %q, got %q", name, defineHead, out)
		}
		return nil
	}
	// `#cmakedefine FOO <content>` → rendered is
	// `<leading>#define FOO <expanded content>`. The expanded
	// content occupies out[len(defineHead+" "):]. Recurse into
	// extractPlain on (content, expanded) to harvest any @VAR@s.
	expectedPrefix := defineHead + " "
	if !strings.HasPrefix(out, expectedPrefix) {
		return fmt.Errorf("#cmakedefine %s <content>: rendered missing the space after %q", name, defineHead)
	}
	expanded := out[len(expectedPrefix):]
	return extractPlain(content, expanded, values, opts)
}

// extractPlain harvests @VAR@ / ${VAR} values from a plain
// line by walking literal pieces of the template against the
// rendered line. The literal pieces between markers must
// appear in the rendered line in the same order; each marker's
// value is the bytes spanning between adjacent literal anchors.
func extractPlain(tpl, out string, values map[string]string, opts Options) error {
	pieces := splitTemplatePieces(tpl, opts)
	if len(pieces) == 0 {
		// Pure literal; rendered must equal template.
		if tpl != out {
			return fmt.Errorf("literal line drift: template %q vs rendered %q", tpl, out)
		}
		return nil
	}
	// Reject adjacent markers without a literal anchor between
	// them: alignment is ambiguous (which marker owns which
	// bytes is unrecoverable from a single rendered output —
	// the round-trip test would pass for the recorded
	// rendered but mis-allocate when a future CMakeLists.txt
	// edit changes either marker's value). Caller falls back
	// to the legacy base64-cmd shape.
	for i := 1; i < len(pieces); i++ {
		if pieces[i].kind == pieceMarker && pieces[i-1].kind == pieceMarker {
			return fmt.Errorf("adjacent markers @%s@ + @%s@ without separating literal anchor; alignment ambiguous", pieces[i-1].text, pieces[i].text)
		}
	}
	// First literal anchor matches at position 0.
	pos := 0
	for i, p := range pieces {
		switch p.kind {
		case pieceLiteral:
			if !strings.HasPrefix(out[pos:], p.text) {
				return fmt.Errorf("literal anchor %q not at offset %d in %q", p.text, pos, out)
			}
			pos += len(p.text)
		case pieceMarker:
			// Find the next literal anchor (or end-of-line).
			next := nextLiteralPiece(pieces, i+1)
			if next == nil {
				// Marker is at end of line; value is the rest.
				v, err := unescapeIfRendered(out[pos:], opts.EscapeQuotes)
				if err != nil {
					return fmt.Errorf("marker @%s@ at end of line: %w", p.text, err)
				}
				if err := setValue(values, p.text, v); err != nil {
					return err
				}
				pos = len(out)
				continue
			}
			idx := strings.Index(out[pos:], next.text)
			if idx < 0 {
				return fmt.Errorf("can't locate next literal anchor %q after marker @%s@ in %q", next.text, p.text, out)
			}
			v, err := unescapeIfRendered(out[pos:pos+idx], opts.EscapeQuotes)
			if err != nil {
				return fmt.Errorf("marker @%s@: %w", p.text, err)
			}
			if err := setValue(values, p.text, v); err != nil {
				return err
			}
			pos += idx
		}
	}
	if pos != len(out) {
		return fmt.Errorf("trailing bytes after alignment: tpl=%q rendered=%q", tpl, out)
	}
	return nil
}

type pieceKind int

const (
	pieceLiteral pieceKind = iota
	pieceMarker
)

type templatePiece struct {
	kind pieceKind
	text string // for marker: the variable name; for literal: the literal text
}

// splitTemplatePieces breaks a line into a sequence of literal
// runs interleaved with @VAR@ / ${VAR} markers. Adjacent
// markers (`@A@@B@`) are alignment-ambiguous; the caller will
// fail verification when Extract's Substitute(values) round-trip
// doesn't match.
func splitTemplatePieces(tpl string, opts Options) []templatePiece {
	var out []templatePiece
	var literal strings.Builder
	flushLiteral := func() {
		if literal.Len() > 0 {
			out = append(out, templatePiece{kind: pieceLiteral, text: literal.String()})
			literal.Reset()
		}
	}
	i := 0
	for i < len(tpl) {
		c := tpl[i]
		switch c {
		case '@':
			if name, end, ok := scanAtVar(tpl, i); ok {
				flushLiteral()
				out = append(out, templatePiece{kind: pieceMarker, text: name})
				i = end
				continue
			}
		case '$':
			if !opts.AtOnly {
				if name, end, ok := scanDollarVar(tpl, i); ok {
					flushLiteral()
					out = append(out, templatePiece{kind: pieceMarker, text: name})
					i = end
					continue
				}
			}
		}
		literal.WriteByte(c)
		i++
	}
	flushLiteral()
	// If pieces start with a marker, prepend an empty literal
	// to make the alignment loop's "first piece is literal"
	// invariant easier. (Empty literal HasPrefix matches every
	// position.)
	if len(out) > 0 && out[0].kind == pieceMarker {
		out = append([]templatePiece{{kind: pieceLiteral, text: ""}}, out...)
	}
	return out
}

func nextLiteralPiece(pieces []templatePiece, from int) *templatePiece {
	for i := from; i < len(pieces); i++ {
		if pieces[i].kind == pieceLiteral {
			return &pieces[i]
		}
	}
	return nil
}

// setValue records values[name] = v, returning an error when a
// previously-recorded value disagrees. Disagreement means the
// template references the same variable twice with conflicting
// rendered values, which is either a bug in the extractor or
// an alignment-ambiguous template.
//
// Sentinel-based reconciliation: #cmakedefine recovery records
// "1" for truthy and "" for falsy. The same variable may also
// appear as @NAME@ elsewhere, where extractPlain captures the
// more specific rendered string. cmake's truthiness check only
// cares about "is it in the falsy set" — so any two values that
// agree on truthiness are compatible; we prefer the more
// specific (non-sentinel) one. Concretely:
//
//   - existing == "1"   ∧ v is truthy → upgrade existing to v.
//   - v == "1"          ∧ existing is truthy → keep existing.
//   - existing == ""    ∧ v is falsy (any value) → upgrade
//     existing to v.
//   - v == ""           ∧ existing is falsy → keep existing.
//   - same truthiness, both specific, but byte-different → conflict.
func setValue(values map[string]string, name, v string) error {
	existing, ok := values[name]
	if !ok {
		values[name] = v
		return nil
	}
	if existing == v {
		return nil
	}
	// Truthy reconciliation.
	if existing == "1" && isTruthy(v) {
		values[name] = v
		return nil
	}
	if v == "1" && isTruthy(existing) {
		return nil
	}
	// Falsy reconciliation: "" is the #cmakedefine-falsy
	// sentinel; any falsy nonempty value is compatible.
	if existing == "" && !isTruthy(v) {
		values[name] = v
		return nil
	}
	if v == "" && !isTruthy(existing) {
		return nil
	}
	return fmt.Errorf("variable %q has conflicting values %q vs %q", name, existing, v)
}

// unescapeIfRendered is the inverse of substitute.go's escapeIf:
// when ESCAPE_QUOTES was applied at substitution time, marker
// values come back with `\\` and `\"` escapes; this undoes them
// so the recovered values dict is byte-equal to what cmake had
// at configure time. Returns an error on malformed input (a
// dangling backslash with no following character) so the
// caller can fall back to legacy.
func unescapeIfRendered(rendered string, escape bool) (string, error) {
	if !escape || !strings.Contains(rendered, `\`) {
		return rendered, nil
	}
	var b strings.Builder
	b.Grow(len(rendered))
	for i := 0; i < len(rendered); i++ {
		c := rendered[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(rendered) {
			return "", fmt.Errorf("dangling backslash at end of escaped value %q", rendered)
		}
		i++
		next := rendered[i]
		if next != '\\' && next != '"' {
			// We only emit \\ and \" — any other backslash
			// pair came from somewhere we don't understand.
			// Refuse to round-trip blindly.
			return "", fmt.Errorf("unexpected escape \\%c in escaped value %q", next, rendered)
		}
		b.WriteByte(next)
	}
	return b.String(), nil
}
