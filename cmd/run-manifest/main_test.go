package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/regression"
)

// stageBuiltProjectA fakes a built project A: <A>/bazel-bin/elements/<name>/
// per element, with a BUILD.bazel.out (the marker run-manifest keys on)
// plus whatever extra output files the case wants.
func stageBuiltProjectA(t *testing.T, elems map[string]map[string]string) string {
	t.Helper()
	a := t.TempDir()
	for name, files := range elems {
		dir := filepath.Join(a, "bazel-bin", "elements", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for fname, body := range files {
			p := filepath.Join(dir, fname)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return a
}

func TestRun_ConvertedAndDeterminismRoundTripThroughRegression(t *testing.T) {
	// Two converted elements, each with the kind:cmake output trio.
	a := stageBuiltProjectA(t, map[string]map[string]string{
		"prod": {
			"BUILD.bazel.out":         "cc_library(name = \"prod\")\n",
			"cmake-config-bundle.tar": "tar-bytes-prod",
			"read_paths.json":         "[]",
		},
		"cons": {
			"BUILD.bazel.out":         "cc_library(name = \"cons\", deps = [\"//elements/prod\"])\n",
			"cmake-config-bundle.tar": "tar-bytes-cons",
			"read_paths.json":         "[]",
		},
	})
	runDir := t.TempDir()
	if err := run(a, runDir); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The whole point: what we wrote must load cleanly through the
	// consumer, internal/regression.LoadRun.
	r, err := regression.LoadRun(runDir)
	if err != nil {
		t.Fatalf("regression.LoadRun on our output: %v", err)
	}
	got := r.Names()
	if len(got) != 2 || got[0] != "cons" || got[1] != "prod" {
		t.Fatalf("converted elements = %v, want [cons prod]", got)
	}
	for _, name := range got {
		oc := r.Outcomes[name]
		if oc.Converted == nil {
			t.Errorf("%s: Converted outcome is nil", name)
			continue
		}
		if oc.Converted.Fingerprint == "" {
			t.Errorf("%s: empty fingerprint — determinism.json didn't cover it", name)
		}
		// Only BUILD.bazel.out is fingerprinted (the bundle tar is
		// mtime-poisoned and deliberately excluded).
		if len(oc.Converted.Files) != 1 {
			t.Errorf("%s: %d files in fingerprint, want 1 (BUILD.bazel.out)", name, len(oc.Converted.Files))
		}
		if _, ok := oc.Converted.Files["BUILD.bazel.out"]; !ok {
			t.Errorf("%s: fingerprint files = %v, want BUILD.bazel.out", name, oc.Converted.Files)
		}
	}
}

func TestRun_SkipsDirWithoutBuildBazelOut(t *testing.T) {
	// A bazel-bin/elements/<name>/ dir with no BUILD.bazel.out is not a
	// converted element (e.g. a non-action-graph kind, or a stray dir).
	a := stageBuiltProjectA(t, map[string]map[string]string{
		"real":  {"BUILD.bazel.out": "cc_library(name = \"real\")\n"},
		"noout": {"some-other-file": "irrelevant"},
	})
	runDir := t.TempDir()
	if err := run(a, runDir); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := regression.LoadRun(runDir)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got := r.Names(); len(got) != 1 || got[0] != "real" {
		t.Fatalf("converted = %v, want [real]", got)
	}
}

func TestRun_FingerprintShiftsOnOutputContentChange(t *testing.T) {
	// Two runs that differ only in one element's BUILD.bazel.out content
	// must produce a different per-element fingerprint — that's the
	// drift signal regression-diff is built on.
	mk := func(buildOut string) string {
		a := stageBuiltProjectA(t, map[string]map[string]string{
			"e": {"BUILD.bazel.out": buildOut},
		})
		runDir := t.TempDir()
		if err := run(a, runDir); err != nil {
			t.Fatalf("run: %v", err)
		}
		r, err := regression.LoadRun(runDir)
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		return r.Outcomes["e"].Converted.Fingerprint
	}
	fp1 := mk("cc_library(name = \"e\")\n")
	fp2 := mk("cc_library(name = \"e\", srcs = [\"x.c\"])\n")
	if fp1 == "" || fp2 == "" {
		t.Fatal("empty fingerprint")
	}
	if fp1 == fp2 {
		t.Error("fingerprint did not shift on BUILD.bazel.out content change")
	}
}

func TestRun_RejectsUnbuiltProjectA(t *testing.T) {
	// No bazel-bin/elements — `bazel build` wasn't run in project A.
	if err := run(t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("expected error for a project A with no bazel-bin/elements, got nil")
	}
}
