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

// TestEmitCMakePTestTodos_GroupsByRunner mirrors brotli: many cmake -P
// add_test registrations sharing one runner collapse to ONE todo with N
// anchors, while a test whose executable WAS converted is excluded.
func TestEmitCMakePTestTodos_GroupsByRunner(t *testing.T) {
	dir := t.TempDir()
	body := `add_test([=[roundtrip-a]=] "/usr/bin/cmake" "-P" "run_test.cmake" "a")
add_test([=[roundtrip-b]=] "/usr/bin/cmake" "-P" "run_test.cmake" "b")
add_test([=[unit]=] "/build/unit_test")
`
	mustWriteFile(t, filepath.Join(dir, "CTestTestfile.cmake"), body)
	reg, err := ctest.Parse(dir)
	if err != nil {
		t.Fatalf("ctest.Parse: %v", err)
	}

	c := todos.New()
	// "unit" was emitted as a cc_test; the two cmake runners were not.
	emitted := []ir.Target{{Name: "unit"}}
	emitCMakePTestTodos(c, reg, emitted)

	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 1 {
		t.Fatalf("expected 1 grouped todo (one cmake runner), got %d: %+v", len(rep.Todos), rep.Todos)
	}
	td := rep.Todos[0]
	if td.Kind != "cmake-p-test" {
		t.Errorf("kind = %q, want cmake-p-test", td.Kind)
	}
	if td.GroupKey != "cmake" {
		t.Errorf("group_key = %q, want cmake (the runner basename)", td.GroupKey)
	}
	if len(td.Anchors) != 2 {
		t.Errorf("expected 2 anchors (roundtrip-a, -b), got %d", len(td.Anchors))
	}
	if td.ID == "" {
		t.Error("todo missing stable id")
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
