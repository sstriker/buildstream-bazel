package bazel_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

var _ = flag.Lookup("update") // emit_test.go declares the -update flag

func TestEmit_CodegenTarget_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/codegen-target")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/codegen-target")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g, err := ninja.ParseFile("../../testdata/fileapi/codegen-target/build.ninja")
	if err != nil {
		t.Fatalf("ninja Parse: %v", err)
	}
	pkg, err := lower.ToIR(r, g, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Scrub paths whose values are non-deterministic across recordings:
	//   - the absolute host source root used at test time (in the file
	//     header comment, computed from filepath.Abs)
	//   - the absolute source root recorded into build.ninja at recording
	//     time (different host: my dev box vs. CI runners)
	//   - the absolute build dir baked into ${cmake_ninja_workdir} (always
	//     a per-recording mktemp path)
	got = []byte(strings.ReplaceAll(string(got), src, "<SRC>"))
	if recorded := r.Codemodel.Paths.Source; recorded != "" && recorded != src {
		got = []byte(strings.ReplaceAll(string(got), recorded, "<SRC>"))
	}
	got = scrubBuildTmp(got)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "codegen-target", "BUILD.bazel.golden")
	if isUpdate() {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update?): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// scrubBuildTmp replaces paths matching /tmp/tmp.XXXXXXXXXX with <BUILD_TMP>.
// CMake creates a fresh tmp dir per fixture recording, so the absolute build
// path leaks into recovered command strings. The tmp suffix is exactly 10
// chars from mktemp's default.
func scrubBuildTmp(b []byte) []byte {
	s := string(b)
	for {
		i := strings.Index(s, "/tmp/tmp.")
		if i < 0 {
			break
		}
		j := i + len("/tmp/tmp.")
		// Consume the suffix up to the next slash, space, or quote.
		end := j
		for end < len(s) {
			c := s[end]
			if c == '/' || c == ' ' || c == '"' || c == '\\' {
				break
			}
			end++
		}
		s = s[:i] + "<BUILD_TMP>" + s[end:]
	}
	return []byte(s)
}

func isUpdate() bool {
	f := flag.Lookup("update")
	if f == nil {
		return false
	}
	return f.Value.String() == "true"
}
