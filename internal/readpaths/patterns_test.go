package readpaths_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/readpaths"
)

func TestParse_RoundTrip(t *testing.T) {
	in := `# header comment
include CMakeLists.txt
include cmake/*.cmake
exclude include/internal/*
include include/**/*.h
`
	pp, err := readpaths.Parse(strings.NewReader(in), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []readpaths.Rule{
		{Include: true, Pattern: "CMakeLists.txt"},
		{Include: true, Pattern: "cmake/*.cmake"},
		{Include: false, Pattern: "include/internal/*"},
		{Include: true, Pattern: "include/**/*.h"},
	}
	if !reflect.DeepEqual(pp.Rules, want) {
		t.Errorf("rules = %v, want %v", pp.Rules, want)
	}

	// Format → Parse round-trips losslessly.
	out := pp.Format()
	pp2, err := readpaths.Parse(strings.NewReader(out), "round-trip")
	if err != nil {
		t.Fatalf("re-parse Format output: %v", err)
	}
	if !reflect.DeepEqual(pp.Rules, pp2.Rules) {
		t.Errorf("round-trip mismatch\nbefore: %v\nafter: %v", pp.Rules, pp2.Rules)
	}
}

func TestMatch_NilOrEmpty(t *testing.T) {
	if !(*readpaths.Patterns)(nil).Match("foo") {
		t.Error("nil patterns should match every path")
	}
	if !(&readpaths.Patterns{}).Match("foo") {
		t.Error("empty patterns should match every path")
	}
}

func TestMatch_IncludeOnly(t *testing.T) {
	pp := &readpaths.Patterns{Rules: []readpaths.Rule{
		{Include: true, Pattern: "CMakeLists.txt"},
		{Include: true, Pattern: "cmake/*.cmake"},
		{Include: true, Pattern: "include/**/*.h"},
	}}
	cases := map[string]bool{
		"CMakeLists.txt":      true,
		"cmake/foo.cmake":     true,
		"include/foo.h":       true,
		"include/sub/bar.h":   true,
		"src/foo.c":           false,
		"cmake/sub/bar.cmake": false, // * doesn't cross /
	}
	for path, want := range cases {
		if got := pp.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMatch_ExcludeOnly(t *testing.T) {
	pp := &readpaths.Patterns{Rules: []readpaths.Rule{
		{Include: false, Pattern: "**/internal/**"},
	}}
	cases := map[string]bool{
		"foo.c":                true, // default-include when no include rules
		"include/foo.h":        true,
		"include/internal/x.h": false,
		"src/internal/y.c":     false,
	}
	for path, want := range cases {
		if got := pp.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMatch_IncludeThenExclude(t *testing.T) {
	pp := &readpaths.Patterns{Rules: []readpaths.Rule{
		{Include: true, Pattern: "include/**"},
		{Include: false, Pattern: "include/internal/**"},
	}}
	cases := map[string]bool{
		"include/foo.h":        true,
		"include/sub/bar.h":    true,
		"include/internal/x.h": false,
		"src/foo.c":            false,
	}
	for path, want := range cases {
		if got := pp.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParse_ErrorsOnUnknownRule(t *testing.T) {
	_, err := readpaths.Parse(strings.NewReader("bogus pattern\n"), "test")
	if err == nil {
		t.Fatal("expected error on unknown rule")
	}
	if !strings.Contains(err.Error(), "test:1") {
		t.Errorf("error %q missing line number", err)
	}
}

// TestMatch_GlobGrammar exercises the glob matcher's edge
// cases via single-rule include patterns. Each case asserts
// the include-rule grammar (the equivalent of the old
// matchPattern unit test in cmd/write-a, lifted here when the
// matcher was consolidated into this package).
func TestMatch_GlobGrammar(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"CMakeLists.txt", "CMakeLists.txt", true},
		{"CMakeLists.txt", "src/CMakeLists.txt", false},
		{"*", "foo", true},
		{"*", "foo/bar", false},
		{"*.h", "foo.h", true},
		{"*.h", "sub/foo.h", false},
		{"cmake/*.cmake", "cmake/Find.cmake", true},
		{"cmake/*.cmake", "cmake/sub/Find.cmake", false},
		{"include/**/*.h", "include/foo.h", true},
		{"include/**/*.h", "include/sub/foo.h", true},
		{"include/**/*.h", "include/sub/deep/foo.h", true},
		{"include/**/*.h", "src/foo.h", false},
		{"**/*.h", "foo.h", true},
		{"**/*.h", "src/foo.h", true},
		{"**", "anything/at/all", true},
		{"foo/**/bar", "foo/bar", true},
		{"foo/**/bar", "foo/x/bar", true},
		{"foo/**/bar", "foo/x/y/bar", true},
		{"foo/**/bar", "foo/baz", false},
		{"include/internal/*", "include/internal/x.h", true},
		{"include/internal/*", "include/public/x.h", false},
		{"?.c", "a.c", true},
		{"?.c", "ab.c", false},
	}
	for _, c := range cases {
		t.Run(c.pattern+"::"+c.path, func(t *testing.T) {
			pp := &readpaths.Patterns{Rules: []readpaths.Rule{
				{Include: true, Pattern: c.pattern},
			}}
			if got := pp.Match(c.path); got != c.want {
				t.Errorf("Match(%q) on include %q = %v, want %v", c.path, c.pattern, got, c.want)
			}
		})
	}
}

func TestFormat_NilOrEmpty(t *testing.T) {
	if got := (*readpaths.Patterns)(nil).Format(); got != "" {
		t.Errorf("nil Format = %q, want empty", got)
	}
	if got := (&readpaths.Patterns{}).Format(); got != "" {
		t.Errorf("empty Format = %q, want empty", got)
	}
}
