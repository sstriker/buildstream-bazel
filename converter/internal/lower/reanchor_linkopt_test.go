package lower

import "testing"

// TestRewriteGenruleCmd pins the survey-2026-05-28 follow-on
// behaviour for genrule cmd strings: the cmake-Ninja-generator's
// `cd <abs-build> && ` prefix and any embedded source-tree / build-
// tree absolute path references get rewritten to a form Bazel's
// hermetic sandbox can resolve at action time.
func TestRewriteGenruleCmd(t *testing.T) {
	cmakeSrc := "/tmp/proj/src"
	buildDir := "/tmp/proj/build"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "passthrough — no abs paths",
			in:   "echo hello",
			want: "echo hello",
		},
		{
			name: "strip cd prefix targeting buildDir",
			in:   "cd /tmp/proj/build && /usr/bin/cmake -E touch foo",
			// /usr/bin/ host-prefix stripped + cmake -E touch
			// rewritten to POSIX touch (cmake_e_rewrite.go).
			want: "touch foo",
		},
		{
			name: "strip cd prefix targeting buildDir subdir",
			in:   "cd /tmp/proj/build/sub/dir && cmake -P x.cmake",
			want: "cmake -P x.cmake",
		},
		{
			name: "strip cd prefix targeting cmakeSrc subdir",
			in:   "cd /tmp/proj/src/sub && /usr/bin/foo bar",
			want: "foo bar",
		},
		{
			name: "leave cd prefix when target outside anchors",
			in:   "cd /other/place && do_thing",
			want: "cd /other/place && do_thing",
		},
		{
			name: "rewrite cmakeSrc-rooted path references",
			in:   "/usr/bin/cmake -P /tmp/proj/src/scripts/run.cmake -DIN=/tmp/proj/src/in.txt",
			want: "cmake -P scripts/run.cmake -DIN=in.txt",
		},
		{
			name: "rewrite buildDir-rooted path references",
			in:   "/usr/bin/cmake -E copy /tmp/proj/build/scripts/foo.out /tmp/proj/build/libfoo.sym",
			// /usr/bin/ host-prefix stripped + cmake -E copy
			// rewritten to POSIX cp (cmake_e_rewrite.go).
			want: "cp scripts/foo.out libfoo.sym",
		},
		{
			name: "combo — strip cd, rewrite both anchors",
			in:   "cd /tmp/proj/build && /tmp/proj/build/bin/vtkH5detect /tmp/proj/src/H5Tinit.c",
			want: "bin/vtkH5detect H5Tinit.c",
		},
		{
			name: "partial-match safety — buildDir vs buildDir_other",
			in:   "cd /tmp/proj/build_other && do_thing /tmp/proj/build_other/foo",
			want: "cd /tmp/proj/build_other && do_thing /tmp/proj/build_other/foo",
		},
		{
			name: "rewrite bare cmakeSrc at -D arg boundary",
			in:   "cmake -DLLVM_SOURCE_DIR=/tmp/proj/src -P script.cmake",
			want: "cmake -DLLVM_SOURCE_DIR=. -P script.cmake",
		},
		{
			name: "rewrite bare buildDir at -D arg boundary",
			in:   "cmake -DCMAKE_BINARY_DIR=/tmp/proj/build -P script.cmake",
			want: "cmake -DCMAKE_BINARY_DIR=. -P script.cmake",
		},
		{
			name: "partial-match safety — bare anchor not followed by boundary",
			in:   "do_thing /tmp/proj/src_other_thing",
			want: "do_thing /tmp/proj/src_other_thing",
		},
		{
			name: "strip /usr/bin/ prefix at cmd start",
			in:   "/usr/bin/cmake -E remove foo",
			// /usr/bin/ host-prefix stripped + cmake -E remove
			// rewritten to POSIX rm -f (cmake_e_rewrite.go).
			want: "rm -f foo",
		},
		{
			name: "strip /usr/local/bin/ prefix at cmd start",
			in:   "/usr/local/bin/python3 script.py",
			want: "python3 script.py",
		},
		{
			name: "strip host-bin after && separator",
			in:   "/usr/bin/cmake -E remove foo && /usr/bin/cmake -E copy a b",
			// /usr/bin/ host-prefix stripped + cmake -E remove/copy
			// rewritten to POSIX rm/cp (cmake_e_rewrite.go).
			want: "rm -f foo && cp a b",
		},
		{
			name: "preserve host-bin embedded inside argv arg",
			in:   "do_thing --option=/usr/bin/foo",
			want: "do_thing --option=/usr/bin/foo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteGenruleCmd(tc.in, cmakeSrc, buildDir)
			if got != tc.want {
				t.Errorf("rewriteGenruleCmd:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReanchorDefineValue pins the survey-2026-05-28 follow-on
// behaviour for preprocessor define values: convert-time absolute
// paths embedded in KEY="<path>" defines either get re-anchored
// to workspace-relative form (when under the cmake source root)
// or the define gets dropped entirely (when under the cmake build
// dir, since the referenced file is convert-time-only and won't
// reach Bazel's input closure).
//
// Canonical case: VTK's `vtkRenderingCore_AUTOINIT_INCLUDE="/abs/
// build/CMakeFiles/vtkModuleAutoInit_<hash>.h"` — cmake generates
// these auto-init headers per-module at configure time. Bazel
// sandbox-misses the file at action time; dropping the define is
// the table-stakes fix (an operator using VTK in earnest needs
// to wire AUTOINIT via a Bazel-aware mechanism anyway).
func TestReanchorDefineValue(t *testing.T) {
	cmakeSrc := "/tmp/proj/src"
	buildDir := "/tmp/proj/build"

	cases := []struct {
		name    string
		in      string
		wantDef string
		wantOk  bool
	}{
		{
			name:    "passthrough — no equals",
			in:      "DEBUG",
			wantDef: "DEBUG",
			wantOk:  true,
		},
		{
			name:    "passthrough — non-path value",
			in:      "FOO=1",
			wantDef: "FOO=1",
			wantOk:  true,
		},
		{
			name:    "passthrough — non-absolute path value",
			in:      `HEADER_PATH="some/header.h"`,
			wantDef: `HEADER_PATH="some/header.h"`,
			wantOk:  true,
		},
		{
			name:    "drop — value points at build dir (AUTOINIT_INCLUDE shape)",
			in:      `vtkRenderingCore_AUTOINIT_INCLUDE="/tmp/proj/build/CMakeFiles/vtkModuleAutoInit_abc123.h"`,
			wantDef: "",
			wantOk:  false,
		},
		{
			name:    "re-anchor — value points at source",
			in:      `CONFIG_HEADER="/tmp/proj/src/include/config.h"`,
			wantDef: `CONFIG_HEADER="include/config.h"`,
			wantOk:  true,
		},
		{
			name:    "passthrough — absolute path outside both anchors (operator's problem)",
			in:      `SYS_HEADER="/usr/include/foo.h"`,
			wantDef: `SYS_HEADER="/usr/include/foo.h"`,
			wantOk:  true,
		},
		{
			name:    "passthrough — empty value",
			in:      `KEY=`,
			wantDef: `KEY=`,
			wantOk:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDef, gotOk := reanchorDefineValue(tc.in, cmakeSrc, buildDir)
			if gotDef != tc.wantDef || gotOk != tc.wantOk {
				t.Errorf("reanchorDefineValue(%q) = (%q, %v); want (%q, %v)",
					tc.in, gotDef, gotOk, tc.wantDef, tc.wantOk)
			}
		})
	}
}

// TestReanchorLinkOptToken pins the survey-2026-05-28 follow-on
// behaviour: convert-time absolute paths embedded in tokenised
// linker flags either get re-anchored to workspace-relative form
// (when they're under the cmake source root) or dropped entirely
// (when they're under the cmake build dir, since the referenced
// file is convert-time-only and won't survive into Bazel's
// hermetic link action).
func TestReanchorLinkOptToken(t *testing.T) {
	cmakeSrc := "/tmp/proj/src"
	buildDir := "/tmp/proj/build"

	cases := []struct {
		name    string
		in      string
		wantTok string
		wantOk  bool
	}{
		{
			name:    "passthrough — no embedded path",
			in:      "-Wl,--gc-sections",
			wantTok: "-Wl,--gc-sections",
			wantOk:  true,
		},
		{
			name:    "passthrough — non-absolute embedded path",
			in:      `-Wl,--version-script,version.map`,
			wantTok: `-Wl,--version-script,version.map`,
			wantOk:  true,
		},
		{
			name:    "rpath-link to build dir — drop",
			in:      "-Wl,-rpath-link,/tmp/proj/build/Release/lib",
			wantTok: "",
			wantOk:  false,
		},
		{
			name:    "rpath to build dir — drop",
			in:      "-Wl,-rpath,/tmp/proj/build/lib",
			wantTok: "",
			wantOk:  false,
		},
		{
			name:    "rpath to source dir — keep (legitimate runtime metadata)",
			in:      "-Wl,-rpath,/tmp/proj/src/runtime",
			wantTok: "-Wl,-rpath,/tmp/proj/src/runtime",
			wantOk:  true,
		},
		{
			name:    "version-script under source root — re-anchor + requote",
			in:      `-Wl,--version-script,"/tmp/proj/src/zlib.map"`,
			wantTok: `-Wl,--version-script,"zlib.map"`,
			wantOk:  true,
		},
		{
			name:    "version-script under source root, unquoted — re-anchor + add quotes",
			in:      `-Wl,--version-script,/tmp/proj/src/sub/exports.txt`,
			wantTok: `-Wl,--version-script,"sub/exports.txt"`,
			wantOk:  true,
		},
		{
			name:    "version-script under build dir — drop",
			in:      `-Wl,--version-script,"/tmp/proj/build/foo.exports"`,
			wantTok: "",
			wantOk:  false,
		},
		{
			name:    "retain-symbols-file under source — re-anchor",
			in:      `-Wl,--retain-symbols-file,"/tmp/proj/src/syms.txt"`,
			wantTok: `-Wl,--retain-symbols-file,"syms.txt"`,
			wantOk:  true,
		},
		{
			name:    "dynamic-list under source — re-anchor",
			in:      `-Wl,--dynamic-list,/tmp/proj/src/dyn.list`,
			wantTok: `-Wl,--dynamic-list,"dyn.list"`,
			wantOk:  true,
		},
		{
			name:    "empty token — passthrough",
			in:      "",
			wantTok: "",
			wantOk:  true,
		},
		{
			name:    "version-script single-quoted under build dir — drop (libpng shape)",
			in:      `-Wl,--version-script,'/tmp/proj/build/libpng.vers'`,
			wantTok: "",
			wantOk:  false,
		},
		{
			name:    "version-script single-quoted under source — re-anchor + requote with double",
			in:      `-Wl,--version-script,'/tmp/proj/src/syms.map'`,
			wantTok: `-Wl,--version-script,"syms.map"`,
			wantOk:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTok, gotOk := reanchorLinkOptToken(tc.in, cmakeSrc, buildDir)
			if gotTok != tc.wantTok || gotOk != tc.wantOk {
				t.Errorf("reanchorLinkOptToken(%q) = (%q, %v); want (%q, %v)",
					tc.in, gotTok, gotOk, tc.wantTok, tc.wantOk)
			}
		})
	}
}
