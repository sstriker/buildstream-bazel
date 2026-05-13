package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/ir"
)

// TestEmit_KeepMarkers covers Phase 7a's `# keep` injection
// across the rule kinds the cc emitter produces. Each
// emitted attribute the conventions doc tags as load-
// bearing should pick up a trailing `# keep` comment, and
// the rule-kind whole-rule keeps (genrule, filegroup,
// package(...)) should land their markers too.
func TestEmit_KeepMarkers(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{
			{
				Name:       "lib",
				Kind:       ir.KindCCLibrary,
				Srcs:       []string{"a.c"},
				Hdrs:       []string{"a.h"},
				Copts:      []string{"-O3"},
				Defines:    []string{"NDEBUG"},
				LinkOpts:   []string{"-lm"},
				Includes:   []string{"include"},
				Linkstatic: true,
				Alwayslink: false,
				Tags:       []string{"my-tag"},
			},
			{
				Name:        "gen",
				Kind:        ir.KindGenrule,
				GenruleCmd:  "touch $@",
				GenruleOuts: []string{"x.txt"},
			},
		},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(out)
	for _, want := range []string{
		`package(default_visibility = ["//visibility:public"])  # keep`,
		`copts = ["-O3"],  # keep`,
		`defines = ["NDEBUG"],  # keep`,
		`linkopts = ["-lm"],  # keep`,
		`includes = ["include"],  # keep`,
		`linkstatic = True,  # keep`,
		`tags = ["my-tag"],  # keep`,
		// Whole-rule keep on the genrule's closing `)` line.
		`)  # keep`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("emitted BUILD missing %q\n%s", want, body)
		}
	}
	// `name` and `srcs` on cc_library should NOT receive
	// keep markers (not in ccKeepAttrs).
	for _, dontWant := range []string{
		`name = "lib",  # keep`,
		`srcs = ["a.c"],  # keep`,
	} {
		if strings.Contains(body, dontWant) {
			t.Errorf("emitted BUILD unexpectedly has %q\n%s", dontWant, body)
		}
	}
}

// TestEmit_KeepMarkers_Idempotent verifies that re-running
// canonicalize on an already-marked file doesn't double up
// the keep markers — the hasKeepSuffix guard makes
// addKeepMarkers idempotent.
func TestEmit_KeepMarkers_Idempotent(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:    "lib",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"a.c"},
			Copts:   []string{"-O3"},
			Defines: []string{"NDEBUG"},
		}},
	}
	out1, _ := Emit(pkg)
	// Re-canonicalize the already-emitted body — simulates
	// what write-a does when it re-canonicalizes BUILD.bazel-
	// named files via its own buildtools pass.
	out2, err := canonicalize(out1)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	// Each marker should appear exactly once per attribute.
	for _, attr := range []string{"copts", "defines"} {
		count := strings.Count(string(out2), attr+` = `)
		marks := strings.Count(string(out2), attr+` = ["-O3"],  # keep`)
		marks2 := strings.Count(string(out2), attr+` = ["NDEBUG"],  # keep`)
		if count != 1 {
			t.Errorf("attr %q appears %d times, want 1", attr, count)
		}
		// Sum of either value's keep variants should be 1.
		if marks+marks2 > 1 {
			t.Errorf("attr %q has duplicate keep markers", attr)
		}
	}
}
