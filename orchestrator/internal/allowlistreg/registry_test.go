package allowlistreg_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/allowlistreg"
)

func TestRegistry_RoundTrip(t *testing.T) {
	root := t.TempDir()
	r := allowlistreg.New(root)

	// First update creates the file.
	if err := r.Update("components/hello", []string{"include/version.h", "cmake/Helpers.cmake"}); err != nil {
		t.Fatal(err)
	}
	body := mustRead(t, filepath.Join(root, "components", "hello.json"))
	for _, want := range []string{
		`"version": 1`,
		`"element": "components/hello"`,
		`"include/version.h"`,
		`"cmake/Helpers.cmake"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}

	// Reload into a fresh Registry: same paths.
	r2 := allowlistreg.New(root)
	if err := r2.Load("components/hello"); err != nil {
		t.Fatal(err)
	}
	got := r2.Paths("components/hello")
	want := []string{"cmake/Helpers.cmake", "include/version.h"}
	if !sliceEqual(got, want) {
		t.Errorf("Paths = %v, want %v", got, want)
	}
}

func TestRegistry_UpdateMergesAndDedupes(t *testing.T) {
	root := t.TempDir()
	r := allowlistreg.New(root)
	if err := r.Update("a", []string{"x", "y", "x"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Update("a", []string{"y", "z"}); err != nil {
		t.Fatal(err)
	}
	got := r.Paths("a")
	if !sliceEqual(got, []string{"x", "y", "z"}) {
		t.Errorf("got %v, want sorted-deduped union", got)
	}
}

func TestRegistry_Matcher_UnionsDefaultAndRegistered(t *testing.T) {
	root := t.TempDir()
	r := allowlistreg.New(root)
	if err := r.Update("a", []string{"src/data.bin"}); err != nil {
		t.Fatal(err)
	}
	m := r.Matcher("a")

	// Default allowlist still fires.
	if !m.Allowed("CMakeLists.txt") {
		t.Errorf("CMakeLists.txt not allowed; default-allowlist regression")
	}
	// Registry-augmented path now fires.
	if !m.Allowed("src/data.bin") {
		t.Errorf("registered path src/data.bin not allowed")
	}
	// Nothing else does.
	if m.Allowed("src/random.txt") {
		t.Errorf("unrelated path src/random.txt unexpectedly allowed")
	}
}

func TestRegistry_LoadMissingFileIsNoop(t *testing.T) {
	r := allowlistreg.New(t.TempDir())
	if err := r.Load("never-saved"); err != nil {
		t.Errorf("Load on missing file should be a noop, got %v", err)
	}
	if got := r.Paths("never-saved"); len(got) != 0 {
		t.Errorf("Paths = %v, want []", got)
	}
}

func TestRegistry_LoadRejectsUnknownVersion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.json"),
		[]byte(`{"version":99,"element":"bad","paths":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := allowlistreg.New(root)
	if err := r.Load("bad"); err == nil {
		t.Error("expected version-mismatch error")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
