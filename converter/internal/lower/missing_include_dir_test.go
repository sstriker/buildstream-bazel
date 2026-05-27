package lower_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
)

// TestMissingIncludeDir_SilentlySkipped pins the LLVM-surfaced
// behaviour: a target_include_directories(... <dir>) reference
// whose <dir> doesn't physically exist on disk produces zero
// discovered headers, not an aborting "no such file or directory"
// error. cmake legitimately permits forward-declared include
// paths (LLVM's llvm-mca module declares `include` for future
// headers that don't yet exist), and dying on the walk drops
// every other surviving target from the conversion.
func TestMissingIncludeDir_SilentlySkipped(t *testing.T) {
	hostSrc := t.TempDir()
	// Create the source file the target compiles.
	srcRel := "lib.cc"
	if err := os.WriteFile(filepath.Join(hostSrc, srcRel), []byte("int x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two include dirs: one that exists, one that doesn't. The
	// nonexistent one is what the regression covers.
	existingInc := filepath.Join(hostSrc, "include")
	if err := os.MkdirAll(existingInc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existingInc, "real.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingInc := filepath.Join(hostSrc, "phantom")

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: hostSrc},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "lib", Id: "lib::@a"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@a": {
				Name:    "lib",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: srcRel, CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
					Includes: []fileapi.CompileInclude{
						{Path: existingInc},
						{Path: missingInc},
					},
				}},
			},
		},
	}
	var warnings bytes.Buffer
	collector := rejection.New()
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: hostSrc,
		Warnings:       &warnings,
		Rejections:     collector,
	})
	if err != nil {
		t.Fatalf("ToIR returned error on missing include dir: %v", err)
	}
	if pkg == nil || len(pkg.Targets) != 1 {
		t.Fatalf("expected 1 target, got %+v", pkg)
	}
	// The real header was discovered through the existing dir; the
	// missing dir contributes nothing and doesn't error.
	got := pkg.Targets[0]
	foundReal := false
	for _, h := range got.Hdrs {
		if filepath.Base(h) == "real.h" {
			foundReal = true
		}
	}
	if !foundReal {
		t.Errorf("expected real.h to be discovered via the existing include dir; hdrs=%v", got.Hdrs)
	}
	// The skip surfaces via stderr-shaped warning (operator-visible)
	// AND, when the diagnostic collector is active, via a recorded
	// rejection (machine-readable survey signal).
	if !strings.Contains(warnings.String(), missingInc) {
		t.Errorf("expected stderr warning to name the missing dir %q; got:\n%s", missingInc, warnings.String())
	}
	if collector.Len() == 0 {
		t.Errorf("expected at least one collected rejection for the missing dir; got 0")
	}
	if items := collector.Items(); len(items) > 0 && items[0].Code != failure.UnsupportedSourcePath {
		t.Errorf("rejection code = %q, want %q", items[0].Code, failure.UnsupportedSourcePath)
	}
}

// Non-diagnostic mode (no Warnings sink, no Rejections collector)
// still silently skips the missing dir. Preserves the
// lower-as-pure-function shape every existing test depends on.
func TestMissingIncludeDir_SilentInStrictMode(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "lib.cc"), []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: hostSrc},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@a"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@a": {
				Name:    "lib",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "lib.cc", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
					Includes: []fileapi.CompileInclude{
						{Path: filepath.Join(hostSrc, "ghost")},
					},
				}},
			},
		},
	}
	if _, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: hostSrc}); err != nil {
		t.Fatalf("strict-mode ToIR errored on missing include dir: %v", err)
	}
}
