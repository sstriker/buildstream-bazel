package lower

import (
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
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
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
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
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
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
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
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
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "source root") {
		t.Errorf("refusal reason should name the source-root anchor failure; got %q", refusals[0].Reason)
	}
}
