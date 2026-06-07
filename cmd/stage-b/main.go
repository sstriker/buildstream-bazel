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
	"bytes"
	"flag"
	"fmt"
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

		// --split-packages elements are converted by the
		// cmake_split_convert custom rule (rules/cmake_packages.bzl),
		// whose action declares the discovered-at-action-time
		// per-sub-package BUILD tree as a TreeArtifact directory. It
		// materializes under bazel-bin at
		// elements/<name>/<name>_converted/packages/ (the rule name is
		// "<name>_converted"; declare_directory path is
		// "<rule-name>/packages"). Prefer it when present: merge the
		// live directory into project B's elements/<name>/ by per-file
		// content compare, overwriting the root placeholder and creating
		// the sub-package BUILD files. No tar to unpack — each generated
		// BUILD is content-addressed individually.
		pkgDir := filepath.Join(aElements, name, name+"_converted", "packages")
		if info, statErr := os.Stat(pkgDir); statErr == nil && info.IsDir() {
			ch, err := stageSplitDir(pkgDir, filepath.Join(bElements, name))
			if err != nil {
				return nil, fmt.Errorf("stage split element %s: %v", name, err)
			}
			if ch {
				changed = append(changed, "elements/"+name)
			}
			continue
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("stat %s: %v", pkgDir, statErr)
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
		// Stage the agent-prompts sidecar alongside the BUILD (the
		// converter emits conversion-todos.json next to BUILD.bazel.out by
		// default). Independent of the BUILD diff — a todos-only change
		// still needs to land — and not counted as a "changed" package
		// since it isn't a BUILD file (no gazelle/buildifier pass needed).
		// The --split-packages path lands it via stageSplitDir (it's a file
		// inside the packages TreeArtifact), so this covers only the
		// single-file genrule shape.
		if err := stageSidecar(filepath.Join(aElements, name), filepath.Join(bElements, name), "conversion-todos.json"); err != nil {
			return nil, fmt.Errorf("stage conversion-todos for %s: %v", name, err)
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

// stageSidecar copies a per-element converter sidecar file (e.g.
// conversion-todos.json) from project A's bazel-bin element dir (aDir)
// into project B's element dir (bDir), writing only when the content
// differs. A missing source is not an error — the converter may not have
// produced it (e.g. --conversion-todos=false, or a kind that emits no
// such file). Unlike the BUILD staging, this does not feed the "changed"
// list: the sidecar isn't a BUILD file, so it needs no gazelle/buildifier
// follow-up.
func stageSidecar(aDir, bDir, file string) error {
	src := filepath.Join(aDir, file)
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %v", src, err)
	}
	dst := filepath.Join(bDir, file)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, srcBytes) {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %v", dst, err)
	}
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %v", bDir, err)
	}
	if err := os.WriteFile(dst, srcBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %v", dst, err)
	}
	return nil
}

// stageSplitDir merges a --split-packages TreeArtifact directory (the
// per-sub-package BUILD.bazel tree the cmake_split_convert rule's action
// materialized under project A's bazel-bin) into destDir, the project-B
// elements/<name>/ package root. It walks srcDir recursively, writing
// each regular file under its srcDir-relative path and creating
// sub-package directories as needed, and reports whether any staged
// file's content changed (added or differing) versus what was already
// there — the same idempotent "what re-converted" signal the single-file
// path returns, so a re-merge of an unchanged tree reports nothing.
//
// Relative paths are sanitized: a "../"-escaping or absolute member is
// rejected rather than allowed to write outside destDir. (A TreeArtifact
// can't normally contain such entries, but the guard keeps the merge
// safe regardless of what's on disk.)
func stageSplitDir(srcDir, destDir string) (bool, error) {
	changed := false
	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil // directories are created lazily per file below
		}
		if !info.Mode().IsRegular() {
			return nil // skip symlinks / sockets / etc.
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %v", path, err)
		}
		rel = filepath.Clean(rel)
		if rel == "" || rel == "." {
			return nil
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("split dir %s: member %q escapes destination", srcDir, rel)
		}
		dst := filepath.Join(destDir, rel)
		newBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read split member %s: %v", path, err)
		}
		oldBytes, rdErr := os.ReadFile(dst)
		if rdErr != nil && !os.IsNotExist(rdErr) {
			return fmt.Errorf("read staged %s: %v", dst, rdErr)
		}
		if bytes.Equal(oldBytes, newBytes) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %v", dst, err)
		}
		if err := os.WriteFile(dst, newBytes, 0o644); err != nil {
			return fmt.Errorf("stage %s: %v", dst, err)
		}
		changed = true
		return nil
	})
	if walkErr != nil {
		return false, walkErr
	}
	return changed, nil
}
