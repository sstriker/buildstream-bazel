package sourcecheckout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/element"
)

func TestResolve_LocalRelative(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "elements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "files", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	bstPath := filepath.Join(root, "elements", "x.bst")
	if err := os.WriteFile(bstPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	el := &element.Element{
		Name:       "x",
		SourcePath: bstPath,
		Sources: []element.Source{
			{Kind: "local", Extra: map[string]any{"path": "../files/x"}},
		},
	}
	r := &Resolver{
		ElementSourceDir: func(e *element.Element) string { return filepath.Dir(e.SourcePath) },
	}
	got, err := r.Resolve(context.Background(), el)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want, _ := filepath.Abs(filepath.Join(root, "files", "x"))
	if got.Path != want {
		t.Errorf("Resolve.Path = %q, want %q", got.Path, want)
	}
}

func TestResolve_SourcesBaseTakesPrecedence(t *testing.T) {
	base := t.TempDir()
	elemDir := filepath.Join(base, "components", "y")
	if err := os.MkdirAll(elemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{
		SourcesBase: base,
	}
	got, err := r.Resolve(context.Background(), &element.Element{Name: "components/y"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != elemDir {
		t.Errorf("Resolve.Path = %q, want %q", got.Path, elemDir)
	}
}

func TestResolve_UnsupportedKind(t *testing.T) {
	r := &Resolver{}
	_, err := r.Resolve(context.Background(), &element.Element{
		Name: "z",
		Sources: []element.Source{
			{Kind: "tar", Extra: map[string]any{"url": "https://example/x.tar"}},
		},
	})
	if err == nil {
		t.Fatal("expected unsupported-kind error")
	}
	if !strings.Contains(err.Error(), "unsupported source kind") {
		t.Errorf("err = %v, want unsupported-kind", err)
	}
}

// TestResolve_GitFromLocalRepo exercises the kind:git path against a
// real-but-local git repository created in a tmpdir. No network; the
// "url" is just a filesystem path.
func TestResolve_GitFromLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}

	upstream := t.TempDir()
	mustGit(t, upstream, "init", "-q", "-b", "main")
	mustWrite(t, filepath.Join(upstream, "CMakeLists.txt"), "project(g)\n")
	mustGit(t, upstream, "add", ".")
	// Override the per-host git config that the dev sandbox enforces
	// (commit signing) so the test commit lands without the host's
	// signing infrastructure being in scope.
	mustGit(t, upstream,
		"-c", "user.email=t@t",
		"-c", "user.name=t",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
		"commit", "-q", "--no-gpg-sign", "-m", "init",
	)
	ref := strings.TrimSpace(captureGit(t, upstream, "rev-parse", "HEAD"))

	r := &Resolver{CacheDir: t.TempDir()}
	el := &element.Element{
		Name: "g",
		Sources: []element.Source{
			{Kind: "git", Extra: map[string]any{"url": upstream, "ref": ref}},
		},
	}

	first, err := r.Resolve(context.Background(), el)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first.Path, "CMakeLists.txt")); err != nil {
		t.Errorf("checkout missing CMakeLists.txt: %v", err)
	}

	// Second call returns the same dir (cache hit).
	second, err := r.Resolve(context.Background(), el)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("cache miss on second call: %q vs %q", first.Path, second.Path)
	}
}

func TestResolve_GitMissingRef(t *testing.T) {
	r := &Resolver{CacheDir: t.TempDir()}
	_, err := r.Resolve(context.Background(), &element.Element{
		Name: "g",
		Sources: []element.Source{
			{Kind: "git", Extra: map[string]any{"url": "https://example/x.git"}},
		},
	})
	if err == nil {
		t.Fatal("expected missing-ref error")
	}
	if !strings.Contains(err.Error(), "ref") {
		t.Errorf("err = %v, want missing-ref", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func captureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
