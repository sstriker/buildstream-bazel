package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Note: TestMatchPattern moved to
// internal/readpaths/patterns_test.go (TestMatch_GlobGrammar)
// when the matcher was lifted into the shared package.

func TestLoadReadPathsPatterns_Absent(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "x.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pp, err := loadReadPathsPatterns(bst)
	if err != nil {
		t.Fatal(err)
	}
	if pp != nil {
		t.Errorf("expected nil patterns when file absent, got %+v", pp)
	}
}

func TestLoadReadPathsPatterns_Parse(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "x.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := `# top-level comment
include CMakeLists.txt

include cmake/*.cmake
exclude include/internal/*
`
	if err := os.WriteFile(strings.TrimSuffix(bst, ".bst")+".read-paths.txt", []byte(patterns), 0o644); err != nil {
		t.Fatal(err)
	}
	pp, err := loadReadPathsPatterns(bst)
	if err != nil {
		t.Fatal(err)
	}
	if pp == nil {
		t.Fatal("expected non-nil patterns")
	}
	if len(pp.Rules) != 3 {
		t.Errorf("rules count = %d, want 3 (%+v)", len(pp.Rules), pp.Rules)
	}
	if !pp.Rules[0].Include || pp.Rules[0].Pattern != "CMakeLists.txt" {
		t.Errorf("rule 0 wrong: %+v", pp.Rules[0])
	}
	if pp.Rules[2].Include || pp.Rules[2].Pattern != "include/internal/*" {
		t.Errorf("rule 2 wrong: %+v", pp.Rules[2])
	}
}

func TestLoadReadPathsPatterns_Errors(t *testing.T) {
	tmp := t.TempDir()
	bst := filepath.Join(tmp, "x.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"unknown rule":    "foobar pattern",
		"missing pattern": "include",
		"invalid keyword": "INCLUDE Cmake",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(strings.TrimSuffix(bst, ".bst")+".read-paths.txt", []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadReadPathsPatterns(bst)
			if err == nil {
				t.Errorf("expected parse error for %q", body)
			}
		})
	}
}

func TestApplyReadPathsPatterns_Default(t *testing.T) {
	universe := []string{"a", "b/c", "d/e/f"}
	real, zero := applyReadPathsPatterns(nil, universe)
	if len(real) != 3 || len(zero) != 0 {
		t.Errorf("nil patterns should leave everything real; got real=%v zero=%v", real, zero)
	}
}

func TestApplyReadPathsPatterns_IncludeOnly(t *testing.T) {
	pp := &readPathsPatterns{Rules: []patternRule{
		{Include: true, Pattern: "CMakeLists.txt"},
		{Include: true, Pattern: "include/**/*.h"},
	}}
	universe := []string{
		"CMakeLists.txt",
		"src/main.c",
		"include/api.h",
		"include/sub/inner.h",
		"docs/readme.md",
	}
	real, zero := applyReadPathsPatterns(pp, universe)
	wantReal := map[string]bool{
		"CMakeLists.txt":      true,
		"include/api.h":       true,
		"include/sub/inner.h": true,
	}
	wantZero := map[string]bool{
		"src/main.c":     true,
		"docs/readme.md": true,
	}
	for _, r := range real {
		if !wantReal[r] {
			t.Errorf("unexpected real path: %s", r)
		}
	}
	for _, z := range zero {
		if !wantZero[z] {
			t.Errorf("unexpected zero path: %s", z)
		}
	}
	if len(real) != len(wantReal) {
		t.Errorf("real count: got %d want %d (%v)", len(real), len(wantReal), real)
	}
}

func TestApplyReadPathsPatterns_ExcludeRefinesInclude(t *testing.T) {
	pp := &readPathsPatterns{Rules: []patternRule{
		{Include: true, Pattern: "include/**/*.h"},
		{Include: false, Pattern: "include/internal/*"},
	}}
	universe := []string{
		"include/api.h",
		"include/internal/private.h",
	}
	real, zero := applyReadPathsPatterns(pp, universe)
	if len(real) != 1 || real[0] != "include/api.h" {
		t.Errorf("expected only include/api.h real; got %v", real)
	}
	if len(zero) != 1 || zero[0] != "include/internal/private.h" {
		t.Errorf("expected internal/private.h zeroed; got %v", zero)
	}
}

func TestApplyReadPathsPatterns_ExcludeOnlyDefaultsToReal(t *testing.T) {
	pp := &readPathsPatterns{Rules: []patternRule{
		{Include: false, Pattern: "build/*"},
	}}
	universe := []string{"src/a.c", "build/cache"}
	real, zero := applyReadPathsPatterns(pp, universe)
	if len(real) != 1 || real[0] != "src/a.c" {
		t.Errorf("default-to-real should preserve src/a.c; got %v", real)
	}
	if len(zero) != 1 || zero[0] != "build/cache" {
		t.Errorf("build/cache should be zeroed; got %v", zero)
	}
}

func TestApplyReadPathsPatterns_CMakeListsAlwaysReal(t *testing.T) {
	pp := &readPathsPatterns{Rules: []patternRule{
		{Include: true, Pattern: "include/*.h"},
	}}
	universe := []string{
		"CMakeLists.txt",
		"src/CMakeLists.txt",
		"src/main.c",
	}
	real, _ := applyReadPathsPatterns(pp, universe)
	cmakeFound := 0
	for _, r := range real {
		if strings.HasSuffix(r, "CMakeLists.txt") {
			cmakeFound++
		}
	}
	if cmakeFound != 2 {
		t.Errorf("expected both CMakeLists.txt files real; got %v", real)
	}
}

func TestComposeReadPathsPatterns_Layers(t *testing.T) {
	defaults := &readPathsPatterns{Rules: []patternRule{
		{Include: true, Pattern: "**/CMakeLists.txt"},
		{Include: true, Pattern: "cmake/*.cmake"},
	}}
	overrides := &readPathsPatterns{Rules: []patternRule{
		{Include: true, Pattern: "extra/*.txt"},
		{Include: false, Pattern: "cmake/internal/*"},
	}}
	got := composeReadPathsPatterns(defaults, overrides)
	if got == nil || len(got.Rules) != 4 {
		t.Fatalf("composed rules: got %+v, want 4 rules", got)
	}
	// Order: defaults first, overrides last (so per-element
	// rules can refine — the include-followed-by-exclude pattern
	// for narrowing internal files relies on this ordering).
	wantPatterns := []string{
		"**/CMakeLists.txt",
		"cmake/*.cmake",
		"extra/*.txt",
		"cmake/internal/*",
	}
	for i, want := range wantPatterns {
		if got.Rules[i].Pattern != want {
			t.Errorf("rule %d: got %q, want %q", i, got.Rules[i].Pattern, want)
		}
	}
}

func TestComposeReadPathsPatterns_NilCases(t *testing.T) {
	if composeReadPathsPatterns(nil, nil) != nil {
		t.Errorf("nil + nil should be nil (default no-narrowing)")
	}
	a := &readPathsPatterns{Rules: []patternRule{{Include: true, Pattern: "x"}}}
	if composeReadPathsPatterns(nil, a) != a {
		t.Errorf("nil + a should pass through a")
	}
	if composeReadPathsPatterns(a, nil) != a {
		t.Errorf("a + nil should pass through a")
	}
}
