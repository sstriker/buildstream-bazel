package lower

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectWorkspaceRoot_HighestUmbrellaWins covers the zstd
// bug: nested `build/.gitignore` shouldn't shadow the actual
// project-root `.gitignore` one level up.
func TestDetectWorkspaceRoot_HighestUmbrellaWins(t *testing.T) {
	root := t.TempDir()
	// Layout:
	//   <root>/
	//     .gitignore            ← TRUE project root
	//     build/
	//       .gitignore          ← nested, should NOT win
	//       cmake/
	//         CMakeLists.txt    ← cmakeSrc
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	build := filepath.Join(root, "build")
	if err := os.Mkdir(build, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, ".gitignore"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmakeSrc := filepath.Join(build, "cmake")
	if err := os.Mkdir(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmakeSrc, "CMakeLists.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := detectWorkspaceRoot(cmakeSrc)
	if got != root {
		t.Errorf("detectWorkspaceRoot = %q; want %q (highest umbrella, not nested build/)", got, root)
	}
}

// TestDetectWorkspaceRoot_HardMarkerShortCircuits covers the
// preference: a hard marker (.git, MODULE.bazel) trumps the
// continued walk and returns immediately. Backstop against the
// fix accidentally walking past genuine workspace roots.
func TestDetectWorkspaceRoot_HardMarkerShortCircuits(t *testing.T) {
	root := t.TempDir()
	// <root>/.git → workspace root
	// <root>/parent/.gitignore → would be umbrella, BUT .git
	//   above it short-circuits anyway via walk semantics.
	// Test the simpler case: cmakeSrc directly under a .git'd repo.
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmakeSrc := filepath.Join(root, "build")
	if err := os.Mkdir(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}

	got := detectWorkspaceRoot(cmakeSrc)
	if got != root {
		t.Errorf("detectWorkspaceRoot = %q; want %q (.git short-circuits)", got, root)
	}
}

// TestDetectWorkspaceRoot_NoMarkerReturnsEmpty pins that the
// walk exits cleanly with "" when no marker fires.
func TestDetectWorkspaceRoot_NoMarkerReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	cmakeSrc := filepath.Join(root, "subdir")
	if err := os.Mkdir(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	got := detectWorkspaceRoot(cmakeSrc)
	if got != "" {
		t.Errorf("detectWorkspaceRoot = %q; want \"\" (no markers)", got)
	}
}
