package bazel_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// TestEmit_SubdirLibrary_Split_Golden exercises --split-packages on the
// subdir-library fixture: the top-level CMakeLists declares `toplib`
// (root package) and add_subdirectory(src/util) declares `util`
// (src/util package), both including the project-root `include` header
// dir. EmitSplit must:
//   - emit one BUILD.bazel per package (root, src/util, include);
//   - synthesize an `include_headers` cc_library in the include package
//     globbing the headers with includes = ["."];
//   - rewrite toplib's deps to the cross-package util label + the
//     header lib, and re-relativize util's srcs to "util.c";
//   - keep install-derived targets (filegroup, cmake_config_bundle,
//     *_import) in the root package.
//
// Goldens live one-per-package under
// testdata/golden/subdir-library-split/<dir>/BUILD.bazel.golden
// (root → BUILD.bazel.golden), compared with the same scrubSourceLine +
// -update harness the single-BUILD goldens use.
func TestEmit_SubdirLibrary_Split_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/subdir-library"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}

	dirs := make([]string, 0, len(tree))
	for d := range tree {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	goldenDir := filepath.Join("..", "..", "testdata", "golden", "subdir-library-split")
	for _, d := range dirs {
		got := scrubSourceLine(tree[d], src)
		rel := d
		if rel == "" {
			rel = "."
		}
		goldenPath := filepath.Join(goldenDir, rel, "BUILD.bazel.golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s", goldenPath)
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read golden %s (run with -update?): %v", goldenPath, err)
		}
		if string(got) != string(want) {
			t.Errorf("split BUILD.bazel mismatch for dir %q\n--- got ---\n%s\n--- want ---\n%s", d, got, want)
		}
	}

	// Guard the package set so a future change that drops or adds a
	// package surfaces here rather than as a silent golden gap.
	if *update {
		return
	}
	wantDirs := []string{"", "include", "src/util"}
	if len(dirs) != len(wantDirs) {
		t.Fatalf("emitted packages = %v, want %v", dirs, wantDirs)
	}
	for i := range wantDirs {
		if dirs[i] != wantDirs[i] {
			t.Errorf("package set = %v, want %v", dirs, wantDirs)
			break
		}
	}
}

// TestEmit_Split_SourceKey_LeavesElementRootRelative asserts the
// SourceKey (orchestrator FUSE-sources) regime under --split-packages:
// srcs/hdrs are emitted as @src_<key>//:tree_dir/<element-root-relative>
// absolute labels that are package-location-independent, so the
// transform must NOT re-relativize them to the sub-package — it only
// trims paths in the local (SourceKey=="") regime. Deps still rewrite to
// cross-package labels in both regimes.
func TestEmit_Split_SourceKey_LeavesElementRootRelative(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{
		BazelPackagePath: "elements/subdir-library",
		SourceKey:        "abc",
	})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	util, ok := tree["src/util"]
	if !ok {
		t.Fatalf("no src/util package emitted")
	}
	body := string(util)
	// util.c stays element-root-relative under the @src label, NOT
	// trimmed to "util.c".
	if !contains(body, "@src_abc//:tree_dir/src/util/util.c") {
		t.Errorf("SourceKey regime should keep element-root-relative @src path; got:\n%s", body)
	}
	if contains(body, "\"util.c\"") {
		t.Errorf("SourceKey regime must not re-relativize to a bare \"util.c\"; got:\n%s", body)
	}
	// Deps still rewrite to the cross-package header-lib label.
	if !contains(body, "//elements/subdir-library/include:include_headers") {
		t.Errorf("SourceKey regime should still rewrite deps to cross-package labels; got:\n%s", body)
	}
}

// TestEmit_SplitOff_ByteIdenticalToSingleGolden asserts the OFF
// byte-identity constraint at the EmitSplit boundary: --split-packages
// false (the single-BUILD path) on subdir-library must byte-match the
// existing single-BUILD golden. This complements the unchanged
// TestEmit_SubdirLibrary_Golden by pinning the contract that the
// feature's presence does not perturb the default emit.
func TestEmit_SplitOff_ByteIdenticalToSingleGolden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got, err := bazel.Emit(pkg) // default (split OFF) path
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got = scrubSourceLine(got, src)
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "subdir-library", "BUILD.bazel.golden")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("split-OFF emit drifted from single golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
