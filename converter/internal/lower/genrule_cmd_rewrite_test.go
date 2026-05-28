package lower

import "testing"

func TestRewriteGenruleCmd(t *testing.T) {
	cmakeSrc := "/tmp/proj/src"
	buildDir := "/tmp/proj/build"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "passthrough — no anchored paths",
			in:   "echo hello",
			want: "echo hello",
		},
		{
			name: "strip cd targeting buildDir",
			in:   "cd /tmp/proj/build && echo hi",
			want: "echo hi",
		},
		{
			name: "strip cd targeting buildDir subdir",
			in:   "cd /tmp/proj/build/Remote && /usr/bin/cmake -E echo hi",
			want: "/usr/bin/cmake -E echo hi",
		},
		{
			name: "strip cd targeting cmakeSrc subdir",
			in:   "cd /tmp/proj/src/sub && something",
			want: "something",
		},
		{
			name: "leave cd when target outside both anchors",
			in:   "cd /other/dir && do",
			want: "cd /other/dir && do",
		},
		{
			name: "rewrite cmakeSrc-rooted refs",
			in:   "cmake -P /tmp/proj/src/scripts/run.cmake",
			want: "cmake -P scripts/run.cmake",
		},
		{
			name: "rewrite buildDir-rooted refs",
			in:   "echo /tmp/proj/build/out.txt",
			want: "echo out.txt",
		},
		{
			name: "combo — strip cd, rewrite both",
			in:   "cd /tmp/proj/build && /tmp/proj/build/bin/tool /tmp/proj/src/in.txt",
			want: "bin/tool in.txt",
		},
		{
			name: "partial-match safety — buildDir vs buildDir_other",
			in:   "echo /tmp/proj/build_other/file",
			want: "echo /tmp/proj/build_other/file",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "bare anchor at `=` boundary",
			in:   "/usr/bin/cmake -DCMAKE_BINARY_DIR=/tmp/proj/build -P script.cmake",
			want: "/usr/bin/cmake -DCMAKE_BINARY_DIR=. -P script.cmake",
		},
		{
			name: "bare anchor at `-S` flag boundary",
			in:   "/usr/bin/cmake --regenerate-during-build -S/tmp/proj/src -B/tmp/proj/build",
			want: "/usr/bin/cmake --regenerate-during-build -S. -B.",
		},
		{
			name: "bare anchor at trailing position",
			in:   "echo /tmp/proj/build",
			want: "echo .",
		},
		{
			name: "bare anchor inside longer token does not match",
			in:   "echo /tmp/proj/build_other",
			want: "echo /tmp/proj/build_other",
		},
		{
			name: "bare anchor followed by `;`",
			in:   "do /tmp/proj/build;rest",
			want: "do .;rest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteGenruleCmd(tc.in, cmakeSrc, buildDir); got != tc.want {
				t.Errorf("rewriteGenruleCmd:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
