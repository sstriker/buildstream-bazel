package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// findTodo returns the first todo with the given kind, or nil.
func findTodo(rep todos.Report, kind string) *todos.Todo {
	for i := range rep.Todos {
		if rep.Todos[i].Kind == kind {
			return &rep.Todos[i]
		}
	}
	return nil
}

func TestEmitRejectionTodos_DispositionByCode(t *testing.T) {
	rej := rejection.New()
	// unsupported-execute-process is deliberately SKIPPED here — its
	// structured per-call mirror is emitExecuteProcessRefusalTodos.
	rej.AddWithContext(failure.UnsupportedExecuteProcess, "execute_process refused", "tgtA", "CMakeLists.txt")
	rej.AddWithContext(failure.FileAPIMalformed, "target id mismatch", "", "")

	c := todos.New()
	emitRejectionTodos(c, rej, "")
	rep := c.Report(todos.Preamble{}, "")

	if ep := findTodo(rep, "rejection:unsupported-execute-process"); ep != nil {
		t.Errorf("coarse execute_process mirror should be skipped (superseded by execute-process-refusal); got %+v", ep)
	}
	fa := findTodo(rep, "rejection:fileapi-malformed")
	if fa == nil || fa.Disposition != todos.Informational {
		t.Errorf("fileapi-malformed should be informational; got %+v", fa)
	}
}

func TestEmitRejectionTodos_NilCollectorOrEmpty(t *testing.T) {
	emitRejectionTodos(nil, rejection.New(), "") // nil collector: no panic
	c := todos.New()
	emitRejectionTodos(c, rejection.New(), "") // empty rejections: no todos
	if c.Len() != 0 {
		t.Errorf("empty rejections should yield no todos, got %d", c.Len())
	}
}

func TestEmitBakeTodos_DefaultImprovementAndOverride(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "probe_gen", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-execute-process"}},
		{Name: "stamp_gen", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-execute-process"}},
	}}
	overrides := map[string]todos.Disposition{"stamp_gen": todos.Actionable}

	c := todos.New()
	emitBakeTodos(c, pkg, overrides)
	rep := c.Report(todos.Preamble{}, "")

	var probe, stamp *todos.Todo
	for i := range rep.Todos {
		switch rep.Todos[i].GroupKey {
		case "probe_gen":
			probe = &rep.Todos[i]
		case "stamp_gen":
			stamp = &rep.Todos[i]
		}
	}
	if probe == nil || probe.Disposition != todos.Improvement {
		t.Errorf("baked probe should default to improvement; got %+v", probe)
	}
	if stamp == nil || stamp.Disposition != todos.Actionable {
		t.Errorf("overridden baked stamp should be actionable; got %+v", stamp)
	}
	for _, tt := range []*todos.Todo{probe, stamp} {
		if tt.Kind != "bake" {
			t.Errorf("bake todo kind = %q, want bake", tt.Kind)
		}
	}
}

func TestEmitBakeTodos_NoBakes(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "plain", Kind: ir.KindCCLibrary}}}
	c := todos.New()
	emitBakeTodos(c, pkg, nil)
	if c.Len() != 0 {
		t.Errorf("no baked targets should yield no bake todos, got %d", c.Len())
	}
}

func TestEmitUnresolvedGenexTodos(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-cmd-genex-unresolved"}},
		{Name: "b", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-cmd-genex-unresolved"}},
		{Name: "c", Kind: ir.KindCCLibrary}, // untagged: no todo
	}}
	c := todos.New()
	emitUnresolvedGenexTodos(c, pkg)
	rep := c.Report(todos.Preamble{}, "")

	g := findTodo(rep, "genex-unresolved")
	if g == nil {
		t.Fatal("missing genex-unresolved todo")
	}
	if g.Disposition != todos.Improvement {
		t.Errorf("genex-unresolved disposition = %q, want improvement", g.Disposition)
	}
	if g.GroupKey != "cmake-codegen-cmd-genex-unresolved" || len(g.Anchors) != 2 {
		t.Errorf("genex-unresolved should group 2 targets under the tag; got %+v", g)
	}
}

func TestDispositionForCode_UnknownDefaultsActionable(t *testing.T) {
	if got := dispositionForCode(failure.Code("totally-made-up")); got != todos.Actionable {
		t.Errorf("unknown code disposition = %q, want actionable", got)
	}
}
