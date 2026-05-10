package main

import (
	"reflect"
	"testing"
)

// TestParseInstallRecipeLine covers the canonical install(1)
// recipe line shapes the parser handles. Variable expansion +
// $(DESTDIR) stripping let the recorded source/dest paths be
// install-tree-relative regardless of how the Makefile writes
// the recipe.
func TestParseInstallRecipeLine(t *testing.T) {
	vars := map[string]string{
		"DESTDIR":    "",
		"prefix":     "/usr",
		"bindir":     "/usr/bin",
		"libdir":     "/usr/lib",
		"includedir": "/usr/include",
		"datadir":    "/usr/share",
	}
	cases := []struct {
		name string
		line string
		ok   bool
		want InstallMappingE
	}{
		{
			"canonical install -D -m 0755",
			"install -D -m 0755 app $(DESTDIR)$(bindir)/app",
			true,
			InstallMappingE{Source: "app", Dest: "usr/bin/app", Mode: "0755"},
		},
		{
			"install -D -m 0644 (header)",
			"install -D -m 0644 include/mathlib.h $(DESTDIR)$(includedir)/mathlib.h",
			true,
			InstallMappingE{Source: "include/mathlib.h", Dest: "usr/include/mathlib.h", Mode: "0644"},
		},
		{
			"install -D without -m",
			"install -D libfoo.a $(DESTDIR)$(libdir)/libfoo.a",
			true,
			InstallMappingE{Source: "libfoo.a", Dest: "usr/lib/libfoo.a"},
		},
		{
			"non-install line (skipped)",
			"mkdir -p $(DESTDIR)$(bindir)",
			false, InstallMappingE{},
		},
		{
			"too few positional args",
			"install -D",
			false, InstallMappingE{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseInstallRecipeLine(c.line, vars)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !c.ok {
				return
			}
			if got.Source != c.want.Source || got.Dest != c.want.Dest || got.Mode != c.want.Mode {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestBuildInstallMapping_E2E ties the full install-mapping
// pipeline together: a make-db with a plausible install:
// recipe + a list of converter-emitted CCRules → the right
// source/dest pairs with cross-referenced rule names.
func TestBuildInstallMapping_E2E(t *testing.T) {
	db := &MakeDB{
		Variables: map[string]string{
			"DESTDIR":    "",
			"prefix":     "/usr",
			"bindir":     "/usr/bin",
			"libexecdir": "/usr/libexec",
			"libdir":     "/usr/lib",
			"includedir": "/usr/include",
		},
		Rules: map[string]MakeRule{
			"install": {
				Target: "install",
				Recipe: []string{
					"install -D -m 0755 app $(DESTDIR)$(bindir)/app",
					"install -D -m 0755 helper $(DESTDIR)$(libexecdir)/helper",
					"install -D -m 0644 libmathlib.a $(DESTDIR)$(libdir)/libmathlib.a",
					"install -D -m 0644 include/mathlib.h $(DESTDIR)$(includedir)/mathlib.h",
				},
			},
		},
	}
	rules := []CCRule{
		{RuleKind: "cc_library", Name: "mathlib"},
		{RuleKind: "cc_binary", Name: "app"},
		{RuleKind: "cc_binary", Name: "helper"},
	}
	im := buildInstallMapping(db, rules)
	if im == nil {
		t.Fatalf("expected non-nil mapping")
	}
	if len(im.Mappings) != 4 {
		t.Fatalf("Mappings len = %d, want 4", len(im.Mappings))
	}
	want := map[string]InstallMappingE{
		"app":               {Source: "app", Dest: "usr/bin/app", Mode: "0755", Rule: "app"},
		"helper":            {Source: "helper", Dest: "usr/libexec/helper", Mode: "0755", Rule: "helper"},
		"libmathlib.a":      {Source: "libmathlib.a", Dest: "usr/lib/libmathlib.a", Mode: "0644", Rule: "mathlib"},
		"include/mathlib.h": {Source: "include/mathlib.h", Dest: "usr/include/mathlib.h", Mode: "0644"},
	}
	got := map[string]InstallMappingE{}
	for _, m := range im.Mappings {
		got[m.Source] = m
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("install mapping mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestExpandMakeVars exercises the recursive expansion: nested
// $(prefix) → $(bindir) → /usr/bin. Bounded depth defends
// against malformed cycles.
func TestExpandMakeVars(t *testing.T) {
	vars := map[string]string{
		"prefix": "/usr",
		"bindir": "$(prefix)/bin", // nested
	}
	got := expandMakeVars("$(bindir)/app", vars)
	if got != "/usr/bin/app" {
		t.Errorf("expandMakeVars = %q, want %q", got, "/usr/bin/app")
	}
	// Cycle: bounded depth prevents infinite loop.
	cyclic := map[string]string{
		"A": "$(B)",
		"B": "$(A)",
	}
	got = expandMakeVars("$(A)", cyclic)
	// We don't assert exact output — just that it terminates.
	if len(got) > 1024 {
		t.Errorf("expandMakeVars on cycle produced suspiciously long output (%d)", len(got))
	}
}
