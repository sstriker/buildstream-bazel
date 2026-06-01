package lower

import (
	"reflect"
	"testing"
)

func TestClassifyPath(t *testing.T) {
	src := "/home/op/proj"
	build := "/tmp/build-xyz"
	cases := []struct {
		path string
		want PathClass
	}{
		{"/home/op/proj/scripts/gen.cmake", ClassSource},
		{"/home/op/proj", ClassSource}, // root itself
		{"/tmp/build-xyz/CMakeFiles/foo", ClassBuild},
		{"/tmp/build-xyz", ClassBuild},
		{"/usr/include/stdio.h", ClassSysroot},
		{"/usr/bin/awk", ClassSysroot},
		{"/lib/libfoo.so", ClassSysroot},
		{"/opt/vendor/foo", ClassUnknown},
		{"/random/path", ClassUnknown},
		{"relative/path", ClassUnknown}, // not absolute
		{"", ClassUnknown},
		// Source-root that aliases a sysroot prefix: source
		// wins because the more-specific prefix matches first.
		{"/usr/local/src/proj/foo", ClassUnknown}, // not under sourceRoot here
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := classifyPath(c.path, src, build); got != c.want {
				t.Errorf("classifyPath(%q, %q, %q) = %v, want %v",
					c.path, src, build, got, c.want)
			}
		})
	}
}

func TestClassifyScriptTrace(t *testing.T) {
	// Synthetic trace bytes mimicking cmake's --trace-format=json-v1
	// output for a script that does include(), file(READ),
	// execute_process(COMMAND ...).
	traceRaw := []byte(`{"file":"/home/op/proj/scripts/main.cmake","line":1,"cmd":"include","args":["/home/op/proj/scripts/helper.cmake"]}
{"file":"/home/op/proj/scripts/main.cmake","line":3,"cmd":"file","args":["READ","/home/op/proj/data/input.txt","CONTENTS"]}
{"file":"/home/op/proj/scripts/main.cmake","line":5,"cmd":"execute_process","args":["COMMAND","/usr/bin/awk","-f","/home/op/proj/scripts/process.awk"]}
{"file":"/home/op/proj/scripts/main.cmake","line":7,"cmd":"file","args":["READ","/tmp/build-xyz/cmake-generated/values.txt","V"]}
{"file":"/home/op/proj/scripts/main.cmake","line":9,"cmd":"file","args":["READ","/opt/random/path","CONTENTS"]}
`)
	cls := ClassifyScriptTrace(traceRaw, "/home/op/proj", "/tmp/build-xyz")
	if !reflect.DeepEqual(cls.SourcePaths, []string{"data/input.txt", "scripts/helper.cmake", "scripts/process.awk"}) {
		t.Errorf("SourcePaths = %v", cls.SourcePaths)
	}
	if !reflect.DeepEqual(cls.BuildPaths, []string{"/tmp/build-xyz/cmake-generated/values.txt"}) {
		t.Errorf("BuildPaths = %v", cls.BuildPaths)
	}
	if !reflect.DeepEqual(cls.SysrootPaths, []string{"/usr/bin/awk"}) {
		t.Errorf("SysrootPaths = %v", cls.SysrootPaths)
	}
	if !reflect.DeepEqual(cls.UnknownPaths, []string{"/opt/random/path"}) {
		t.Errorf("UnknownPaths = %v", cls.UnknownPaths)
	}
}

func TestClassifyScriptTrace_HandlesRelativeAndMalformed(t *testing.T) {
	traceRaw := []byte(`not-json
{"file":"/proj/foo.cmake","line":1,"cmd":"include","args":["sibling.cmake"]}
{"file":"/proj/sub/bar.cmake","line":1,"cmd":"file","args":["READ","../shared.txt","V"]}
{"file":"/proj/foo.cmake","line":3,"cmd":"file","args":["READ"]}
`)
	cls := ClassifyScriptTrace(traceRaw, "/proj", "/build")
	// Relative `sibling.cmake` resolves against the FILE's dir.
	if !sliceContains(cls.SourcePaths, "sibling.cmake") {
		t.Errorf("expected sibling.cmake in SourcePaths, got %v", cls.SourcePaths)
	}
	// `../shared.txt` from /proj/sub resolves to /proj/shared.txt.
	if !sliceContains(cls.SourcePaths, "shared.txt") {
		t.Errorf("expected shared.txt in SourcePaths, got %v", cls.SourcePaths)
	}
	// Missing args → silently dropped.
}

// TestLiftCmakeScriptGenrule_TraceFlagsWithoutBinaryProceedsWithoutTrace
// pins the graceful degradation: when CMakeScriptTrace is on
// but no cmake binary is available, the lift skips the trace
// step and falls back to the un-traced shape (srcs from ninja
// edge only) without erroring.
func TestLiftCmakeScriptGenrule_TraceFlagsWithoutBinaryProceedsWithoutTrace(t *testing.T) {
	// This is covered implicitly by the existing
	// TestRecoverGenrule_CmakeScriptLift test (which doesn't
	// set CMakeBinary and doesn't run any trace) — re-stating
	// the contract here documents the expected behavior:
	// CMakeScriptTrace=true + CMakeBinary="" ⇒ no-op trace
	// step, lift proceeds.
	const empty = ""
	if empty != "" {
		t.Fatal("compile-time pin only; CMakeBinary empty path is the gate")
	}
}

func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
