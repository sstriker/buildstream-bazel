package lower

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestRecoverExecuteProcess_LiftCp_SingleFile covers the raw
// `cp <file> <dst>` form where dst is a fully-qualified output
// path (has a file extension): one genrule, src anchored under
// the source root, out anchored under the build dir, cmd uses
// $(location <src>). Issue #312.
func TestRecoverExecuteProcess_LiftCp_SingleFile(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "src.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:     5,
		Commands: [][]string{{"cp", filepath.Join(hostSrc, "src.txt"), "/build/sub/src.txt"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 1 || g.Srcs[0] != "src.txt" {
		t.Errorf("srcs: %v want [src.txt]", g.Srcs)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "sub/src.txt" {
		t.Errorf("outs: %v want [sub/src.txt]", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, "$(location src.txt)") {
		t.Errorf("cmd should reference $(location src.txt); got %q", g.GenruleCmd)
	}
	hasCpTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=cp" {
			hasCpTag = true
		}
	}
	if !hasCpTag {
		t.Errorf("expected execute-process-op=cp tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftCp_FileIntoDir covers `cp a.txt
// <dir>/` where the trailing slash marks the destination as a
// directory: cp drops the source basename into it, so the output is
// <dir>/a.txt.
func TestRecoverExecuteProcess_LiftCp_FileIntoDir(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"cp", filepath.Join(hostSrc, "a.txt"), "/build/dir/"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "dir/a.txt" {
		t.Errorf("outs: %v want [dir/a.txt]", g.GenruleOuts)
	}
}

// TestRecoverExecuteProcess_LiftCp_FileToExtensionlessDest pins the
// POSIX-faithful default: `cp src/script ${BINARY}/script` copies to a
// FILE named `script` (the dst doesn't pre-exist as a directory at
// convert time), NOT `script/script`. Only a trailing slash or the
// build-dir root marks a directory destination.
func TestRecoverExecuteProcess_LiftCp_FileToExtensionlessDest(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "script"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"cp", filepath.Join(hostSrc, "script"), "/build/script"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "script" {
		t.Errorf("outs: %v want [script]", g.GenruleOuts)
	}
}

// TestRecoverExecuteProcess_LiftCp_RecursiveSymlinkDir is the
// issue #312 fixture: `cp -RauL <symlinked-dir> ${BINARY}` where
// the source argument is a SYMLINK to a sibling real dir holding
// files in nested subdirs. The lift must:
//   - deref the symlink so emitted Srcs point at the REAL
//     source-root paths;
//   - land outputs at <build>/<basename(symlink)>/<rel> (cp -R
//     appends the source-argument basename);
//   - emit ONE multi-output genrule whose cmd uses $(RULEDIR).
func TestRecoverExecuteProcess_LiftCp_RecursiveSymlinkDir(t *testing.T) {
	hostSrc := t.TempDir()
	// Real data dir at <src>/data with nested files.
	if err := os.MkdirAll(filepath.Join(hostSrc, "data", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "data", "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "data", "nested", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// tests/types/data -> ../data symlink shape.
	if err := os.MkdirAll(filepath.Join(hostSrc, "tests", "types"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(hostSrc, "tests", "types", "data")
	if err := os.Symlink(filepath.Join(hostSrc, "data"), link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "tests", "types", "CMakeLists.txt"),
		Line:     12,
		Commands: [][]string{{"cp", "-RauL", link, "/build"}},
	}}
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("want 1 multi-out genrule; got %+v", cc.Genrules)
	}
	g := cc.Genrules[0]

	gotOuts := append([]string(nil), g.GenruleOuts...)
	sort.Strings(gotOuts)
	wantOuts := []string{"data/a.txt", "data/nested/b.txt"}
	if strings.Join(gotOuts, ",") != strings.Join(wantOuts, ",") {
		t.Errorf("outs: %v want %v", gotOuts, wantOuts)
	}
	// Srcs must resolve to the REAL deref'd source-root paths
	// (data/...), not through the tests/types/data symlink.
	gotSrcs := append([]string(nil), g.Srcs...)
	sort.Strings(gotSrcs)
	wantSrcs := []string{"data/a.txt", "data/nested/b.txt"}
	if strings.Join(gotSrcs, ",") != strings.Join(wantSrcs, ",") {
		t.Errorf("srcs: %v want %v (should deref symlink to real source-root paths)", gotSrcs, wantSrcs)
	}
	if !strings.Contains(g.GenruleCmd, "$(RULEDIR)") {
		t.Errorf("multi-out cmd should use $(RULEDIR); got %q", g.GenruleCmd)
	}
	if strings.Contains(g.GenruleCmd, `"$@"`) {
		t.Errorf("multi-out cmd must not use $@; got %q", g.GenruleCmd)
	}
	// outs surface for downstream attribution.
	if len(outs) != 2 {
		t.Errorf("outs slice: want 2, got %+v", outs)
	}
	for _, o := range g.GenruleOuts {
		if cc.OutToGenrule[o] != g.Name {
			t.Errorf("OutToGenrule[%q]=%q want %q", o, cc.OutToGenrule[o], g.Name)
		}
	}
}

// TestRecoverExecuteProcess_LiftCp_ThreeOperandsRefuses asserts
// the 3-operand `cp a b dst/` (multi-src into dir) form refuses
// with the precise "2-operand only" diagnostic rather than
// crashing or mis-lifting.
func TestRecoverExecuteProcess_LiftCp_ThreeOperandsRefuses(t *testing.T) {
	hostSrc := t.TempDir()
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:     3,
		Commands: [][]string{{"cp", filepath.Join(hostSrc, "a"), filepath.Join(hostSrc, "b"), "/build/dst/"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "2-operand") {
		t.Errorf("refusal reason: %q want mention of 2-operand", refusals[0].Reason)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrule on refusal; got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftCp_MissingSourceRefuses covers
// the stat-fails path: a source path under the root that doesn't
// exist on disk refuses with a precise diagnostic.
func TestRecoverExecuteProcess_LiftCp_MissingSourceRefuses(t *testing.T) {
	hostSrc := t.TempDir()
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"cp", filepath.Join(hostSrc, "ghost.txt"), "/build/ghost.txt"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "does not exist on disk") {
		t.Errorf("refusal reason: %q", refusals[0].Reason)
	}
}

// TestRecoverExecuteProcess_LiftCp_DirWithoutRecursiveRefuses
// asserts `cp <dir> <dst>` without a recursive flag refuses
// (matches POSIX cp, which errors on a directory source absent
// -R/-r/-a).
func TestRecoverExecuteProcess_LiftCp_DirWithoutRecursiveRefuses(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "d", "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"cp", filepath.Join(hostSrc, "d"), "/build"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "recursive flag") {
		t.Errorf("refusal reason: %q", refusals[0].Reason)
	}
}
