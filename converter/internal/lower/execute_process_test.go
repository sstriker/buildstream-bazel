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

// TestRecoverExecuteProcess_LiftFileProducing covers the
// build-time hoist of a file-producing execute_process call
// (OUTPUT_FILE declared, argv reads an in-tree input). The
// recovered genrule has the input as a real Bazel src, the
// output anchored to the build dir, and the
// cmake-codegen-execute-process-hoisted tag flagging the
// configure-time → build-time move.
func TestRecoverExecuteProcess_LiftFileProducing(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:       "/src/CMakeLists.txt",
		Line:       12,
		Commands:   [][]string{{"/usr/bin/python3", "/src/scripts/gen.py", "--in", "/src/spec.txt"}},
		OutputFile: "/build/generated.h",
	}}
	cc := newCodegenContext()
	if err := recoverExecuteProcess(calls, "/src", "/src", "/build", "/build", cc); err != nil {
		t.Fatalf("expected lift to succeed; got %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: want 1, got %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if g.Name != "exec_generated_h" {
		t.Errorf("name: %q want exec_generated_h", g.Name)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "generated.h" {
		t.Errorf("outs: %v want [generated.h]", g.GenruleOuts)
	}
	if len(g.Srcs) != 2 || g.Srcs[0] != "scripts/gen.py" || g.Srcs[1] != "spec.txt" {
		t.Errorf("srcs: %v want [scripts/gen.py spec.txt]", g.Srcs)
	}
	for _, want := range []string{
		"$(location scripts/gen.py)",
		"$(location spec.txt)",
		`> "$@"`,
	} {
		if !strings.Contains(g.GenruleCmd, want) {
			t.Errorf("cmd missing %q; got %q", want, g.GenruleCmd)
		}
	}
	hasHoisted := false
	hasDriver := false
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-hoisted" {
			hasHoisted = true
		}
		if tg == "cmake-codegen-driver=python3" {
			hasDriver = true
		}
	}
	if !hasHoisted {
		t.Errorf("tags should include cmake-codegen-execute-process-hoisted; got %v", g.Tags)
	}
	if !hasDriver {
		t.Errorf("tags should include cmake-codegen-driver=python3; got %v", g.Tags)
	}
}

// TestRecoverExecuteProcess_LiftFileProducing_SourceRootArgv
// covers the corner where an argv element resolves to the
// source root itself (cmake's ${CMAKE_CURRENT_SOURCE_DIR}
// expanding to the project root). relativeIfInside maps that
// to "" — without normalisation, shellQuoteArg("") would
// emit literal `”` in the cmd, dropping the path argument
// entirely. The fix re-normalises empty rel to "." so the
// argument remains valid; the directory-handling branch
// then renders it as a literal `.` rather than a
// $(location) wrap (which would also fail on the empty
// path).
func TestRecoverExecuteProcess_LiftFileProducing_SourceRootArgv(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:       "/src/CMakeLists.txt",
		Line:       12,
		Commands:   [][]string{{"/usr/bin/python3", "/src/scripts/gen.py", "/src"}},
		OutputFile: "/build/generated.h",
	}}
	cc := newCodegenContext()
	if err := recoverExecuteProcess(calls, "/src", "/src", "/build", "/build", cc); err != nil {
		t.Fatalf("expected lift to succeed; got %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	// argv[2] is /src (the source root itself) — must
	// render as a literal "." in the cmd, not as `''` or
	// $(location ).
	if strings.Contains(g.GenruleCmd, " '' ") || strings.Contains(g.GenruleCmd, "$(location )") {
		t.Errorf("source-root argv element should normalise to %q; got cmd: %s", ".", g.GenruleCmd)
	}
	if !strings.Contains(g.GenruleCmd, " . > ") && !strings.HasSuffix(strings.SplitN(g.GenruleCmd, ` > "$@"`, 2)[0], " .") {
		t.Errorf("expected literal `.` for source-root argv; got cmd: %s", g.GenruleCmd)
	}
}

// TestRecoverExecuteProcess_LiftFileProducing_RefusesUnmodeledOpts
// asserts that v1 conservatively refuses calls that set
// WORKING_DIRECTORY / ENVIRONMENT / TIMEOUT / INPUT_FILE /
// ERROR_FILE — the lifter doesn't model these yet, and a
// silent drop would change semantics. Refusal is the safe
// default until a real fixture forces the support.
func TestRecoverExecuteProcess_LiftFileProducing_RefusesUnmodeledOpts(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*shadow.ExecuteProcessCall)
		want string
	}{
		{
			name: "WORKING_DIRECTORY",
			mut:  func(c *shadow.ExecuteProcessCall) { c.WorkingDirectory = "/build/sub" },
			want: "WORKING_DIRECTORY",
		},
		{
			name: "ENVIRONMENT",
			mut:  func(c *shadow.ExecuteProcessCall) { c.Environment = []string{"FOO=bar"} },
			want: "ENVIRONMENT",
		},
		{
			name: "TIMEOUT",
			mut:  func(c *shadow.ExecuteProcessCall) { c.Timeout = "30" },
			want: "TIMEOUT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := shadow.ExecuteProcessCall{
				File:       "/src/CMakeLists.txt",
				Line:       4,
				Commands:   [][]string{{"python3", "/src/gen.py"}},
				OutputFile: "/build/out.h",
			}
			tc.mut(&call)
			cc := newCodegenContext()
			err := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "/build", "/build", cc)
			if err == nil {
				t.Fatalf("expected refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal should mention %q; got: %v", tc.want, err)
			}
			if len(cc.Genrules) != 0 {
				t.Errorf("no genrule should be appended on refusal; got %+v", cc.Genrules)
			}
		})
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
