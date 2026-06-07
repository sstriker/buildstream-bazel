package cmakeargv

import (
	"bufio"
	"os"
	"strings"
)

// Comment recovery for the comment-carrying feature (ROADMAP.md "Carry
// CMakeLists comments into BUILD files"). cmake discards comments at lex time,
// so neither the File API nor the trace carries them — they are recoverable
// only from raw source. These helpers read a CMakeLists.txt / *.cmake file and
// return the raw comment tokens ("# ...") associated with a declaration site,
// to be re-attached as leading comments on the emitted Bazel rule.
//
// Raw tokens are returned verbatim (trimmed of surrounding whitespace, leading
// `#` kept) so the author's exact wording survives; the emit layer normalizes
// them into valid BUILD comments. Only `#` line comments are recovered here;
// cmake bracket comments (`#[[ ... ]]`) are rare for doc comments and are left
// for a follow-up.

// readSourceLines reads path into newline-stripped lines, using the same
// generous buffer ReadCall uses (CMakeLists lines have no practical limit).
func readSourceLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &Error{Path: path, Err: err}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, &Error{Path: path, Err: err}
	}
	return lines, nil
}

// isLineComment reports whether a trimmed line is a `#` line comment (and not
// the start of a bracket comment `#[[` / `#[=[`, which is out of scope).
func isLineComment(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	rest := trimmed[1:]
	// `#[[` or `#[=*[` opens a bracket comment — skip (not handled yet).
	if strings.HasPrefix(rest, "[") {
		j := 1
		for j < len(rest) && rest[j] == '=' {
			j++
		}
		if j < len(rest) && rest[j] == '[' {
			return false
		}
	}
	return true
}

// LeadingComment returns the contiguous block of `#` line comments immediately
// preceding the cmake command at the given 1-based line, as raw comment tokens
// ("# ...") in top-to-bottom order. Scanning walks upward from the line above
// and stops at the first blank line, non-comment line, or bracket comment.
// Returns nil when the command has no immediately-preceding comment.
//
// A comment trailing code on the line above is NOT a leading comment: that
// line's trimmed text won't start with `#`, so the scan stops.
func LeadingComment(path string, line int) ([]string, error) {
	lines, err := readSourceLines(path)
	if err != nil {
		return nil, err
	}
	// line-2 is the 0-based index of the line directly above the command.
	start := line - 2
	if start < 0 || start >= len(lines) {
		return nil, nil
	}
	var rev []string
	for i := start; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || !isLineComment(t) {
			break
		}
		rev = append(rev, t)
	}
	if len(rev) == 0 {
		return nil, nil
	}
	// rev is bottom-to-top; reverse to source order.
	out := make([]string, len(rev))
	for i, s := range rev {
		out[len(rev)-1-i] = s
	}
	return out, nil
}

// FileHeaderComment returns the file's leading `#` line-comment block — the
// contiguous comment lines at the top of the file (after any leading blank
// lines), before the first command or bracket comment. This is the
// license/copyright/file-doc header. Returns nil when the file opens with a
// command.
func FileHeaderComment(path string) ([]string, error) {
	lines, err := readSourceLines(path)
	if err != nil {
		return nil, err
	}
	var out []string
	started := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			if started {
				break // blank line ends the header block
			}
			continue // tolerate leading blank lines before the header
		}
		if !isLineComment(t) {
			break
		}
		started = true
		out = append(out, t)
	}
	return out, nil
}
