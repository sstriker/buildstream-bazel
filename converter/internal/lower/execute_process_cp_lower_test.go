package lower_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestToIR_ExecuteProcessCp_RecursiveDirLifts is the end-to-end
// regression for issue #312: a target plus a configure-time
// `cp -R <dir> ${BINARY}` execute_process call must lower
// cleanly (no unsupported-execute-process Tier-1) and land a
// copy genrule in the package. Before #312 the raw cp routed to
// BucketRefuse and failed the whole element.
func TestToIR_ExecuteProcessCp_RecursiveDirLifts(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "data", "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(hostSrc, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "src", "lib.c"), []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := "/build"

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: hostSrc, Build: build},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "thelib", Id: "thelib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": {
				Name: "thelib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "src/lib.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{Language: "C"}},
			},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","cp","-RauL","` + filepath.Join(hostSrc, "data") + `","` + build + `"],"cmd":"execute_process","file":"` + filepath.Join(hostSrc, "CMakeLists.txt") + `","line":7}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: hostSrc,
		BuildDir:       build,
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR should not fail with unsupported-execute-process; got: %v", err)
	}
	var gen *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Kind == ir.KindGenrule {
			gen = &pkg.Targets[i]
			break
		}
	}
	if gen == nil {
		t.Fatalf("expected a copy genrule in the package; targets: %+v", pkg.Targets)
	}
	if len(gen.GenruleOuts) != 1 || gen.GenruleOuts[0] != "data/f.txt" {
		t.Errorf("genrule outs: %v want [data/f.txt]", gen.GenruleOuts)
	}
	if !strings.Contains(gen.GenruleCmd, "cp -L") {
		t.Errorf("genrule cmd should copy the file; got %q", gen.GenruleCmd)
	}
}
