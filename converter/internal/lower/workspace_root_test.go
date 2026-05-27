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

// TestDetectWorkspaceRoot_DepthCap pins the bounded walk-up
// behavior. An unbounded walk would catch any .git arbitrarily
// far above cmakeSrc, which is wrong for the common "this is a
// subdir of a git repo, not a separate workspace" case
// (it would promote in-repo test fixtures' workspace to the
// repo root and break per-target include resolution). The cap
// lets real-world build/<X>/CMakeLists.txt layouts (depth 1-2
// above the marker) work while keeping deeper unrelated repos
// from triggering the heuristic.
func TestDetectWorkspaceRoot_DepthCap(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Build a chain deeper than workspaceMarkerMaxDepth. The
	// detection should return "" because the .git is beyond the
	// cap, even though it exists.
	deep := root
	for i := 0; i < workspaceMarkerMaxDepth+2; i++ {
		deep = filepath.Join(deep, "deeper")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspaceRoot(deep); got != "" {
		t.Errorf("detectWorkspaceRoot(%q) = %q; want \"\" (beyond depth cap)", deep, got)
	}

	// One step shallower than the cap should still find the marker.
	withinCap := root
	for i := 0; i < workspaceMarkerMaxDepth; i++ {
		withinCap = filepath.Join(withinCap, "deeper")
	}
	if err := os.MkdirAll(withinCap, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspaceRoot(withinCap); got != root {
		t.Errorf("detectWorkspaceRoot(%q) = %q; want %q (within cap)", withinCap, got, root)
	}
}

// TestDetectWorkspaceRoot_UmbrellaMonorepo covers LLVM's shape:
// `llvm-project/llvm/CMakeLists.txt` is the cmake source root,
// `llvm-project/` has no top-level CMakeLists.txt but carries
// `.gitignore`, and the build references sources at
// `llvm-project/third-party/benchmark/*`. The walk should
// promote `llvm-project/` to workspace root so those sibling-
// tree paths fall inside `labelRoot`.
func TestDetectWorkspaceRoot_UmbrellaMonorepo(t *testing.T) {
	root := t.TempDir()
	monorepo := filepath.Join(root, "llvm-project")
	cmakeSrc := filepath.Join(monorepo, "llvm")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	// llvm-project/.gitignore (umbrella marker).
	if err := os.WriteFile(filepath.Join(monorepo, ".gitignore"), []byte("*.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// llvm-project/llvm/CMakeLists.txt (the cmake project).
	if err := os.WriteFile(filepath.Join(cmakeSrc, "CMakeLists.txt"), []byte("project(llvm)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// llvm/.gitignore (subproject's own ignore — should NOT
	// short-circuit; the cmakeSrc itself is excluded from
	// umbrella-marker matching).
	if err := os.WriteFile(filepath.Join(cmakeSrc, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspaceRoot(cmakeSrc); got != monorepo {
		t.Errorf("detectWorkspaceRoot(%q) = %q; want %q (monorepo umbrella)",
			cmakeSrc, got, monorepo)
	}
}

// TestDetectWorkspaceRoot_UmbrellaSkipsParentWithCMakeLists
// pins the safety guard: a parent dir with .gitignore AND a
// CMakeLists.txt is itself a cmake project, not an umbrella.
// The walk should NOT promote it (would break per-target
// include resolution for the cmakeSrc).
func TestDetectWorkspaceRoot_UmbrellaSkipsParentWithCMakeLists(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	cmakeSrc := filepath.Join(parent, "subproject")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	// Parent has both .gitignore AND CMakeLists.txt — looks like
	// a cmake project that happens to have a subdir build, not
	// an umbrella.
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("*.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "CMakeLists.txt"), []byte("project(parent)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspaceRoot(cmakeSrc); got != "" {
		t.Errorf("detectWorkspaceRoot(%q) = %q; want \"\" (parent has CMakeLists, not umbrella)",
			cmakeSrc, got)
	}
}

// TestDetectWorkspaceRoot_SingleProjectGitignoreNoFalsePositive
// covers spdlog's shape: cmakeSrc itself has `.gitignore` (its
// own git housekeeping) and a CMakeLists.txt. The detection
// should return "" — the cmakeSrc isn't an umbrella, and no
// higher dir carries a marker.
func TestDetectWorkspaceRoot_SingleProjectGitignoreNoFalsePositive(t *testing.T) {
	root := t.TempDir()
	cmakeSrc := filepath.Join(root, "spdlog")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmakeSrc, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmakeSrc, "CMakeLists.txt"), []byte("project(spdlog)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectWorkspaceRoot(cmakeSrc); got != "" {
		t.Errorf("detectWorkspaceRoot(%q) = %q; want \"\" (single-project, no umbrella)",
			cmakeSrc, got)
	}
}
