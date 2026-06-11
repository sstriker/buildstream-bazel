package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestReanchorIDirAfterCopts pins the token mapping: source-tree dirs
// re-anchor to exec-root joined form (both the joined and separate-pair
// spellings) and surface as walk dirs; system / out-of-tree / relative
// tokens pass through verbatim.
func TestReanchorIDirAfterCopts(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		pkgPath  string
		wantOut  []string
		wantWalk []string
	}{
		{
			"joined source-tree",
			[]string{"-O2", "-idirafter/src/vendor/khronos", "-Wall"},
			"elements/sdl",
			[]string{"-O2", "-idirafterelements/sdl/vendor/khronos", "-Wall"},
			[]string{"vendor/khronos"},
		},
		{
			"separate pair source-tree",
			[]string{"-idirafter", "/src/vendor/khronos"},
			"",
			[]string{"-idiraftervendor/khronos"},
			[]string{"vendor/khronos"},
		},
		{
			"system dir verbatim",
			[]string{"-idirafter/usr/lib/gcc/include-fixed"},
			"elements/sdl",
			[]string{"-idirafter/usr/lib/gcc/include-fixed"},
			nil,
		},
		{
			"relative verbatim",
			[]string{"-idirafter", "extra"},
			"",
			[]string{"-idirafter", "extra"},
			nil,
		},
		{
			"no idirafter untouched",
			[]string{"-O2", "-include", "x.h"},
			"",
			[]string{"-O2", "-include", "x.h"},
			nil,
		},
	}
	for _, tc := range cases {
		in := append([]string(nil), tc.in...)
		out, walk := reanchorIDirAfterCopts(in, "/src", nil, tc.pkgPath)
		if !reflect.DeepEqual(append([]string{}, out...), tc.wantOut) {
			t.Errorf("%s: out = %v, want %v", tc.name, out, tc.wantOut)
		}
		if !reflect.DeepEqual(walk, tc.wantWalk) {
			t.Errorf("%s: walk = %v, want %v", tc.name, walk, tc.wantWalk)
		}
	}
}

// TestReanchorIDirAfterCopts_UmbrellaReanchor confirms the promoted-root
// prefix applies (LLVM shape: labels rooted above cmakeSrc).
func TestReanchorIDirAfterCopts_UmbrellaReanchor(t *testing.T) {
	out, walk := reanchorIDirAfterCopts(
		[]string{"-idirafter/src/vendor"}, "/src",
		func(rel string) string { return "llvm/" + rel }, "")
	if !reflect.DeepEqual(out, []string{"-idirafterllvm/vendor"}) || !reflect.DeepEqual(walk, []string{"llvm/vendor"}) {
		t.Errorf("umbrella = (%v, %v)", out, walk)
	}
}

// TestLowerTarget_IDirAfter_StagesHeaders is the sdl-shape end-to-end
// check: a target whose compile fragments carry a joined absolute
// `-idirafter<src>/vendor/khronos` gets (a) the copt re-anchored to the
// element-relative exec-root form, and (b) the dir's headers DECLARED
// via the discoverHeaders walk — without (b) the copt sets a search
// path into files the sandbox never stages (the EGL/egl.h failure).
func TestLowerTarget_IDirAfter_StagesHeaders(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "vendor/khronos/EGL"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "vendor/khronos/EGL/egl.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "lib.c"), []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: src, Build: filepath.Join(src, "b")},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "thelib::@1", Name: "thelib"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": {
				Name: "thelib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "lib.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					CompileCommandFragments: []fileapi.CommandFragment{
						{Fragment: "-O2 -idirafter" + src + "/vendor/khronos"},
					},
				}},
			},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{HostSourceRoot: src, BuildDir: filepath.Join(src, "b")})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var lib *struct{ Copts, Hdrs []string }
	for _, tgt := range pkg.Targets {
		if tgt.Name == "thelib" {
			lib = &struct{ Copts, Hdrs []string }{tgt.Copts, tgt.Hdrs}
		}
	}
	if lib == nil {
		t.Fatal("thelib not lowered")
	}
	joined := strings.Join(lib.Copts, " ")
	if !strings.Contains(joined, "-idiraftervendor/khronos") {
		t.Errorf("copt not re-anchored to exec-root form: %v", lib.Copts)
	}
	if strings.Contains(joined, src) {
		t.Errorf("convert-time absolute path leaked into copts: %v", lib.Copts)
	}
	if !stringSliceContains(lib.Hdrs, "vendor/khronos/EGL/egl.h") {
		t.Errorf("-idirafter dir's headers not staged via the walk: %v", lib.Hdrs)
	}
}
