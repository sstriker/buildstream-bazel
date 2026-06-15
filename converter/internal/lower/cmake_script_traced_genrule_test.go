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

func TestArgvWritesToDir(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	anc := execAnchors{hostSrcDir: hostSrc, recordedSrcDir: hostSrc, hostBuildDir: hostBuild, recordedBuildDir: hostBuild}
	// A --out-dir= pointing at the build root matches dir ".".
	if !argvWritesToDir([]string{"sh", "gen.sh", "--out-dir=" + hostBuild, filepath.Join(hostSrc, "in.def")}, ".", anc) {
		t.Errorf("expected build-root output dir to match via --out-dir=")
	}
	// A bare positional build-root dir counts too.
	if !argvWritesToDir([]string{"mygen", hostBuild, filepath.Join(hostSrc, "in.def")}, ".", anc) {
		t.Errorf("expected build-root output dir to match via positional dir")
	}
	// A build SUBDIR output (--out-dir=<build>/gen) matches dir "gen" (the widening).
	if !argvWritesToDir([]string{"sh", "gen.sh", "--out-dir=" + filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.def")}, "gen", anc) {
		t.Errorf("expected build-subdir output dir to match via --out-dir=")
	}
	// The subdir argv does NOT match the build root.
	if argvWritesToDir([]string{"sh", "gen.sh", "--out-dir=" + filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.def")}, ".", anc) {
		t.Errorf("a subdir output dir should not match the build root")
	}
	// No build-dir operand (only source inputs) → no match.
	if argvWritesToDir([]string{"mygen", filepath.Join(hostSrc, "in.def")}, ".", anc) {
		t.Errorf("source-only argv should not match a build output dir")
	}
}

// anchorGenruleOutputDirFlags is the SHARED bit the reuse refactor adds so a
// flag-derived output dir (protoc --cpp_out=DIR, `gen --out-dir=DIR`) anchors to
// $(RULEDIR) on both the ordinary custom-command path and the cmake -P script
// path. It fires only when the flag NAME is a known output-dir spelling AND its
// value EXACTLY equals a declared output's parent dir — a coincidental
// non-output `=.`/`=sub` (a `-DSOMEDIR=.` define) is left alone.
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
			// The reviewer's concern: a coincidental non-output `=.` (a -D define)
			// shares the build-root parent dir but is NOT an output-dir flag, so it
			// must be left alone — only the real output-dir flag anchors.
			name: "non-output flag with matching value left alone",
			cmd:  "tool -DSOMEDIR=. --out-dir=. in",
			outs: []string{"out.h"},
			want: "tool -DSOMEDIR=. --out-dir=$(RULEDIR) in",
		},
		{
			name: "protoc *_out family recognized",
			cmd:  "protoc --grpc_out=. foo.proto",
			outs: []string{"foo.grpc.pb.cc"},
			want: "protoc --grpc_out=$(RULEDIR) foo.proto",
		},
		{
			name: "prefix value not mauled",
			cmd:  "tool --out-dir=./nested in",
			outs: []string{"out.h"},
			want: "tool --out-dir=./nested in", // value "./nested" != "."
		},
		{
			name: "output-dir flag pointing elsewhere left alone",
			cmd:  "tool --out-dir=other in",
			outs: []string{"out.h"},
			want: "tool --out-dir=other in", // value "other" != "." (the out's parent)
		},
		{
			name: "no =-form output-dir operand — no-op",
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
