package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranslateTree_RoundTripsAGitElement: drop a kind:cmake element
// with a kind:git source into in/, run translateTree, assert out/ has
// the same .bst with a remote-asset source.
func TestTranslateTree_RoundTripsAGitElement(t *testing.T) {
	in := t.TempDir()
	out := t.TempDir()
	mustWriteBst(t, filepath.Join(in, "components", "hello.bst"), `kind: cmake
description: |
  hello world
depends:
- base.bst
sources:
- kind: git
  url: https://example/hello.git
  ref: deadbeef
`)
	mustWriteBst(t, filepath.Join(in, "base.bst"), `kind: manual
sources:
- kind: local
  path: files/base
`)

	count, err := translateTree(in, out)
	if err != nil {
		t.Fatalf("translateTree: %v", err)
	}
	if count != 2 {
		t.Errorf("translated %d, want 2", count)
	}

	// hello.bst: source rewritten to remote-asset.
	hello := mustReadFile(t, filepath.Join(out, "components", "hello.bst"))
	for _, want := range []string{
		"kind: remote-asset",
		"uri: bst:source:components/hello",
		"bst-source-kind: git",
		"bst-source-url: https://example/hello.git",
		"bst-source-ref: deadbeef",
	} {
		if !strings.Contains(hello, want) {
			t.Errorf("hello.bst missing %q\n%s", want, hello)
		}
	}

	// base.bst: kind:local passthrough.
	base := mustReadFile(t, filepath.Join(out, "base.bst"))
	if !strings.Contains(base, "kind: local") {
		t.Errorf("base.bst should still be kind:local:\n%s", base)
	}
	if strings.Contains(base, "remote-asset") {
		t.Errorf("base.bst should NOT have been translated:\n%s", base)
	}
}

func TestTranslateTree_EmptyTreeIsAnError(t *testing.T) {
	in := t.TempDir()
	out := t.TempDir()
	if _, err := translateTree(in, out); err == nil {
		t.Error("expected error for empty tree")
	}
}

func mustWriteBst(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(body)
}
