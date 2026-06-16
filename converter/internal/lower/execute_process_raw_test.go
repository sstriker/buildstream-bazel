package lower

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestRecoverExecuteProcess_LiftTouch_SingleFile covers the raw
// `touch <path>` form: the call lifts to one empty-file genrule under
// the build dir, sharing liftCMakeETouch's shape and its
// execute-process-op=touch tag (raw touch and cmake -E touch are the
// same operation).
func TestRecoverExecuteProcess_LiftTouch_SingleFile(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     5,
		Commands: [][]string{{"touch", "/build/marker.stamp"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "marker.stamp" {
		t.Errorf("outs: %v want [marker.stamp]", g.GenruleOuts)
	}
	if len(g.Srcs) != 0 {
		t.Errorf("touch should have no srcs; got %v", g.Srcs)
	}
	if !strings.Contains(g.GenruleCmd, "touch") {
		t.Errorf("cmd should invoke touch; got %q", g.GenruleCmd)
	}
	hasTouchTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=touch" {
			hasTouchTag = true
		}
	}
	if !hasTouchTag {
		t.Errorf("expected execute-process-op=touch tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftTouch_MultiplePaths covers `touch a b`:
// cmake's touch (and POSIX touch) accept multiple paths; the lift emits
// one genrule per path, matching liftCMakeETouch.
func TestRecoverExecuteProcess_LiftTouch_MultiplePaths(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Commands: [][]string{{"touch", "/build/a.stamp", "/build/sub/b.stamp"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 2 {
		t.Fatalf("want 2 genrules (one per path); got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftTouch_FlagRefuses asserts that a touch
// flag (which would change create/timestamp semantics) refuses with a
// precise diagnostic rather than emitting a genrule whose output the
// original call might not have created.
func TestRecoverExecuteProcess_LiftTouch_FlagRefuses(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     3,
		Commands: [][]string{{"touch", "-c", "/build/marker.stamp"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "flag") {
		t.Errorf("refusal reason should mention the flag; got %q", refusals[0].Reason)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrule should be emitted on refusal; got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftTouch_OutsideBuildRefuses covers the
// anchor-failure path: a touch path outside the build dir can't be a
// Bazel output, so the lift refuses naming the offending path.
func TestRecoverExecuteProcess_LiftTouch_OutsideBuildRefuses(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Commands: [][]string{{"touch", "/elsewhere/marker.stamp"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "build dir") {
		t.Errorf("refusal reason should name the build-dir anchor failure; got %q", refusals[0].Reason)
	}
}

// TestRecoverExecuteProcess_LiftLn_SymlinkLiftsAsCopy covers the raw
// `ln -s <target> <linkname>` form — the POSIX analog of
// `cmake -E create_symlink`. The target must resolve under the source
// root (becomes the genrule's srcs), the linkname under the build dir
// (becomes outs); the cmd reproduces the link as a copy. The op tag is
// the raw driver name "ln" (the raw-cp precedent).
func TestRecoverExecuteProcess_LiftLn_SymlinkLiftsAsCopy(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     20,
		Commands: [][]string{{"ln", "-s", "/src/bin/clang-18", "/build/bin/clang"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 1 || g.Srcs[0] != "bin/clang-18" {
		t.Errorf("srcs: %v want [bin/clang-18]", g.Srcs)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "bin/clang" {
		t.Errorf("outs: %v want [bin/clang]", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, "$(location bin/clang-18)") {
		t.Errorf("cmd should reference $(location bin/clang-18); got %q", g.GenruleCmd)
	}
	hasLnTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=ln" {
			hasLnTag = true
		}
	}
	if !hasLnTag {
		t.Errorf("expected execute-process-op=ln tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftLn_NoFlagHardlink covers `ln a b`
// (no -s): a hardlink reproduces the same way — the linkname holds the
// target's bytes by path, so it lifts as a copy.
func TestRecoverExecuteProcess_LiftLn_NoFlagHardlink(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Commands: [][]string{{"ln", "/src/data/real.bin", "/build/link.bin"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "link.bin" {
		t.Errorf("outs: %v want [link.bin]", g.GenruleOuts)
	}
}

// TestRecoverExecuteProcess_LiftLn_OneOperandRefuses asserts the
// 1-operand `ln -s target` form (linkname defaults to the
// configure-time cwd basename — unanchorable) refuses with the
// 2-operand diagnostic.
func TestRecoverExecuteProcess_LiftLn_OneOperandRefuses(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Commands: [][]string{{"ln", "-s", "/src/bin/clang-18"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "2-operand") {
		t.Errorf("refusal reason should mention 2-operand; got %q", refusals[0].Reason)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrule on refusal; got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftLn_TargetOutsideTreeRefuses covers the
// anchor-failure path: a link target that isn't under the source root
// (e.g. both target and link live in the build dir, the versioned-.so
// shape) refuses, matching cmake -E create_symlink's own constraint —
// the call falls through to the round-2 fallback rather than mis-lifting.
func TestRecoverExecuteProcess_LiftLn_TargetOutsideTreeRefuses(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Commands: [][]string{{"ln", "-sf", "/build/lib/libfoo.so.1", "/build/lib/libfoo.so"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "source root") {
		t.Errorf("refusal reason should name the source-root anchor failure; got %q", refusals[0].Reason)
	}
}

// TestRecoverExecuteProcess_LiftCMakeECopyDirectory covers
// `cmake -E copy_directory <src> <dst>`: cmake copies the CONTENTS of
// src into dst (no source-basename insert, unlike cp -R), so a nested
// file lands at <dst>/<rel> — NOT <dst>/<basename(src)>/<rel>. One
// multi-output genrule under $(RULEDIR).
func TestRecoverExecuteProcess_LiftCMakeECopyDirectory(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "data", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "data", "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "data", "nested", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:     9,
		Commands: [][]string{{"cmake", "-E", "copy_directory", filepath.Join(hostSrc, "data"), "/build/out"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("want 1 multi-out genrule; got %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	gotOuts := append([]string(nil), g.GenruleOuts...)
	sort.Strings(gotOuts)
	// Contents copied into out/ — no "data/" prefix.
	want := []string{"out/a.txt", "out/nested/b.txt"}
	if strings.Join(gotOuts, ",") != strings.Join(want, ",") {
		t.Errorf("outs: %v want %v (copy_directory copies CONTENTS, no source basename)", gotOuts, want)
	}
	if !strings.Contains(g.GenruleCmd, "$(RULEDIR)") {
		t.Errorf("multi-out cmd should use $(RULEDIR); got %q", g.GenruleCmd)
	}
	hasTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=copy_directory" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Errorf("expected execute-process-op=copy_directory tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftRename_File covers `cmake -E rename
// <file> <dst>`: lifted as a copy — src under the source root becomes
// the genrule's srcs, dst under the build dir becomes outs.
func TestRecoverExecuteProcess_LiftRename_File(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "old.h"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"cmake", "-E", "rename", filepath.Join(hostSrc, "old.h"), "/build/new.h"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 1 || g.Srcs[0] != "old.h" {
		t.Errorf("srcs: %v want [old.h]", g.Srcs)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "new.h" {
		t.Errorf("outs: %v want [new.h]", g.GenruleOuts)
	}
	hasTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=rename" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Errorf("expected execute-process-op=rename tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftMv_File covers raw `mv <file> <dst>`:
// the POSIX analog of cmake -E rename, lifted as a copy with the raw
// driver name "mv" in the op tag.
func TestRecoverExecuteProcess_LiftMv_File(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"mv", "-f", filepath.Join(hostSrc, "a.txt"), "/build/b.txt"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "b.txt" {
		t.Errorf("outs: %v want [b.txt]", g.GenruleOuts)
	}
	hasTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=mv" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Errorf("expected execute-process-op=mv tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftMv_BuildTempRefuses pins the
// documented clean-refusal: `mv build/x.tmp build/x` (source in the
// build dir, not the source tree) can't anchor its source as a Bazel
// input, so it refuses rather than mis-lifting a temp file that
// doesn't exist at convert time.
func TestRecoverExecuteProcess_LiftMv_BuildTempRefuses(t *testing.T) {
	hostSrc := t.TempDir()
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"mv", "/build/x.tmp", "/build/x"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "source root") {
		t.Errorf("refusal reason should name the source-root anchor failure; got %q", refusals[0].Reason)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrule on refusal; got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_NoopOps covers the benign no-op ops: cmake
// -E make_directory / remove / remove_directory and the raw mkdir / rm
// / rmdir analogs. Each is recognized (not refused) and produces no
// genrule — there's no consumable Bazel output to anchor.
func TestRecoverExecuteProcess_NoopOps(t *testing.T) {
	cases := [][]string{
		{"cmake", "-E", "make_directory", "/build/d"},
		{"cmake", "-E", "remove", "/build/stale.o"},
		{"cmake", "-E", "remove_directory", "/build/olddir"},
		{"mkdir", "-p", "/build/d"},
		{"rm", "-rf", "/build/stale"},
		{"rmdir", "/build/d"},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv, "_"), func(t *testing.T) {
			calls := []shadow.ExecuteProcessCall{{
				File:     "/src/CMakeLists.txt",
				Commands: [][]string{argv},
			}}
			cc := newCodegenContext()
			outs, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
			if len(refusals) != 0 {
				t.Errorf("no-op should not refuse; got %+v", refusals)
			}
			if len(cc.Genrules) != 0 {
				t.Errorf("no-op should emit no genrule; got %+v", cc.Genrules)
			}
			if len(outs) != 0 {
				t.Errorf("no-op should produce no outs; got %+v", outs)
			}
		})
	}
}

// TestRecoverExecuteProcess_CreateSymlink_DirectoryTarget is the
// regression for the directory-target gap: `cmake -E create_symlink
// <dir> <link>` (LLVM/VTK symlink an include tree into the build dir)
// must copy the directory's CONTENTS recursively, not emit a broken
// single-file `cp <dir>`. One multi-output genrule under $(RULEDIR),
// contents landing at <link>/<rel> (no source-basename insert).
func TestRecoverExecuteProcess_CreateSymlink_DirectoryTarget(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "include", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "include", "a.h"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "include", "sub", "b.h"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:     14,
		Commands: [][]string{{"cmake", "-E", "create_symlink", filepath.Join(hostSrc, "include"), "/build/staged_include"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("want 1 multi-out genrule; got %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	gotOuts := append([]string(nil), g.GenruleOuts...)
	sort.Strings(gotOuts)
	want := []string{"staged_include/a.h", "staged_include/sub/b.h"}
	if strings.Join(gotOuts, ",") != strings.Join(want, ",") {
		t.Errorf("outs: %v want %v (directory symlink copies CONTENTS recursively)", gotOuts, want)
	}
	if !strings.Contains(g.GenruleCmd, "$(RULEDIR)") {
		t.Errorf("multi-out cmd should use $(RULEDIR); got %q", g.GenruleCmd)
	}
	hasTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=create_symlink" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Errorf("expected execute-process-op=create_symlink tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_CreateSymlink_FileTargetOffline keeps the
// pre-existing behaviour pinned: a file target whose path isn't on disk
// (offline / synthetic conversion) still lifts as a single-file copy —
// the file-vs-dir stat soft-falls-back to the file shape rather than
// refusing.
func TestRecoverExecuteProcess_CreateSymlink_FileTargetOffline(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     20,
		Commands: [][]string{{"cmake", "-E", "create_symlink", "/src/bin/clang-18", "/build/bin/clang"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "bin/clang" {
		t.Errorf("outs: %v want [bin/clang]", g.GenruleOuts)
	}
}

// TestRecoverExecuteProcess_LiftLn_DirectoryTarget covers raw
// `ln -s <dir> <link>`: same directory-aware copy as create_symlink,
// with the raw "ln" op tag.
func TestRecoverExecuteProcess_LiftLn_DirectoryTarget(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "data", "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{"ln", "-s", filepath.Join(hostSrc, "data"), "/build/linked"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, nil, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift; got refusals %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "linked/x.txt" {
		t.Errorf("outs: %v want [linked/x.txt]", g.GenruleOuts)
	}
	hasTag := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-op=ln" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Errorf("expected execute-process-op=ln tag; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_CreateSymlink_InstallCompatAliasSkips is the
// libpng regression: `cmake -E create_symlink libpng16-config
// libpng-config` (and the `libpng16.pc -> libpng.pc` shape) is a
// versioned install-compat alias over a BUILD-GENERATED file — the
// source anchors nowhere under the source root and the link anchors
// nowhere under the build dir. With nothing to track on either side it
// must skip benignly (like the make_directory/remove no-ops), NOT
// hard-fail the element with unsupported-execute-process.
func TestRecoverExecuteProcess_CreateSymlink_InstallCompatAliasSkips(t *testing.T) {
	cases := [][]string{
		{"cmake", "-E", "create_symlink", "libpng16-config", "libpng-config"},
		{"cmake", "-E", "create_symlink", "libpng16.pc", "libpng.pc"},
		{"ln", "-s", "libpng16-config", "libpng-config"},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv, "_"), func(t *testing.T) {
			calls := []shadow.ExecuteProcessCall{{
				File:     "/src/CMakeLists.txt",
				Line:     993,
				Commands: [][]string{argv},
			}}
			cc := newCodegenContext()
			outs, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
			if len(refusals) != 0 {
				t.Errorf("install-compat alias should skip, not refuse; got %+v", refusals)
			}
			if len(cc.Genrules) != 0 {
				t.Errorf("install-compat alias should emit no genrule; got %+v", cc.Genrules)
			}
			if len(outs) != 0 {
				t.Errorf("install-compat alias should produce no outs; got %+v", outs)
			}
		})
	}
}

// TestRecoverExecuteProcess_CreateSymlink_BuildDirLinkStillRefuses pins
// the narrowness of the skip: a link that DOES anchor under the build
// dir is a potential real output (a build-generated header alias a later
// step #includes), so an unrecoverable source must still REFUSE — we
// only skip the anchors-nowhere alias, never a possibly-load-bearing
// symlink.
func TestRecoverExecuteProcess_CreateSymlink_BuildDirLinkStillRefuses(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Commands: [][]string{{"cmake", "-E", "create_symlink", "/build/gen/real.h", "/build/gen/alias.h"}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("build-dir-anchored link with unrecoverable source should refuse; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "source root") {
		t.Errorf("refusal should name the source-root anchor failure; got %q", refusals[0].Reason)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrule on refusal; got %+v", cc.Genrules)
	}
}
