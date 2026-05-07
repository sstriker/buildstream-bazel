package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/shadow"
)

// TestRecoverExecuteProcess_LiftCMakeETouch asserts the cmake
// -E touch lift: the call is removed from the refusal set and
// surfaces as one ir.Target{KindGenrule} on cc.Genrules with
// the expected outs/cmd/tags shape.
func TestRecoverExecuteProcess_LiftCMakeETouch(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     5,
		Commands: [][]string{{"cmake", "-E", "touch", "/build/marker.stamp"}},
	}}
	cc := newCodegenContext()
	if err := recoverExecuteProcess(calls, "/src", "/src", "/build", "/build", cc); err != nil {
		t.Fatalf("expected lift to succeed; got %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: want 1, got %d (%+v)", len(cc.Genrules), cc.Genrules)
	}
	g := cc.Genrules[0]
	if g.Name != "exec_marker_stamp" {
		t.Errorf("name: %q want exec_marker_stamp", g.Name)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "marker.stamp" {
		t.Errorf("outs: %v want [marker.stamp]", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, "touch") {
		t.Errorf("cmd should invoke touch; got %q", g.GenruleCmd)
	}
	wantTags := map[string]bool{
		"cmake-codegen":                          true,
		"cmake-codegen-cmake-e":                  true,
		"cmake-codegen-driver=cmake_e":           true,
		"cmake-codegen-execute-process":          true,
		"cmake-codegen-execute-process-op=touch": true,
	}
	if len(g.Tags) != len(wantTags) {
		t.Errorf("tags: %v want %v", g.Tags, wantTags)
	}
	for _, tg := range g.Tags {
		if !wantTags[tg] {
			t.Errorf("unexpected tag %q in %v", tg, g.Tags)
		}
	}
	if cc.OutToGenrule["marker.stamp"] != "exec_marker_stamp" {
		t.Errorf("OutToGenrule: %v", cc.OutToGenrule)
	}
}

// TestRecoverExecuteProcess_LiftCMakeECopy asserts the 2-arg
// cmake -E copy lift: src must resolve under the source root
// (becomes the genrule's srcs), dst must resolve under the
// build dir (becomes outs); cmd uses $(location <src>) so
// Bazel's source-graph correctly tracks the input.
func TestRecoverExecuteProcess_LiftCMakeECopy(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     10,
		Commands: [][]string{{"cmake", "-E", "copy", "/src/inputs/template.cfg", "/build/staged/template.cfg"}},
	}}
	cc := newCodegenContext()
	if err := recoverExecuteProcess(calls, "/src", "/src", "/build", "/build", cc); err != nil {
		t.Fatalf("expected lift to succeed; got %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 1 || g.Srcs[0] != "inputs/template.cfg" {
		t.Errorf("srcs: %v want [inputs/template.cfg]", g.Srcs)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "staged/template.cfg" {
		t.Errorf("outs: %v want [staged/template.cfg]", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, "$(location inputs/template.cfg)") {
		t.Errorf("cmd should reference $(location inputs/template.cfg); got %q", g.GenruleCmd)
	}
}

// TestRecoverExecuteProcess_LiftCMakeECopy_RejectsSourceOutsideTree
// covers the anchor-failure path: if the recorded source path
// isn't under the source root, the lift falls through to
// refusal with a precise diagnostic identifying the offending
// path (operators see exactly which path didn't resolve, not
// just "lift failed").
func TestRecoverExecuteProcess_LiftCMakeECopy_RejectsSourceOutsideTree(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     10,
		Commands: [][]string{{"cmake", "-E", "copy", "/usr/share/foo/data.bin", "/build/staged/data.bin"}},
	}}
	cc := newCodegenContext()
	err := recoverExecuteProcess(calls, "/src", "/src", "/build", "/build", cc)
	if err == nil {
		t.Fatal("expected refusal failure")
	}
	if !strings.Contains(err.Error(), "/usr/share/foo/data.bin") {
		t.Errorf("expected refusal to name the offending path; got: %v", err)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrule should be appended on refusal; got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftPlusRefuse covers the mixed-bag
// case: one cmake -E touch (lifts) + one git rev-parse (refuses).
// The lift succeeds and adds a genrule; the refusal still
// produces the typed Tier-1 failure for the unliftable call.
// This guarantees the per-bucket dispatcher in
// recoverExecuteProcess doesn't all-or-nothing on partial success.
func TestRecoverExecuteProcess_LiftPlusRefuse(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{
		{
			File:     "/src/CMakeLists.txt",
			Line:     3,
			Commands: [][]string{{"cmake", "-E", "touch", "/build/marker.stamp"}},
		},
		{
			File:           "/src/CMakeLists.txt",
			Line:           5,
			Commands:       [][]string{{"git", "rev-parse", "HEAD"}},
			OutputVariable: "GIT_SHA",
		},
	}
	cc := newCodegenContext()
	err := recoverExecuteProcess(calls, "/src", "/src", "/build", "/build", cc)
	if err == nil {
		t.Fatal("expected refusal failure for the git call")
	}
	// The touch lift still went through.
	if len(cc.Genrules) != 1 {
		t.Errorf("touch should have lifted to one genrule even though git refused; got %+v", cc.Genrules)
	}
	if !strings.Contains(err.Error(), "[stamp]") {
		t.Errorf("refusal should mention [stamp]; got: %v", err)
	}
}
