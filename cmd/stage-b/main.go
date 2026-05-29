// stage-b stages project A's per-element converted BUILD.bazel.out
// outputs into project B and reports which elements' BUILD content
// changed — the "what just re-converted" signal the Phase 8b
// targeted gazelle step consumes.
//
// The write-a + Bazel pipeline is two-pass: write-a renders project
// A + project B; `bazel build` in project A runs each element's
// convert-element-* genrule, producing
// <A>/bazel-bin/elements/<name>/BUILD.bazel.out. stage-b copies each
// of those over project B's elements/<name>/BUILD.bazel (replacing
// the BUILD_NOT_YET_STAGED placeholder write-a rendered on first
// render) and prints, one `elements/<name>` per line on stdout, the
// packages whose staged content actually changed versus what was
// already there.
//
// That changed set is what a driver feeds to `relax-keeps` and
// `bazel run //:gazelle -- <packages>` so the post-conversion
// gazelle step touches only the elements that re-converted —
// O(changed) instead of O(workspace). It is the write-a + Bazel
// path's replacement for the (now-deleted) orchestrator's
// res.Converted signal.
//
// Elements with no project-A converted output are skipped: kind:stack
// / filter / import and other non-action-graph kinds contribute only
// project-B starlark, which write-a renders directly — there is
// nothing to stage and nothing converted to re-run gazelle against.
//
// See ROADMAP.md for the full
// post-conversion + gazelle workflow.
package main

import (
	"archive/tar"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	exitSuccess = 0
	exitUsage   = 64
	exitError   = 65
)

type args struct {
	projectA string
	projectB string
}

func main() {
	a, code := parseArgs(os.Args[1:], os.Stderr)
	if code != exitSuccess {
		os.Exit(code)
	}
	changed, err := run(a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stage-b: %v\n", err)
		os.Exit(exitError)
	}
	for _, pkg := range changed {
		fmt.Println(pkg)
	}
}

func parseArgs(argv []string, stderr *os.File) (args, int) {
	flags := flag.NewFlagSet("stage-b", flag.ContinueOnError)
	flags.SetOutput(stderr)
	a := args{}
	flags.StringVar(&a.projectA, "project-a", "", "absolute path to project A's root (the directory whose bazel-bin/ holds the converter genrule outputs)")
	flags.StringVar(&a.projectB, "project-b", "", "absolute path to project B's root (the directory containing MODULE.bazel + elements/)")
	if err := flags.Parse(argv); err != nil {
		return a, exitUsage
	}
	if a.projectA == "" || a.projectB == "" {
		fmt.Fprintln(stderr, "stage-b: --project-a and --project-b are both required")
		flags.Usage()
		return a, exitUsage
	}
	for _, r := range []struct {
		name string
		p    *string
	}{{"--project-a", &a.projectA}, {"--project-b", &a.projectB}} {
		abs, err := filepath.Abs(*r.p)
		if err != nil {
			fmt.Fprintf(stderr, "stage-b: resolve %s %q: %v\n", r.name, *r.p, err)
			return a, exitUsage
		}
		*r.p = abs
	}
	return a, exitSuccess
}

// run stages every element with a project-A converted output and
// returns the sorted list of `elements/<name>` packages whose staged
// BUILD.bazel content changed. The set of elements is discovered from
// project B's elements/ directory; the project-A side is consulted
// per element to find the converted output to stage.
func run(a args) ([]string, error) {
	// bazel-bin is a convenience symlink Bazel maintains at the
	// workspace root after a build. Its absence means `bazel build`
	// never ran in project A — staging would be a silent no-op that
	// later surfaces as "every BUILD is still a placeholder", so
	// fail loudly here instead.
	aElements := filepath.Join(a.projectA, "bazel-bin", "elements")
	if _, err := os.Stat(aElements); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("project A has no bazel-bin/elements (%s) — run `bazel build` in project A before staging", aElements)
		}
		return nil, fmt.Errorf("stat %s: %v", aElements, err)
	}

	bElements := filepath.Join(a.projectB, "elements")
	entries, err := os.ReadDir(bElements)
	if err != nil {
		return nil, fmt.Errorf("read project B elements/ (%s): %v", bElements, err)
	}

	var changed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()

		// --split-packages elements emit a single build-packages.tar
		// (the per-sub-package BUILD tree) instead of BUILD.bazel.out,
		// because a genrule can't statically declare the discovered-at-
		// action-time sub-package set. Prefer it when present: unpack
		// the tree into project B's elements/<name>/, overwriting the
		// root placeholder and creating the sub-package BUILD files.
		tarPath := filepath.Join(aElements, name, "build-packages.tar")
		if info, statErr := os.Stat(tarPath); statErr == nil && info.Size() > 0 {
			ch, err := stageSplitTar(tarPath, filepath.Join(bElements, name))
			if err != nil {
				return nil, fmt.Errorf("stage split element %s: %v", name, err)
			}
			if ch {
				changed = append(changed, "elements/"+name)
			}
			continue
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("stat %s: %v", tarPath, statErr)
		}

		src := filepath.Join(aElements, name, "BUILD.bazel.out")
		srcBytes, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				// Non-action-graph kind (stack / filter / import / …):
				// no converted output, nothing to stage.
				continue
			}
			return nil, fmt.Errorf("read converted output %s: %v", src, err)
		}
		dst := filepath.Join(bElements, name, "BUILD.bazel")
		dstBytes, err := os.ReadFile(dst)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read staged BUILD %s: %v", dst, err)
		}
		// os.IsNotExist leaves dstBytes nil, which Equal treats as
		// "differs from any non-empty src" — a missing dest stages
		// and counts as changed, the correct first-render behavior.
		if bytes.Equal(srcBytes, dstBytes) {
			continue
		}
		if err := os.WriteFile(dst, srcBytes, 0o644); err != nil {
			return nil, fmt.Errorf("stage %s: %v", dst, err)
		}
		changed = append(changed, "elements/"+name)
	}
	sort.Strings(changed)
	return changed, nil
}

// stageSplitTar unpacks a --split-packages build-packages.tar (the
// per-sub-package BUILD.bazel tree produced by project A's converter
// genrule) into destDir, the project-B elements/<name>/ package root.
// It writes each regular-file entry, creating sub-package directories
// as needed, and reports whether any staged file's content changed
// (added or differing) versus what was already there — the same
// idempotent "what re-converted" signal the single-file path returns,
// so a re-stage with an unchanged tar reports nothing.
//
// Entry paths are sanitized: a "../"-escaping or absolute member is
// rejected rather than allowed to write outside destDir.
func stageSplitTar(tarPath, destDir string) (bool, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	changed := false
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read tar %s: %v", tarPath, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // skip dir entries; we MkdirAll per file below
		}
		rel := filepath.Clean(filepath.FromSlash(hdr.Name))
		if rel == "" || rel == "." {
			continue
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false, fmt.Errorf("tar %s: member %q escapes destination", tarPath, hdr.Name)
		}
		dst := filepath.Join(destDir, rel)
		newBytes, err := io.ReadAll(tr)
		if err != nil {
			return false, fmt.Errorf("read tar member %q: %v", hdr.Name, err)
		}
		oldBytes, rdErr := os.ReadFile(dst)
		if rdErr != nil && !os.IsNotExist(rdErr) {
			return false, fmt.Errorf("read staged %s: %v", dst, rdErr)
		}
		if bytes.Equal(oldBytes, newBytes) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return false, fmt.Errorf("mkdir for %s: %v", dst, err)
		}
		if err := os.WriteFile(dst, newBytes, 0o644); err != nil {
			return false, fmt.Errorf("stage %s: %v", dst, err)
		}
		changed = true
	}
	return changed, nil
}
