package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

func epRefusal(file string, line int, bucket Bucket, reason, argv string) executeProcessRefusal {
	return executeProcessRefusal{File: file, Line: line, Bucket: bucket, Reason: reason, Argv: argv}
}

// Grouping: one todo per (file, bucket); same group folds anchors; distinct
// buckets in one file split.
func TestEmitExecuteProcessRefusalTodos_Grouping(t *testing.T) {
	c := todos.New()
	refusals := []executeProcessRefusal{
		epRefusal("/src/CMakeLists.txt", 3, BucketRefuse, "opaque driver", "python gen.py"),
		epRefusal("/src/CMakeLists.txt", 9, BucketRefuse, "opaque driver", "python gen2.py"),
		epRefusal("/src/CMakeLists.txt", 12, BucketProbe, "host probe", "uname -m"),
		epRefusal("/src/sub/CMakeLists.txt", 1, BucketRefuse, "opaque driver", "perl x.pl"),
	}
	emitExecuteProcessRefusalTodos(c, refusals, "/src", "/b")
	rep := c.Report(todos.Preamble{}, "")
	if len(rep.Todos) != 3 {
		t.Fatalf("want 3 todos (2 file×bucket groups in root + 1 in sub); got %d: %+v", len(rep.Todos), rep.Todos)
	}
	var rootRefuse *todos.Todo
	for i := range rep.Todos {
		if rep.Todos[i].GroupKey == "<SRC>/CMakeLists.txt|"+string(BucketRefuse) {
			rootRefuse = &rep.Todos[i]
		}
	}
	if rootRefuse == nil {
		t.Fatalf("missing root refuse group: %+v", rep.Todos)
	}
	if len(rootRefuse.Anchors) != 2 {
		t.Errorf("root refuse group should fold 2 anchors; got %+v", rootRefuse.Anchors)
	}
	if rootRefuse.Kind != "execute-process-refusal" || rootRefuse.Disposition != todos.Actionable {
		t.Errorf("kind/disposition: %+v", rootRefuse)
	}
}

// Duplicate (file, line-irrelevant, argv) calls dedupe to one anchor; paths
// normalize so the per-run build dir never leaks.
func TestEmitExecuteProcessRefusalTodos_DedupeAndNormalize(t *testing.T) {
	c := todos.New()
	refusals := []executeProcessRefusal{
		epRefusal("/src/CMakeLists.txt", 3, BucketRefuse, "wrote under /b/out", "tool /b/out/gen.h"),
		epRefusal("/src/CMakeLists.txt", 3, BucketRefuse, "wrote under /b/out", "tool /b/out/gen.h"),
	}
	emitExecuteProcessRefusalTodos(c, refusals, "/src", "/b")
	rep := c.Report(todos.Preamble{}, "")
	if len(rep.Todos) != 1 || len(rep.Todos[0].Anchors) != 1 {
		t.Fatalf("duplicate calls must fold to one anchor; got %+v", rep.Todos)
	}
	for _, a := range rep.Todos[0].Anchors {
		if strings.Contains(a.Construct, "/b/") {
			t.Errorf("build dir leaked into construct: %q", a.Construct)
		}
	}
	ev, _ := rep.Todos[0].Evidence["reasons"].([]string)
	for _, r := range ev {
		if strings.Contains(r, "/b/") {
			t.Errorf("build dir leaked into reasons: %q", r)
		}
	}
}

// Nil collector / empty refusals are no-ops; id stable when only Line moves.
func TestEmitExecuteProcessRefusalTodos_NoOpsAndIDStability(t *testing.T) {
	emitExecuteProcessRefusalTodos(nil, []executeProcessRefusal{epRefusal("f", 1, BucketRefuse, "r", "a")}, "", "")
	c := todos.New()
	emitExecuteProcessRefusalTodos(c, nil, "", "")
	if c.Len() != 0 {
		t.Errorf("empty refusals should add nothing")
	}
	id := func(line int) string {
		cc := todos.New()
		emitExecuteProcessRefusalTodos(cc, []executeProcessRefusal{epRefusal("/src/CMakeLists.txt", line, BucketRefuse, "r", "tool x")}, "/src", "/b")
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
