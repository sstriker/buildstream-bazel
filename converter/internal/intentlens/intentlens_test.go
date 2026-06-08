package intentlens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

func sampleTodos() todos.Report {
	c := todos.New()
	c.Add(todos.Todo{
		Kind: "cmake-p-test", Disposition: todos.Actionable, GroupKey: "tests/run.cmake",
		Anchors: []todos.Anchor{{File: "tests/run.cmake", Line: 3, Construct: "add_test(...)"}},
	})
	return c.Report(todos.Preamble{}, "")
}

func sampleRejections() []rejection.Rejection {
	r := rejection.New()
	r.AddWithContext(failure.UnsupportedExecuteProcess, "refused", "tgt", "cmake/Probe.cmake")
	return r.Items()
}

func TestAssemblePrompt_GroundedAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	// Lay down a couple of cmake sources so the manifest is non-empty.
	mustWrite(t, filepath.Join(dir, "CMakeLists.txt"), "project(x)\n")
	mustWrite(t, filepath.Join(dir, "cmake", "Probe.cmake"), "# probe\n")
	// A packaged converted workspace: MODULE at top, BUILD under the package.
	conv := t.TempDir()
	mustWrite(t, filepath.Join(conv, "MODULE.bazel"), "module(name=\"x\")\n")
	mustWrite(t, filepath.Join(conv, "pkg", "BUILD.bazel"), "cc_library(name=\"x\")\n")

	in := PromptInputs{
		Element:      "demo",
		ConvertedDir: conv,
		CMakeSrcDir:  dir,
		Todos:        sampleTodos(),
		Rejections:   sampleRejections(),
	}
	a := AssemblePrompt(in)
	b := AssemblePrompt(in)
	if a != b {
		t.Fatal("AssemblePrompt not deterministic")
	}
	for _, want := range []string{
		"Intent-capture review — demo",
		"targets Bazel 9",                          // standing context
		"What did it MISS?",                        // the question
		"only NET-NEW",                             // dedup instruction
		filepath.Join(conv, "MODULE.bazel"),        // discovered bazel files
		filepath.Join(conv, "pkg", "BUILD.bazel"),  //
		filepath.Join(dir, "CMakeLists.txt"),       // cmake source manifest
		filepath.Join(dir, "cmake", "Probe.cmake"), //
		"todo `" + sampleTodos().Todos[0].ID + "`", // already-flagged todo
		"rejection `unsupported-execute-process`",  // already-flagged rejection
		"\"findings\":[",                           // output contract
		"GROUNDING:",                               // grounding rule
	} {
		if !strings.Contains(a, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, a)
		}
	}
}

func TestAssemblePrompt_CustomContextAndEmptyFlagged(t *testing.T) {
	a := AssemblePrompt(PromptInputs{Context: "CUSTOM CONTEXT LINE"})
	if !strings.Contains(a, "CUSTOM CONTEXT LINE") {
		t.Error("custom context not used")
	}
	if strings.Contains(a, "targets Bazel 9") {
		t.Error("default context should be replaced by the override")
	}
	if !strings.Contains(a, "(none)") {
		t.Error("empty already-flagged set should render (none)")
	}
}

func TestTriage_DedupAgainstTodoAndRejection(t *testing.T) {
	j := JudgeOutput{Findings: []Finding{
		{Category: "test", Severity: "high", Summary: "missing roundtrip test", CMakeRef: "tests/run.cmake:9"},
		{Category: "option", Severity: "medium", Summary: "probe baked", CMakeRef: "cmake/Probe.cmake"},
		{Category: "install", Severity: "high", Summary: "install layout dropped", CMakeRef: "CMakeLists.txt:40"},
		{Category: "other", Severity: "low", Summary: "ungrounded hunch"}, // no ref → net-new
	}}
	rep := Triage(j, sampleTodos(), sampleRejections(), "demo", "v1")

	if rep.Version != SchemaVersion || rep.Element != "demo" || rep.ToolVersion != "v1" {
		t.Fatalf("report header wrong: %+v", rep.Summary)
	}
	byStatus := map[string]TriagedFinding{}
	for _, f := range rep.Findings {
		byStatus[f.Summary] = f
	}
	if got := byStatus["missing roundtrip test"]; got.Status != StatusDupTodo || got.MatchedID != sampleTodos().Todos[0].ID {
		t.Errorf("todo-file finding should be dup-todo with the matched id; got %+v", got)
	}
	if got := byStatus["probe baked"]; got.Status != StatusDupRejection || got.MatchedID != "unsupported-execute-process" {
		t.Errorf("rejection-file finding should be dup-rejection; got %+v", got)
	}
	if got := byStatus["install layout dropped"]; got.Status != StatusNetNew {
		t.Errorf("unmatched ref should be net-new; got %+v", got)
	}
	if got := byStatus["ungrounded hunch"]; got.Status != StatusNetNew {
		t.Errorf("ungrounded finding should be net-new; got %+v", got)
	}
	if rep.Summary.Total != 4 || rep.Summary.NetNew != 2 || rep.Summary.AlreadyFlagged != 2 {
		t.Errorf("summary counts wrong: %+v", rep.Summary)
	}
	if rep.Summary.BySeverity["high"] != 2 || rep.Summary.BySeverity["medium"] != 1 || rep.Summary.BySeverity["low"] != 1 {
		t.Errorf("severity buckets wrong: %+v", rep.Summary.BySeverity)
	}
}

func TestTriage_DedupViaGroupKeyAndConstruct(t *testing.T) {
	// A todo with NO structured anchor file (the common producer shape) — its
	// identity lives in GroupKey + Construct. A finding citing the runner by
	// name must still dedup.
	c := todos.New()
	c.Add(todos.Todo{
		Kind: "cmake-p-test", Disposition: todos.Actionable, GroupKey: "run_test.cmake",
		Anchors: []todos.Anchor{{Construct: "add_test(NAME t COMMAND cmake -P run_test.cmake)"}},
	})
	rep := c.Report(todos.Preamble{}, "")

	j := JudgeOutput{Findings: []Finding{
		{Category: "test", Severity: "high", Summary: "runner cited by group_key", CMakeRef: "run_test.cmake:99"},
		{Category: "other", Severity: "low", Summary: "generic CMakeLists ref stays net-new", CMakeRef: "CMakeLists.txt:5"},
	}}
	out := Triage(j, rep, nil, "x", "")
	byCat := map[string]TriagedFinding{}
	for _, f := range out.Findings {
		byCat[f.Category] = f
	}
	if g := byCat["test"]; g.Status != StatusDupTodo || g.MatchedID != rep.Todos[0].ID {
		t.Errorf("group_key-cited finding should dup-todo; got %+v", g)
	}
	if g := byCat["other"]; g.Status != StatusNetNew {
		t.Errorf("bare CMakeLists.txt ref must NOT over-dedup; got %+v", g)
	}
}

func TestRefBaseForMatch_ExcludesGeneric(t *testing.T) {
	if refBaseForMatch("CMakeLists.txt:9") != "" {
		t.Error("bare CMakeLists.txt should be excluded from substring matching")
	}
	if got := refBaseForMatch("sub/run_test.cmake:3"); got != "run_test.cmake" {
		t.Errorf("refBaseForMatch = %q, want run_test.cmake", got)
	}
}

func TestTriage_SortsBySeverityThenSummary(t *testing.T) {
	j := JudgeOutput{Findings: []Finding{
		{Severity: "low", Category: "a", Summary: "z low"},
		{Severity: "high", Category: "a", Summary: "b high"},
		{Severity: "high", Category: "a", Summary: "a high"},
	}}
	rep := Triage(j, todos.Report{}, nil, "", "")
	got := []string{rep.Findings[0].Summary, rep.Findings[1].Summary, rep.Findings[2].Summary}
	want := []string{"a high", "b high", "z low"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sort order wrong at %d: got %v want %v", i, got, want)
		}
	}
}

func TestParseJudgeOutput_TolerantOfFenceAndProse(t *testing.T) {
	raw := "Here is my review:\n```json\n{\"findings\":[{\"summary\":\"x\",\"severity\":\"high\"}]}\n```\nDone.\n"
	j, err := ParseJudgeOutput([]byte(raw))
	if err != nil {
		t.Fatalf("ParseJudgeOutput: %v", err)
	}
	if len(j.Findings) != 1 || j.Findings[0].Summary != "x" {
		t.Errorf("parsed wrong: %+v", j.Findings)
	}
	if _, err := ParseJudgeOutput([]byte("no json here")); err == nil {
		t.Error("expected error on input with no JSON object")
	}
}

func TestSameFile(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"tests/run.cmake:9", "tests/run.cmake", true},
		{"cmake/Probe.cmake", "/abs/cmake/Probe.cmake", true},
		{"a/foo.cmake", "b/foo.cmake", false},
		{"foo.cmake", "bar.cmake", false},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := sameFile(c.a, c.b); got != c.want {
			t.Errorf("sameFile(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
