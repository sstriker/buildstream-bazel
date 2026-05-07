package readpaths_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/readpaths"
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

func TestFormat_NilOrEmpty(t *testing.T) {
	if got := (*readpaths.Patterns)(nil).Format(); got != "" {
		t.Errorf("nil Format = %q, want empty", got)
	}
	if got := (&readpaths.Patterns{}).Format(); got != "" {
		t.Errorf("empty Format = %q, want empty", got)
	}
}
