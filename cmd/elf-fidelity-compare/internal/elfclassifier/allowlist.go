package elfclassifier

import (
	"bufio"
	"bytes"
	"os"
	"strings"
)

// Allowlist is a per-member set of benign dynamic-section deltas the operator
// has reviewed: DT_NEEDED sonames, version-node names, or SONAME values that
// differ for a known-good reason. Mirrors cmd/fidelity-compare's allowlist
// shape (exact entries + `prefix:` entries) — the two lenses can't share a Go
// type (each lives under its own cmd/.../internal tree), but the file format is
// identical so an operator only learns it once.
type Allowlist struct {
	Symbols  map[string]bool
	Prefixes []string
}

// Match reports whether name is allowlisted, by exact match or any prefix.
func (a Allowlist) Match(name string) bool {
	if a.Symbols[name] {
		return true
	}
	for _, p := range a.Prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// LoadAllowlist reads a per-member allowlist file. Format: blank lines and
// '#' comments ignored; `<name>` is an exact-match entry; `prefix:<p>` matches
// any name starting with <p>. An empty path yields an empty allowlist.
func LoadAllowlist(path string) (Allowlist, error) {
	out := Allowlist{Symbols: map[string]bool{}}
	if path == "" {
		return out, nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	s := bufio.NewScanner(bytes.NewReader(buf))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "prefix:"); ok {
			if rest = strings.TrimSpace(rest); rest != "" {
				out.Prefixes = append(out.Prefixes, rest)
			}
			continue
		}
		out.Symbols[line] = true
	}
	return out, nil
}
