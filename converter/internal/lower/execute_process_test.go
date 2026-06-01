package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift to succeed; got refusals %+v", refusals)
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift to succeed; got refusals %+v", refusals)
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

// TestRecoverExecuteProcess_LiftCMakeECreateSymlink covers the
// `cmake -E create_symlink <target> <link_name>` shape — LLVM's
// AddLLVM.cmake uses this for tool-vs-link aliases (clang →
// clang-18). Under Bazel's hermetic action model the link-vs-
// copy distinction is meaningless (consumers read bytes by
// path), so the lift reuses liftCMakeECopy. Tag reflects the
// original op so audit/triage can distinguish symlink calls
// from real copies.
func TestRecoverExecuteProcess_LiftCMakeECreateSymlink(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:     "/src/CMakeLists.txt",
		Line:     20,
		Commands: [][]string{{"cmake", "-E", "create_symlink", "/src/bin/clang-18", "/build/bin/clang"}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift to succeed; got refusals %+v", refusals)
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
	// The tag preserves the original op for triage.
	foundSymlinkTag := false
	for _, tag := range g.Tags {
		if strings.Contains(tag, "create_symlink") {
			foundSymlinkTag = true
		}
	}
	if !foundSymlinkTag {
		t.Errorf("expected create_symlink tag; got tags %v", g.Tags)
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
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) == 0 {
		t.Fatal("expected refusal")
	}
	err := formatExecuteProcessFailure(refusals)
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift to succeed; got refusals %+v", refusals)
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
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
		t.Fatalf("expected lift to succeed; got refusals: %+v", refusals)
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
			_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, cc)
			if len(refusals) == 0 {
				t.Fatalf("expected refusal")
			}
			err := formatExecuteProcessFailure(refusals)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal should mention %q; got: %v", tc.want, err)
			}
			if len(cc.Genrules) != 0 {
				t.Errorf("no genrule should be appended on refusal; got %+v", cc.Genrules)
			}
		})
	}
}

// TestRecoverExecuteProcess_LiftCMakeEConfigureFile covers the
// cmake -E configure_file lift. Verify-pass succeeds when the
// template + rendered bytes have a recoverable values dict;
// emit a cmake_configure_file target whose tool is
// //tools:cmake-configure-file (no cmake on the executor) with
// the template anchored on the spec and the cmake-codegen-lifted
// tag set.
func TestRecoverExecuteProcess_LiftCMakeEConfigureFile(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	template := "ver=@VER@\n"
	rendered := []byte("ver=2.0\n")
	if err := os.MkdirAll(filepath.Join(hostSrc, "inc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "inc", "v.h.in"), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostBuild, "gen.h"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		Line: 8,
		Commands: [][]string{{
			"/usr/bin/cmake", "-E", "configure_file",
			filepath.Join(hostSrc, "inc", "v.h.in"),
			filepath.Join(hostBuild, "gen.h"),
		}},
	}}
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("expected lift, got refusals: %+v", refusals)
	}
	if len(outs) != 1 || outs[0].RelOutput != "gen.h" {
		t.Fatalf("outs: %+v", outs)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if g.Kind != ir.KindCMakeConfigureFile || g.CMakeConfigureFile == nil {
		t.Fatalf("kind = %v (spec nil? %v); want KindCMakeConfigureFile", g.Kind, g.CMakeConfigureFile == nil)
	}
	if g.CMakeConfigureFile.Template != "inc/v.h.in" {
		t.Errorf("template: %q want inc/v.h.in", g.CMakeConfigureFile.Template)
	}
	if g.CMakeConfigureFile.Tool != "//tools:cmake-configure-file" {
		t.Errorf("tool: %q", g.CMakeConfigureFile.Tool)
	}
	wantTags := map[string]bool{
		"cmake-codegen":                                   true,
		"cmake-codegen-cmake-e":                           true,
		"cmake-codegen-driver=cmake_e":                    true,
		"cmake-codegen-execute-process":                   true,
		"cmake-codegen-execute-process-op=configure_file": true,
		"cmake-codegen-lifted":                            true,
	}
	if len(g.Tags) != len(wantTags) {
		t.Errorf("tags: %v want %v", g.Tags, wantTags)
	}
	for _, tg := range g.Tags {
		if !wantTags[tg] {
			t.Errorf("unexpected tag %q in %v", tg, g.Tags)
		}
	}
}

// TestRecoverExecuteProcess_LiftCMakeEConfigureFile_LiftDisabledFallback
// asserts that liftEnabled=false keeps the lift on the legacy
// bytes-embedded shape: the cmd base64-decodes the rendered
// bytes, tools is empty, and srcs is also empty (the legacy
// cmd doesn't reference the template, so staging it would
// create confusing rebuild semantics — a template edit would
// invalidate the genrule but the action would re-emit the same
// baked-in bytes).
func TestRecoverExecuteProcess_LiftCMakeEConfigureFile_LiftDisabledFallback(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "t.in"), []byte("v=@VER@\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostBuild, "t.out"), []byte("v=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{
			"/usr/bin/cmake", "-E", "configure_file",
			filepath.Join(hostSrc, "t.in"),
			filepath.Join(hostBuild, "t.out"),
		}},
	}}
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, hostBuild, hostBuild, false, nil, cc); len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	g := cc.Genrules[0]
	if len(g.GenruleTools) != 0 {
		t.Errorf("liftEnabled=false should not stage tools; got %v", g.GenruleTools)
	}
	if len(g.Srcs) != 0 {
		t.Errorf("liftEnabled=false should not stage srcs (bake doesn't use the template); got %v", g.Srcs)
	}
	// The rendered bytes are \n-only text, so the bake lowers to the
	// readable skylib write_file (shared bakeFileTarget) rather than the
	// legacy base64 genrule.
	if g.Kind != ir.KindWriteFile {
		t.Errorf("text bake should use write_file; got kind %v cmd %q", g.Kind, g.GenruleCmd)
	}
	if join := strings.Join(g.WriteFileContent, "\n"); join != "v=1\n" {
		t.Errorf("write_file content round-trip = %q, want %q", join, "v=1\n")
	}
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-lifted" {
			t.Errorf("lifted tag should NOT be present when liftEnabled=false: %v", g.Tags)
		}
	}
}

// TestRecoverExecuteProcess_LiftCMakeEConfigureFile_NoBuildDirSoftSkips
// covers the trace-only / offline path where lower.Options.BuildDir
// is unset: liftCMakeEConfigureFile can't read rendered bytes
// without it, and surfacing every cmake -E configure_file call
// as a refusal would force every offline ToIR run to flip
// --unsupported-execute-process-fallback. Soft-skip instead
// (no genrule emitted, no refusal recorded), parity with
// recoverConfigureFiles and recoverFileGenerate.
func TestRecoverExecuteProcess_LiftCMakeEConfigureFile_NoBuildDirSoftSkips(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "t.in"), []byte("v=@VER@\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{
			"/usr/bin/cmake", "-E", "configure_file",
			filepath.Join(hostSrc, "t.in"),
			"/build/t.out",
		}},
	}}
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", true, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("empty hostBuildDir should soft-skip, not refuse; got refusals %+v", refusals)
	}
	if len(outs) != 0 {
		t.Errorf("no outs should be produced when hostBuildDir is empty; got %+v", outs)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("no genrules should be produced when hostBuildDir is empty; got %+v", cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftCMakeEConfigureFile_MissingRenderedSoftSkips
// covers the live-build-dir-set-but-output-missing case (stale
// fixture, deleted output, etc.). Same soft-skip behavior as
// the no-build-dir case, parity with recoverConfigureFiles's
// per-call read-error treatment.
func TestRecoverExecuteProcess_LiftCMakeEConfigureFile_MissingRenderedSoftSkips(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "t.in"), []byte("v=@VER@\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No t.out in hostBuild — simulates the rendered output going missing.
	calls := []shadow.ExecuteProcessCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{
			"/usr/bin/cmake", "-E", "configure_file",
			filepath.Join(hostSrc, "t.in"),
			filepath.Join(hostBuild, "t.out"),
		}},
	}}
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("missing rendered output should soft-skip, not refuse; got %+v", refusals)
	}
	if len(outs) != 0 || len(cc.Genrules) != 0 {
		t.Errorf("no outputs expected on soft-skip; got outs=%+v genrules=%+v", outs, cc.Genrules)
	}
}

// TestRecoverExecuteProcess_LiftCMakeEConfigureFile_RefusesFlags
// asserts that v1 refuses cmake -E configure_file forms that
// carry flags (--copy-only, --escape-quotes, --at-only, -D...).
// These need real fixtures + values-recovery plumbing before
// the lift can extend safely; until then, refusing keeps the
// classifier honest about coverage.
func TestRecoverExecuteProcess_LiftCMakeEConfigureFile_RefusesFlags(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "x.in"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostBuild, "x.out"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ExecuteProcessCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		Commands: [][]string{{
			"/usr/bin/cmake", "-E", "configure_file",
			"--copy-only",
			filepath.Join(hostSrc, "x.in"),
			filepath.Join(hostBuild, "x.out"),
		}},
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want 1 refusal for flag-bearing form; got %+v", refusals)
	}
	if !strings.Contains(refusals[0].Reason, "v1 supports the 2-arg form only") {
		t.Errorf("refusal reason: %q", refusals[0].Reason)
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
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) == 0 {
		t.Fatal("expected refusal for the git call")
	}
	// The touch lift still went through.
	if len(cc.Genrules) != 1 {
		t.Errorf("touch should have lifted to one genrule even though git refused; got %+v", cc.Genrules)
	}
	err := formatExecuteProcessFailure(refusals)
	if !strings.Contains(err.Error(), "[stamp]") {
		t.Errorf("refusal should mention [stamp]; got: %v", err)
	}
}

// TestRecoverExecuteProcess_RescueProbeViaDumpVars covers Phase 4's
// probe-bucket rescue: when a probe call's OUTPUT_VARIABLE is in
// cmakeVars (captured by the dump-vars hook), the rescue arm
// skips the refusal — downstream configure_file / file(GENERATE)
// lifts consume the value through cmakeVars.
func TestRecoverExecuteProcess_RescueProbeViaDumpVars(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           7,
		Commands:       [][]string{{"gcc", "-dumpversion"}},
		OutputVariable: "GCC_VERSION",
	}}
	cc := newCodegenContext()
	cmakeVars := map[string]string{"GCC_VERSION": "13.2.0"}
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, cmakeVars, cc)
	if len(refusals) != 0 {
		t.Errorf("expected probe rescue; got refusals: %v", refusals)
	}
}

// TestRecoverExecuteProcess_NoRescueWhenVarMissing confirms the
// capture gate still bites for a STAMP: an uncaptured VCS-revision
// stamp refuses (its value would otherwise bake into srckey, silently
// pinning the build to one commit). Probes broadened to skip-when-
// uncaptured (see _GenericProbeSkips); the gate now meaningfully
// applies only to stamps.
func TestRecoverExecuteProcess_NoRescueWhenVarMissing(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           7,
		Commands:       [][]string{{"git", "rev-parse", "HEAD"}},
		OutputVariable: "GIT_SHA",
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) == 0 {
		t.Error("expected refusal for an uncaptured stamp")
	}
}

// TestRecoverExecuteProcess_RescueStamp covers the same rescue for
// the stamp bucket (`git rev-parse HEAD`).
func TestRecoverExecuteProcess_RescueStamp(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           5,
		Commands:       [][]string{{"git", "rev-parse", "HEAD"}},
		OutputVariable: "GIT_SHA",
	}}
	cc := newCodegenContext()
	cmakeVars := map[string]string{"GIT_SHA": "abc123def456"}
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, cmakeVars, cc)
	if len(refusals) != 0 {
		t.Errorf("expected stamp rescue via dump-vars; got refusals: %v", refusals)
	}
}

// TestRecoverExecuteProcess_RescueCapabilityProbe covers an `ar` /
// `ranlib` "does this tool support flag X" check: a RESULT_VARIABLE-only
// probe whose exit status's effect lands in the recovered compile flags,
// never a build input. It skips as a recognized probe (the broaden skips
// any non-feature probe regardless of capture), producing no Bazel
// artifact and no refusal.
func TestRecoverExecuteProcess_RescueCapabilityProbe(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           9,
		Commands:       [][]string{{"ar", "rD", "t.a"}},
		ResultVariable: "_AR_D",
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) != 0 {
		t.Errorf("expected capability-probe rescue; got refusals: %v", refusals)
	}
}

// TestRecoverExecuteProcess_RescueHostDetectionScript covers the
// host-triple detection script (config.guess): its stdout is the build
// host's triple, which lands in generated config headers the converter
// recovers directly. The call produces no Bazel artifact, so it's
// rescued even though its OUTPUT_VARIABLE isn't in cmakeVars.
func TestRecoverExecuteProcess_RescueHostDetectionScript(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           11,
		Commands:       [][]string{{"/bin/sh", "/src/cmake/config.guess"}},
		OutputVariable: "TT_OUT",
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) != 0 {
		t.Errorf("expected host-detection rescue; got refusals: %v", refusals)
	}
}

// TestRecoverExecuteProcess_GenericProbeSkips confirms the broadened
// stance: a recognized host/toolchain probe with a generic (non-feature-
// declaration) result var skips whether or not the dump-vars hook
// captured its value — the probe is never a build input, so emitting
// nothing is faithful. (A HAVE_X-style result var instead lifts to a
// build setting — see _FeatureProbeToBuildSetting; a stamp still gates on
// capture — see _NoRescueWhenVarMissing.)
func TestRecoverExecuteProcess_GenericProbeSkips(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           13,
		Commands:       [][]string{{"python3", "-c", "import pygments"}},
		ResultVariable: "PYGMENTS_STATUS",
	}}
	// Uncaptured: skips (no refusal) under the broaden.
	cc := newCodegenContext()
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc); len(refusals) != 0 {
		t.Errorf("uncaptured generic probe should skip; got %v", refusals)
	}
	// Captured: also skips (the value is recovered via Reply.Vars).
	cc = newCodegenContext()
	captured := map[string]string{"PYGMENTS_STATUS": "0"}
	if _, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, captured, cc); len(refusals) != 0 {
		t.Errorf("captured generic probe should skip; got %v", refusals)
	}
}

// TestRecoverExecuteProcess_FeatureProbeToBuildSetting covers the
// probe-as-declaration lift: a probe writing a HAVE_X-style variable
// becomes a bool_flag + config_setting and skips the refusal, instead of
// being dropped or refused. The probe writes RESULT_VARIABLE, whose
// captured value is the command's exit status — "0" means `import zlib`
// succeeded, i.e. zlib IS present — so the flag defaults True (exit
// success inverts cmake's string-truthiness; see featureProbeDefault).
func TestRecoverExecuteProcess_FeatureProbeToBuildSetting(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           7,
		Commands:       [][]string{{"python3", "-c", "import zlib"}},
		ResultVariable: "HAVE_ZLIB",
	}}
	cc := newCodegenContext()
	captured := map[string]string{"HAVE_ZLIB": "0"} // exit 0 == success == zlib present
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, captured, cc)
	if len(refusals) != 0 {
		t.Fatalf("feature probe should lift, not refuse; got %v", refusals)
	}
	var bf, cs *ir.Target
	for i := range cc.Genrules {
		switch cc.Genrules[i].Kind {
		case ir.KindBoolFlag:
			bf = &cc.Genrules[i]
		case ir.KindConfigSetting:
			cs = &cc.Genrules[i]
		}
	}
	if bf == nil || cs == nil {
		t.Fatalf("expected bool_flag + config_setting; got %+v", cc.Genrules)
	}
	if bf.Name != "have_zlib" || !bf.BoolFlagDefault {
		t.Errorf("bool_flag: got name=%q default=%v, want have_zlib/true", bf.Name, bf.BoolFlagDefault)
	}
	if cs.Name != "have_zlib_enabled" || cs.ConfigSettingFlag != ":have_zlib" || cs.ConfigSettingValue != "True" {
		t.Errorf("config_setting: got %+v", *cs)
	}
}

// TestRecoverExecuteProcess_FeatureProbeDedup confirms a feature probe
// that recurs in the trace (configure re-evaluation) lifts to a single
// bool_flag/config_setting pair — duplicate target names would break emit.
func TestRecoverExecuteProcess_FeatureProbeDedup(t *testing.T) {
	probe := shadow.ExecuteProcessCall{
		File:           "/src/CMakeLists.txt",
		Line:           7,
		Commands:       [][]string{{"python3", "-c", "import zlib"}},
		ResultVariable: "HAVE_ZLIB",
	}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{probe, probe}, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("unexpected refusals: %v", refusals)
	}
	var flags, settings int
	for _, tgt := range cc.Genrules {
		switch tgt.Kind {
		case ir.KindBoolFlag:
			flags++
		case ir.KindConfigSetting:
			settings++
		}
	}
	if flags != 1 || settings != 1 {
		t.Errorf("dedup: got %d bool_flag + %d config_setting, want 1 + 1", flags, settings)
	}
}

// TestRecoverExecuteProcess_FeatureProbeNameCollision confirms two
// DISTINCT cmake variables that sanitize to the same Bazel build-setting
// name (here a case-only HAVE_ZLIB vs have_zlib) refuse rather than
// silently dropping the second knob onto the first's target. The first
// lifts; the second surfaces a refusal naming both variables.
func TestRecoverExecuteProcess_FeatureProbeNameCollision(t *testing.T) {
	calls := []shadow.ExecuteProcessCall{
		{
			File:           "/src/CMakeLists.txt",
			Line:           7,
			Commands:       [][]string{{"python3", "-c", "import zlib"}},
			ResultVariable: "HAVE_ZLIB",
		},
		{
			File:           "/src/CMakeLists.txt",
			Line:           9,
			Commands:       [][]string{{"python3", "-c", "import zlib"}},
			ResultVariable: "have_zlib",
		},
	}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("collision should surface exactly one refusal; got %d: %v", len(refusals), refusals)
	}
	// The refusal names both colliding variables and the shared target.
	for _, want := range []string{`"HAVE_ZLIB"`, `"have_zlib"`} {
		if !strings.Contains(refusals[0].Reason, want) {
			t.Errorf("refusal reason %q missing %s", refusals[0].Reason, want)
		}
	}
	// The first probe still lifts to a single pair.
	var flags, settings int
	for _, tgt := range cc.Genrules {
		switch tgt.Kind {
		case ir.KindBoolFlag:
			flags++
		case ir.KindConfigSetting:
			settings++
		}
	}
	if flags != 1 || settings != 1 {
		t.Errorf("collision: first probe should still lift; got %d bool_flag + %d config_setting, want 1 + 1", flags, settings)
	}
}

// TestFeatureProbeDefault locks in the channel-aware default: a
// RESULT_VARIABLE is an exit status ("0" == success == feature present),
// the INVERSE of an OUTPUT_VARIABLE stdout string (run through
// cmakeTruthy). An uncaptured value ("") defaults False on both channels.
func TestFeatureProbeDefault(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		fromResult bool
		want       bool
	}{
		{"result exit-0 success -> true", "0", true, true},
		{"result exit-1 failure -> false", "1", true, false},
		{"result exit-127 failure -> false", "127", true, false},
		{"result uncaptured -> false", "", true, false},
		{"output truthy ON -> true", "ON", false, true},
		{"output truthy 1 -> true", "1", false, true},
		{"output falsey 0 -> false", "0", false, false},
		{"output numeric-zero 0.0 -> false", "0.0", false, false},
		{"output uncaptured -> false", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := featureProbeDefault(tc.value, tc.fromResult); got != tc.want {
				t.Errorf("featureProbeDefault(%q, %v) = %v, want %v", tc.value, tc.fromResult, got, tc.want)
			}
		})
	}
}

func TestConfigureLogVars_TryCompileSuccess(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind:        "try_compile-v1",
			BuildResult: &fileapi.EventBuildResult{Variable: "HAVE_FOO", ExitCode: 0},
		},
	}
	got := configureLogVars(events)
	if got["HAVE_FOO"] != "1" {
		t.Errorf("HAVE_FOO: %q, want 1", got["HAVE_FOO"])
	}
}

func TestConfigureLogVars_TryCompileFailure(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind:        "try_compile-v1",
			BuildResult: &fileapi.EventBuildResult{Variable: "HAVE_BAR", ExitCode: 1},
		},
	}
	if configureLogVars(events)["HAVE_BAR"] != "0" {
		t.Errorf("HAVE_BAR should be 0 on failure")
	}
}

func TestConfigureLogVars_TryRunRecorded(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind:      "try_run-v1",
			RunResult: &fileapi.EventRunResult{Variable: "RUN_RESULT", ExitCode: 0},
		},
	}
	if configureLogVars(events)["RUN_RESULT"] != "1" {
		t.Errorf("RUN_RESULT should be 1")
	}
}

func TestConfigureLogVars_EmptyEvents(t *testing.T) {
	if got := configureLogVars(nil); got != nil {
		t.Errorf("nil events should return nil; got %v", got)
	}
}

// TestRecoverExecuteProcess_RescueViaConfigureLog covers the
// configureLog-driven rescue: a probe whose OUTPUT_VARIABLE
// isn't in cmakeVars but IS in the configureLog as a try_compile
// result variable rescues without refusal.
func TestRecoverExecuteProcess_RescueViaConfigureLog(t *testing.T) {
	// Simulate the merge that lower.go does between cmakeVars and
	// configureLogVars before passing to recoverExecuteProcess.
	clVars := configureLogVars([]fileapi.Event{
		{
			Kind:        "try_compile-v1",
			BuildResult: &fileapi.EventBuildResult{Variable: "GCC_VERSION", ExitCode: 0},
		},
	})
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           42,
		Commands:       [][]string{{"gcc", "--version"}},
		OutputVariable: "GCC_VERSION",
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, clVars, cc)
	if len(refusals) != 0 {
		t.Errorf("expected configureLog rescue; got refusals: %v", refusals)
	}
}

func TestConfigureLogVars_FindPackageFound(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind:  "find_package-v1",
			Found: &fileapi.EventFindPackageFound{Package: "ZLIB", IsFound: true},
		},
	}
	if got := configureLogVars(events)["ZLIB_FOUND"]; got != "1" {
		t.Errorf("ZLIB_FOUND: %q, want 1", got)
	}
}

func TestConfigureLogVars_FindPackageNotFound(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind:  "find_package-v1",
			Found: &fileapi.EventFindPackageFound{Package: "Foo", IsFound: false},
		},
	}
	if got := configureLogVars(events)["Foo_FOUND"]; got != "0" {
		t.Errorf("Foo_FOUND: %q, want 0", got)
	}
}

// TestConfigureLogVars_FindPackageNoPackageName confirms a
// find_package-v1 event with no resolved package name (or the
// cmake 4.3 find-v1 scalar shape, which carries a path rather than
// a package name) contributes no synthesised variable — there's no
// real `<Pkg>_FOUND` name to bind.
func TestConfigureLogVars_FindPackageNoPackageName(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind:  "find_package-v1",
			Found: &fileapi.EventFindPackageFound{Path: "/usr/bin/cc", IsFound: true},
		},
	}
	if got := configureLogVars(events); len(got) != 0 {
		t.Errorf("expected no vars for package-less find event; got %v", got)
	}
}

// TestRecoverExecuteProcess_RescueViaFindPackage covers the
// find_package leg of the configureLog rescue: a probe whose
// OUTPUT_VARIABLE is bound to a `<Pkg>_FOUND` outcome recorded in
// the configureLog rescues without a refusal, and the resolved
// value reaches downstream consumers through the merged rescueVars
// map (the same path try_compile-keyed probes use). This mirrors
// the merge lower.go performs before calling recoverExecuteProcess.
func TestRecoverExecuteProcess_RescueViaFindPackage(t *testing.T) {
	clVars := configureLogVars([]fileapi.Event{
		{
			Kind:  "find_package-v1",
			Found: &fileapi.EventFindPackageFound{Package: "OpenSSL", IsFound: true},
		},
	})
	if clVars["OpenSSL_FOUND"] != "1" {
		t.Fatalf("precondition: OpenSSL_FOUND not projected; got %v", clVars)
	}
	calls := []shadow.ExecuteProcessCall{{
		File:           "/src/CMakeLists.txt",
		Line:           17,
		Commands:       [][]string{{"pkg-config", "--exists", "openssl"}},
		OutputVariable: "OpenSSL_FOUND",
	}}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess(calls, "/src", "/src", "", "/build", false, clVars, cc)
	if len(refusals) != 0 {
		t.Errorf("expected find_package rescue; got refusals: %v", refusals)
	}
}
