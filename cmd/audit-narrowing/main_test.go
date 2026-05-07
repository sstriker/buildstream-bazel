package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_CmakeOracleUndercoverage exercises the cmake-side oracle
// path: a JSON array of source-relative reads + a patterns file
// where some of those paths aren't covered. Reports the misses
// (sorted), one per line.
func TestRun_CmakeOracleUndercoverage(t *testing.T) {
	tmp := t.TempDir()
	patterns := filepath.Join(tmp, "patterns.txt")
	mustWrite(t, patterns, `# kind:cmake conservative defaults
include CMakeLists.txt
include cmake/*.cmake
`)
	cmakeReads := filepath.Join(tmp, "cmake-reads.json")
	mustWrite(t, cmakeReads, `[
		"CMakeLists.txt",
		"cmake/Toolchain.cmake",
		"src/config.h.in",
		"include/version.h.in"
	]`)
	out := filepath.Join(tmp, "report.txt")

	if err := run(patterns, cmakeReads, "", out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := mustRead(t, out)
	want := "include/version.h.in\nsrc/config.h.in\n"
	if got != want {
		t.Errorf("report mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestRun_CmakeOracleClean: oracle paths all covered → empty report.
func TestRun_CmakeOracleClean(t *testing.T) {
	tmp := t.TempDir()
	patterns := filepath.Join(tmp, "patterns.txt")
	mustWrite(t, patterns, "include CMakeLists.txt\n")
	cmakeReads := filepath.Join(tmp, "cmake-reads.json")
	mustWrite(t, cmakeReads, `["CMakeLists.txt"]`)
	out := filepath.Join(tmp, "report.txt")

	if err := run(patterns, cmakeReads, "", out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := mustRead(t, out); got != "" {
		t.Errorf("clean report should be empty; got %q", got)
	}
}

// TestRun_TraceOracleUndercoverage: extract reads from a
// canonicalized trace.log, diff against patterns.
func TestRun_TraceOracleUndercoverage(t *testing.T) {
	tmp := t.TempDir()
	patterns := filepath.Join(tmp, "patterns.txt")
	mustWrite(t, patterns, `include configure.ac
include **/*.am
include **/*.h
`)
	// Canonicalized trace shape (post-CanonicalizeBytesWith with
	// matching SourceRoot). All openat lines have the `= ?` fd
	// suffix tracenorm.ExtractReads expects.
	trace := filepath.Join(tmp, "trace.log")
	mustWrite(t, trace, `execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
openat(AT_FDCWD, "configure.ac", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "src/Makefile.am", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "include/foo.h", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "scripts/build-aux.sh", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "data/lookup.table", O_RDONLY|O_CLOEXEC) = ?
`)
	out := filepath.Join(tmp, "report.txt")
	if err := run(patterns, "", trace, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := mustRead(t, out)
	want := "data/lookup.table\nscripts/build-aux.sh\n"
	if got != want {
		t.Errorf("report mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestRun_BothOracles: union of cmake reads + trace reads. A path
// flagged by either oracle shows up if patterns don't cover it.
func TestRun_BothOracles(t *testing.T) {
	tmp := t.TempDir()
	patterns := filepath.Join(tmp, "patterns.txt")
	mustWrite(t, patterns, "include CMakeLists.txt\n")
	cmakeReads := filepath.Join(tmp, "cmake-reads.json")
	mustWrite(t, cmakeReads, `["CMakeLists.txt", "cmake/probe.cmake"]`)
	trace := filepath.Join(tmp, "trace.log")
	mustWrite(t, trace, `openat(AT_FDCWD, "scripts/gen-version.sh", O_RDONLY) = ?
openat(AT_FDCWD, "cmake/probe.cmake", O_RDONLY) = ?
`)
	out := filepath.Join(tmp, "report.txt")
	if err := run(patterns, cmakeReads, trace, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := mustRead(t, out)
	want := "cmake/probe.cmake\nscripts/gen-version.sh\n"
	if got != want {
		t.Errorf("union report mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestRun_NilPatternsCoversEverything: empty patterns file →
// matches everything → report is empty even when oracle is busy.
// The conservative-default behaviour: with no narrowing applied,
// every file's content is in srckey, so by construction nothing
// is undercovered.
func TestRun_NilPatternsCoversEverything(t *testing.T) {
	tmp := t.TempDir()
	patterns := filepath.Join(tmp, "patterns.txt")
	mustWrite(t, patterns, "# no rules\n")
	cmakeReads := filepath.Join(tmp, "cmake-reads.json")
	mustWrite(t, cmakeReads, `["a.txt", "b/c.txt", "d/e/f.txt"]`)
	out := filepath.Join(tmp, "report.txt")
	if err := run(patterns, cmakeReads, "", out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := mustRead(t, out); got != "" {
		t.Errorf("no-narrow patterns should produce empty report; got %q", got)
	}
}

// TestRun_MissingPatternsFile is a sanity check: a missing pattern
// file is a hard error so CI doesn't silently report "everything
// fine" because nothing was loaded.
func TestRun_MissingPatternsFile(t *testing.T) {
	tmp := t.TempDir()
	cmakeReads := filepath.Join(tmp, "cmake-reads.json")
	mustWrite(t, cmakeReads, `["a"]`)
	out := filepath.Join(tmp, "report.txt")
	err := run(filepath.Join(tmp, "nonexistent.txt"), cmakeReads, "", out)
	if err == nil {
		t.Fatal("expected error on missing patterns file")
	}
	if !strings.Contains(err.Error(), "load patterns") {
		t.Errorf("error %q missing load-patterns prefix", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
