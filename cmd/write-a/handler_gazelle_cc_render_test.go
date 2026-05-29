package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderGazelleCCProjectB renders project B for a single trivial
// kind:cmake element with the gazelleCC global set to ccOn, and
// returns project B's MODULE.bazel + root BUILD.bazel text.
func renderGazelleCCProjectB(t *testing.T, ccOn bool) (moduleBazel, rootBUILD string) {
	t.Helper()
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\nadd_library(thelib STATIC lib.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "lib.c"), []byte("int f(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}

	prev := gazelleCC
	gazelleCC = ccOn
	t.Cleanup(func() { gazelleCC = prev })

	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	mb, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	rb, err := os.ReadFile(filepath.Join(outB, "BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	return string(mb), string(rb)
}

// TestWriter_GazelleCC_On_Wiring covers --gazelle-cc on: project B's
// MODULE.bazel gains the gazelle / gazelle_cc / rules_go bazel_deps
// and its root BUILD.bazel gains the gazelle_binary (compiled with
// @gazelle_cc//language/cc) + gazelle(name="gazelle") pair, loaded
// from @gazelle//:def.bzl, so `bazel run //:gazelle` maintains the
// converted BUILDs.
func TestWriter_GazelleCC_On_Wiring(t *testing.T) {
	mb, rb := renderGazelleCCProjectB(t, true)

	for _, want := range []string{
		`bazel_dep(name = "gazelle", version = "0.46.0")`,
		`bazel_dep(name = "gazelle_cc", version = "0.5.0")`,
		`bazel_dep(name = "rules_go", version = "0.59.0")`,
	} {
		if !strings.Contains(mb, want) {
			t.Errorf("--gazelle-cc MODULE.bazel missing %q\n%s", want, mb)
		}
	}

	for _, want := range []string{
		`load("@gazelle//:def.bzl", "gazelle", "gazelle_binary")`,
		`gazelle_binary(`,
		`languages = ["@gazelle_cc//language/cc"]`,
		`name = "gazelle"`,
		`gazelle = ":gazelle_cc_bin"`,
	} {
		if !strings.Contains(rb, want) {
			t.Errorf("--gazelle-cc root BUILD.bazel missing %q\n%s", want, rb)
		}
	}
}

// TestWriter_GazelleCC_Off_ByteIdentical covers the default (off):
// project B's MODULE.bazel + root BUILD.bazel carry none of the
// gazelle_cc wiring and match the pre-flag content byte-for-byte.
func TestWriter_GazelleCC_Off_ByteIdentical(t *testing.T) {
	mb, rb := renderGazelleCCProjectB(t, false)

	// MODULE.bazel's Phase-7b config block legitimately mentions
	// gazelle_cc in comments today, so assert on the bazel_dep
	// lines the flag adds, not the bare module name.
	for _, dontWant := range []string{
		`bazel_dep(name = "gazelle"`,
		`bazel_dep(name = "gazelle_cc"`,
		`bazel_dep(name = "rules_go"`,
	} {
		if strings.Contains(mb, dontWant) {
			t.Errorf("default MODULE.bazel unexpectedly has %q\n%s", dontWant, mb)
		}
	}
	for _, dontWant := range []string{
		`gazelle_cc`,
		`gazelle_binary`,
		`@gazelle//:def.bzl`,
	} {
		if strings.Contains(rb, dontWant) {
			t.Errorf("default root BUILD.bazel unexpectedly has %q\n%s", dontWant, rb)
		}
	}

	// The off-path root BUILD.bazel is the historical single-line
	// comment marker, byte-for-byte.
	const wantRoot = "# project B root; per-element packages live under elements/<name>/.\n"
	if rb != wantRoot {
		t.Errorf("default root BUILD.bazel not byte-identical:\n got: %q\nwant: %q", rb, wantRoot)
	}
}
