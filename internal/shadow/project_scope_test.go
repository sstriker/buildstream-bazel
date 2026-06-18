package shadow

import "testing"

func TestInProjectScope(t *testing.T) {
	const src, build = "/proj/src", "/proj/build"
	cases := []struct {
		name, file, sourceRoot, buildRoot string
		want                              bool
	}{
		{"source-tree", "/proj/src/CMakeLists.txt", src, build, true},
		{"build-tree recipe", "/proj/build/recipe.cmake", src, build, true},
		{"build-tree CMakeFiles scratch", "/proj/build/CMakeFiles/CMakeScratch/x.cmake", src, build, false},
		{"prefix module", "/usr/share/cmake-3.28/Modules/m.cmake", src, build, false},
		{"unrelated", "/elsewhere/x.cmake", src, build, false},
		// buildRoot == "" => source-tree-only fallback (prior behavior).
		{"no buildRoot, build file", "/proj/build/recipe.cmake", src, "", false},
		{"no buildRoot, source file", "/proj/src/a.cmake", src, "", true},
	}
	for _, tc := range cases {
		if got := inProjectScope(tc.file, tc.sourceRoot, tc.buildRoot); got != tc.want {
			t.Errorf("%s: inProjectScope(%q,%q,%q) = %v, want %v", tc.name, tc.file, tc.sourceRoot, tc.buildRoot, got, tc.want)
		}
	}
}

// TestDecodeWithBuild_AcceptsBuildTreeOutputForms: configure_file / file(GENERATE)
// / add_custom_command(OUTPUT) issued from a build-tree recipe .cmake are recovered
// under DecodeWithBuild (build root set) but dropped by plain Decode (the prior
// source-tree-only gate) — the consistency-audit gap. A prefix-module call and a
// build-tree CMakeFiles-scratch call stay dropped either way.
func TestDecodeWithBuild_AcceptsBuildTreeOutputForms(t *testing.T) {
	const src, build = "/proj/src", "/proj/build"
	trace := `{"args":["/proj/src/in.h.in","/proj/build/out.h"],"cmd":"configure_file","file":"/proj/build/recipe.cmake","line":1}
{"args":["GENERATE","OUTPUT","/proj/build/gen.h","INPUT","/proj/src/gen.h.in"],"cmd":"file","file":"/proj/build/recipe.cmake","line":2}
{"args":["OUTPUT","/proj/build/gen.c","COMMAND","tool","--out","/proj/build/gen.c"],"cmd":"add_custom_command","file":"/proj/build/recipe.cmake","line":3}
{"args":["/proj/src/p.in","/proj/build/p.out"],"cmd":"configure_file","file":"/usr/share/cmake-4.3/Modules/Pkg.cmake","line":4}
{"args":["/proj/src/s.in","/proj/build/s.out"],"cmd":"configure_file","file":"/proj/build/CMakeFiles/CMakeScratch/try.cmake","line":5}
`
	// Build root set: the three build-tree-recipe forms are recovered.
	d := DecodeWithBuild([]byte(trace), src, build, nil)
	if len(d.ConfigFiles) != 1 || d.ConfigFiles[0].Output != "/proj/build/out.h" {
		t.Errorf("configure_file from build-tree recipe not recovered: %+v", d.ConfigFiles)
	}
	if len(d.FileGenerates) != 1 {
		t.Errorf("file(GENERATE) from build-tree recipe not recovered: %+v", d.FileGenerates)
	}
	if len(d.AddCustomCommands) != 1 {
		t.Errorf("add_custom_command from build-tree recipe not recovered: %+v", d.AddCustomCommands)
	}

	// No build root: prior behavior — all of these (build-tree + prefix + scratch)
	// are out of the source tree and dropped.
	d0 := Decode([]byte(trace), src, nil)
	if len(d0.ConfigFiles) != 0 || len(d0.FileGenerates) != 0 || len(d0.AddCustomCommands) != 0 {
		t.Errorf("plain Decode should drop all non-source-tree forms; got cfg=%v gen=%v acc=%v",
			d0.ConfigFiles, d0.FileGenerates, d0.AddCustomCommands)
	}
}
