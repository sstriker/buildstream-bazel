package cmakecfg_test

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

var update = flag.Bool("update", false, "overwrite *.golden files")

func TestEmit_HelloWorld_Bundle(t *testing.T) {
	src, err := filepath.Abs("../../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Stable iteration over the file list for golden compare.
	var names []string
	for n := range bundle.Files {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		got := bundle.Files[name]
		goldenPath := filepath.Join("..", "..", "..", "testdata", "golden", "hello-world", "cmake-config", name+".golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s", goldenPath)
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read %s (run with -update?): %v", goldenPath, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
		}
	}
}
