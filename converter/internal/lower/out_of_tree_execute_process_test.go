package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
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
	lift, note := partitionOutOfTreeExec(calls, build, prefix, consumed, nil)
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

// The codemodel signal takes precedence over the issuing site: a call ISSUED
// from the prefix tree but whose WORKING_DIRECTORY operates on a build-dir
// location the codemodel lists sources under is a subproject to LIFT, not a
// prefix-tree probe to note. A sibling prefix-tree call with no build-dir
// working dir stays a noted prefix probe.
func TestPartitionOutOfTreeExec_WorkingDirCodemodelPrecedence(t *testing.T) {
	build := "/build"
	prefix := "/synth"
	// Single-LEVEL sub-build dir: sources sit directly under it. This pins the
	// WORKING_DIRECTORY anchoring at the dir itself — a slashDir() off-by-one
	// would strip "subbuild" to "" and miss the lift.
	consumed := map[string]bool{"subbuild/foo.c": true}
	driveSubBuild := ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 1, "cmake", "--build", ".")
	driveSubBuild.WorkingDirectory = "/build/subbuild" // operates on build dir w/ codemodel srcs
	pureProbe := ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 2, "foo-config", "--cflags")
	pureProbe.WorkingDirectory = "/synth/lib/cmake/Foo" // stays in the prefix tree
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{driveSubBuild, pureProbe}, build, prefix, consumed, nil)
	if len(lift) != 1 || lift[0].Line != 1 {
		t.Fatalf("lift: want the prefix-issued sub-build (line 1); got %+v", lift)
	}
	if len(note) != 1 || note[0].Signal != signalPrefixTree || note[0].Line != 2 {
		t.Fatalf("note: want the pure prefix probe (line 2, prefix-tree); got %+v", note)
	}
}

// A WORKING_DIRECTORY one level deep whose OWN subtree has no codemodel
// sources is NOT a subproject, even when a SIBLING dir under the shared parent
// does — guards against the slashDir-parent-widening the previous shape hid.
func TestPartitionOutOfTreeExec_WorkingDirSiblingNotOverMatched(t *testing.T) {
	consumed := map[string]bool{"_deps/other-src/x.c": true} // sibling, not under sub-build
	c := ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 1, "cmake", "--build", ".")
	c.WorkingDirectory = "/build/_deps/sub-build" // its own subtree has no consumed sources
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{c}, "/build", "/synth", consumed, nil)
	if len(lift) != 0 {
		t.Fatalf("sibling sources under the shared parent must NOT lift; got %+v", lift)
	}
	if len(note) != 1 || note[0].Signal != signalPrefixTree {
		t.Fatalf("want a prefix-tree note; got %+v", note)
	}
}

// A relative WORKING_DIRECTORY must NOT be mistaken for a build-relative path
// (which would false-positive the codemodel check): the prefix-issued call
// with a relative WD and no other build-dir signal stays a prefix probe.
func TestPartitionOutOfTreeExec_RelativeWorkingDirNotBuildRel(t *testing.T) {
	c := ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 1, "foo", "--x")
	c.WorkingDirectory = "_deps/sub-build" // relative — not an absolute build-dir path
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{c},
		"/build", "/synth", map[string]bool{"_deps/sub-build/foo.c": true}, nil)
	if len(lift) != 0 || len(note) != 1 || note[0].Signal != signalPrefixTree {
		t.Errorf("relative WD must not trigger the codemodel lift; got lift=%+v note=%+v", lift, note)
	}
}

// CMakeFiles anywhere in the build-relative path is confident noise, even
// nested under a subproject dir (cmake's per-subproject scratch) — neither
// lifted nor noted.
func TestPartitionOutOfTreeExec_CMakeFilesSegment(t *testing.T) {
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{
		ootCall("/build/_deps/sub-build/CMakeFiles/foo.dir/probe.cmake", 1, "cc", "-x"),
	}, "/build", "", map[string]bool{"_deps/sub-build/foo.c": true}, nil)
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
	}, "", "", nil, nil)
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
	emitOutOfTreeExecuteProcessTodos(c, notes, "/src", "/b", "/synth")
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
	emitOutOfTreeExecuteProcessTodos(nil, []outOfTreeExecNote{{File: "f", Line: 1, Argv: []string{"a"}, Signal: signalPrefixTree}}, "", "", "")
	c := todos.New()
	emitOutOfTreeExecuteProcessTodos(c, nil, "", "", "")
	if c.Len() != 0 {
		t.Errorf("empty notes should add nothing")
	}
	id := func(line int) string {
		cc := todos.New()
		emitOutOfTreeExecuteProcessTodos(cc, []outOfTreeExecNote{
			{File: "/synth/FooConfig.cmake", Line: line, Argv: []string{"foo", "--v"}, Signal: signalPrefixTree},
		}, "/src", "/b", "/synth")
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

// Prefix-tree anchors and argv must rewrite to <PREFIX>/… so the per-run
// staged find_package prefix never leaks into the report (the byte-identity
// contract normalizeReportPath alone doesn't cover for prefix paths).
func TestEmitOutOfTreeExecuteProcessTodos_PrefixNormalized(t *testing.T) {
	c := todos.New()
	emitOutOfTreeExecuteProcessTodos(c, []outOfTreeExecNote{
		{File: "/synth/lib/cmake/Foo/FooConfig.cmake", Line: 5, Argv: []string{"/synth/bin/foo-config", "--cflags"}, Signal: signalPrefixTree},
	}, "/src", "/b", "/synth")
	rep := c.Report(todos.Preamble{}, "")
	if len(rep.Todos) != 1 || len(rep.Todos[0].Anchors) != 1 {
		t.Fatalf("want 1 todo with 1 anchor; got %+v", rep.Todos)
	}
	a := rep.Todos[0].Anchors[0]
	if strings.Contains(a.File, "/synth") || strings.Contains(a.Construct, "/synth") {
		t.Errorf("prefix leaked: file=%q construct=%q", a.File, a.Construct)
	}
	if a.File != "<PREFIX>/lib/cmake/Foo/FooConfig.cmake" {
		t.Errorf("anchor file = %q, want <PREFIX>/lib/cmake/Foo/FooConfig.cmake", a.File)
	}
	if !strings.Contains(a.Construct, "<PREFIX>/bin/foo-config") {
		t.Errorf("argv not prefix-normalized: %q", a.Construct)
	}
}

// TestPartitionOutOfTreeExec_RecognizerSignal: the recognizer's tool signal is
// location-independent. A build-dir codegen call the codemodel didn't attribute
// (signalBuildDirOther) that a recognizer CLAIMS (protoc --cpp_out) is the
// project's own codegen → routed to the LIFT (so the native rule is recovered),
// not dumped to a vague note. A prefix-tree recognized call stays a NOTE (the
// dependency emits it) but is marked Recognized for a sharper todo. With the
// recognizer OFF, behavior is unchanged (note, not lift).
func TestPartitionOutOfTreeExec_RecognizerSignal(t *testing.T) {
	on := newCodegenContext()
	on.RecognizeCodegen = true

	// build-dir protoc, no codemodel sources beneath → signalBuildDirOther.
	bdo := ootCall("/build/gen/CMakeLists.txt", 1, "protoc", "--cpp_out=.", "foo.proto")
	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{bdo}, "/build", "/synth", nil, on)
	if len(lift) != 1 || len(note) != 0 {
		t.Fatalf("recognized build-dir codegen should LIFT; got lift=%d note=%d", len(lift), len(note))
	}

	// Recognizer OFF: the same call stays a note (unchanged behavior).
	lift, note = partitionOutOfTreeExec([]shadow.ExecuteProcessCall{bdo}, "/build", "/synth", nil, nil)
	if len(lift) != 0 || len(note) != 1 || note[0].Recognized {
		t.Fatalf("recognizer off must keep the note (not lift, not recognized); got lift=%d note=%+v", len(lift), note)
	}

	// prefix-tree protoc → stays a NOTE but Recognized=true (dependency's codegen).
	pt := ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 1, "protoc", "--cpp_out=.", "x.proto")
	lift, note = partitionOutOfTreeExec([]shadow.ExecuteProcessCall{pt}, "/build", "/synth", nil, on)
	if len(lift) != 0 || len(note) != 1 {
		t.Fatalf("prefix-tree codegen should stay a NOTE (dependency's); got lift=%d note=%d", len(lift), len(note))
	}
	if note[0].Signal != signalPrefixTree || !note[0].Recognized {
		t.Errorf("prefix-tree note should be Recognized=true; got %+v", note[0])
	}

	// A non-codegen build-dir call (no recognizer match) stays a note.
	nc := ootCall("/build/gen/CMakeLists.txt", 1, "python3", "gen.py")
	lift, note = partitionOutOfTreeExec([]shadow.ExecuteProcessCall{nc}, "/build", "/synth", nil, on)
	if len(lift) != 0 || len(note) != 1 || note[0].Recognized {
		t.Errorf("unrecognized build-dir call should stay an un-recognized note; got lift=%d note=%+v", len(lift), note)
	}
}

// TestPartitionOutOfTreeExec_FidelityLift: best-effort lifts a build-dir codegen
// call even when NO recognizer catches it (genrule fallback); strict only lifts
// when recognized (else surface). prefix-tree (a dependency's) is never
// genrule-lifted regardless of fidelity.
func TestPartitionOutOfTreeExec_FidelityLift(t *testing.T) {
	be := newCodegenContext()
	be.Fidelity = string(convmode.FidelityBestEffort)
	st := newCodegenContext() // Fidelity "" == strict

	// Unrecognized build-dir codegen call.
	nc := ootCall("/build/gen/CMakeLists.txt", 1, "python3", "gen.py")

	lift, note := partitionOutOfTreeExec([]shadow.ExecuteProcessCall{nc}, "/build", "/synth", nil, be)
	if len(lift) != 1 || len(note) != 0 {
		t.Fatalf("best-effort should LIFT an unrecognized build-dir call (genrule fallback); got lift=%d note=%d", len(lift), len(note))
	}

	lift, note = partitionOutOfTreeExec([]shadow.ExecuteProcessCall{nc}, "/build", "/synth", nil, st)
	if len(lift) != 0 || len(note) != 1 {
		t.Fatalf("strict should NOT genrule-lift an unrecognized build-dir call; got lift=%d note=%d", len(lift), len(note))
	}

	// prefix-tree (dependency's) is never genrule-lifted, even under best-effort.
	pt := ootCall("/synth/lib/cmake/Foo/FooConfig.cmake", 1, "python3", "gen.py")
	lift, note = partitionOutOfTreeExec([]shadow.ExecuteProcessCall{pt}, "/build", "/synth", nil, be)
	if len(lift) != 0 || len(note) != 1 || note[0].Signal != signalPrefixTree {
		t.Fatalf("prefix-tree must stay a note even under best-effort (dependency's codegen); got lift=%d note=%+v", len(lift), note)
	}
}
