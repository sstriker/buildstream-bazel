//go:build e2e

// e2e_test runs the full pipeline (cmakerun -> fileapi -> lower -> emit)
// against a real cmake invocation. Gated by the `e2e` build tag and by
// `make test-e2e`; not part of the default `go test ./...` to keep the
// no-cmake unit-test loop fast.
package cmakerun_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

func TestE2E_HelloWorld(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}

	buildDir := t.TempDir()
	reply, err := cmakerun.Configure(t.Context(), cmakerun.Options{
		SourceRoot: src,
		BuildDir:   buildDir,
		Stdout:     testWriter{t},
		Stderr:     testWriter{t},
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	r, err := fileapi.Load(reply.Path)
	if err != nil {
		t.Fatalf("fileapi.Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Scrub the absolute host source path to match the golden's <SOURCE_ROOT>.
	got = []byte(strings.ReplaceAll(string(got), src, "<SOURCE_ROOT>"))

	want, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "hello-world", "BUILD.bazel.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("e2e BUILD.bazel diverges from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// testWriter routes cmake stdout/stderr lines into the test log so failures
// have full context but successful runs stay quiet under -v=false.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
