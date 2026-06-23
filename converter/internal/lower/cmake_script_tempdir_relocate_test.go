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

// TestCmakeRelocateMulti pins the multi-source `cmake -E copy <s1> <s2> …
// <destdir>` expansion: one (src, destdir/basename(src)) per source; the
// 2-operand form and `rename` (no multi form) decline.
func TestCmakeRelocateMulti(t *testing.T) {
	call := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}
	got, ok := cmakeRelocateMulti(call("cmake", "-E", "copy", "/tmp/x/a.c", "/tmp/x/b.c", "/b/gen"))
	if !ok {
		t.Fatal("expected the multi-source copy form to match")
	}
	want := []scriptRelocation{{src: "/tmp/x/a.c", dst: "/b/gen/a.c"}, {src: "/tmp/x/b.c", dst: "/b/gen/b.c"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("cmakeRelocateMulti = %+v, want %+v", got, want)
	}
	// copy_if_different multi form matches too.
	if _, ok := cmakeRelocateMulti(call("cmake", "-E", "copy_if_different", "/a", "/b", "/destdir")); !ok {
		t.Error("copy_if_different multi-source form should match")
	}
	// The 2-operand form is cmakeRelocateSingle's; multi declines.
	if _, ok := cmakeRelocateMulti(call("cmake", "-E", "copy", "/a", "/b")); ok {
		t.Error("2-operand copy must NOT match the multi-source form")
	}
	// rename has no multi-source form.
	if _, ok := cmakeRelocateMulti(call("cmake", "-E", "rename", "/a", "/b", "/destdir")); ok {
		t.Error("rename has no multi-source form")
	}
}

// TestCmakeCopyDirectoryOperands pins `cmake -E copy_directory[_if_different]
// <src> <dst>` detection and that copy / multi-source forms decline.
func TestCmakeCopyDirectoryOperands(t *testing.T) {
	call := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}
	for _, op := range []string{"copy_directory", "copy_directory_if_different"} {
		src, dst, ok := cmakeCopyDirectoryOperands(call("cmake", "-E", op, "/tmp/x", "/b/gen"))
		if !ok || src != "/tmp/x" || dst != "/b/gen" {
			t.Errorf("copy_directory(%s) = (%q, %q, %v), want (/tmp/x, /b/gen, true)", op, src, dst, ok)
		}
	}
	if _, _, ok := cmakeCopyDirectoryOperands(call("cmake", "-E", "copy", "/a", "/b")); ok {
		t.Error("plain copy must NOT match copy_directory")
	}
}

// TestSlashChildRel pins the below-parent relative-path helper used by the
// copy_directory expansion.
func TestSlashChildRel(t *testing.T) {
	cases := []struct {
		child, parent, wantRel string
		wantOK                 bool
	}{
		{"gen/a.c", "gen", "a.c", true},
		{"gen/sub/a.c", "gen", "sub/a.c", true},
		{"a.c", ".", "a.c", true},
		{"a.c", "", "a.c", true},
		{"gen", "gen", "", false},       // a dir is not its own file output
		{"genx/a.c", "gen", "", false},  // sibling-prefix, not below
		{"other/a.c", "gen", "", false}, // unrelated
	}
	for _, tc := range cases {
		rel, ok := slashChildRel(tc.child, tc.parent)
		if ok != tc.wantOK || rel != tc.wantRel {
			t.Errorf("slashChildRel(%q, %q) = (%q, %v), want (%q, %v)", tc.child, tc.parent, rel, ok, tc.wantRel, tc.wantOK)
		}
	}
}

// TestAddCopyDirRelocations pins the copy_directory → per-declared-output
// expansion: each declared output below the anchored dest dir maps to
// srcDir/<rel>; outputs outside it are left unclaimed.
func TestAddCopyDirRelocations(t *testing.T) {
	anc := execAnchors{hostBuildDir: "/b", recordedBuildDir: "/b"}
	declaredSet := map[string]bool{"gen/a.c": true, "gen/b.c": true, "other/c.c": true}
	relocate := map[string]string{}
	addCopyDirRelocations(relocate, "/tmp/x", "/b/gen", anc, declaredSet)
	want := map[string]string{"gen/a.c": "/tmp/x/a.c", "gen/b.c": "/tmp/x/b.c"}
	if len(relocate) != len(want) || relocate["gen/a.c"] != want["gen/a.c"] || relocate["gen/b.c"] != want["gen/b.c"] {
		t.Errorf("addCopyDirRelocations = %v, want %v (other/c.c must stay unclaimed)", relocate, want)
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
