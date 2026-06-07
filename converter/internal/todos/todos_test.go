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
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			t.Fatalf("MarshalIndent: %v", err)
		}
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
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"todos":[]`) {
		t.Errorf("empty report should marshal todos as []; got %s", b)
	}
}

// TestReport_DoesNotMutateInputAnchors guards the aliasing bug: Report
// must deep-copy each Todo's Anchors before sorting, leaving the caller's
// (and the Collector's) slice untouched.
func TestReport_DoesNotMutateInputAnchors(t *testing.T) {
	c := New()
	anchors := []Anchor{{File: "b", Line: 2, Construct: "x"}, {File: "a", Line: 1, Construct: "y"}}
	c.Add(Todo{Kind: "cmake-p-test", GroupKey: "g", Anchors: anchors})
	rep := c.Report(DefaultPreamble(), "")
	// The report is sorted...
	if rep.Todos[0].Anchors[0].File != "a" {
		t.Errorf("report anchors not sorted: %+v", rep.Todos[0].Anchors)
	}
	// ...but the caller's original slice is untouched.
	if anchors[0].File != "b" || anchors[1].File != "a" {
		t.Errorf("Report mutated the caller's anchors: %+v", anchors)
	}
}

func TestReset_ClearsAcrossPasses(t *testing.T) {
	c := New()
	c.Add(Todo{Kind: "cmake-p-test", GroupKey: "pass1.cmake", Anchors: []Anchor{{Construct: "x"}}})
	c.Reset() // simulate a re-run pass replacing the prior result
	c.Add(Todo{Kind: "cmake-p-test", GroupKey: "pass2.cmake", Anchors: []Anchor{{Construct: "y"}}})
	rep := c.Report(DefaultPreamble(), "")
	if len(rep.Todos) != 1 || rep.Todos[0].GroupKey != "pass2.cmake" {
		t.Fatalf("Reset should leave only the final pass; got %+v", rep.Todos)
	}
	var nilc *Collector
	nilc.Reset() // must not panic
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

// TestDefaultPreamble_ValidatesComments pins the rule instructing the post-pass
// to carry AND validate the construct's source comment onto the authored
// target — re-authored targets must not propagate a stale comment describing
// the old cmake mechanics. (Agent-authored targets are the comment-carrying
// path that the mechanical converter-side carry does not cover.)
func TestDefaultPreamble_ValidatesComments(t *testing.T) {
	p := DefaultPreamble()
	// Pin a phrase unique to rule (5) so the test fails if the rule is
	// substantively changed or removed, not merely if the words appear
	// anywhere in the preamble.
	const rule5 = "never carry a comment that misdescribes the target"
	if !strings.Contains(p.Rules, rule5) {
		t.Errorf("default preamble rules should carry rule (5) (%q); got %q", rule5, p.Rules)
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
