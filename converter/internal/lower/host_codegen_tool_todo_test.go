package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestClassifyHostCodegenTool(t *testing.T) {
	cases := []struct {
		name        string
		cmd         string
		wantDriver  string
		wantAbs, ok bool
	}{
		{"basename tool", "gen.sh greeting.in $(RULEDIR)/greeting.c", "gen.sh", false, true},
		{"interpreter", "python3 scripts/gen.py -o $(RULEDIR)/out.c", "python3", false, true},
		{"absolute host path", "/opt/host/bin/protoc --cpp_out=. foo.proto", "protoc", true, true},
		{"cd-prefixed", "cd sub && flatc --cpp x.fbs", "flatc", false, true},
		{"hermeticized execpath", "$(execpath //:gen_tool) in out", "", false, false},
		{"hermeticized location", "$(location :tool) in out", "", false, false},
		{"benign cmake -E", "cmake -E copy a b", "", false, false},
		{"benign cp", "cp a b", "", false, false},
		{"shell assignment preamble", `tmp=$(mktemp -d) && gen.sh x`, "", false, false},
		{"subshell preamble", `( cd d && gen.sh x )`, "", false, false},
		{"empty", "", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, abs, ok := classifyHostCodegenTool(c.cmd)
			if d != c.wantDriver || abs != c.wantAbs || ok != c.ok {
				t.Errorf("classifyHostCodegenTool(%q) = (%q,%v,%v), want (%q,%v,%v)",
					c.cmd, d, abs, ok, c.wantDriver, c.wantAbs, c.ok)
			}
		})
	}
}

// TestNoteAndEmitHostCodegenTools: notes recorded through the chokepoint fold
// per-driver into one todo with N anchors; an absolute-path driver is
// Actionable, a basename driver Improvement; the suggested shape carries the
// match key.
func TestNoteAndEmitHostCodegenTools(t *testing.T) {
	cc := newCodegenContext()
	// Two genrules driven by the same PATH tool (fold to one todo, two anchors)
	// + one absolute-path tool (separate todo, Actionable).
	noteHostCodegenTool(cc, ir.Target{Name: "gen_a", GenruleCmd: "flatc --cpp a.fbs"})
	noteHostCodegenTool(cc, ir.Target{Name: "gen_b", GenruleCmd: "flatc --cpp b.fbs"})
	noteHostCodegenTool(cc, ir.Target{Name: "gen_c", GenruleCmd: "/opt/host/bin/gen --out c.c"})
	// A hermeticized + a benign one record nothing.
	noteHostCodegenTool(cc, ir.Target{Name: "gen_d", GenruleCmd: "$(execpath //:t) x"})
	noteHostCodegenTool(cc, ir.Target{Name: "gen_e", GenruleCmd: "cmake -E copy a b"})

	col := todos.New()
	emitHostCodegenToolTodos(col, cc.HostCodegenTools)
	rep := col.Report(todos.Preamble{}, "")
	var ht []todos.Todo
	for _, td := range rep.Todos {
		if td.Kind == "host-codegen-tool" {
			ht = append(ht, td)
		}
	}
	if len(ht) != 2 {
		t.Fatalf("want 2 host-codegen-tool todos (flatc + gen), got %d: %+v", len(ht), ht)
	}
	// Sorted by group_key: "/opt/host/bin/gen"? No — group_key is the DRIVER
	// basename ("gen" vs "flatc"); "flatc" < "gen".
	flatc, gen := ht[0], ht[1]
	if flatc.GroupKey != "flatc" || gen.GroupKey != "gen" {
		t.Fatalf("group keys = %q,%q want flatc,gen", flatc.GroupKey, gen.GroupKey)
	}
	if flatc.Disposition != todos.Improvement {
		t.Errorf("flatc (PATH basename) should be Improvement, got %q", flatc.Disposition)
	}
	if len(flatc.Anchors) != 2 {
		t.Errorf("flatc should fold 2 genrules into 2 anchors, got %d", len(flatc.Anchors))
	}
	if gen.Disposition != todos.Actionable {
		t.Errorf("gen (absolute host path) should be Actionable, got %q", gen.Disposition)
	}
	if gen.Evidence["match"] != "/opt/host/bin/gen" {
		t.Errorf("absolute driver match should be the verbatim path, got %v", gen.Evidence["match"])
	}
	if !strings.Contains(gen.SuggestedShape, `"match": "/opt/host/bin/gen"`) {
		t.Errorf("suggested shape should carry the absolute match key:\n%s", gen.SuggestedShape)
	}
}

// Nil collector / empty notes are no-ops.
func TestEmitHostCodegenToolTodos_Empty(t *testing.T) {
	emitHostCodegenToolTodos(nil, []hostCodegenToolNote{{Driver: "x"}}) // nil collector: no panic
	col := todos.New()
	emitHostCodegenToolTodos(col, nil)
	if col.Len() != 0 {
		t.Errorf("empty notes should emit nothing, got %d", col.Len())
	}
}
