// Package configurefile implements cmake's configure_file
// substitution semantics in a form usable from Bazel genrules.
//
// The lift this enables: today
// converter/internal/lower/configure_file.go reads cmake's
// already-rendered config.h bytes from the build dir and base64-
// embeds them in a recovered genrule's `cmd`. That makes the
// .h.in template's CONTENT a load-bearing input of convert-
// element's cache key — every byte of config.h.in must enter
// srckey for soundness, since editing it changes BUILD.bazel.
//
// With this package + a small tool wrapper (cmd/cmake-configure-
// file), the genrule shape becomes
//
//	genrule(
//	    name = "gen_config_h",
//	    srcs = ["src/config.h.in"],
//	    outs = ["config.h"],
//	    cmd = "$(location //tools:cmake-configure-file) " +
//	          "--values=<json> $< $@",
//	    tools = ["//tools:cmake-configure-file"],
//	)
//
// The substitution runs at Bazel time with .h.in as a real srcs.
// Editing .h.in invalidates the genrule directly through Bazel's
// source graph — convert-element-cmake doesn't have to rerun. The
// values dict (captured at convert-element-cmake time by
// reverse-extracting from cmake's rendered output) goes inline
// in the cmd; it changes only when CMakeLists.txt-driven
// variables change, which already invalidates srckey via the
// CMakeLists.txt content-include rule. Net: .h.in becomes
// name-only for srckey purposes.
//
// Substitution rules (per cmake's configure_file documentation):
//
//   - @VAR@ → values["VAR"], or empty if absent.
//   - ${VAR} → same, unless AtOnly is set.
//   - Recursive expansion: Substitute loops until fixpoint or
//     until an internal depth cap (matching cmake's bounded
//     re-expansion of values that themselves contain markers).
//   - #cmakedefine FOO → "#define FOO" if FOO is truthy in
//     values, "/* #undef FOO */" otherwise.
//   - #cmakedefine FOO <content> → "#define FOO <expanded
//     content>" if FOO is truthy, "/* #undef FOO */" otherwise.
//   - #cmakedefine01 FOO → "#define FOO 1" or "#define FOO 0".
//   - CopyOnly: emit the template verbatim with no @VAR@ /
//     ${VAR} / #cmakedefine substitution (mirrors COPYONLY).
//     EscapeQuotes is also a no-op when CopyOnly is set (no
//     substituted bytes to escape); NewlineStyle is still
//     honored — cmake re-emits line terminators per the
//     NEWLINE_STYLE choice even with COPYONLY.
//   - EscapeQuotes: backslash-escape `"` in expanded values
//     (mirrors ESCAPE_QUOTES).
//   - NewlineStyle: control the line terminator written between
//     lines — LF for UNIX/LF and CRLF for DOS/WIN32/CRLF (mirrors
//     NEWLINE_STYLE). NewlineDefault preserves the template's
//     original terminator (auto-detected from the first newline).
//
// Truthiness per cmake's if(): empty string and the constants
// 0, OFF, NO, FALSE, N, IGNORE, NOTFOUND, and any value ending
// in "-NOTFOUND" are false; everything else is true. Case-
// insensitive for the named constants.
package configurefile

import (
	"bytes"
	"strings"
)

// NewlineStyle controls the line terminator Substitute writes
// between lines, mirroring cmake's NEWLINE_STYLE flag.
type NewlineStyle int

const (
	// NewlineDefault preserves the template's original line
	// terminator (cmake's default when NEWLINE_STYLE is omitted).
	NewlineDefault NewlineStyle = iota
	// NewlineLF writes "\n" (cmake UNIX / LF).
	NewlineLF
	// NewlineCRLF writes "\r\n" (cmake DOS / WIN32 / CRLF).
	NewlineCRLF
)

// recursionLimit caps the @VAR@/${VAR} re-expansion loop. cmake's
// own configure_file applies a small bounded depth too (the
// exact limit isn't documented but is on the order of a handful);
// 16 is generous and still small enough that pathological input
// can't waste time.
const recursionLimit = 16

// Options controls substitution behaviour, covering each cmake
// configure_file flag whose semantics affect the rendered
// output bytes.
type Options struct {
	// AtOnly skips ${VAR} substitution; only @VAR@ markers are
	// replaced. Mirrors configure_file's @ONLY flag.
	AtOnly bool

	// CopyOnly skips @VAR@ / ${VAR} / #cmakedefine
	// substitution — content bytes are emitted verbatim.
	// Mirrors configure_file's COPYONLY flag. EscapeQuotes is
	// effectively a no-op alongside CopyOnly (no substituted
	// bytes to escape), but NewlineStyle is still honored
	// because cmake itself re-emits line terminators per
	// NEWLINE_STYLE even with COPYONLY.
	CopyOnly bool

	// EscapeQuotes backslash-escapes `"` in @VAR@ / ${VAR}
	// expansions (only the substituted bytes — literal `"` in
	// the template passes through unchanged). Mirrors
	// configure_file's ESCAPE_QUOTES flag.
	EscapeQuotes bool

	// NewlineStyle controls the line terminator between lines
	// in the rendered output. Default (NewlineDefault) preserves
	// the template's original terminator.
	NewlineStyle NewlineStyle
}

// Substitute renders template against values, returning the
// rendered output. See package doc for the supported
// substitution rules.
func Substitute(template []byte, values map[string]string, opts Options) []byte {
	if opts.CopyOnly {
		// COPYONLY: write the template verbatim, modulo the
		// caller's NewlineStyle preference (cmake re-emits
		// line terminators per NEWLINE_STYLE even with
		// COPYONLY).
		return rewriteNewlines(template, opts.NewlineStyle)
	}

	out := substituteOnce(template, values, opts)
	// Recursive @VAR@ expansion — re-run Substitute on the
	// output until it converges, capping at recursionLimit.
	// cmake's documented behavior is bounded; reaching the cap
	// matches what cmake does too (it stops, leaving any
	// deeper markers unexpanded).
	for i := 0; i < recursionLimit-1; i++ {
		next := substituteOnce(out, values, opts)
		if bytes.Equal(next, out) {
			break
		}
		out = next
	}
	return out
}

// substituteOnce performs a single pass over the template,
// expanding @VAR@ / ${VAR} markers and #cmakedefine* directives
// per options. The recursive-expansion loop in Substitute calls
// this repeatedly.
func substituteOnce(template []byte, values map[string]string, opts Options) []byte {
	var out bytes.Buffer
	out.Grow(len(template))
	terminator := newlineTerminator(opts.NewlineStyle, template)
	// bytes.Split rather than bufio.Scanner: Scanner has a max
	// token size (defaults 64K, capped at 1MiB even with a
	// custom buffer) that would silently truncate long lines.
	// configure_file templates are usually small, but we need
	// the byte-equal-to-cmake guarantee to be unconditional —
	// silent truncation on a pathologically long line is the
	// kind of correctness bug that wouldn't surface until a
	// real template trips it.
	for i, line := range bytes.Split(template, []byte{'\n'}) {
		// Strip trailing \r so CRLF templates don't carry the
		// \r through to the rendered output. Whatever
		// terminator NewlineStyle picks gets re-applied below.
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if i > 0 {
			out.WriteString(terminator)
		}
		out.WriteString(processLine(string(line), values, opts))
	}
	return trimTrailingIfTemplateLacked(out.Bytes(), template, terminator)
}

// newlineTerminator picks the line terminator substituteOnce
// writes between lines: LF/CRLF per opts.NewlineStyle, or — when
// the option is NewlineDefault — a heuristic detection from the
// template's first newline (CRLF if the first \n is preceded by
// \r, LF otherwise). Single-newline sniff rather than full-byte
// scan: real-world configure_file templates have a consistent
// line-ending style, and sampling the first newline matches what
// cmake's NEWLINE_STYLE-omitted behavior produces. If a fixture
// shows up with mixed line endings, the right answer is to set
// an explicit NewlineStyle; the heuristic isn't load-bearing.
func newlineTerminator(style NewlineStyle, template []byte) string {
	switch style {
	case NewlineLF:
		return "\n"
	case NewlineCRLF:
		return "\r\n"
	}
	// Default: detect from the template. First newline; if
	// preceded by \r, the template is CRLF.
	if idx := bytes.IndexByte(template, '\n'); idx > 0 && template[idx-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// rewriteNewlines is COPYONLY's transform: copy the template's
// non-newline bytes verbatim, but apply the caller's NewlineStyle
// to each line terminator. Default style preserves the
// template's original terminators.
func rewriteNewlines(template []byte, style NewlineStyle) []byte {
	if style == NewlineDefault {
		return append([]byte(nil), template...)
	}
	terminator := "\n"
	if style == NewlineCRLF {
		terminator = "\r\n"
	}
	var out bytes.Buffer
	out.Grow(len(template) + len(template)/64)
	for i, line := range bytes.Split(template, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if i > 0 {
			out.WriteString(terminator)
		}
		out.Write(line)
	}
	return trimTrailingIfTemplateLacked(out.Bytes(), template, terminator)
}

// trimTrailingIfTemplateLacked drops the final newline-terminator
// we always add per line if the original template didn't end with
// one. Keeps Substitute's output byte-equal to cmake's when the
// template's last line is unterminated.
func trimTrailingIfTemplateLacked(rendered, template []byte, terminator string) []byte {
	if len(template) == 0 {
		return rendered[:0]
	}
	if template[len(template)-1] == '\n' {
		return rendered
	}
	if bytes.HasSuffix(rendered, []byte(terminator)) {
		return rendered[:len(rendered)-len(terminator)]
	}
	return rendered
}

// processLine applies cmakedefine + cmakedefine01 directive
// recognition first (whole-line shape), then runs @VAR@ /
// ${VAR} substitution on the result.
func processLine(line string, values map[string]string, opts Options) string {
	if rewritten, ok := rewriteCmakeDefine01(line, values); ok {
		return rewritten
	}
	if rewritten, ok := rewriteCmakeDefine(line, values, opts); ok {
		return rewritten
	}
	return expandVars(line, values, opts)
}

// rewriteCmakeDefine01 handles `#cmakedefine01 FOO`. Per
// cmake's [configure_file
// docs](https://cmake.org/cmake/help/latest/command/configure_file.html):
// truthy → `#define FOO 1`, falsy → `#define FOO 0`. Returns
// (rewritten, true) on match; (line, false) otherwise so the
// caller can fall through to the next directive.
func rewriteCmakeDefine01(line string, values map[string]string) (string, bool) {
	body, ok := matchDirective(line, "#cmakedefine01")
	if !ok {
		return line, false
	}
	name := strings.TrimSpace(body)
	if name == "" {
		return line, false
	}
	if isTruthy(values[name]) {
		return cmakeDefineLeading(line) + "#define " + name + " 1", true
	}
	return cmakeDefineLeading(line) + "#define " + name + " 0", true
}

// rewriteCmakeDefine handles `#cmakedefine FOO [content...]`.
// Truthy → `#define FOO [expanded content]`; falsy →
// `/* #undef FOO */`. The leading whitespace from the original
// line is preserved.
func rewriteCmakeDefine(line string, values map[string]string, opts Options) (string, bool) {
	body, ok := matchDirective(line, "#cmakedefine")
	if !ok {
		return line, false
	}
	body = strings.TrimLeft(body, " \t")
	if body == "" {
		return line, false
	}
	// First whitespace-delimited token is the symbol name; rest
	// (if any) is the value template.
	name, rest := splitFirstField(body)
	if name == "" {
		return line, false
	}
	leading := cmakeDefineLeading(line)
	if !isTruthy(values[name]) {
		return leading + "/* #undef " + name + " */", true
	}
	if rest == "" {
		return leading + "#define " + name, true
	}
	return leading + "#define " + name + " " + expandVars(rest, values, opts), true
}

// matchDirective reports whether line is a `#<directive>` line
// (after optional leading whitespace) and returns the body
// after the directive. Returns (body, true) when matched. The
// directive must be followed by whitespace or end-of-line; this
// rejects `#cmakedefine01` matching as `#cmakedefine` (the
// caller checks the longer form first anyway, but be strict).
func matchDirective(line, directive string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, directive) {
		return "", false
	}
	rest := trimmed[len(directive):]
	if rest == "" {
		return "", true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	return rest, true
}

// cmakeDefineLeading returns the leading whitespace prefix
// (spaces + tabs) on line, so rewritten lines preserve the
// original indent.
func cmakeDefineLeading(line string) string {
	for i, c := range line {
		if c != ' ' && c != '\t' {
			return line[:i]
		}
	}
	return line
}

// splitFirstField splits s into (first whitespace-delimited
// token, rest stripped of leading whitespace).
func splitFirstField(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i], strings.TrimLeft(s[i:], " \t")
		}
	}
	return s, ""
}

// expandVars replaces @VAR@ and (unless AtOnly) ${VAR} markers
// in s with values[VAR]. Unknown variables become empty string,
// matching cmake's default. No recursive re-expansion: this is
// a single-pass walk.
func expandVars(s string, values map[string]string, opts Options) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '@':
			if name, end, ok := scanAtVar(s, i); ok {
				b.WriteString(escapeIf(values[name], opts.EscapeQuotes))
				i = end
				continue
			}
		case '$':
			if !opts.AtOnly {
				if name, end, ok := scanDollarVar(s, i); ok {
					b.WriteString(escapeIf(values[name], opts.EscapeQuotes))
					i = end
					continue
				}
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// escapeIf backslash-escapes `"` (and the escape itself) in v
// when escape is set, mirroring cmake's ESCAPE_QUOTES behavior.
// Only the substituted bytes go through this — literal `"` in
// the template passes through untouched (cmake's documented
// behavior matches: ESCAPE_QUOTES only applies to expanded
// values, not to literal characters in the template body).
func escapeIf(v string, escape bool) string {
	if !escape || (!strings.ContainsRune(v, '"') && !strings.ContainsRune(v, '\\')) {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 4)
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\\' || c == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// scanAtVar tries to match `@NAME@` starting at s[i]. NAME is
// [A-Za-z_][A-Za-z0-9_]* (cmake's identifier shape). Returns
// (name, indexAfterClosingAt, true) on match; otherwise
// (_, _, false).
func scanAtVar(s string, i int) (string, int, bool) {
	if s[i] != '@' {
		return "", 0, false
	}
	j := i + 1
	if j >= len(s) || !isIdentStart(s[j]) {
		return "", 0, false
	}
	for j < len(s) && isIdentCont(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '@' {
		return "", 0, false
	}
	return s[i+1 : j], j + 1, true
}

// scanDollarVar tries to match `${NAME}` starting at s[i].
func scanDollarVar(s string, i int) (string, int, bool) {
	if s[i] != '$' || i+1 >= len(s) || s[i+1] != '{' {
		return "", 0, false
	}
	j := i + 2
	if j >= len(s) || !isIdentStart(s[j]) {
		return "", 0, false
	}
	for j < len(s) && isIdentCont(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '}' {
		return "", 0, false
	}
	return s[i+2 : j], j + 1, true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// isTruthy applies cmake's if() truthiness rules. Per the
// configure_file doc cross-ref, this is the "is value defined
// AND not in the falsy set" check #cmakedefine uses.
//
// Falsy: empty string; one of (0, OFF, NO, FALSE, N, IGNORE,
// NOTFOUND), case-insensitive; any value ending in "-NOTFOUND".
// Everything else is truthy.
func isTruthy(value string) bool {
	if value == "" {
		return false
	}
	upper := strings.ToUpper(value)
	switch upper {
	case "0", "OFF", "NO", "FALSE", "N", "IGNORE", "NOTFOUND":
		return false
	}
	return !strings.HasSuffix(upper, "-NOTFOUND")
}
