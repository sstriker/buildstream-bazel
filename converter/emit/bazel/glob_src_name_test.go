package bazel

import (
	"regexp"
	"testing"
)

// globSrcName must (a) be a valid Bazel target name even for glob patterns
// whose extension carries glob syntax ("*.[ch]", "*.?pp"), and (b) be
// unique per (rel, pattern) so distinct globs sharing a directory +
// extension — GLOB "*.td" vs GLOB_RECURSE "**/*.td", or "*.txt" vs
// "foo*.txt" — don't collide into one (wrongly deduped) filegroup.
func TestGlobSrcNameUniquePerPattern(t *testing.T) {
	cases := [][2]string{
		{"", "*.td"},
		{"", "**/*.td"},
		{"", "*.txt"},
		{"", "foo*.txt"},
		{"llvm", "*.td"},
		{"llvm", "**/*.td"},
		{"", "*.[ch]"}, // exotic: must not leak '[' ']' into the name
		{"", "*.?pp"},  // exotic: must not leak '?' into the name
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	seen := map[string]string{}
	for _, c := range cases {
		key := c[0] + "|" + c[1]
		name := globSrcName(c[0], c[1])
		if !valid.MatchString(name) {
			t.Errorf("globSrcName(%q, %q) = %q is not a valid Bazel target name", c[0], c[1], name)
		}
		if prev, ok := seen[name]; ok {
			t.Errorf("name collision: %q produced by both %q and %q", name, prev, key)
		}
		seen[name] = key
		if again := globSrcName(c[0], c[1]); again != name {
			t.Errorf("globSrcName(%q, %q) not deterministic: %q != %q", c[0], c[1], again, name)
		}
	}
}
