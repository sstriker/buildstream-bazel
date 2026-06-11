package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func ootCall(file string, line int, argv ...string) shadow.ExecuteProcessCall {
	return shadow.ExecuteProcessCall{File: file, Line: line, Commands: [][]string{argv}}
}

// partitionOutOfTreeExec routes the codemodel-source-backed build-dir
// subproject into the LIFT, sets the uncertain calls aside to NOTE, and keeps
// cmake's own confident noise silent (try_compile scratch under
// <build>/CMakeFiles, bundled-module probes that issue from neither the build
// dir nor the prefix tree).
func TestPartitionOutOfTreeExec_Buckets(t *testing.T) {
	build := "/build"
	prefix := "/synth"
	consumed := map[string]bool{"_deps/sub-src/foo.c": true}
	calls := []shadow.ExecuteProcessCall{
		// Confident noise: try_compile scratch.
		ootCall("/build/CMakeFiles/CMakeScratch/TryCompile-aaaa/CMakeLists.txt", 1, "cc", "-o", "a.out"),
		// Confident noise: a bundled cmake module, not under build or prefix.
		ootCall("/usr/share/cmake-4.0/Modules/CMakeDetermineCompilerId.cmake", 2, "cc", "-dumpversion"),
		// Lift: build-dir CMakeLists with codemodel sources beneath it.
		ootCall("/build/_deps/sub-src/CMakeLists.txt", 3, "python3", "gen.py"),
		// Note: build-dir call with NO codemodel sources beneath it.
		ootCall("/build/scripts/CMakeLists.txt", 4, "sh", "stamp.sh"),
		// Note: prefix-tree probe.
		ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 5, "foo-config", "--cflags"),
	}
	lift, note := partitionOutOfTreeExec(calls, build, prefix, consumed)
	if len(lift) != 1 || lift[0].File != "/build/_deps/sub-src/CMakeLists.txt" {
		t.Fatalf("lift: want the codemodel-backed subproject call; got %+v", lift)
	}
	bySig := map[outOfTreeExecSignal]int{}
	for _, n := range note {
		bySig[n.Signal]++
	}
	if len(note) != 2 {
		t.Fatalf("want 2 noted (1 lifted + 2 confident-noise dropped); got %d: %+v", len(note), note)
	}
	if bySig[signalBuildDirOther] != 1 || bySig[signalPrefixTree] != 1 {
		t.Errorf("note signal split wrong: %+v", bySig)
	}
}

// CMakeFiles anywhere in the build-relative path is confident noise, even
// nested under a subproject dir (cmake's per-subproject scratch) — neither
// lifted nor noted.
func TestPartitionOutOfTreeExec_CMakeFilesSegment(t *testing.T) {
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{
		ootCall("/build/_deps/sub-build/CMakeFiles/foo.dir/probe.cmake", 1, "cc", "-x"),
	}, "/build", "", map[string]bool{"_deps/sub-build/foo.c": true})
	if len(lift) != 0 || len(note) != 0 {
		t.Errorf("CMakeFiles scratch must stay silent; got lift=%+v note=%+v", lift, note)
	}
}

// No build dir + no prefix dir: every out-of-tree call is unanchorable, so
// nothing is lifted or noted (we can't distinguish noise from intent — and the
// build-dir signal is the whole point).
func TestPartitionOutOfTreeExec_NoAnchors(t *testing.T) {
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{
		ootCall("/build/_deps/sub-src/CMakeLists.txt", 1, "python3", "gen.py"),
	}, "", "", nil)
	if len(lift) != 0 || len(note) != 0 {
		t.Errorf("no anchors => nothing surfaced; got lift=%+v note=%+v", lift, note)
	}
}

// emitOutOfTreeExecuteProcessTodos groups by signal, folds anchors, dedupes
// identical (file,line,argv) triples, and normalizes paths so the per-run
// build dir never leaks.
func TestEmitOutOfTreeExecuteProcessTodos_GroupingAndNormalize(t *testing.T) {
	c := todos.New()
	notes := []outOfTreeExecNote{
		{File: "/b/_deps/sub-src/CMakeLists.txt", Line: 3, Argv: []string{"python3", "gen.py", "/b/in"}, Signal: signalBuildSubproject},
		{File: "/b/_deps/sub-src/CMakeLists.txt", Line: 3, Argv: []string{"python3", "gen.py", "/b/in"}, Signal: signalBuildSubproject},
		{File: "/b/other/CMakeLists.txt", Line: 9, Argv: []string{"sh", "x.sh"}, Signal: signalBuildSubproject},
		{File: "/synth/FooConfig.cmake", Line: 1, Argv: []string{"foo", "--v"}, Signal: signalPrefixTree},
	}
	emitOutOfTreeExecuteProcessTodos(c, notes, "/src", "/b")
	rep := c.Report(todos.Preamble{}, "")
	if len(rep.Todos) != 2 {
		t.Fatalf("want 2 todos (one per signal); got %d: %+v", len(rep.Todos), rep.Todos)
	}
	var sub *todos.Todo
	for i := range rep.Todos {
		if rep.Todos[i].GroupKey == string(signalBuildSubproject) {
			sub = &rep.Todos[i]
		}
		if rep.Todos[i].Kind != "out-of-tree-execute-process" {
			t.Errorf("kind: %q", rep.Todos[i].Kind)
		}
	}
	if sub == nil {
		t.Fatalf("missing build-subproject group: %+v", rep.Todos)
	}
	if len(sub.Anchors) != 2 {
		t.Errorf("subproject group should fold 2 anchors (dup dropped); got %+v", sub.Anchors)
	}
	for _, a := range sub.Anchors {
		if strings.Contains(a.Construct, "/b/") {
			t.Errorf("build dir leaked into construct: %q", a.Construct)
		}
	}
}

// Nil collector / empty notes are no-ops; the id is line-independent so it
// stays stable across unrelated edits.
func TestEmitOutOfTreeExecuteProcessTodos_NoOpsAndIDStability(t *testing.T) {
	emitOutOfTreeExecuteProcessTodos(nil, []outOfTreeExecNote{{File: "f", Line: 1, Argv: []string{"a"}, Signal: signalPrefixTree}}, "", "")
	c := todos.New()
	emitOutOfTreeExecuteProcessTodos(c, nil, "", "")
	if c.Len() != 0 {
		t.Errorf("empty notes should add nothing")
	}
	id := func(line int) string {
		cc := todos.New()
		emitOutOfTreeExecuteProcessTodos(cc, []outOfTreeExecNote{
			{File: "/synth/FooConfig.cmake", Line: line, Argv: []string{"foo", "--v"}, Signal: signalPrefixTree},
		}, "/src", "/b")
		rep := cc.Report(todos.Preamble{}, "")
		if len(rep.Todos) != 1 {
			t.Fatalf("want 1 todo, got %d", len(rep.Todos))
		}
		return rep.Todos[0].ID
	}
	if id(3) != id(99) {
		t.Errorf("todo id must be line-independent")
	}
}
