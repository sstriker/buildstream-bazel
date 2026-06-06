package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmitCMakePTestTodos_GroupsByScript mirrors brotli: cmake -P
// add_test registrations sharing ONE script collapse to one todo with N
// anchors, DIFFERENT -P scripts are separate todos (different contracts),
// and a test whose executable WAS converted is excluded.
func TestEmitCMakePTestTodos_GroupsByScript(t *testing.T) {
	dir := t.TempDir()
	body := `add_test([=[roundtrip-a]=] "/usr/bin/cmake" "-P" "/proj/run_test.cmake" "a")
add_test([=[roundtrip-b]=] "/usr/bin/cmake" "-P" "/proj/run_test.cmake" "b")
add_test([=[compat]=] "/usr/bin/cmake" "-P" "/proj/compat.cmake" "x")
add_test([=[unit]=] "/build/unit_test")
`
	mustWriteFile(t, filepath.Join(dir, "CTestTestfile.cmake"), body)
	reg, err := ctest.Parse(dir)
	if err != nil {
		t.Fatalf("ctest.Parse: %v", err)
	}

	c := todos.New()
	// "unit" was emitted as a cc_test; the cmake -P tests were not.
	emitted := []ir.Target{{Name: "unit"}}
	emitCMakePTestTodos(c, reg, emitted, "/proj", "/build")

	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("expected 2 todos (one per -P script), got %d: %+v", len(rep.Todos), rep.Todos)
	}
	// Sorted by group_key (source-relative): compat.cmake before run_test.cmake.
	compat, runTest := rep.Todos[0], rep.Todos[1]
	if compat.GroupKey != "compat.cmake" || runTest.GroupKey != "run_test.cmake" {
		t.Fatalf("group keys = %q, %q; want compat.cmake, run_test.cmake", compat.GroupKey, runTest.GroupKey)
	}
	if len(compat.Anchors) != 1 || len(runTest.Anchors) != 2 {
		t.Errorf("anchor counts = %d, %d; want 1, 2", len(compat.Anchors), len(runTest.Anchors))
	}
	if compat.ID == runTest.ID || compat.ID == "" {
		t.Errorf("distinct scripts must have distinct ids: %q vs %q", compat.ID, runTest.ID)
	}
	// Evidence carries the source-normalized script path + the driver command.
	if runTest.Evidence["script"] != "<SRC>/run_test.cmake" {
		t.Errorf("script evidence = %v; want <SRC>/run_test.cmake", runTest.Evidence["script"])
	}
	if cmds, ok := runTest.Evidence["command"].([]string); !ok || len(cmds) != 1 || cmds[0] != "cmake" {
		t.Errorf("command evidence = %v; want [cmake]", runTest.Evidence["command"])
	}
}

// TestEmitCMakePTestTodos_NonCmakeRunner: an add_test whose COMMAND is an
// unconverted executable (not cmake -P) groups by the target.
func TestEmitCMakePTestTodos_NonCmakeRunner(t *testing.T) {
	dir := t.TempDir()
	body := `add_test([=[harness-1]=] "/build/harness" "case1")
add_test([=[harness-2]=] "/build/harness" "case2")
`
	mustWriteFile(t, filepath.Join(dir, "CTestTestfile.cmake"), body)
	reg, err := ctest.Parse(dir)
	if err != nil {
		t.Fatalf("ctest.Parse: %v", err)
	}
	c := todos.New()
	emitCMakePTestTodos(c, reg, nil, "", "")
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 1 || rep.Todos[0].GroupKey != "harness" {
		t.Fatalf("expected one todo grouped by target 'harness'; got %+v", rep.Todos)
	}
	if _, hasScript := rep.Todos[0].Evidence["script"]; hasScript {
		t.Error("non-cmake-P runner should carry no script evidence")
	}
}

func TestEmitCMakePTestTodos_NilCollector_NoPanic(t *testing.T) {
	emitCMakePTestTodos(nil, nil, nil, "", "") // must not panic
}

// TestEmitCMakePTestTodos_SameBasenameDifferentDir locks in that two
// runners sharing a basename but in different directories stay distinct
// todos (source-relative group key), not collapsed into one id.
func TestEmitCMakePTestTodos_SameBasenameDifferentDir(t *testing.T) {
	dir := t.TempDir()
	body := `add_test([=[a]=] "/usr/bin/cmake" "-P" "/proj/tests/run.cmake" "x")
add_test([=[b]=] "/usr/bin/cmake" "-P" "/proj/tools/run.cmake" "y")
`
	mustWriteFile(t, filepath.Join(dir, "CTestTestfile.cmake"), body)
	reg, err := ctest.Parse(dir)
	if err != nil {
		t.Fatalf("ctest.Parse: %v", err)
	}
	c := todos.New()
	emitCMakePTestTodos(c, reg, nil, "/proj", "/build")
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("same-basename runners in different dirs must stay 2 todos, got %d", len(rep.Todos))
	}
	if rep.Todos[0].GroupKey != "tests/run.cmake" || rep.Todos[1].GroupKey != "tools/run.cmake" {
		t.Errorf("group keys = %q, %q; want tests/run.cmake, tools/run.cmake", rep.Todos[0].GroupKey, rep.Todos[1].GroupKey)
	}
	if rep.Todos[0].ID == rep.Todos[1].ID {
		t.Error("distinct runners must have distinct ids")
	}
}

// TestEmitCMakePTestTodos_NormalizesBuildDirPath is the determinism guard
// for the common $<TARGET_FILE:t> shape: cmake bakes the resolved target
// path (under the transient build dir) into add_test args. The report must
// tokenize it to <BUILD>/… so it doesn't leak/churn on the random build
// dir across configures.
func TestEmitCMakePTestTodos_NormalizesBuildDirPath(t *testing.T) {
	dir := t.TempDir()
	body := `add_test([=[rt]=] "/usr/bin/cmake" "-DTOOL=/tmp/cmbuild-9999/revtool" "-DINPUT=/proj/testdata/x.txt" "-P" "/proj/run.cmake")
`
	mustWriteFile(t, filepath.Join(dir, "CTestTestfile.cmake"), body)
	reg, err := ctest.Parse(dir)
	if err != nil {
		t.Fatalf("ctest.Parse: %v", err)
	}
	c := todos.New()
	// Roots passed WITH trailing separators (common operator input) must
	// still match + slice cleanly into "<BUILD>/…" / "<SRC>/…".
	emitCMakePTestTodos(c, reg, nil, "/proj/", "/tmp/cmbuild-9999/")
	rep := c.Report(todos.DefaultPreamble(), "")
	td := rep.Todos[0]
	invs, _ := td.Evidence["invocations"].([][]string)
	if len(invs) != 1 {
		t.Fatalf("expected 1 invocation, got %v", td.Evidence["invocations"])
	}
	joined := strings.Join(invs[0], " ")
	if strings.Contains(joined, "/tmp/cmbuild-9999") {
		t.Errorf("build-dir path leaked into invocation: %q", joined)
	}
	if !strings.Contains(joined, "-DTOOL=<BUILD>/revtool") {
		t.Errorf("build-dir path not tokenized: %q", joined)
	}
	if !strings.Contains(joined, "-DINPUT=<SRC>/testdata/x.txt") {
		t.Errorf("source path not tokenized: %q", joined)
	}
	if strings.Contains(td.Anchors[0].Construct, "/tmp/cmbuild-9999") {
		t.Errorf("build-dir path leaked into construct: %q", td.Anchors[0].Construct)
	}
}

// TestEmitInternalDropTodos_GroupsByKind checks one todo per drop kind,
// each carrying the dropped outputs as anchors + evidence.
func TestEmitInternalDropTodos_GroupsByKind(t *testing.T) {
	c := todos.New()
	filtered := map[string]string{
		"install/manifest.txt": "install",
		"cpack/pkg.tar.gz":     "cpack",
		"install/other.txt":    "install",
	}
	emitInternalDropTodos(c, filtered)
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("expected 2 todos (install, cpack), got %d", len(rep.Todos))
	}
	// Sorted by group_key: cpack before install.
	if rep.Todos[0].GroupKey != "cpack" || rep.Todos[1].GroupKey != "install" {
		t.Errorf("group keys = %q, %q; want cpack, install", rep.Todos[0].GroupKey, rep.Todos[1].GroupKey)
	}
	if len(rep.Todos[1].Anchors) != 2 {
		t.Errorf("install todo should fold 2 outputs; got %d anchors", len(rep.Todos[1].Anchors))
	}
}

func TestEmitInternalDropTodos_Empty_NoOp(t *testing.T) {
	c := todos.New()
	emitInternalDropTodos(c, nil)
	if c.Len() != 0 {
		t.Errorf("empty drop set should add nothing; got %d", c.Len())
	}
	emitInternalDropTodos(nil, map[string]string{"x": "install"}) // no panic
}

// TestEmitInstallScriptTodos_ScriptAndCode checks install(SCRIPT) /
// install(CODE) become per-(site,scriptFile) todos with the backtrace
// site resolved onto the anchor.
func TestEmitInstallScriptTodos_ScriptAndCode(t *testing.T) {
	bg := fileapi.BacktraceGraph{
		Commands: []string{"install"},
		Files:    []string{"CMakeLists.txt"},
		Nodes: []fileapi.BacktraceNode{
			{File: 0},
			{File: 0, Line: 7, Command: 0},
		},
	}
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"d": {
				BacktraceGraph: bg,
				Installers: []fileapi.DirectoryInstaller{
					{Type: "script", ScriptFile: "post.cmake", Backtrace: 1},
					{Type: "code", Backtrace: 1},
					{Type: "file"}, // not a todo
				},
			},
		},
	}
	c := todos.New()
	emitInstallScriptTodos(c, r)
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("expected 2 todos (script + code), got %d", len(rep.Todos))
	}
	var sawScript, sawCode bool
	for _, td := range rep.Todos {
		switch td.Kind {
		case "install-script":
			sawScript = true
			if td.Evidence["script_file"] != "post.cmake" {
				t.Errorf("install-script evidence missing script_file: %+v", td.Evidence)
			}
			if td.Anchors[0].File != "CMakeLists.txt" || td.Anchors[0].Line != 7 {
				t.Errorf("install-script anchor site = %s:%d, want CMakeLists.txt:7", td.Anchors[0].File, td.Anchors[0].Line)
			}
		case "install-code":
			sawCode = true
		}
	}
	if !sawScript || !sawCode {
		t.Errorf("expected both install-script and install-code todos; script=%v code=%v", sawScript, sawCode)
	}
}

// TestEmitInstallScriptTodos_FoldsSitelessCode is the regression guard
// for the duplicate-id bug: multiple install(CODE) directives with no
// recoverable backtrace site all key on the kind, so they must FOLD into
// one todo with N anchors (not N todos colliding on (kind, group_key) →
// identical id), keeping the report deterministic and ids unique.
func TestEmitInstallScriptTodos_FoldsSitelessCode(t *testing.T) {
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"d": {Installers: []fileapi.DirectoryInstaller{
				{Type: "code"},
				{Type: "code"},
				{Type: "code"},
			}},
		},
	}
	c := todos.New()
	emitInstallScriptTodos(c, r)
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 1 {
		t.Fatalf("siteless install(CODE) must fold to 1 todo, got %d", len(rep.Todos))
	}
	if len(rep.Todos[0].Anchors) != 3 {
		t.Errorf("expected 3 folded anchors, got %d", len(rep.Todos[0].Anchors))
	}
	if rep.Todos[0].ID == "" {
		t.Error("folded todo missing id")
	}
}

// TestEmitInstallScriptTodos_ReanchorsAbsoluteSite guards determinism for
// the configure_file-generated-CMakeLists case: an absolute build-dir site
// from the BacktraceGraph must be re-anchored to a workspace-relative path
// so it doesn't leak into (and churn) the group_key / id / anchor.
func TestEmitInstallScriptTodos_ReanchorsAbsoluteSite(t *testing.T) {
	bg := fileapi.BacktraceGraph{
		Commands: []string{"install"},
		Files:    []string{"/tmp/cmbuild-xyz/generated/CMakeLists.txt"},
		Nodes: []fileapi.BacktraceNode{
			{File: 0},
			{File: 0, Line: 12, Command: 0},
		},
	}
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"d": {
				Paths:          fileapi.CodemodelPaths{Source: "/home/u/proj/src", Build: "/tmp/cmbuild-xyz"},
				BacktraceGraph: bg,
				Installers:     []fileapi.DirectoryInstaller{{Type: "code", Backtrace: 1}},
			},
		},
	}
	c := todos.New()
	emitInstallScriptTodos(c, r)
	rep := c.Report(todos.DefaultPreamble(), "")
	td := rep.Todos[0]
	if td.GroupKey != "generated/CMakeLists.txt" {
		t.Errorf("group_key = %q; want re-anchored generated/CMakeLists.txt", td.GroupKey)
	}
	if strings.Contains(td.Anchors[0].File, "/tmp/cmbuild-xyz") {
		t.Errorf("absolute build path leaked into anchor: %q", td.Anchors[0].File)
	}
	if td.Anchors[0].Line != 12 {
		t.Errorf("anchor line = %d; want 12", td.Anchors[0].Line)
	}
}

func TestEmitInstallScriptTodos_NilReply_NoOp(t *testing.T) {
	c := todos.New()
	emitInstallScriptTodos(c, nil)
	if c.Len() != 0 {
		t.Errorf("nil reply should add nothing; got %d", c.Len())
	}
	emitInstallScriptTodos(nil, &fileapi.Reply{}) // no panic
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
