package lower

import (
	"os"
	"path/filepath"
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
	emitCMakePTestTodos(c, reg, emitted)

	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("expected 2 todos (one per -P script), got %d: %+v", len(rep.Todos), rep.Todos)
	}
	// Sorted by group_key (basename): compat.cmake before run_test.cmake.
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
	// Evidence carries the full script path + the driver command.
	if runTest.Evidence["script"] != "/proj/run_test.cmake" {
		t.Errorf("script evidence = %v; want /proj/run_test.cmake", runTest.Evidence["script"])
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
	emitCMakePTestTodos(c, reg, nil)
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 1 || rep.Todos[0].GroupKey != "harness" {
		t.Fatalf("expected one todo grouped by target 'harness'; got %+v", rep.Todos)
	}
	if _, hasScript := rep.Todos[0].Evidence["script"]; hasScript {
		t.Error("non-cmake-P runner should carry no script evidence")
	}
}

func TestEmitCMakePTestTodos_NilCollector_NoPanic(t *testing.T) {
	emitCMakePTestTodos(nil, nil, nil) // must not panic
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
