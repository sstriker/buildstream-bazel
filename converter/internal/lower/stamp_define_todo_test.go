package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestEmitStampInDefineTodos(t *testing.T) {
	stampVars := map[string]string{
		"GIT_SHA": "STABLE_GIT_SHA",
		"SHORT":   "STABLE_SHORT", // value too short to match (guards false positives)
	}
	cmakeVars := map[string]string{
		"GIT_SHA": "abc1234def5678", // distinctive (>= 8)
		"SHORT":   "1.0",            // < 8, must never match
	}
	pkg := &ir.Package{Targets: []ir.Target{
		// Stamp baked into a -D define (quoted) -> flagged.
		{Name: "app", Kind: ir.KindCCBinary, Defines: []string{`GIT_SHA="abc1234def5678"`, "NDEBUG=1"}},
		// Same value via local_defines -> flagged.
		{Name: "lib", Kind: ir.KindCCLibrary, LocalDefines: []string{"REV=abc1234def5678"}},
		// A short stamp value must not match even if equal.
		{Name: "ver", Kind: ir.KindCCLibrary, Defines: []string{"VERSION=1.0"}},
		// No stamp -> no todo.
		{Name: "plain", Kind: ir.KindCCLibrary, Defines: []string{"FOO=bar"}},
	}}

	c := todos.New()
	emitStampInDefineTodos(c, pkg, stampVars, cmakeVars)

	rep := c.Report(todos.Preamble{}, "")
	flagged := map[string]todos.Todo{}
	for _, td := range rep.Todos {
		if td.Kind == "stamp-baked-define" {
			flagged[td.GroupKey] = td
		}
	}
	if len(flagged) != 2 {
		t.Fatalf("expected 2 stamp-baked-define todos (app, lib), got %d: %v", len(flagged), flagged)
	}
	app, ok := flagged["app"]
	if !ok {
		t.Fatalf("app not flagged: %v", flagged)
	}
	if app.Disposition != todos.Actionable {
		t.Errorf("disposition = %q, want actionable", app.Disposition)
	}
	if len(app.Anchors) != 1 || !strings.Contains(app.Anchors[0].Construct, "GIT_SHA=STABLE_GIT_SHA") {
		t.Errorf("app anchor should name the define -> status key: %+v", app.Anchors)
	}
	if _, ok := flagged["ver"]; ok {
		t.Error("a too-short stamp value must not be flagged (false-positive guard)")
	}
	if _, ok := flagged["plain"]; ok {
		t.Error("a non-stamp define must not be flagged")
	}
}

func TestEmitStampInDefineTodos_NoStampsNoOp(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "x", Defines: []string{"GIT_SHA=abc1234def5678"}}}}
	c := todos.New()
	emitStampInDefineTodos(c, pkg, nil, map[string]string{"GIT_SHA": "abc1234def5678"})
	if got := len(c.Report(todos.Preamble{}, "").Todos); got != 0 {
		t.Errorf("no stamp vars -> no todos, got %d", got)
	}
	emitStampInDefineTodos(nil, pkg, nil, nil) // nil collector must not panic
}

func TestSplitDefine(t *testing.T) {
	cases := []struct {
		in          string
		name, value string
		ok          bool
	}{
		{`GIT_SHA="abc"`, "GIT_SHA", "abc", true},
		{"REV=abc", "REV", "abc", true},
		{"X='y'", "X", "y", true},
		{"NDEBUG", "", "", false}, // value-less
		{"EMPTY=", "", "", false}, // empty value
		{`Q=""`, "", "", false},   // empty quoted value
	}
	for _, tc := range cases {
		name, value, ok := splitDefine(tc.in)
		if ok != tc.ok || (ok && (name != tc.name || value != tc.value)) {
			t.Errorf("splitDefine(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, name, value, ok, tc.name, tc.value, tc.ok)
		}
	}
}
