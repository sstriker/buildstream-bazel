package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestCmakeRelocateSingle pins the single-file `cmake -E copy|copy_if_different|
// rename <src> <dst>` detection used by the temp-dir-relocate recovery.
func TestCmakeRelocateSingle(t *testing.T) {
	call := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}
	cases := []struct {
		name             string
		c                shadow.ExecuteProcessCall
		wantSrc, wantDst string
		wantOK           bool
	}{
		{"copy", call("cmake", "-E", "copy", "/tmp/x/v.c", "/b/gen/v.c"), "/tmp/x/v.c", "/b/gen/v.c", true},
		{"copy_if_different", call("cmake", "-E", "copy_if_different", "/tmp/x/v.c", "/b/gen/v.c"), "/tmp/x/v.c", "/b/gen/v.c", true},
		{"rename", call("cmake", "-E", "rename", "/tmp/x/v.c", "/b/gen/v.c"), "/tmp/x/v.c", "/b/gen/v.c", true},
		{"unsubstituted cmake", call("${CMAKE_COMMAND}", "-E", "rename", "a", "b"), "a", "b", true},
		{"copy_directory not matched", call("cmake", "-E", "copy_directory", "a", "b"), "", "", false},
		{"multi-source dir form not matched", call("cmake", "-E", "copy", "a", "b", "destdir"), "", "", false},
		{"not cmake", call("cp", "-E", "copy", "a", "b"), "", "", false},
		{"not a tool call", call("cmake", "-E", "make_directory", "d"), "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst, ok := cmakeRelocateSingle(tc.c)
			if ok != tc.wantOK || src != tc.wantSrc || dst != tc.wantDst {
				t.Errorf("cmakeECopySingle = (%q, %q, %v), want (%q, %q, %v)", src, dst, ok, tc.wantSrc, tc.wantDst, tc.wantOK)
			}
		})
	}
}

// TestArgvStructurallyLiftableInWrapper pins that the wrapper-scoped eligibility
// DROPS the WorkingDirectory / Environment gates (temp-dir + cmake -E env are
// wrapper artifacts) while keeping the live-consumer gates (stderr capture,
// output-file, timeout).
func TestArgvStructurallyLiftableInWrapper(t *testing.T) {
	base := shadow.ExecuteProcessCall{Commands: [][]string{{"tool", "arg"}}}
	if !argvStructurallyLiftableInWrapper(base) {
		t.Error("a plain single-command call should be liftable")
	}
	// WorkingDirectory / Environment no longer block.
	wd := base
	wd.WorkingDirectory = "/tmp/x"
	if !argvStructurallyLiftableInWrapper(wd) {
		t.Error("WorkingDirectory must NOT block in the wrapper scope")
	}
	env := base
	env.Environment = []string{"K=V"}
	if !argvStructurallyLiftableInWrapper(env) {
		t.Error("Environment must NOT block in the wrapper scope")
	}
	// Live-consumer gates still block.
	for _, mut := range []func(*shadow.ExecuteProcessCall){
		func(c *shadow.ExecuteProcessCall) { c.OutputFile = "o" },
		func(c *shadow.ExecuteProcessCall) { c.ErrorFile = "e" },
		func(c *shadow.ExecuteProcessCall) { c.ErrorVariable = "v" },
		func(c *shadow.ExecuteProcessCall) { c.Timeout = "5" },
		func(c *shadow.ExecuteProcessCall) { c.InputFile = "i" },
	} {
		c := base
		mut(&c)
		if argvStructurallyLiftableInWrapper(c) {
			t.Errorf("a live-consumer field must still block; got liftable for %+v", c)
		}
	}
	// Empty / no-arg calls are not liftable.
	if argvStructurallyLiftableInWrapper(shadow.ExecuteProcessCall{Commands: [][]string{{"tool"}}}) {
		t.Error("a no-arg command should not be liftable")
	}
}
