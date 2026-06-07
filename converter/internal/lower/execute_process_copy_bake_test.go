package lower_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestToIR_ExecuteProcessCopyIfDifferent_BuildDirIntermediateBakes reproduces
// LLVM's AddLLVM.cmake shape: a `cmake -E copy_if_different <build>/X.tmp
// <build>/X` whose SOURCE is a configure-time build-dir intermediate (not a
// source-tree input), where X is a generated header consumers #include
// (Extension.def / ExtensionDependencies.inc). The source can't anchor under
// the source root, but the final output exists on disk in the build dir, so
// the converter bakes its bytes as a write_file (config.h-class) instead of
// refusing with unsupported-execute-process (which would abort the strict
// build-lens convert).
func TestToIR_ExecuteProcessCopyIfDifferent_BuildDirIntermediateBakes(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "src", "lib.c"), []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The build dir holds the FINAL output on disk (the copy already ran at
	// configure time); the .tmp source need not exist.
	hostBuild := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostBuild, "include", "llvm", "Support"), 0o755); err != nil {
		t.Fatal(err)
	}
	defContent := "// generated\n#undef HANDLE_EXTENSION\n"
	if err := os.WriteFile(filepath.Join(hostBuild, "include", "llvm", "Support", "Extension.def"), []byte(defContent), 0o644); err != nil {
		t.Fatal(err)
	}

	tmp := filepath.Join(hostBuild, "include", "llvm", "Support", "Extension.def.tmp")
	dst := filepath.Join(hostBuild, "include", "llvm", "Support", "Extension.def")
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: hostSrc, Build: hostBuild},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "thelib", Id: "thelib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": {
				Name:          "thelib",
				Type:          "STATIC_LIBRARY",
				Sources:       []fileapi.TargetSource{{Path: "src/lib.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{Language: "C"}},
			},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","/usr/bin/cmake","-E","copy_if_different","` + tmp + `","` + dst + `"],"cmd":"execute_process","file":"` + filepath.Join(hostSrc, "CMakeLists.txt") + `","line":1286}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: hostSrc,
		BuildDir:       hostBuild,
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR should bake the build-dir copy output, not refuse; got: %v", err)
	}
	var bake *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].WriteFileOut == "include/llvm/Support/Extension.def" {
			bake = &pkg.Targets[i]
			break
		}
	}
	if bake == nil {
		t.Fatalf("expected a write_file baking include/llvm/Support/Extension.def; targets: %+v", pkg.Targets)
	}
	got := joinLines(bake.WriteFileContent)
	if got != defContent {
		t.Errorf("baked content = %q, want %q", got, defContent)
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
