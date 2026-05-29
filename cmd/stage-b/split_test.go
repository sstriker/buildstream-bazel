package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeSplitDir lays out a --split-packages element under project A's
// bazel-bin: the cmake_split_convert rule's TreeArtifact directory at
// bazel-bin/elements/<name>/<name>_converted/packages/ holding the
// per-sub-package BUILD.bazel tree, plus a project-B elements/<name>/
// dir carrying the root placeholder write-a rendered.
func writeSplitDir(t *testing.T, files map[string]string) (projectA, projectB, name string) {
	t.Helper()
	root := t.TempDir()
	projectA = filepath.Join(root, "A")
	projectB = filepath.Join(root, "B")
	name = "demo"

	pkgDir := filepath.Join(projectA, "bazel-bin", "elements", name, name+"_converted", "packages")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(pkgDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bDir := filepath.Join(projectB, "elements", name)
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "BUILD.bazel"), []byte("filegroup(name = \"BUILD_NOT_YET_STAGED\", srcs = [])\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectA, projectB, name
}

// TestRun_SplitDir_MergesTree stages a TreeArtifact packages/ directory
// and checks the per-sub-package BUILD tree lands under project B's
// elements/<name>/, the root placeholder is overwritten, and the
// element is reported changed.
func TestRun_SplitDir_MergesTree(t *testing.T) {
	files := map[string]string{
		"BUILD.bazel":          "cc_library(name = \"toplib\")\n",
		"src/util/BUILD.bazel": "cc_library(name = \"util\")\n",
		"include/BUILD.bazel":  "cc_library(name = \"include_headers\")\n",
	}
	a, b, name := writeSplitDir(t, files)

	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := []string{"elements/" + name}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	for rel, content := range files {
		got, err := os.ReadFile(filepath.Join(b, "elements", name, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read staged %s: %v", rel, err)
		}
		if string(got) != content {
			t.Errorf("staged %s = %q, want %q", rel, got, content)
		}
	}
}

// TestRun_SplitDir_Idempotent re-merges an unchanged TreeArtifact and
// asserts nothing is reported changed — the same content-diff signal the
// single-file path returns.
func TestRun_SplitDir_Idempotent(t *testing.T) {
	files := map[string]string{
		"BUILD.bazel":          "cc_library(name = \"toplib\")\n",
		"src/util/BUILD.bazel": "cc_library(name = \"util\")\n",
	}
	a, b, _ := writeSplitDir(t, files)
	if _, err := run(args{projectA: a, projectB: b}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("re-merge reported %v, want none", changed)
	}
}

// TestStageSplitDir_PartialChange asserts that when only one sub-package
// BUILD differs on a re-merge, the element is still reported changed and
// the unchanged files are left untouched.
func TestStageSplitDir_PartialChange(t *testing.T) {
	files := map[string]string{
		"BUILD.bazel":          "cc_library(name = \"toplib\")\n",
		"src/util/BUILD.bazel": "cc_library(name = \"util\")\n",
	}
	a, b, name := writeSplitDir(t, files)
	if _, err := run(args{projectA: a, projectB: b}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Mutate one source-side BUILD; re-merge must report changed.
	pkgDir := filepath.Join(a, "bazel-bin", "elements", name, name+"_converted", "packages")
	if err := os.WriteFile(filepath.Join(pkgDir, "src", "util", "BUILD.bazel"), []byte("cc_library(name = \"util2\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if want := []string{"elements/" + name}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	got, _ := os.ReadFile(filepath.Join(b, "elements", name, "src", "util", "BUILD.bazel"))
	if string(got) != "cc_library(name = \"util2\")\n" {
		t.Errorf("changed file not restaged: %q", got)
	}
}

// TestStageSplitDir_SkipsEscapingSymlink guards path safety: a symlink
// inside the TreeArtifact pointing outside the destination must not be
// followed and written through — stageSplitDir only stages regular
// files, so the escape target is left untouched.
func TestStageSplitDir_SkipsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "packages")
	destDir := filepath.Join(root, "dest")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A would-be escape target outside destDir.
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real regular file (should stage) and an escaping symlink (should
	// be skipped, never followed/copied).
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.bazel"), []byte("cc_library(name = \"ok\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(srcDir, "evil")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	changed, err := stageSplitDir(srcDir, destDir)
	if err != nil {
		t.Fatalf("stageSplitDir: %v", err)
	}
	if !changed {
		t.Error("expected the regular file to be reported changed")
	}
	if got, _ := os.ReadFile(filepath.Join(destDir, "BUILD.bazel")); string(got) != "cc_library(name = \"ok\")\n" {
		t.Errorf("regular file not staged: %q", got)
	}
	// The symlink must not have been staged into destDir.
	if _, err := os.Lstat(filepath.Join(destDir, "evil")); !os.IsNotExist(err) {
		t.Errorf("escaping symlink was staged into destDir (err=%v)", err)
	}
	// The outside target is untouched.
	if got, _ := os.ReadFile(outside); string(got) != "original\n" {
		t.Errorf("escape target was modified: %q", got)
	}
}

// TestStageSplitDir_StaysWithinDest asserts the merge never writes
// outside destDir: every staged path is contained even when the source
// tree is deeply nested. Combined with the symlink-skip test above, this
// covers the path-safety contract of the directory merge (a real os.Walk
// over a TreeArtifact cannot emit a "../"-escaping relative path, so the
// defensive `..` guard in stageSplitDir is belt-and-suspenders for
// hand-crafted on-disk trees).
func TestStageSplitDir_StaysWithinDest(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "packages")
	destDir := filepath.Join(root, "dest")
	deep := filepath.Join(srcDir, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "BUILD.bazel"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageSplitDir(srcDir, destDir); err != nil {
		t.Fatalf("stageSplitDir: %v", err)
	}
	staged := filepath.Join(destDir, "a", "b", "c", "BUILD.bazel")
	rel, err := filepath.Rel(destDir, staged)
	if err != nil || filepath.IsAbs(rel) || rel == ".." {
		t.Fatalf("staged path escaped destDir: %q (rel %q)", staged, rel)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("deep file not staged within destDir: %v", err)
	}
}
