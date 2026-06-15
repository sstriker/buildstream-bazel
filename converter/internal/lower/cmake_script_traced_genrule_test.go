package lower

import (
	"path/filepath"
	"testing"
)

func TestArgvFlagValue(t *testing.T) {
	cases := map[string]string{
		"--out-dir=/b/gen": "/b/gen",
		"-o=/x":            "/x",
		"/plain/path":      "/plain/path",
		"-I":               "-I",
		"--flag":           "--flag",
	}
	for in, want := range cases {
		if got := argvFlagValue(in); got != want {
			t.Errorf("argvFlagValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArgvOutputAnchorsBuildRoot(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	anc := execAnchors{hostSrcDir: hostSrc, recordedSrcDir: hostSrc, hostBuildDir: hostBuild, recordedBuildDir: hostBuild}
	// A --out-dir= pointing at the build root is the writes-to-build-root signal.
	if !argvOutputAnchorsBuildRoot([]string{"sh", "gen.sh", "--out-dir=" + hostBuild, filepath.Join(hostSrc, "in.def")}, anc) {
		t.Errorf("expected build-root output to be detected via --out-dir=")
	}
	// A bare positional build-root dir counts too.
	if !argvOutputAnchorsBuildRoot([]string{"mygen", hostBuild, filepath.Join(hostSrc, "in.def")}, anc) {
		t.Errorf("expected build-root output to be detected via positional dir")
	}
	// No build-root operand (only source inputs) → not detected.
	if argvOutputAnchorsBuildRoot([]string{"mygen", filepath.Join(hostSrc, "in.def")}, anc) {
		t.Errorf("source-only argv should not anchor a build-root output")
	}
}

// anchorGenruleOutputDirFlags is the SHARED bit the reuse refactor adds so a
// flag-derived output dir (protoc --cpp_out=DIR, `gen --out-dir=DIR`) anchors to
// $(RULEDIR) on both the ordinary custom-command path and the cmake -P script
// path. It must fire only on an EXACT match against a declared output's parent
// dir, and leave unrelated `--flag=.`/`--flag=sub` arguments alone.
func TestAnchorGenruleOutputDirFlags(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		outs []string
		want string
	}{
		{
			name: "build-root output dir",
			cmd:  "sh gen.sh --out-dir=. greeting.def",
			outs: []string{"greeting.cpp"},
			want: "sh gen.sh --out-dir=$(RULEDIR) greeting.def",
		},
		{
			name: "subdir output dir",
			cmd:  "protoc --cpp_out=sub -I . sub/foo.proto",
			outs: []string{"sub/foo.pb.cc", "sub/foo.pb.h"},
			want: "protoc --cpp_out=$(RULEDIR)/sub -I . sub/foo.proto",
		},
		{
			name: "exact match only — unrelated --flag=. untouched",
			cmd:  "tool --keep=. --out-dir=. in",
			outs: []string{"out.h"},
			// Both `--keep=.` and `--out-dir=.` have value "." == the out's parent,
			// so both anchor; the point of the guard is the token boundary, not
			// semantic flag knowledge. A value that is NOT an out dir is untouched:
			want: "tool --keep=$(RULEDIR) --out-dir=$(RULEDIR) in",
		},
		{
			name: "prefix value not mauled",
			cmd:  "tool --out-dir=./nested in",
			outs: []string{"out.h"},
			want: "tool --out-dir=./nested in", // value "./nested" != "."
		},
		{
			name: "no output-dir operand — no-op",
			cmd:  "tool -o out.h in.x",
			outs: []string{"out.h"},
			want: "tool -o out.h in.x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anchorGenruleOutputDirFlags(tt.cmd, tt.outs); got != tt.want {
				t.Errorf("anchorGenruleOutputDirFlags(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}
