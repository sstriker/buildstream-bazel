// Package cmakeparse is a deliberately tiny parser for the
// subset of cmake-language(7) needed by Tier 2 of the platform-
// conditional source partitioning story (#217 follow-on).
//
// Lives under internal/ (not converter/internal/) so
// internal/shadow can import it for its Tier-2 driver — Go's
// internal-package rules forbid an internal/ package from
// reaching across the converter/ boundary.
//
// The Tier 1 walker (internal/shadow/platform_conditional.go)
// attributes sources cmake's --trace-expand actually executed.
// Tier 2 closes the cross-platform half: the SKIPPED arm of an
// `if(CMAKE_SYSTEM_NAME ...) ... else() ... endif()` block holds
// sources that exist in the CMakeLists.txt source but never
// appear in the trace (cmake only traces what runs). To recover
// them we read the actual CMakeLists.txt at the `file`/`line`
// the trace's `if()` event records, parse its if-body, and
// extract `target_sources` / `add_library` / `add_executable`
// calls cmake never executed — attributing those to the other
// platform's constraint.
//
// Scope discipline: this is NOT a cmake interpreter. It parses
// command-invocation syntax (identifier-paren-args-paren) plus
// the structural delimiters `if` / `elseif` / `else` / `endif`,
// nothing more. function/macro definitions and the foreach /
// while / include flow-control commands round-trip through as
// plain command_calls — we don't follow them. Generator
// expressions and variable references inside arguments are
// preserved as opaque tokens; the Tier 2 driver refuses on any
// `${...}` expansion that would actually need evaluation to
// produce a source path.
package cmakeparse

import (
	"fmt"
	"strings"
	"unicode"
)

// tokenKind enumerates the lexeme classes the lexer emits.
// Command names and arguments share the tokWord class —
// distinguishing them is a parser job (the first tokWord in a
// command-call is its name; subsequent tokWords inside the
// parens are its args). Quoted and bracket arguments collapse
// into tokWord too; callers reading the lexed text get the
// argument bytes as-written.
type tokenKind int

const (
	tokInvalid tokenKind = iota
	tokWord              // identifier / unquoted / quoted / bracket argument
	tokLParen            // (
	tokRParen            // )
	tokEOF
)

// token is one lexeme. Line is the 1-based source line where
// the lexeme begins. The parser uses Line to attach (file,
// line) coordinates to command_call records so Tier 2 can
// correlate parsed if-bodies with the trace's recorded `if()`
// event line.
type token struct {
	kind tokenKind
	text string
	line int
}

// lex tokenizes the entire cmake source buffer. Returns the
// token stream and any first lexical error encountered (errors
// are not recoverable in this minimal parser — the goal is
// "refuse cleanly on anything we don't understand", not
// "best-effort recover and keep going" the way cmake itself
// does).
//
// cmake-language(7) covers a richer surface than we model:
//   - Bracket arguments `[[ ... ]]` are supported (with the
//     equal-sign-padded variant `[=[ ... ]=]` etc.).
//   - Bracket comments `#[[ ... ]]` mirror the same shape.
//   - Quoted arguments preserve internal newlines.
//   - Line continuation via `\` at end-of-line is supported
//     inside unquoted arguments.
//   - Variable references `${var}` and generator expressions
//     `$<...>` are preserved as opaque substrings inside the
//     argument they're part of.
func lex(src string) ([]token, error) {
	l := &lexer{src: src, line: 1}
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		l.out = append(l.out, t)
		if t.kind == tokEOF {
			return l.out, nil
		}
	}
}

type lexer struct {
	src  string
	pos  int
	line int
	out  []token
}

// next returns the next token from the input, advancing pos.
// Returns a TokEOF token when the input is exhausted.
func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '\n':
			l.line++
			l.pos++
		case c == '#':
			// Bracket comment shape: #[==[ ... ]==] (equal-sign
			// pads must match). Line-comment is the fallback.
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '[' {
				if end, ok := l.scanBracket(l.pos + 1); ok {
					// Count newlines inside the comment so line
					// tracking stays accurate.
					l.advanceCountingNewlines(l.pos, end)
					l.pos = end
					continue
				}
			}
			// Line comment.
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '(':
			tok := token{kind: tokLParen, text: "(", line: l.line}
			l.pos++
			return tok, nil
		case c == ')':
			tok := token{kind: tokRParen, text: ")", line: l.line}
			l.pos++
			return tok, nil
		case c == '"':
			return l.scanQuoted()
		case c == '[':
			// Bracket argument shape: [==[ ... ]==] with matched
			// equal-sign padding. Anything else is a regular
			// unquoted-argument character.
			if end, ok := l.scanBracket(l.pos); ok {
				startLine := l.line
				text := l.src[l.pos:end]
				l.advanceCountingNewlines(l.pos, end)
				l.pos = end
				return token{kind: tokWord, text: text, line: startLine}, nil
			}
			return l.scanUnquoted()
		case isIdentStart(c):
			return l.scanIdentOrUnquoted()
		default:
			return l.scanUnquoted()
		}
	}
	return token{kind: tokEOF, line: l.line}, nil
}

// scanIdentOrUnquoted scans a token that starts with an
// identifier character. cmake's syntax allows command names
// (identifiers) at the head of an invocation; downstream
// arguments can be unquoted-argument-shaped (which overlaps
// with identifier-shape). The disambiguation happens at the
// parser level — the lexer returns TokIdent here and the
// parser reinterprets it as an argument when it appears
// inside an arg-list.
func (l *lexer) scanIdentOrUnquoted() (token, error) {
	start := l.pos
	startLine := l.line
	for l.pos < len(l.src) && isIdentCont(l.src[l.pos]) {
		l.pos++
	}
	// If the next non-ws char is `(`, this is a command name;
	// otherwise it's an unquoted argument that happens to be
	// ident-shaped. The lexer can't peek across whitespace
	// without entangling argument-vs-command logic — we always
	// emit TokIdent and let the parser decide.
	//
	// But if the next character (no whitespace skip) is part
	// of an unquoted-arg continuation (e.g. `foo/bar`, `foo.c`,
	// `${var}`, `$<genex>`), we keep scanning and emit a TokArg.
	if l.pos < len(l.src) && isUnquotedCont(l.src[l.pos]) {
		// Continue scanning as an unquoted argument from where
		// the ident ended. Reset to the start so scanUnquoted
		// re-reads from the beginning with its own loop.
		l.pos = start
		l.line = startLine
		return l.scanUnquoted()
	}
	text := l.src[start:l.pos]
	return token{kind: tokWord, text: text, line: startLine}, nil
}

// scanUnquoted scans an unquoted argument. cmake's grammar
// allows most non-whitespace characters; we accept anything
// that isn't a delimiter (whitespace / parens / quotes /
// comment). Variable references `${var}` and generator
// expressions `$<...>` are not expanded — they're preserved
// as opaque substrings.
func (l *lexer) scanUnquoted() (token, error) {
	start := l.pos
	startLine := l.line
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '(' || c == ')' || c == '"' || c == '#' {
			break
		}
		// Track nested ${...} and $<...> so a `)` or `(` inside
		// them doesn't terminate the argument. cmake allows
		// nested var refs inside genex args and vice versa, so
		// we just count brace/angle pairs after a `$` sigil.
		if c == '\\' && l.pos+1 < len(l.src) {
			// cmake's escape sequences: `\\`, `\;`, `\n`, `\r`,
			// `\t`, `\"`. Skip the next char regardless of which
			// — we don't unescape, just preserve the bytes.
			l.pos += 2
			continue
		}
		if c == '$' && l.pos+1 < len(l.src) {
			open := l.src[l.pos+1]
			if open == '{' || open == '<' {
				end, ok := l.scanDollarRef(l.pos)
				if !ok {
					return token{}, l.errorf("unterminated variable or generator expression")
				}
				l.advanceCountingNewlines(l.pos, end)
				l.pos = end
				continue
			}
		}
		l.pos++
	}
	text := l.src[start:l.pos]
	if text == "" {
		return token{}, l.errorf("unexpected character %q", l.src[start])
	}
	return token{kind: tokWord, text: text, line: startLine}, nil
}

// scanDollarRef walks a `${...}` or `$<...>` reference and
// returns the byte offset of the matching close. Nested refs
// are supported (cmake allows e.g. `${${var}}`).
func (l *lexer) scanDollarRef(start int) (int, bool) {
	i := start + 1
	open := l.src[i]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '<':
		close = '>'
	default:
		return 0, false
	}
	depth := 1
	i++
	for i < len(l.src) && depth > 0 {
		c := l.src[i]
		switch c {
		case open:
			// Same-style nested open: depth++.
			depth++
			i++
		case close:
			depth--
			i++
		case '$':
			if i+1 < len(l.src) {
				next := l.src[i+1]
				if next == '{' || next == '<' {
					// Skip into the nested ref; reuse the same
					// routine recursively via a manual stack.
					sub, ok := l.scanDollarRef(i)
					if !ok {
						return 0, false
					}
					i = sub
					continue
				}
			}
			i++
		default:
			i++
		}
	}
	if depth != 0 {
		return 0, false
	}
	return i, true
}

// scanQuoted scans a "..." argument. cmake's quoted-argument
// rules: backslash escapes the next character (including
// newlines for line continuation); embedded variable
// references and generator expressions are preserved verbatim
// because the lexer doesn't expand.
func (l *lexer) scanQuoted() (token, error) {
	if l.src[l.pos] != '"' {
		return token{}, l.errorf("expected quoted argument")
	}
	startLine := l.line
	start := l.pos
	l.pos++ // skip leading quote
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.pos++ // include trailing quote
			text := l.src[start:l.pos]
			return token{kind: tokWord, text: text, line: startLine}, nil
		case '\\':
			if l.pos+1 < len(l.src) {
				if l.src[l.pos+1] == '\n' {
					l.line++
				}
				l.pos += 2
				continue
			}
			l.pos++
		case '\n':
			l.line++
			l.pos++
		default:
			l.pos++
		}
	}
	return token{}, l.errorf("unterminated quoted argument")
}

// scanBracket attempts to scan a `[==[ ... ]==]` bracket
// argument or comment starting at pos. Returns the byte offset
// one past the closing `]==]` and true on success; (0, false)
// when the input at pos isn't bracket-shaped or doesn't
// terminate.
//
// cmake-language(7) §"Bracket Argument": `[` followed by zero
// or more `=` followed by another `[` opens; the matching
// close has `]`, the same number of `=`, then `]`.
func (l *lexer) scanBracket(pos int) (int, bool) {
	if pos >= len(l.src) || l.src[pos] != '[' {
		return 0, false
	}
	i := pos + 1
	equals := 0
	for i < len(l.src) && l.src[i] == '=' {
		equals++
		i++
	}
	if i >= len(l.src) || l.src[i] != '[' {
		return 0, false
	}
	// Body starts after the second `[`.
	bodyStart := i + 1
	closer := "]" + strings.Repeat("=", equals) + "]"
	end := strings.Index(l.src[bodyStart:], closer)
	if end < 0 {
		return 0, false
	}
	return bodyStart + end + len(closer), true
}

// advanceCountingNewlines bumps l.line for every \n in
// src[from:to]. Used after a bulk-skip (bracket arg / bracket
// comment / nested $<...> with embedded newlines) so subsequent
// tokens get the right line attribution.
func (l *lexer) advanceCountingNewlines(from, to int) {
	for i := from; i < to && i < len(l.src); i++ {
		if l.src[i] == '\n' {
			l.line++
		}
	}
}

func (l *lexer) errorf(format string, args ...any) error {
	return fmt.Errorf("cmakeparse: line %d: %s", l.line, fmt.Sprintf(format, args...))
}

// isIdentStart reports whether c can start a cmake identifier.
// cmake identifiers are case-insensitively matched at command-
// call boundaries; the lexical class is C-shape (letter or
// underscore, followed by letters/digits/underscores).
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// isUnquotedCont reports whether a character can extend an
// ident-prefixed token into an unquoted argument (e.g. `foo.c`,
// `foo/bar`, `lib${suffix}`). Whitespace, parens, quote, and
// `#` always terminate; everything else extends.
func isUnquotedCont(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '(', ')', '"', '#':
		return false
	}
	// Skip the cmake-printable range silently. Anything an
	// identifier char doesn't match is fair game.
	if isIdentCont(c) {
		return true
	}
	// Allow common path / generator chars.
	return !unicode.IsControl(rune(c))
}
