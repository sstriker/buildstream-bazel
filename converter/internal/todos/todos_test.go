package todos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestID_StableAndLineFree(t *testing.T) {
	a := ID("cmake-p-test", "//:brotli")
	b := ID("cmake-p-test", "//:brotli")
	if a != b {
		t.Fatalf("ID not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "todo-") {
		t.Errorf("ID missing todo- prefix: %q", a)
	}
	if ID("cmake-p-test", "//:brotli") == ID("install-script", "//:brotli") {
		t.Error("ID should differ by kind")
	}
	if ID("cmake-p-test", "//:a") == ID("cmake-p-test", "//:b") {
		t.Error("ID should differ by group_key")
	}
}

func TestReport_DeterministicSortAndIDs(t *testing.T) {
	// Add out of order; Report must sort by (kind, group_key) and fill IDs.
	c := New()
	c.Add(Todo{Kind: "install-script", GroupKey: "z|post.cmake", Anchors: []Anchor{{Construct: "install(SCRIPT post.cmake)"}}})
	c.Add(Todo{Kind: "cmake-p-test", GroupKey: "//:b", Anchors: []Anchor{
		{File: "b.txt", Line: 9, Construct: "add_test(...b9)"},
		{File: "a.txt", Line: 2, Construct: "add_test(...a2)"},
	}})
	c.Add(Todo{Kind: "cmake-p-test", GroupKey: "//:a", Anchors: []Anchor{{Construct: "add_test(...a)"}}})

	rep := c.Report(DefaultPreamble(), "")
	if rep.Version != SchemaVersion {
		t.Errorf("Version = %d, want %d", rep.Version, SchemaVersion)
	}
	if len(rep.Todos) != 3 {
		t.Fatalf("expected 3 todos, got %d", len(rep.Todos))
	}
	wantOrder := []struct{ kind, gk string }{
		{"cmake-p-test", "//:a"},
		{"cmake-p-test", "//:b"},
		{"install-script", "z|post.cmake"},
	}
	for i, w := range wantOrder {
		if rep.Todos[i].Kind != w.kind || rep.Todos[i].GroupKey != w.gk {
			t.Errorf("todo[%d] = (%s, %s), want (%s, %s)", i, rep.Todos[i].Kind, rep.Todos[i].GroupKey, w.kind, w.gk)
		}
		if rep.Todos[i].ID != ID(w.kind, w.gk) {
			t.Errorf("todo[%d] ID = %q, want %q", i, rep.Todos[i].ID, ID(w.kind, w.gk))
		}
	}
	// Anchors sorted by (file, line).
	anchors := rep.Todos[1].Anchors
	if anchors[0].File != "a.txt" || anchors[1].File != "b.txt" {
		t.Errorf("anchors not sorted by file: %+v", anchors)
	}
}

func TestReport_ByteIdenticalAcrossRuns(t *testing.T) {
	build := func() []byte {
		c := New()
		c.Add(Todo{Kind: "cmake-internal-drop", GroupKey: "install", Anchors: []Anchor{{Construct: "install: x"}}, Evidence: map[string]any{"drop_kind": "install", "outputs": []string{"x", "y"}}})
		c.Add(Todo{Kind: "cmake-p-test", GroupKey: "//:cli", Anchors: []Anchor{{Construct: "add_test(...)"}}})
		rep := c.Report(DefaultPreamble(), "")
		b, _ := json.MarshalIndent(rep, "", "  ")
		return b
	}
	first := string(build())
	second := string(build())
	if first != second {
		t.Error("report not byte-identical across runs")
	}
}

func TestReport_EmptyEmitsArray(t *testing.T) {
	c := New()
	rep := c.Report(DefaultPreamble(), "")
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"todos":[]`) {
		t.Errorf("empty report should marshal todos as []; got %s", b)
	}
}

func TestReport_NilCollector(t *testing.T) {
	var c *Collector
	rep := c.Report(DefaultPreamble(), "v1")
	if rep.Todos == nil || len(rep.Todos) != 0 {
		t.Errorf("nil collector should yield empty todos; got %+v", rep.Todos)
	}
	if c.Len() != 0 {
		t.Errorf("nil collector Len = %d, want 0", c.Len())
	}
	c.Add(Todo{Kind: "x"}) // must not panic
}

func TestDefaultPreamble_HasBrotliExample(t *testing.T) {
	p := DefaultPreamble()
	if !strings.Contains(p.Example, "brotli") {
		t.Errorf("default preamble example should mention brotli; got %q", p.Example)
	}
	if p.Text != "" {
		t.Errorf("default preamble should not set Text; got %q", p.Text)
	}
}

func TestLoadPreamble_OverrideAndDefault(t *testing.T) {
	got, err := LoadPreamble("")
	if err != nil {
		t.Fatalf("LoadPreamble(\"\"): %v", err)
	}
	if got.Example == "" {
		t.Error("empty path should yield the built-in default")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "preamble.txt")
	if err := os.WriteFile(path, []byte("custom guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LoadPreamble(path)
	if err != nil {
		t.Fatalf("LoadPreamble(path): %v", err)
	}
	if got.Text != "custom guidance" {
		t.Errorf("override Text = %q, want %q", got.Text, "custom guidance")
	}
	if got.Intent != "" {
		t.Errorf("override should clear structured default; got Intent %q", got.Intent)
	}
}
