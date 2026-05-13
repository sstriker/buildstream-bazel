package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildCCIndex_HelloWorld covers the basic walk: one
// cc_library with hdrs + .h-in-srcs, one cc_binary, all
// recorded with their package-relative header paths.
func TestBuildCCIndex_HelloWorld(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "MODULE.bazel", "")
	writeFile(t, root, "elements/hello/BUILD.bazel", `
package(default_visibility = ["//visibility:public"])

cc_library(
    name = "hello",
    srcs = [
        "hello.c",
        "include/hello/private.h",
    ],
    hdrs = ["include/hello.h"],
)

cc_binary(
    name = "hello_bin",
    srcs = ["main.c"],
    deps = [":hello"],
)
`)
	a := args{
		root:         root,
		outCCIndex:   "tools/cc_index.json",
		outPyModules: "tools/python_modules.json",
	}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	cc := readJSON(t, filepath.Join(root, "tools/cc_index.json"))
	for path, want := range map[string]string{
		"elements/hello/include/hello.h":         "//elements/hello",
		"elements/hello/include/hello/private.h": "//elements/hello",
	} {
		if got, ok := cc[path]; !ok {
			t.Errorf("cc_index.json missing %q", path)
		} else if got != want {
			t.Errorf("cc_index.json[%q] = %q, want %q", path, got, want)
		}
	}
	// hello.c (a .c source) must NOT enter the cc index —
	// only headers do.
	if _, ok := cc["elements/hello/hello.c"]; ok {
		t.Errorf("cc_index.json unexpectedly contains hello.c")
	}
}

// TestBuildCCIndex_LabelShortening covers the `//pkg:pkg` →
// `//pkg` canonicalization for libraries whose target name
// matches the package basename — matches the Phase 3
// buildifier-canonical shortening the cc emitter uses.
func TestBuildCCIndex_LabelShortening(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "MODULE.bazel", "")
	// Target name matches package basename: short form.
	writeFile(t, root, "elements/hello/BUILD.bazel", `
cc_library(name = "hello", hdrs = ["a.h"])
`)
	// Target name doesn't match: long form preserved.
	writeFile(t, root, "elements/other/BUILD.bazel", `
cc_library(name = "internal", hdrs = ["b.h"])
`)
	a := args{
		root:         root,
		outCCIndex:   "tools/cc_index.json",
		outPyModules: "tools/python_modules.json",
	}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	cc := readJSON(t, filepath.Join(root, "tools/cc_index.json"))
	if got := cc["elements/hello/a.h"]; got != "//elements/hello" {
		t.Errorf("short-form label wrong: %q", got)
	}
	if got := cc["elements/other/b.h"]; got != "//elements/other:internal" {
		t.Errorf("long-form label wrong: %q", got)
	}
}

// TestBuildCCIndex_PyModules covers the py side: py_binary
// + py_library names land in python_modules.json mapped to
// their full labels.
func TestBuildCCIndex_PyModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "MODULE.bazel", "")
	writeFile(t, root, "elements/demo/BUILD.bazel", `
py_library(name = "demo", srcs = ["demo/__init__.py"])
py_binary(name = "greet", srcs = ["demo/cli.py"], main = "demo/cli.py", deps = [":demo"])
`)
	a := args{
		root:         root,
		outCCIndex:   "tools/cc_index.json",
		outPyModules: "tools/python_modules.json",
	}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	py := readJSON(t, filepath.Join(root, "tools/python_modules.json"))
	if got := py["demo"]; got != "//elements/demo" {
		t.Errorf("python_modules.json[demo] = %q, want //elements/demo", got)
	}
	if got := py["greet"]; got != "//elements/demo:greet" {
		t.Errorf("python_modules.json[greet] = %q, want //elements/demo:greet", got)
	}
}

// TestBuildCCIndex_SkipsUnparseableBuildFiles verifies the
// walk doesn't abort on a malformed BUILD — operator-hand-
// edited files may contain experimental shapes the parser
// rejects.
func TestBuildCCIndex_SkipsUnparseableBuildFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "elements/broken/BUILD.bazel", "cc_library(name = \"x\", srcs = ["+"\n") // syntax error
	writeFile(t, root, "elements/ok/BUILD.bazel", `cc_library(name = "ok", hdrs = ["ok.h"])`)
	a := args{
		root:         root,
		outCCIndex:   "tools/cc_index.json",
		outPyModules: "tools/python_modules.json",
	}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	cc := readJSON(t, filepath.Join(root, "tools/cc_index.json"))
	if got := cc["elements/ok/ok.h"]; got != "//elements/ok" {
		t.Errorf("ok/ok.h: %q", got)
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, p string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return out
}
