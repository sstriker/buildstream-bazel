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
// file, in a follow-up commit), the genrule shape becomes
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
// source graph — convert-element doesn't have to rerun. The
// values dict (captured at convert-element time by
// reverse-extracting from cmake's rendered output) goes either
// inline in the cmd or in a sidecar; either way it changes only
// when CMakeLists.txt-driven variables change, which already
// invalidates srckey via the CMakeLists.txt content-include
// rule. Net: .h.in becomes name-only for srckey purposes.
//
// Substitution rules (per cmake's configure_file documentation):
//
//   - @VAR@ → values["VAR"], or empty if absent.
//   - ${VAR} → same, unless AtOnly is set.
//   - #cmakedefine FOO → "#define FOO" if FOO is truthy in
//     values, "/* #undef FOO */" otherwise.
//   - #cmakedefine FOO <content> → "#define FOO <expanded
//     content>" if FOO is truthy, "/* #undef FOO */" otherwise.
//   - #cmakedefine01 FOO → "#define FOO 1" or "#define FOO 0".
//
// Truthiness per cmake's if(): empty string and the constants
// 0, OFF, NO, FALSE, N, IGNORE, NOTFOUND, and any value ending
// in "-NOTFOUND" are false; everything else is true. Case-
// insensitive for the named constants.
//
// NOT yet implemented (queued for follow-up if a fixture forces
// them): NEWLINE_STYLE control, ESCAPE_QUOTES, COPYONLY,
// recursive @VAR@ expansion (the rendered output is taken at
// face value; if a value itself contains @SUBVAR@ markers, they
// pass through verbatim — cmake's recursive expansion is
// bounded but not modeled here).
package configurefile

import (
	"bytes"
	"strings"
)

// Options controls substitution behaviour, mirroring the
// configure_file flags relevant to the lift's v1 scope.
type Options struct {
	// AtOnly skips ${VAR} substitution; only @VAR@ markers are
	// replaced. Mirrors configure_file's @ONLY flag.
	AtOnly bool
}

// Substitute renders template against values, returning the
// rendered output. See package doc for the supported
// substitution rules.
func Substitute(template []byte, values map[string]string, opts Options) []byte {
	var out bytes.Buffer
	out.Grow(len(template))
	// bytes.Split rather than bufio.Scanner: Scanner has a max
	// token size (defaults 64K, capped at 1MiB even with a
	// custom buffer) that would silently truncate long lines.
	// configure_file templates are usually small, but we need
	// the byte-equal-to-cmake guarantee to be unconditional —
	// silent truncation on a pathologically long line is the
	// kind of correctness bug that wouldn't surface until a
	// real template trips it.
	for i, line := range bytes.Split(template, []byte{'\n'}) {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(processLine(string(line), values, opts))
	}
	return trimTrailingIfTemplateLacked(out.Bytes(), template)
}

// trimTrailingIfTemplateLacked drops the final newline we always
// add per line if the original template didn't end with one.
// Keeps Substitute's output byte-equal to cmake's when the
// template's last line is unterminated.
func trimTrailingIfTemplateLacked(rendered, template []byte) []byte {
	if len(template) == 0 {
		return rendered[:0]
	}
	if template[len(template)-1] != '\n' && len(rendered) > 0 && rendered[len(rendered)-1] == '\n' {
		return rendered[:len(rendered)-1]
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
				b.WriteString(values[name])
				i = end
				continue
			}
		case '$':
			if !opts.AtOnly {
				if name, end, ok := scanDollarVar(s, i); ok {
					b.WriteString(values[name])
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
	if strings.HasSuffix(upper, "-NOTFOUND") {
		return false
	}
	return true
}
