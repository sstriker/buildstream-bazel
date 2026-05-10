package main

import (
	"reflect"
	"testing"
)

// TestParseMakeDB_Variables covers top-level variable
// assignments. The four main flavors (=, :=, ::=, ?=) all
// land in db.Variables.
func TestParseMakeDB_Variables(t *testing.T) {
	body := []byte(`# Variables

# environment
CC = cc
# default
CFLAGS := -O2
# automatic
HOST_CFLAGS ::= -O3 -fPIC
# conditional (already set elsewhere)
CXXFLAGS ?= -O0
prefix = /usr/local
`)
	db := parseMakeDB(body)
	cases := map[string]string{
		"CC":          "cc",
		"CFLAGS":      "-O2",
		"HOST_CFLAGS": "-O3 -fPIC",
		"CXXFLAGS":    "-O0",
		"prefix":      "/usr/local",
	}
	for k, v := range cases {
		if got := db.Variables[k]; got != v {
			t.Errorf("Variables[%q] = %q, want %q", k, got, v)
		}
	}
}

// TestParseMakeDB_Rules covers the rule + recipe shape make
// emits in the `# Files` section. Tab-prefixed lines after a
// rule are its recipe.
func TestParseMakeDB_Rules(t *testing.T) {
	body := []byte(`# Files

greet: greet.o
#  Last modified 2026-05-01
#  recipe to execute (from 'Makefile', line 11):
	$(CC) $(CFLAGS) -o $@ $^

greet.o: greet.c greet.h
#  recipe to execute (from 'Makefile', line 14):
	$(CC) $(CFLAGS) -c $<

install: greet
#  recipe to execute (from 'Makefile', line 17):
	install -D -m 0755 greet $(DESTDIR)$(bindir)/greet
`)
	db := parseMakeDB(body)

	greet, ok := db.Rules["greet"]
	if !ok {
		t.Fatalf("missing rule for greet")
	}
	if !reflect.DeepEqual(greet.Prereqs, []string{"greet.o"}) {
		t.Errorf("greet prereqs = %v, want [greet.o]", greet.Prereqs)
	}
	if len(greet.Recipe) != 1 || greet.Recipe[0] != "$(CC) $(CFLAGS) -o $@ $^" {
		t.Errorf("greet recipe = %v", greet.Recipe)
	}

	greetO, ok := db.Rules["greet.o"]
	if !ok {
		t.Fatalf("missing rule for greet.o")
	}
	if !reflect.DeepEqual(greetO.Prereqs, []string{"greet.c", "greet.h"}) {
		t.Errorf("greet.o prereqs = %v", greetO.Prereqs)
	}

	install, ok := db.Rules["install"]
	if !ok {
		t.Fatalf("missing rule for install (phony)")
	}
	if len(install.Recipe) != 1 ||
		install.Recipe[0] != "install -D -m 0755 greet $(DESTDIR)$(bindir)/greet" {
		t.Errorf("install recipe = %v", install.Recipe)
	}
}

// TestParseMakeDB_RejectsPatternAndTargetSpecificVars covers
// the two edge cases the spike-quality parser intentionally
// drops: pattern rules (`%.o: %.c`) and target-specific
// variable assignments (`foo: CC = clang`). Out of scope for
// the simple rule-graph extraction; surface as no entry.
func TestParseMakeDB_RejectsPatternAndTargetSpecificVars(t *testing.T) {
	body := []byte(`# Files

%.o: %.c
	$(CC) -c $< -o $@

foo: CC = clang
foo: foo.c
	$(CC) foo.c -o foo
`)
	db := parseMakeDB(body)

	// Pattern rule dropped.
	if _, ok := db.Rules["%.o"]; ok {
		t.Errorf("pattern rule shouldn't surface in Rules")
	}

	// Target-specific assignment (`foo: CC = clang`) doesn't
	// land in Variables (it's per-target, not global), and
	// shouldn't kill the rule for `foo`.
	if got := db.Variables["CC"]; got == "clang" {
		t.Errorf("target-specific CC=clang leaked to global Variables")
	}
	foo, ok := db.Rules["foo"]
	if !ok {
		t.Fatalf("missing rule for foo")
	}
	if !reflect.DeepEqual(foo.Prereqs, []string{"foo.c"}) {
		t.Errorf("foo prereqs = %v, want [foo.c]", foo.Prereqs)
	}
}

// TestParseMakeDB_Empty surfaces the tolerance contract:
// empty / malformed make-db files produce a non-nil empty
// db rather than an error. The genrule's `make -np` capture
// uses `|| true` — we may receive nothing.
func TestParseMakeDB_Empty(t *testing.T) {
	for _, body := range []string{"", "garbage that isn't make output\n"} {
		db := parseMakeDB([]byte(body))
		if db == nil {
			t.Errorf("parseMakeDB(%q) = nil, want non-nil", body)
		}
	}
}
