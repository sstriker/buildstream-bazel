package main

import (
	"strings"
	"testing"
)

// TestEmit_StrictPyBinary covers Phase 5's strict-mode
// emission shape: when Target.Main is set, emit a single
// py_binary pointing directly at the module file with no
// sibling shim genrule.
func TestEmit_StrictPyBinary(t *testing.T) {
	targets := []Target{{
		Name:       "greet",
		Kind:       KindPyBinary,
		Main:       "demo/cli.py",
		Srcs:       []string{"demo/cli.py"},
		EntryDep:   ":demo",
		Visibility: []string{"//visibility:public"},
	}}
	got := string(Emit(targets))
	// No genrule when Main is set.
	if strings.Contains(got, "genrule(") {
		t.Errorf("strict shape emitted a genrule:\n%s", got)
	}
	// py_binary points at the module file directly.
	for _, want := range []string{
		`name = "greet"`,
		`srcs = ["demo/cli.py"]`,
		`main = "demo/cli.py"`,
		`deps = [":demo"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("strict shape missing %q\n%s", want, got)
		}
	}
}

// TestEmit_ShimPyBinary covers the legacy shim path stays
// intact when Target.Main is empty.
func TestEmit_ShimPyBinary(t *testing.T) {
	targets := []Target{{
		Name:        "greet",
		Kind:        KindPyBinary,
		EntryModule: "demo.cli",
		EntryFunc:   "main",
		EntryDep:    ":demo",
		Visibility:  []string{"//visibility:public"},
	}}
	got := string(Emit(targets))
	if !strings.Contains(got, "genrule(") {
		t.Errorf("shim shape didn't emit a genrule:\n%s", got)
	}
	if !strings.Contains(got, `name = "greet_entry"`) {
		t.Errorf("shim shape missing entry-shim genrule name:\n%s", got)
	}
	if !strings.Contains(got, `srcs = [":greet_entry"]`) {
		t.Errorf("shim shape's py_binary srcs not pointing at shim:\n%s", got)
	}
}
