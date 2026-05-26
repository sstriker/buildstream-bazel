package lower

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectWorkspaceRoot_GitMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "build", "cmake"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmakeSrc := filepath.Join(root, "build", "cmake")
	got := detectWorkspaceRoot(cmakeSrc)
	if got != root {
		t.Errorf("detectWorkspaceRoot(%q) = %q; want %q", cmakeSrc, got, root)
	}
}

func TestDetectWorkspaceRoot_ModuleBazelMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "MODULE.bazel"), []byte("module(name=\"x\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cmake-src"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := detectWorkspaceRoot(filepath.Join(root, "cmake-src"))
	if got != root {
		t.Errorf("got %q; want %q", got, root)
	}
}

func TestDetectWorkspaceRoot_WorkspaceMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "WORKSPACE"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := detectWorkspaceRoot(filepath.Join(root, "a", "b", "c"))
	if got != root {
		t.Errorf("got %q; want %q", got, root)
	}
}

func TestDetectWorkspaceRoot_StartingDirHasMarker(t *testing.T) {
	// The starting dir itself has .git — detection should return
	// it (the workspace is the cmake source dir itself, common
	// case for projects whose CMakeLists is at repo root).
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := detectWorkspaceRoot(root)
	if got != root {
		t.Errorf("got %q; want %q (starting dir IS workspace)", got, root)
	}
}

func TestDetectWorkspaceRoot_NoMarkerFound(t *testing.T) {
	// No marker anywhere up the chain → return "".
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "build", "cmake"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := detectWorkspaceRoot(filepath.Join(root, "build", "cmake"))
	if got != "" {
		t.Errorf("got %q; want \"\" (no marker)", got)
	}
}

func TestDetectWorkspaceRoot_EmptyInput(t *testing.T) {
	if got := detectWorkspaceRoot(""); got != "" {
		t.Errorf("got %q; want \"\"", got)
	}
}
