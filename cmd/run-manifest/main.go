// run-manifest snapshots a built project A into the run-manifest shape
// that internal/regression (and orchestrate-diff / orchestrate-history)
// consume, so two write-a + Bazel conversion runs can be diffed for
// output drift.
//
// After `bazel build` in project A, each element's converter genrule
// has produced <A>/bazel-bin/elements/<name>/BUILD.bazel.out (plus,
// for kind:cmake, cmake-config-bundle.tar + read_paths.json).
// run-manifest walks that tree and writes, under <out>/manifest/:
//
//	converted.json    — every element with a BUILD.bazel.out
//	determinism.json  — sha256 of each element's BUILD.bazel.out, keyed
//	                    <name>/BUILD.bazel.out (the per-element
//	                    fingerprint internal/regression diffs on)
//
// Only BUILD.bazel.out is fingerprinted — it is *the* converted
// artifact, and it is byte-deterministic from cmake's configure-time
// view. The sibling cmake-config-bundle.tar is deliberately excluded:
// tar archives embed file mtimes, so the bundle is not byte-stable
// across runs and would make every run spuriously "drift". (Hashing
// the bundle's *contents* rather than its bytes is a possible future
// refinement; BUILD.bazel.out is the load-bearing signal regardless,
// and is what scripts/meta-hello.sh's cache-stability scenarios
// already fingerprint.)
//
// It deliberately does NOT write failures.json. The orchestrator's
// regression model assumed *soft* Tier-1 failures — the run completed
// and failures.json recorded the casualties. The write-a + Bazel path
// is *hard*-fail: a Tier-1 makes the converter genrule exit non-zero
// and `bazel build` in project A fails outright, so a run that exists
// at all has no failed elements. internal/regression tolerates a
// missing failures.json, so regression-diff still does its
// fingerprint-drift job across two successful runs. Re-homing
// newly-failed detection would need write-a to grow a soft-failure
// render mode — a separate decision, noted in
// docs/design/orchestrator-absorption.md.
//
// This is the write-a + Bazel path's replacement for the
// orchestrator's run-level <out>/manifest/ emission, re-homed in the
// orchestrator absorption.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// On-disk schemas — mirror what internal/regression.LoadRun reads.
type convertedDoc struct {
	Version  int                `json:"version"`
	Elements []convertedElement `json:"elements"`
}

type convertedElement struct {
	Name string `json:"name"`
}

type determinismDoc struct {
	Version int               `json:"version"`
	Files   []determinismFile `json:"files"`
}

type determinismFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func main() {
	fs := flag.NewFlagSet("run-manifest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	projectA := fs.String("project-a", "", "absolute path to project A's root (the directory whose bazel-bin/ holds the converter genrule outputs)")
	out := fs.String("out", "", "run directory to write; <out>/manifest/{converted,determinism}.json are created")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(64)
	}
	if *projectA == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "run-manifest: --project-a and --out are required")
		fs.Usage()
		os.Exit(64)
	}
	if err := run(*projectA, *out); err != nil {
		fmt.Fprintf(os.Stderr, "run-manifest: %v\n", err)
		os.Exit(1)
	}
}

func run(projectA, outDir string) error {
	// bazel-bin is a convenience symlink Bazel maintains at project
	// A's root after a build; its elements/ subtree holds one
	// directory per converted element.
	aElements := filepath.Join(projectA, "bazel-bin", "elements")
	entries, err := os.ReadDir(aElements)
	if err != nil {
		return fmt.Errorf("read project A's bazel-bin/elements (%s) — run `bazel build` in project A first: %w", aElements, err)
	}

	converted := convertedDoc{Version: 1}
	det := determinismDoc{Version: 1}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		buildOut := filepath.Join(aElements, name, "BUILD.bazel.out")
		if _, err := os.Stat(buildOut); err != nil {
			// No converter output — not a converted element (a
			// non-action-graph kind contributes only project-B
			// starlark, with nothing under project A's bazel-bin).
			continue
		}
		converted.Elements = append(converted.Elements, convertedElement{Name: name})

		sum, err := hashFile(buildOut)
		if err != nil {
			return fmt.Errorf("hash %s BUILD.bazel.out: %w", name, err)
		}
		det.Files = append(det.Files, determinismFile{
			Path:   name + "/BUILD.bazel.out",
			SHA256: sum,
		})
	}

	sort.Slice(converted.Elements, func(i, j int) bool {
		return converted.Elements[i].Name < converted.Elements[j].Name
	})
	sort.Slice(det.Files, func(i, j int) bool {
		return det.Files[i].Path < det.Files[j].Path
	})

	manifestDir := filepath.Join(outDir, "manifest")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(manifestDir, "converted.json"), converted); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(manifestDir, "determinism.json"), det); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run-manifest: %d converted element(s), %d output file(s) -> %s\n",
		len(converted.Elements), len(det.Files), manifestDir)
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSON(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}
