package lower

import (
	"strings"
)

// qualifyRedirectBasenames prepends `subdir/` to bare-basename
// targets of shell redirects (`>`, `>>`, `<`) in cmd. Closes the
// cwd-shift gap left by rewriteGenruleCmd's cd-strip:
//
//	original (cmake-Ninja):
//	  cd /tmp/<build>/lib/Transforms/Hello && python3 ... > LLVMHello.exports
//
//	after cd-strip + path normalisation (current state):
//	  python3 ... > LLVMHello.exports         ← wrong cwd for `>`
//
//	after this pass (subdir = "lib/Transforms/Hello"):
//	  python3 ... > lib/Transforms/Hello/LLVMHello.exports   ← correct
//
// The genrule's declared `outs = ["lib/Transforms/Hello/
// LLVMHello.exports"]` expects Bazel's action to write at the
// full GENDIR-relative path. Without qualification, the action
// writes to GENDIR/LLVMHello.exports and Bazel rejects the rule
// for missing the declared output.
//
// Conservative scope: only token positions immediately after
// `>`, `>>`, or `<` (with optional whitespace) are rewritten,
// and only when the target token is a bare basename (no `/`,
// no `$`/quote, not already absolute). Tokens with a `/` are
// assumed already-qualified (the upstream path-rewrite pass
// produced workspace-relative refs); tokens with `$` are Bazel
// substitution markers like `$@` that already resolve correctly.
//
// subdir is the build-dir-relative path captured at cd-strip
// time. Empty subdir → no-op (the caller skips the call).
func qualifyRedirectBasenames(cmd, subdir string) string {
	if cmd == "" || subdir == "" {
		return cmd
	}
	// Walk the cmd looking for redirect operators. Each operator
	// is followed by an optional whitespace run, then the redirect
	// target token.
	var b strings.Builder
	b.Grow(len(cmd) + 32)
	i := 0
	// Track quoted-string regions so `>` and `<` inside them
	// (e.g. a `python3 -c "print('x' if y > 1 else None)"`
	// payload) don't get parsed as shell redirects. The quote
	// state is set when we see an opening `"` or `'` and cleared
	// at the matching close. Doesn't handle escape sequences
	// or nested quotes — but cmake's emitter keeps cmd shapes
	// simple enough that the bare quote-pair detection suffices.
	var quote byte
	for i < len(cmd) {
		c := cmd[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			b.WriteByte(c)
			i++
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			b.WriteByte(c)
			i++
			continue
		}
		// Look for `<`, `>`, or `>>` at this position. Skip
		// `&>`, `>&`, `2>` etc — those are fd-numbered or
		// fd-merge shapes we don't try to handle (cmake's
		// CUSTOM_COMMAND emitter doesn't use them).
		op, opLen := redirectOpAt(cmd, i)
		if op == "" {
			b.WriteByte(c)
			i++
			continue
		}
		// Guard against `>&` / `>=` / `<=` / `<<` heredoc etc.
		// `>=` and `<=` are arithmetic — shouldn't appear in
		// genrule cmds but cheap to avoid.
		if redirectFollowGuarded(cmd, i, opLen, op) {
			b.WriteString(op)
			i += opLen
			continue
		}
		// Guard against preceded-by-digit shape (`2>`, `1>`) and
		// the `&>` shell-shape (redirect all streams). For those,
		// keep the bytes but don't rewrite the following target.
		if b.Len() > 0 {
			prev := b.String()[b.Len()-1]
			if (prev >= '0' && prev <= '9') || prev == '&' {
				b.WriteString(op)
				i += opLen
				continue
			}
		}
		b.WriteString(op)
		i += opLen
		// Skip whitespace between operator and target.
		for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
			b.WriteByte(cmd[i])
			i++
		}
		// Extract the target token: up to whitespace, `&`, `|`,
		// `;`, `>`, `<`, or end-of-string.
		tokStart := i
		for i < len(cmd) && !isRedirectTokenBreak(cmd[i]) {
			i++
		}
		tok := cmd[tokStart:i]
		b.WriteString(qualifyRedirectTarget(tok, subdir))
	}
	return b.String()
}

// redirectOpAt returns the shell redirect operator (`<`, `>`, `>>`) beginning
// at cmd[i] and its byte length, or ("", 0) when cmd[i] is not a redirect.
func redirectOpAt(cmd string, i int) (op string, opLen int) {
	switch cmd[i] {
	case '>':
		if i+1 < len(cmd) && cmd[i+1] == '>' {
			return ">>", 2
		}
		return ">", 1
	case '<':
		return "<", 1
	}
	return "", 0
}

// redirectFollowGuarded reports whether the byte after the operator marks a
// shape we don't rewrite: `>&` / fd-merge, `>=` / `<=` arithmetic, or `<<`
// heredoc. Those keep their bytes verbatim and the following token is left alone.
func redirectFollowGuarded(cmd string, i, opLen int, op string) bool {
	if i+opLen >= len(cmd) {
		return false
	}
	n := cmd[i+opLen]
	return n == '&' || n == '=' || (op == "<" && n == '<')
}

// isRedirectTokenBreak reports whether c terminates a redirect target token
// (whitespace, another operator, or a command separator).
func isRedirectTokenBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' ||
		c == '&' || c == '|' || c == ';' ||
		c == '>' || c == '<'
}

// qualifyRedirectTarget prepends `subdir/` to tok when tok looks
// like a bare basename (no `/`, no `$` Bazel substitution marker,
// not absolute). Other tokens pass through unchanged.
func qualifyRedirectTarget(tok, subdir string) string {
	if tok == "" {
		return tok
	}
	// Already qualified — has a path separator.
	if strings.Contains(tok, "/") {
		return tok
	}
	// Bazel substitution markers (`$@`, `$(@D)/x`, etc.) — leave
	// alone.
	if strings.HasPrefix(tok, "$") {
		return tok
	}
	// Quoted-string target: leave alone for now; the conservative
	// path is to not mangle quote boundaries.
	if strings.HasPrefix(tok, "\"") || strings.HasPrefix(tok, "'") {
		return tok
	}
	// Single-token redirect to /dev/null and similar absolutes.
	if strings.HasPrefix(tok, "/") {
		return tok
	}
	return subdir + "/" + tok
}
