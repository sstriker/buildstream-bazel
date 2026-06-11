package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

// protoImportClosure notes a declared .proto that can't be read (so its
// transitive imports may be missing) instead of skipping it silently.
func TestProtoImportClosure_NotesUnreadableSrc(t *testing.T) {
	dir := t.TempDir()
	// svc.proto exists and imports a (missing-on-disk) sibling; the missing
	// one is the well-known-type-style Stat miss (confident silent skip).
	if err := os.WriteFile(filepath.Join(dir, "svc.proto"), []byte("import \"missing.proto\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cc := &codegenContext{}
	// "ghost.proto" is declared but never written → ReadFile fails → noted.
	_ = protoImportClosure([]string{"svc.proto", "ghost.proto"}, dir, cc)
	var refs []string
	for _, in := range cc.UnresolvedRecoveryInputs {
		if in.Kind == unresolvedProtoImportUnreadable {
			refs = append(refs, in.Ref)
		}
	}
	if len(refs) != 1 || refs[0] != "ghost.proto" {
		t.Errorf("want [ghost.proto] noted unreadable; got %v (all=%+v)", refs, cc.UnresolvedRecoveryInputs)
	}
}

// noteUnresolvedRecoveryInput is nil-safe (test/offline call sites pass nil).
func TestNoteUnresolvedRecoveryInput_NilSafe(t *testing.T) {
	var cc *codegenContext
	cc.noteUnresolvedRecoveryInput(unresolvedProtoImportUnreadable, "a/x.proto") // must not panic
	cc = &codegenContext{}
	cc.noteUnresolvedRecoveryInput(unresolvedProtoImportUnreadable, "a/x.proto")
	if len(cc.UnresolvedRecoveryInputs) != 1 || cc.UnresolvedRecoveryInputs[0].Ref != "a/x.proto" {
		t.Fatalf("record: %+v", cc.UnresolvedRecoveryInputs)
	}
}

// One folded todo per kind; refs dedupe and sort; each kind gets its own
// prompt/shape.
func TestEmitUnresolvedRecoveryInputTodos_GroupingAndDedup(t *testing.T) {
	c := todos.New()
	emitUnresolvedRecoveryInputTodos(c, []unresolvedRecoveryInput{
		{Kind: unresolvedProtoImportUnreadable, Ref: "a/svc.proto"},
		{Kind: unresolvedProtoImportUnreadable, Ref: "a/svc.proto"}, // dup
		{Kind: unresolvedProtoImportUnreadable, Ref: "a/msg.proto"},
		{Kind: unresolvedConfigureFileUnanchored, Ref: "foo_export.h"},
		{Kind: unresolvedNestedHeaderUnreadable, Ref: "_deps/sub-build/cfg.h"},
	})
	rep := c.Report(todos.Preamble{}, "")
	if len(rep.Todos) != 3 {
		t.Fatalf("want 3 todos (one per kind); got %d: %+v", len(rep.Todos), rep.Todos)
	}
	var proto *todos.Todo
	for i := range rep.Todos {
		if rep.Todos[i].Kind != "unresolved-recovery-input" {
			t.Errorf("kind: %q", rep.Todos[i].Kind)
		}
		if rep.Todos[i].GroupKey == unresolvedProtoImportUnreadable {
			proto = &rep.Todos[i]
		}
	}
	if proto == nil {
		t.Fatalf("missing proto group: %+v", rep.Todos)
	}
	if len(proto.Anchors) != 2 {
		t.Errorf("proto group should fold 2 anchors (dup dropped); got %+v", proto.Anchors)
	}
	// Refs are leak-safe relative paths and appear verbatim in the anchor.
	if !strings.Contains(proto.Anchors[0].Construct, "a/msg.proto") {
		t.Errorf("anchor missing ref: %q", proto.Anchors[0].Construct)
	}
}

// Nil collector / empty inputs are no-ops; id is stable per kind.
func TestEmitUnresolvedRecoveryInputTodos_NoOps(t *testing.T) {
	emitUnresolvedRecoveryInputTodos(nil, []unresolvedRecoveryInput{{Kind: unresolvedProtoImportUnreadable, Ref: "x"}})
	c := todos.New()
	emitUnresolvedRecoveryInputTodos(c, nil)
	if c.Len() != 0 {
		t.Errorf("empty inputs should add nothing")
	}
}

// warnUnresolvedRecoveryInputs is a no-op on an empty set and feeds the
// collector + breadcrumb when populated.
func TestWarnUnresolvedRecoveryInputs(t *testing.T) {
	var sb strings.Builder
	c := todos.New()
	cc := &codegenContext{}
	warnUnresolvedRecoveryInputs(Options{Warnings: &sb, Todos: c}, cc)
	if c.Len() != 0 || sb.Len() != 0 {
		t.Errorf("empty set must be a no-op; todos=%d warn=%q", c.Len(), sb.String())
	}
	cc.noteUnresolvedRecoveryInput(unresolvedConfigureFileUnanchored, "foo_export.h")
	warnUnresolvedRecoveryInputs(Options{Warnings: &sb, Todos: c}, cc)
	if c.Len() != 1 {
		t.Errorf("want 1 todo; got %d", c.Len())
	}
	if !strings.Contains(sb.String(), "couldn't be resolved") {
		t.Errorf("breadcrumb missing: %q", sb.String())
	}
}
