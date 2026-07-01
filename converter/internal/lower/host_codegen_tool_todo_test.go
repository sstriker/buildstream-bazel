package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestExecProcHostToolNote_SeamWiring proves the execute_process producer
// family routes its emissions through appendExecProcGenrule: an
// un-hermeticized host tool driving an OUTPUT_FILE lift now surfaces a
// host-codegen-tool note — the diagnostic parity the custom-command family
// already gets through recognizeOrGenrule — while a benign cmake -E copy
// records nothing. Before the seam these genrules appended straight to
// cc.Genrules, so the host driver went undiagnosed.
func TestExecProcHostToolNote_SeamWiring(t *testing.T) {
	t.Run("host-tool-output-file-notes", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 3,
			Commands:   [][]string{{"mygen", "--emit"}},
			OutputFile: "/build/gen.h",
		}
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
		if len(refusals) != 0 {
			t.Fatalf("expected lift: %+v", refusals)
		}
		if len(cc.HostCodegenTools) != 1 || cc.HostCodegenTools[0].Driver != "mygen" {
			t.Fatalf("execute_process host tool must surface a host-codegen-tool note, got %+v", cc.HostCodegenTools)
		}
	})

	t.Run("benign-copy-does-not-note", func(t *testing.T) {
		// A cmake -E copy lifts to a `cp` genrule (benign) — the seam fires the
		// note but classifyHostCodegenTool filters the benign driver, so nothing
		// records.
		call := shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 5,
			Commands: [][]string{{"cmake", "-E", "copy", "/src/a.txt", "/build/b.txt"}},
		}
		cc := newCodegenContext()
		recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, nil, nil, cc)
		if len(cc.HostCodegenTools) != 0 {
			t.Fatalf("benign cmake -E copy must not surface a host-codegen-tool note, got %+v", cc.HostCodegenTools)
		}
	})
}

func TestClassifyHostCodegenTool(t *testing.T) {
	cases := []struct {
		name        string
		cmd         string
		wantDriver  string
		wantAbs, ok bool
	}{
		{"basename tool", "gen.sh greeting.in $(RULEDIR)/greeting.c", "gen.sh", false, true},
		{"interpreter relative script", "python3 scripts/gen.py -o $(RULEDIR)/out.c", "python3", false, true},
		// An ABSOLUTE interpreter script is the real tool (a prefix-resident
		// codegen.py needs the imports.json entry, not python3) → classify the
		// script, Actionable.
		{"interpreter absolute script", "python3 /opt/prefix/share/codegen.py -o $(RULEDIR)/out.c", "codegen.py", true, true},
		{"interpreter flag then absolute script", "python3 -B /opt/prefix/share/gen.py", "gen.py", true, true},
		{"perl absolute script", "perl /opt/prefix/bin/xxd.pl in out", "xxd.pl", true, true},
		// Inline code has no script file → stays interpreter-flagged.
		{"interpreter inline code", `python3 -c import x`, "python3", false, true},
		{"absolute host path", "/opt/host/bin/protoc --cpp_out=. foo.proto", "protoc", true, true},
		{"cd-prefixed", "cd sub && flatc --cpp x.fbs", "flatc", false, true},
		// The execute_process file-producing lift prepends a `mkdir -p …` prologue
		// before the driver — peel it so the tool, not the scaffolding, classifies.
		{"mkdir-prologue", `mkdir -p "$$(dirname "$@")" && gen.sh in > "$@"`, "gen.sh", false, true},
		{"multi-mkdir-prologue", `mkdir -p "$(RULEDIR)/a" && mkdir -p "$(RULEDIR)/b" && flatc x.fbs`, "flatc", false, true},
		{"mkdir-then-cd-prologue", `mkdir -p w && cd w && perl /opt/prefix/bin/gen.pl`, "gen.pl", true, true},
		// A cmake -E / cp behind the prologue stays benign (no note).
		{"mkdir-prologue-benign", `mkdir -p "$$(dirname "$@")" && cp "$(location a)" "$@"`, "", false, false},
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
			_, d, abs, ok := classifyHostCodegenTool(c.cmd)
			if d != c.wantDriver || abs != c.wantAbs || ok != c.ok {
				t.Errorf("classifyHostCodegenTool(%q) = (%q,%v,%v), want (%q,%v,%v)",
					c.cmd, d, abs, ok, c.wantDriver, c.wantAbs, c.ok)
			}
		})
	}
}

// TestNoteHostCodegenTool_PrefixAnchoring: an absolute driver under the
// (per-run-ephemeral) synth-prefix is recorded as a `prefix`-origin tool with
// its path ANCHORED to /opt/prefix/… — never the ephemeral path — so the report
// stays byte-identical across converts and the suggestion stays portable.
func TestNoteHostCodegenTool_PrefixAnchoring(t *testing.T) {
	cc := newCodegenContext()
	cc.HostPrefixDir = "/tmp/convert-run-7f3a/synth-prefix"
	noteHostCodegenTool(cc, ir.Target{
		Name:       "gen_x",
		GenruleCmd: "/tmp/convert-run-7f3a/synth-prefix/bin/foogen --out $(RULEDIR)/x.c",
	})
	if len(cc.HostCodegenTools) != 1 {
		t.Fatalf("want 1 note, got %d", len(cc.HostCodegenTools))
	}
	n := cc.HostCodegenTools[0]
	if !n.Prefix || !n.Absolute || n.Driver != "foogen" {
		t.Fatalf("note = %+v, want prefix+absolute foogen", n)
	}
	if n.Path != "/opt/prefix/bin/foogen" {
		t.Errorf("path = %q, want anchored /opt/prefix/bin/foogen (no ephemeral synth-prefix)", n.Path)
	}
}

// TestNoteHostCodegenTool_InterpreterScript: an interpreted PREFIX tool
// (`python3 <prefix>/codegen.py`) records the SCRIPT — not the interpreter — as
// the host codegen tool needing the imports.json entry, prefix-anchored and
// Actionable. Without the interpreter peel the note would land on python3 (a
// PATH-resolved Improvement), hiding the script that actually needs mapping.
func TestNoteHostCodegenTool_InterpreterScript(t *testing.T) {
	cc := newCodegenContext()
	cc.HostPrefixDir = "/tmp/convert-run-7f3a/synth-prefix"
	noteHostCodegenTool(cc, ir.Target{
		Name:       "gen_x",
		GenruleCmd: "python3 /tmp/convert-run-7f3a/synth-prefix/share/codegen.py --out $(RULEDIR)/x.c",
	})
	if len(cc.HostCodegenTools) != 1 {
		t.Fatalf("want 1 note, got %d", len(cc.HostCodegenTools))
	}
	n := cc.HostCodegenTools[0]
	if !n.Prefix || !n.Absolute || n.Driver != "codegen.py" {
		t.Fatalf("note = %+v, want prefix+absolute codegen.py (the SCRIPT, not python3)", n)
	}
	if n.Path != "/opt/prefix/share/codegen.py" {
		t.Errorf("path = %q, want anchored /opt/prefix/share/codegen.py", n.Path)
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
	// The suggested match is the deterministic BASENAME; the absolute path is
	// informational evidence; origin is host (not prefix).
	if gen.Evidence["match"] != "gen" || gen.Evidence["origin"] != "host" {
		t.Errorf("gen evidence match/origin = %v/%v, want gen/host", gen.Evidence["match"], gen.Evidence["origin"])
	}
	if gen.Evidence["path"] != "/opt/host/bin/gen" {
		t.Errorf("absolute driver path evidence = %v, want /opt/host/bin/gen", gen.Evidence["path"])
	}
	if !strings.Contains(gen.SuggestedShape, `"match": "gen"`) {
		t.Errorf("suggested shape should key on the basename match:\n%s", gen.SuggestedShape)
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

// TestHostCodegenTool_ConventionSuggestion: a known tool (protoc) gets the
// REAL canonical label + bazel_dep in its suggested shape and evidence; an
// unknown tool keeps the placeholder. And ToolConventionTools exposes the
// registry as manifest tool mappings.
func TestHostCodegenTool_ConventionSuggestion(t *testing.T) {
	cc := newCodegenContext()
	noteHostCodegenTool(cc, ir.Target{Name: "gen_pb", GenruleCmd: "protoc --cpp_out=. api.proto"})
	noteHostCodegenTool(cc, ir.Target{Name: "gen_z", GenruleCmd: "zzunknown in out"})
	col := todos.New()
	emitHostCodegenToolTodos(col, cc.HostCodegenTools)
	rep := col.Report(todos.Preamble{}, "")
	var protoc, unknown *todos.Todo
	for i := range rep.Todos {
		switch rep.Todos[i].GroupKey {
		case "protoc":
			protoc = &rep.Todos[i]
		case "zzunknown":
			unknown = &rep.Todos[i]
		}
	}
	if protoc == nil || unknown == nil {
		t.Fatalf("missing todos: %+v", rep.Todos)
	}
	if protoc.Evidence["convention_label"] != "@protobuf//:protoc" || protoc.Evidence["convention_module"] != "protobuf" {
		t.Errorf("protoc convention evidence = %v", protoc.Evidence)
	}
	if !strings.Contains(protoc.SuggestedShape, `"label": "@protobuf//:protoc"`) ||
		!strings.Contains(protoc.SuggestedShape, `bazel_dep(name = "protobuf"`) {
		t.Errorf("protoc suggested shape should carry the real label + bazel_dep:\n%s", protoc.SuggestedShape)
	}
	if _, ok := unknown.Evidence["convention_label"]; ok {
		t.Errorf("unknown tool must not carry a convention label: %v", unknown.Evidence)
	}
	if !strings.Contains(unknown.SuggestedShape, "//path/to:") {
		t.Errorf("unknown tool should keep the placeholder label:\n%s", unknown.SuggestedShape)
	}
}

func TestToolConventionTools(t *testing.T) {
	want := map[string]string{
		"protoc":          "@protobuf//:protoc",
		"flatc":           "@flatbuffers//:flatc",
		"grpc_cpp_plugin": "@grpc//src/compiler:grpc_cpp_plugin",
	}
	got := map[string]string{}
	for _, tl := range ToolConventionTools() {
		got[tl.Match] = tl.Label
	}
	for m, label := range want {
		if got[m] != label {
			t.Errorf("convention[%q] = %q, want %q", m, got[m], label)
		}
		conv, ok := toolConventionFor(m)
		if !ok || conv.Label != label || conv.Module == "" {
			t.Errorf("toolConventionFor(%q) = %+v, ok=%v; want label %q + non-empty module", m, conv, ok, label)
		}
	}
}
