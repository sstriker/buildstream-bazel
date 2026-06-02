package bazel

import "testing"

// globSrcName must be unique per (rel, pattern): distinct globs that share a
// directory + extension — GLOB "*.td" vs GLOB_RECURSE "**/*.td", or "*.txt"
// vs "foo*.txt" — must not collide into one filegroup name, which would
// wrongly dedupe them and point some genrules at the wrong glob.
func TestGlobSrcNameUniquePerPattern(t *testing.T) {
	cases := [][2]string{
		{"", "*.td"},
		{"", "**/*.td"},
		{"", "*.txt"},
		{"", "foo*.txt"},
		{"llvm", "*.td"},
		{"llvm", "**/*.td"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		key := c[0] + "|" + c[1]
		name := globSrcName(c[0], c[1])
		if prev, ok := seen[name]; ok {
			t.Errorf("name collision: %q produced by both %q and %q", name, prev, key)
		}
		seen[name] = key
		if again := globSrcName(c[0], c[1]); again != name {
			t.Errorf("globSrcName(%q, %q) not deterministic: %q != %q", c[0], c[1], again, name)
		}
	}
}
