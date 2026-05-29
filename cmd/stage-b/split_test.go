package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeBuildPackagesTar lays out a --split-packages element under
// project A's bazel-bin: a build-packages.tar holding the per-sub-
// package BUILD.bazel tree (entries named "./<dir>/BUILD.bazel" the
// way `tar -C $PKGTREE .` produces them), plus a project-B
// elements/<name>/ dir carrying the root placeholder write-a rendered.
func writeBuildPackagesTar(t *testing.T, files map[string]string) (projectA, projectB, name string) {
	t.Helper()
	root := t.TempDir()
	projectA = filepath.Join(root, "A")
	projectB = filepath.Join(root, "B")
	name = "demo"

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for rel, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     "./" + rel,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	aDir := filepath.Join(projectA, "bazel-bin", "elements", name)
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aDir, "build-packages.tar"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
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

// TestRun_SplitTar_UnpacksTree stages a build-packages.tar and checks
// the per-sub-package BUILD tree lands under project B's
// elements/<name>/, the root placeholder is overwritten, and the
// element is reported changed.
func TestRun_SplitTar_UnpacksTree(t *testing.T) {
	files := map[string]string{
		"BUILD.bazel":          "cc_library(name = \"toplib\")\n",
		"src/util/BUILD.bazel": "cc_library(name = \"util\")\n",
		"include/BUILD.bazel":  "cc_library(name = \"include_headers\")\n",
	}
	a, b, name := writeBuildPackagesTar(t, files)

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

// TestRun_SplitTar_Idempotent re-stages an unchanged tar and asserts
// nothing is reported changed — the same content-diff signal the
// single-file path returns.
func TestRun_SplitTar_Idempotent(t *testing.T) {
	files := map[string]string{
		"BUILD.bazel":          "cc_library(name = \"toplib\")\n",
		"src/util/BUILD.bazel": "cc_library(name = \"util\")\n",
	}
	a, b, _ := writeBuildPackagesTar(t, files)
	if _, err := run(args{projectA: a, projectB: b}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("re-stage reported %v, want none", changed)
	}
}

// TestStageSplitTar_RejectsEscape guards the path-sanitization: a
// "../"-escaping member must be refused rather than written outside
// the destination package.
func TestStageSplitTar_RejectsEscape(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("evil\n")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape/BUILD.bazel", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	tarPath := filepath.Join(dir, "build-packages.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageSplitTar(tarPath, filepath.Join(dir, "dest")); err == nil {
		t.Fatal("expected escape rejection, got nil error")
	}
}
