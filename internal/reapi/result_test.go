package reapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// stageOutputs writes the converter's typical post-run layout under
// rootDir and returns the canonical output paths the action declared.
func stageOutputs(t *testing.T) (rootDir string, outputPaths []string) {
	t.Helper()
	rootDir = t.TempDir()

	mustMkdirP(t, rootDir, "cmake-config")
	mustWrite(t, filepath.Join(rootDir, "BUILD.bazel"), "cc_library(name = \"x\")\n")
	mustWrite(t, filepath.Join(rootDir, "read_paths.json"), `{"version":1,"paths":[]}`)
	mustWrite(t, filepath.Join(rootDir, "cmake-config", "xConfig.cmake"), "include(...)\n")
	mustWrite(t, filepath.Join(rootDir, "cmake-config", "xTargets.cmake"), "add_library(x INTERFACE)\n")
	// no failure.json — successful run
	return rootDir, []string{
		"BUILD.bazel",
		"cmake-config",
		"failure.json",
		"read_paths.json",
	}
}

func TestSynthAndMaterialize_RoundTrip(t *testing.T) {
	src, paths := stageOutputs(t)

	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	ctx := context.Background()

	ar, err := SynthesizeResult(ctx, store, src, paths, 0, []byte("stdout"), nil)
	if err != nil {
		t.Fatalf("SynthesizeResult: %v", err)
	}
	if ar.ExitCode != 0 {
		t.Errorf("exit_code: got %d want 0", ar.ExitCode)
	}
	for _, of := range ar.OutputFiles {
		if of.Path == "failure.json" {
			t.Errorf("failure.json should not appear when missing on disk")
		}
	}
	if ar.StdoutDigest == nil {
		t.Errorf("stdout digest should be set")
	}

	dst := t.TempDir()
	if err := MaterializeResult(ctx, store, ar, dst); err != nil {
		t.Fatalf("MaterializeResult: %v", err)
	}

	for _, rel := range []string{
		"BUILD.bazel",
		"read_paths.json",
		"cmake-config/xConfig.cmake",
		"cmake-config/xTargets.cmake",
	} {
		srcBody, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatalf("read src %s: %v", rel, err)
		}
		dstBody, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read dst %s: %v", rel, err)
		}
		if string(srcBody) != string(dstBody) {
			t.Errorf("file %s mismatch after round-trip", rel)
		}
	}
}

func TestSynthesize_FailureJSONIncludedWhenPresent(t *testing.T) {
	rootDir, paths := stageOutputs(t)
	mustWrite(t, filepath.Join(rootDir, "failure.json"), `{"code":"x"}`)

	store, _ := cas.NewLocalStore(t.TempDir())
	ar, err := SynthesizeResult(context.Background(), store, rootDir, paths, 1, nil, nil)
	if err != nil {
		t.Fatalf("SynthesizeResult: %v", err)
	}
	found := false
	for _, of := range ar.OutputFiles {
		if of.Path == "failure.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("failure.json should appear in OutputFiles when present on disk")
	}
}

func TestMaterialize_MissingBlobReturnsErrMissingBlob(t *testing.T) {
	src, paths := stageOutputs(t)
	store, _ := cas.NewLocalStore(t.TempDir())
	ctx := context.Background()

	ar, err := SynthesizeResult(ctx, store, src, paths, 0, nil, nil)
	if err != nil {
		t.Fatalf("SynthesizeResult: %v", err)
	}
	if len(ar.OutputFiles) == 0 {
		t.Fatalf("no output files to evict")
	}
	target := ar.OutputFiles[0]
	if err := os.Remove(filepath.Join(store.Root, "cas", target.Digest.Hash)); err != nil {
		t.Fatalf("evict: %v", err)
	}

	dst := t.TempDir()
	err = MaterializeResult(ctx, store, ar, dst)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var miss *ErrMissingBlob
	if !errors.As(err, &miss) {
		t.Fatalf("expected ErrMissingBlob, got %T: %v", err, err)
	}
	if !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("ErrMissingBlob should wrap cas.ErrNotFound, got Is=false")
	}
	if miss.Path != target.Path {
		t.Errorf("ErrMissingBlob.Path: got %q want %q", miss.Path, target.Path)
	}
}

func TestSynthesize_DirectoryBlobs_AllUploaded(t *testing.T) {
	src, paths := stageOutputs(t)
	store, _ := cas.NewLocalStore(t.TempDir())
	ctx := context.Background()

	ar, err := SynthesizeResult(ctx, store, src, paths, 0, nil, nil)
	if err != nil {
		t.Fatalf("SynthesizeResult: %v", err)
	}
	if len(ar.OutputDirectories) != 1 {
		t.Fatalf("expected 1 OutputDirectory, got %d", len(ar.OutputDirectories))
	}
	od := ar.OutputDirectories[0]
	body, err := store.GetBlob(ctx, od.TreeDigest)
	if err != nil {
		t.Fatalf("Tree blob missing: %v", err)
	}
	if cas.DigestOf(body).Hash != od.TreeDigest.Hash {
		t.Errorf("Tree digest hash drift")
	}
}
