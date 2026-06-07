package main

import "testing"

func TestFactsFromArgv(t *testing.T) {
	argv := []string{
		"/usr/bin/gcc", "-DFOO", "-DBAR=1", "-D", "BAZ", "-std=c++17",
		"-I/abs/inc", "-Irel/inc", "-isystem", "/sys/inc",
		"-D__DATE__=\"redacted\"", "-D__TIME__=\"redacted\"", // Bazel stamps — must be filtered
		"-c", "src/foo.cc", "-o", "foo.o",
	}
	f := factsFromArgv(argv)
	for _, want := range []string{"FOO", "BAR=1", "BAZ"} {
		if !f.Defines[want] {
			t.Errorf("missing define %q in %v", want, f.Defines)
		}
	}
	if f.Defines["__DATE__=\"redacted\""] || f.Defines["__TIME__=\"redacted\""] {
		t.Errorf("Bazel reproducibility stamps not filtered: %v", f.Defines)
	}
	if f.Std != "c++17" {
		t.Errorf("std = %q want c++17", f.Std)
	}
	for _, want := range []string{"inc", "inc", "inc"} { // basenames
		if !f.IncludeDir[want] {
			t.Errorf("missing include basename %q in %v", want, f.IncludeDir)
		}
	}
}

func TestIgnoredDefine(t *testing.T) {
	for _, d := range []string{"__DATE__=\"redacted\"", "__TIME__", "__TIMESTAMP__=\"x\""} {
		if !ignoredDefine(d) {
			t.Errorf("%q should be ignored", d)
		}
	}
	for _, d := range []string{"ZLIB_DLL", "HAVE_ZLIB=1", "NDEBUG"} {
		if ignoredDefine(d) {
			t.Errorf("%q should NOT be ignored", d)
		}
	}
}

func TestSourceFromArgv(t *testing.T) {
	if got := sourceFromArgv([]string{"gcc", "-c", "a/b.cc", "-o", "b.o"}); got != "a/b.cc" {
		t.Errorf("got %q want a/b.cc", got)
	}
	// no -c: fall back to the lone source-extension arg
	if got := sourceFromArgv([]string{"gcc", "-O2", "x/y.c", "-o", "y.o"}); got != "x/y.c" {
		t.Errorf("got %q want x/y.c", got)
	}
}

func TestDiff_DefineDelta(t *testing.T) {
	cmake := map[string]tuFacts{
		"foo.cc": {Defines: map[string]bool{"A": true, "B": true}, Std: "c++17", IncludeDir: map[string]bool{}},
		"bar.cc": {Defines: map[string]bool{"X": true}, IncludeDir: map[string]bool{}},
	}
	bazel := map[string]tuFacts{
		// foo: extra ZLIB_DLL, missing B, std mismatch
		"foo.cc": {Defines: map[string]bool{"A": true, "ZLIB_DLL": true}, Std: "c++20", IncludeDir: map[string]bool{}},
		"bar.cc": {Defines: map[string]bool{"X": true}, IncludeDir: map[string]bool{}},
	}
	r := diff(cmake, bazel)
	if r.Matched != 2 {
		t.Fatalf("matched = %d want 2", r.Matched)
	}
	d, ok := r.DefineMismatch["foo.cc"]
	if !ok {
		t.Fatalf("expected foo.cc define mismatch")
	}
	if len(d.MissingInBazel) != 1 || d.MissingInBazel[0] != "B" {
		t.Errorf("missing-in-bazel = %v want [B]", d.MissingInBazel)
	}
	if len(d.ExtraInBazel) != 1 || d.ExtraInBazel[0] != "ZLIB_DLL" {
		t.Errorf("extra-in-bazel = %v want [ZLIB_DLL]", d.ExtraInBazel)
	}
	if _, ok := r.DefineMismatch["bar.cc"]; ok {
		t.Errorf("bar.cc should have no define mismatch")
	}
	if v, ok := r.StdMismatch["foo.cc"]; !ok || v[0] != "c++17" || v[1] != "c++20" {
		t.Errorf("std mismatch = %v ok=%v", v, ok)
	}
}
