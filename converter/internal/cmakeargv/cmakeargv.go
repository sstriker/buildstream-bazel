// Package cmakeargv tokenizes one cmake command call's argv at a
// recorded file:line position. Phase 1 task 1 of the
// generator-parity uplift (ROADMAP.md) uses this to recover
// PUBLIC / PRIVATE / INTERFACE keywords from target_link_libraries
// (and similar) call sites via the codemodel's BacktraceGraph —
// no --trace-expand dependency, robust under macro expansion (each
// backtrace frame is its own call site).
//
// Scope: argv parsing only. Variable expansion (`${VAR}`),
// generator-expression resolution (`$<...>`), and bracket-argument
// handling (`[=[...]=]`) follow cmake's documented lexing without
// performing the semantic step. Quoted args strip the surrounding
// quotes but preserve their content verbatim (no `${...}`
// substitution); unquoted args are split on whitespace.
//
// This is enough for keyword recovery: PUBLIC / PRIVATE / INTERFACE
// are always literal keywords in the trace, never inside `${...}`,
// and the recovered argv preserves their position. Variable-named
// dependencies (`target_link_libraries(foo ${SOME_DEP_VAR})`) end
// up with the literal `${SOME_DEP_VAR}` string, which the caller
// can match against itself or fall back to trace-based recovery.
package cmakeargv

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Call describes one cmake command invocation recovered from
// source. Args excludes the command name itself.
type Call struct {
	Command string
	Args    []string
}

// ReadCall opens path, seeks to line, and tokenizes the cmake
// command call whose name is command (case-insensitive). Returns
// the parsed Call or an error.
//
// Locating the call: cmake commands often span multiple lines, so
// the recorded `line` indexes the line containing the OPENING `(`
// of the call. The lexer scans forward from line until the matching
// close-paren, accounting for nested parens inside quoted args.
//
// Why this is robust enough for keyword recovery: the codemodel's
// BacktraceNode.Line is the source line of the command name itself
// (per cmake's File API contract), so the opening paren is on the
// same line or the next non-comment line. The lexer's scanner
// tolerates intervening whitespace and comment lines.
//
// Returns a typed *Error wrapping the underlying I/O / parse
// failure so callers can pattern-match on cmakeargv-specific
// errors (e.g. "file not found" vs "command name mismatch").
func ReadCall(path string, line int, command string) (*Call, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &Error{Path: path, Line: line, Err: err}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// CMakeLists.txt files have no practical line length limit;
	// bump the scanner buffer to accommodate long generated lines.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	// Read up to and including the target line.
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) >= line {
			break
		}
	}
	// Read the rest of the file so multi-line calls can complete.
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, &Error{Path: path, Line: line, Err: err}
	}
	if line < 1 || line > len(lines) {
		return nil, &Error{Path: path, Line: line, Err: fmt.Errorf("line %d out of range (file has %d lines)", line, len(lines))}
	}

	// Concatenate from the target line onward. cmake commands are
	// case-insensitive; we lowercase only the prefix-match against
	// the command name and preserve case inside arg tokens.
	body := strings.Join(lines[line-1:], "\n")

	call, err := tokenizeCall(body, command)
	if err != nil {
		return nil, &Error{Path: path, Line: line, Err: err}
	}
	return call, nil
}

func tokenizeCall(body, command string) (*Call, error) {
	i := 0
	// Skip leading whitespace and `#` comment lines until we find
	// the command identifier.
	for i < len(body) {
		c := body[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '#' {
			// Skip to end of line.
			for i < len(body) && body[i] != '\n' {
				i++
			}
			continue
		}
		break
	}
	if i >= len(body) {
		return nil, fmt.Errorf("no command found")
	}
	// Match command identifier (case-insensitive).
	cmdEnd := i
	for cmdEnd < len(body) && isIdentByte(body[cmdEnd]) {
		cmdEnd++
	}
	got := body[i:cmdEnd]
	if !strings.EqualFold(got, command) {
		return nil, fmt.Errorf("expected command %q, got %q", command, got)
	}
	i = cmdEnd
	// Skip whitespace + comments before the `(`.
	for i < len(body) {
		c := body[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '#' {
			for i < len(body) && body[i] != '\n' {
				i++
			}
			continue
		}
		break
	}
	if i >= len(body) || body[i] != '(' {
		return nil, fmt.Errorf("expected `(` after command name")
	}
	i++

	var args []string
	for {
		// Skip whitespace + comments.
		for i < len(body) {
			c := body[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				i++
				continue
			}
			if c == '#' {
				for i < len(body) && body[i] != '\n' {
					i++
				}
				continue
			}
			break
		}
		if i >= len(body) {
			return nil, fmt.Errorf("unterminated call (no matching `)`)")
		}
		if body[i] == ')' {
			i++
			break
		}
		arg, next, err := readArg(body, i)
		if err != nil {
			return nil, err
		}
		i = next
		args = append(args, arg)
	}

	return &Call{Command: command, Args: args}, nil
}

// readArg reads one argument starting at body[i]. Returns the
// argument's content, the index past the argument's end, or an
// error. Handles three cmake argument shapes:
//
//   - Bracket arg `[=*[...]=*]` — content returned verbatim, no
//     expansion.
//   - Quoted arg `"..."` — content returned with the surrounding
//     quotes stripped, `\` escapes processed for `\n` `\t` `\"`
//     `\\`. No `${...}` expansion (caller's job).
//   - Unquoted arg — read until whitespace, `)`, `#`, or `(`.
func readArg(body string, i int) (string, int, error) {
	if i >= len(body) {
		return "", i, fmt.Errorf("unexpected end of input")
	}
	switch body[i] {
	case '"':
		return readQuoted(body, i)
	case '[':
		// Bracket arg if it starts with [=*[.
		if eq := matchBracketOpen(body, i); eq >= 0 {
			return readBracket(body, i, eq)
		}
		// Otherwise fall through to unquoted handling.
	}
	return readUnquoted(body, i)
}

func readQuoted(body string, i int) (string, int, error) {
	// Skip opening quote.
	i++
	var sb strings.Builder
	for i < len(body) {
		c := body[i]
		if c == '"' {
			return sb.String(), i + 1, nil
		}
		if c == '\\' && i+1 < len(body) {
			next := body[i+1]
			switch next {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"', '\\', ';', ' ', '#':
				sb.WriteByte(next)
			default:
				// Preserve unknown escape as-is.
				sb.WriteByte(c)
				sb.WriteByte(next)
			}
			i += 2
			continue
		}
		sb.WriteByte(c)
		i++
	}
	return "", i, fmt.Errorf("unterminated quoted arg")
}

// matchBracketOpen returns the number of `=` signs in a bracket
// opener `[=*[` starting at body[i], or -1 if body[i:] doesn't
// start one.
func matchBracketOpen(body string, i int) int {
	if i >= len(body) || body[i] != '[' {
		return -1
	}
	j := i + 1
	for j < len(body) && body[j] == '=' {
		j++
	}
	if j < len(body) && body[j] == '[' {
		return j - i - 1
	}
	return -1
}

func readBracket(body string, i, eq int) (string, int, error) {
	// Opener is `[` + eq `=` + `[`.
	start := i + 1 + eq + 1
	closer := "]" + strings.Repeat("=", eq) + "]"
	end := strings.Index(body[start:], closer)
	if end < 0 {
		return "", i, fmt.Errorf("unterminated bracket arg")
	}
	return body[start : start+end], start + end + len(closer), nil
}

func readUnquoted(body string, i int) (string, int, error) {
	var sb strings.Builder
	for i < len(body) {
		c := body[i]
		switch c {
		case ' ', '\t', '\n', '\r', ')', '#':
			return sb.String(), i, nil
		case '\\':
			if i+1 < len(body) {
				sb.WriteByte(body[i+1])
				i += 2
				continue
			}
		}
		sb.WriteByte(c)
		i++
	}
	return sb.String(), i, nil
}

func isIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

// Error wraps a cmakeargv-specific failure with the originating
// file path and line for clearer diagnostics.
type Error struct {
	Path string
	Line int
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("cmakeargv: %s:%d: %v", e.Path, e.Line, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }
