// Package readpaths is the shared pattern-matcher for read-paths
// and srckey narrowing. Lifted out of cmd/write-a so the audit
// tool (cmd/audit-narrowing) and write-a can share one
// authoritative implementation.
//
// File format ("read-paths.txt"):
//
//	# comments + blanks ignored
//	include CMakeLists.txt
//	include cmake/*.cmake
//	include include/**/*.h
//	exclude include/internal/*
//
// Glob grammar:
//   - * matches any sequence except '/'
//   - ** matches any sequence including '/'
//   - ? matches one character except '/'
//   - all other characters match literally
package readpaths

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Rule is one parsed line.
type Rule struct {
	Include bool // true for "include", false for "exclude"
	Pattern string
}

// Patterns is the parsed file content. Empty / nil signals "no
// narrowing" — Match returns true for every path, the
// conservative default.
type Patterns struct {
	Rules []Rule
}

// Parse reads a read-paths.txt-format stream. label is used in
// error messages (typically the source path or "<inline>").
func Parse(r io.Reader, label string) (*Patterns, error) {
	pp := &Patterns{}
	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s:%d: expected '<include|exclude> <pattern>', got %q", label, lineNum, raw)
		}
		var include bool
		switch fields[0] {
		case "include":
			include = true
		case "exclude":
			include = false
		default:
			return nil, fmt.Errorf("%s:%d: unknown rule %q (want include or exclude)", label, lineNum, fields[0])
		}
		pp.Rules = append(pp.Rules, Rule{
			Include: include,
			Pattern: strings.Join(fields[1:], " "),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return pp, nil
}

// Format renders a Patterns back to read-paths.txt syntax.
// Round-trips Parse → Format → Parse losslessly. Used by
// write-a to emit the resolved per-element pattern set
// alongside the rendered srckey artifacts so cmd/audit-
// narrowing has a per-element surface to consume.
func (pp *Patterns) Format() string {
	if pp == nil || len(pp.Rules) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range pp.Rules {
		if r.Include {
			b.WriteString("include ")
		} else {
			b.WriteString("exclude ")
		}
		b.WriteString(r.Pattern)
		b.WriteByte('\n')
	}
	return b.String()
}

// Match reports whether path is "covered" by the patterns —
// i.e., its content contributes to the narrowed view.
//
//   - nil / empty rules: true (conservative default; nothing
//     narrowed away).
//   - At least one include rule: covered iff some include rule
//     matches AND no exclude rule matches.
//   - Only exclude rules: covered iff no exclude rule matches.
//
// Mirrors cmd/write-a's matchesSrckeyPatterns.
func (pp *Patterns) Match(path string) bool {
	if pp == nil || len(pp.Rules) == 0 {
		return true
	}
	hasInclude := false
	for _, r := range pp.Rules {
		if r.Include {
			hasInclude = true
			break
		}
	}
	matched := !hasInclude
	if hasInclude {
		for _, r := range pp.Rules {
			if r.Include && matchPattern(r.Pattern, path) {
				matched = true
				break
			}
		}
	}
	if matched {
		for _, r := range pp.Rules {
			if !r.Include && matchPattern(r.Pattern, path) {
				matched = false
				break
			}
		}
	}
	return matched
}

// matchPattern matches a path against a glob pattern with **
// support. See package doc for grammar.
func matchPattern(pattern, path string) bool {
	return matchPatternRec(pattern, path)
}

func matchPatternRec(pattern, path string) bool {
	for len(pattern) > 0 {
		if strings.HasPrefix(pattern, "**") {
			rest := pattern[2:]
			if rest == "" {
				return true
			}
			if strings.HasPrefix(rest, "/") {
				if matchPatternRec(rest[1:], path) {
					return true
				}
			}
			for i := 0; i <= len(path); i++ {
				if matchPatternRec(rest, path[i:]) {
					return true
				}
			}
			return false
		}
		c := pattern[0]
		switch c {
		case '*':
			rest := pattern[1:]
			if rest == "" {
				return !strings.Contains(path, "/")
			}
			for i := 0; i <= len(path); i++ {
				if i > 0 && path[i-1] == '/' {
					return false
				}
				if matchPatternRec(rest, path[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(path) == 0 || path[0] == '/' {
				return false
			}
			pattern = pattern[1:]
			path = path[1:]
		default:
			if len(path) == 0 || path[0] != c {
				return false
			}
			pattern = pattern[1:]
			path = path[1:]
		}
	}
	return len(path) == 0
}
