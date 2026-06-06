package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestLowerTarget_WorkspaceRoot_ZstdLayout covers the
// `build/cmake/CMakeLists.txt` shape (zstd, lz4, brotli):
// cmake's source tree is one or two levels below the actual
// workspace root, and CMakeLists references sources via
// `${ZSTD_SOURCE_DIR}/lib/...` resolving to absolute paths
// outside cmakeSrc. With a workspace marker (.git) detectable
// above cmakeSrc, the lower must relativize against the
// workspace root rather than refuse with
// unsupported-source-path.
func TestLowerTarget_WorkspaceRoot_ZstdLayout(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmakeSrc := filepath.Join(workspace, "build", "cmake")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	// The source lives at <workspace>/lib/common/debug.c — outside
	// cmakeSrc, inside workspace. Pre-fix, this path would refuse.
	srcAbs := filepath.Join(workspace, "lib", "common", "debug.c")
	if err := os.MkdirAll(filepath.Dir(srcAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcAbs, []byte("/* zstd debug */\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := fileapi.Target{
		Name: "libzstd_shared",
		Type: "SHARED_LIBRARY",
		Sources: []fileapi.TargetSource{
			{Path: srcAbs},
		},
		CompileGroups: []fileapi.CompileGroup{{
			Language:      "C",
			SourceIndexes: []int{0},
		}},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"libzstd_shared::@": target},
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: cmakeSrc,
				Build:  t.TempDir(),
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "libzstd_shared::@", Name: "libzstd_shared"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v (should accept workspace-relative source)", err)
	}
	var lzs *struct {
		Name string
		Srcs []string
	}
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "libzstd_shared" {
			lzs = &struct {
				Name string
				Srcs []string
			}{
				Name: pkg.Targets[i].Name,
				Srcs: pkg.Targets[i].Srcs,
			}
		}
	}
	if lzs == nil {
		t.Fatal("libzstd_shared not in pkg.Targets")
	}
	want := "lib/common/debug.c"
	if len(lzs.Srcs) != 1 || lzs.Srcs[0] != want {
		t.Errorf("Srcs: got %v; want [%q] (workspace-relative)", lzs.Srcs, want)
	}
}

// TestLowerTarget_WorkspaceRoot_NoMarker_PreservesExistingBehavior
// confirms that when no workspace marker is present (the
// shadow-stage path), the lower falls back to cmakeSrc-relative
// labels — preserving existing behavior for write-a's
// orchestrator path.
func TestLowerTarget_WorkspaceRoot_NoMarker_PreservesExistingBehavior(t *testing.T) {
	shadow := t.TempDir() // no .git, no MODULE.bazel, no WORKSPACE
	cmakeSrc := filepath.Join(shadow, "src")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	// Source under cmakeSrc — should resolve to cmakeSrc-relative.
	srcAbs := filepath.Join(cmakeSrc, "main.c")
	if err := os.WriteFile(srcAbs, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	target := fileapi.Target{
		Name: "app",
		Type: "EXECUTABLE",
		Sources: []fileapi.TargetSource{
			{Path: srcAbs},
		},
		CompileGroups: []fileapi.CompileGroup{{
			Language:      "C",
			SourceIndexes: []int{0},
		}},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": target},
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: cmakeSrc, Build: t.TempDir()},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for i := range pkg.Targets {
		if pkg.Targets[i].Name != "app" {
			continue
		}
		if len(pkg.Targets[i].Srcs) != 1 || pkg.Targets[i].Srcs[0] != "main.c" {
			t.Errorf("Srcs: got %v; want [main.c]", pkg.Targets[i].Srcs)
		}
	}
}

// TestLowerTarget_WorkspaceRoot_PathOutsideWorkspace_StillRefuses
// confirms the refusal still fires for absolute paths that
// escape even the workspace root — `/vendored/elsewhere/bar.c`
// shapes that have no business in a Bazel label namespace.
func TestLowerTarget_WorkspaceRoot_PathOutsideWorkspace_StillRefuses(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmakeSrc := filepath.Join(workspace, "build", "cmake")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // genuinely outside workspace
	srcAbs := filepath.Join(other, "vendor.c")
	if err := os.WriteFile(srcAbs, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	target := fileapi.Target{
		Name: "x",
		Type: "EXECUTABLE",
		Sources: []fileapi.TargetSource{
			{Path: srcAbs},
		},
		CompileGroups: []fileapi.CompileGroup{{
			Language:      "C",
			SourceIndexes: []int{0},
		}},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"x::@": target},
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: cmakeSrc, Build: t.TempDir()},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "x::@", Name: "x"}},
			}},
		},
	}
	if _, err := ToIR(r, &ninja.Graph{}, Options{}); err == nil {
		t.Error("expected unsupported-source-path refusal for path outside workspace; got nil")
	}
}

// TestLowerTarget_Umbrella_NoSourceEscape_DoesNotPromote covers the LLVM shape:
// a self-contained cmake subproject (llvm-project/llvm) that sits under a
// monorepo's .git but keeps EVERY source inside cmakeSrc. detectWorkspaceRoot
// trips on the .git, but the promotion must NOT take effect — nothing escapes
// cmakeSrc, so there's no wider namespace to anchor to. Promoting anyway would
// prefix labels with `llvm/`, which the converter applies inconsistently across
// emitters (genrule srcs get it, install(FILES)/root refs don't), producing a
// self-inconsistent single/double package tree that no overlay can satisfy
// (the LLVM build-lens whole-tree drive surfaced exactly this). The source
// therefore stays cmakeSrc-relative. The genuine promote-on-escape case (zstd's
// sibling sources) is covered by TestLowerTarget_WorkspaceRoot_ZstdLayout.
func TestLowerTarget_Umbrella_NoSourceEscape_DoesNotPromote(t *testing.T) {
	monorepo := t.TempDir()
	// Umbrella marker (no top-level CMakeLists.txt → umbrella).
	if err := os.WriteFile(filepath.Join(monorepo, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cmakeSrc one level under the umbrella.
	cmakeSrc := filepath.Join(monorepo, "llvm")
	if err := os.MkdirAll(cmakeSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmakeSrc, "CMakeLists.txt"), []byte("project(llvm)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Source lives inside cmakeSrc — codemodel records it
	// cmakeSrc-relative (LLVM unittests shape).
	srcRel := filepath.Join("unittests", "ADT", "AnyTest.cpp")
	srcAbs := filepath.Join(cmakeSrc, srcRel)
	if err := os.MkdirAll(filepath.Dir(srcAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcAbs, []byte("// gtest stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := fileapi.Target{
		Name: "ADTTests",
		Type: "EXECUTABLE",
		Sources: []fileapi.TargetSource{
			{Path: srcRel}, // cmakeSrc-relative per codemodel-v2 spec
		},
		CompileGroups: []fileapi.CompileGroup{{
			Language:      "CXX",
			SourceIndexes: []int{0},
		}},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"ADTTests::@": target},
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: cmakeSrc,
				Build:  t.TempDir(),
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "ADTTests::@", Name: "ADTTests"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var got *struct {
		Name string
		Srcs []string
	}
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "ADTTests" {
			got = &struct {
				Name string
				Srcs []string
			}{
				Name: pkg.Targets[i].Name,
				Srcs: pkg.Targets[i].Srcs,
			}
		}
	}
	if got == nil {
		t.Fatal("ADTTests not in pkg.Targets")
	}
	// The source must survive (not be elided as missing-on-disk) and stay
	// cmakeSrc-relative: this fixture's only source lives INSIDE cmakeSrc, so
	// nothing escapes and the workspace-root promotion must NOT fire (the .git
	// is the monorepo's, but a self-contained subproject like llvm-project/llvm
	// doesn't need the umbrella — promoting it injects a spurious `llvm/` prefix
	// the converter applies inconsistently across emitters). The promote-on-
	// real-escape case is covered by TestLowerTarget_WorkspaceRoot_ZstdLayout.
	want := "unittests/ADT/AnyTest.cpp"
	if len(got.Srcs) != 1 || got.Srcs[0] != want {
		t.Errorf("Srcs: got %v; want [%q] (cmakeSrc-relative — no promotion without source escape)",
			got.Srcs, want)
	}
}
