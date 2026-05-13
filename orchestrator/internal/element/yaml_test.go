package element_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/element"
)

const fixtureRoot = "../../testdata/fdsdk-subset"

func TestParseElement_ManualWithBareDeps(t *testing.T) {
	el, err := element.ParseElement(
		filepath.Join(fixtureRoot, "elements/base.bst"),
		"base",
	)
	if err != nil {
		t.Fatal(err)
	}
	if el.Kind != "manual" {
		t.Errorf("Kind = %q, want manual", el.Kind)
	}
	if len(el.Depends) != 0 {
		t.Errorf("Depends = %v, want empty", el.Depends)
	}
	if len(el.Sources) != 1 {
		t.Fatalf("Sources = %d, want 1", len(el.Sources))
	}
	if el.Sources[0].Kind != "local" {
		t.Errorf("Sources[0].Kind = %q, want local", el.Sources[0].Kind)
	}
}

func TestParseElement_CMakeWithMixedDeps(t *testing.T) {
	el, err := element.ParseElement(
		filepath.Join(fixtureRoot, "elements/components/uses-hello.bst"),
		"components/uses-hello",
	)
	if err != nil {
		t.Fatal(err)
	}
	if el.Kind != "cmake" {
		t.Errorf("Kind = %q, want cmake", el.Kind)
	}
	if got := len(el.Depends); got != 2 {
		t.Fatalf("Depends = %d, want 2", got)
	}
	// Bare-string form
	if el.Depends[0].Filename != "base.bst" || el.Depends[0].Type != "" {
		t.Errorf("Depends[0] = %+v, want {base.bst, type=\"\"}", el.Depends[0])
	}
	// Dict form with type
	if el.Depends[1].Filename != "components/hello.bst" || el.Depends[1].Type != "build" {
		t.Errorf("Depends[1] = %+v, want {components/hello.bst, build}", el.Depends[1])
	}
}

func TestReadProject_BuildsAndLooksUp(t *testing.T) {
	p, err := element.ReadProject(fixtureRoot, "elements")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"base", "components/hello", "components/uses-hello"}
	if got := p.SortedNames(); !sameStringSlice(got, want) {
		t.Errorf("SortedNames = %v, want %v", got, want)
	}

	hello := p.Lookup("components/hello.bst")
	if hello == nil {
		t.Fatal("Lookup(components/hello.bst) = nil")
	}
	if hello.Name != "components/hello" {
		t.Errorf("Lookup name = %q", hello.Name)
	}
}

func TestReadProject_RejectsMissingDir(t *testing.T) {
	_, err := element.ReadProject("/nonexistent", "elements")
	if err == nil {
		t.Error("expected error for missing root")
	}
}

func TestParseElement_RejectsListAtTopLevel(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.bst")
	if err := writeFile(bad, "- a\n- b\n"); err != nil {
		t.Fatal(err)
	}
	_, err := element.ParseElement(bad, "bad")
	if err == nil || !strings.Contains(err.Error(), "bad.bst") {
		t.Errorf("err = %v, want path-mentioning failure", err)
	}
}

// sameStringSlice / writeFile helpers — kept tiny on purpose so the test
// file's signal-to-noise stays high.

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeFile(path, body string) error {
	return writeBytes(path, []byte(body))
}
