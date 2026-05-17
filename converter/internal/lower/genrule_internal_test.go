package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

func TestUsesCmakeScriptMode(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{
			name: "bare /usr/bin/cmake -P",
			cmd:  "/usr/bin/cmake -P /src/scripts/gen.cmake /build/x.h",
			want: true,
		},
		{
			name: "${CMAKE_COMMAND} -P",
			cmd:  "${CMAKE_COMMAND} -P /src/scripts/gen.cmake",
			want: true,
		},
		{
			name: "cd prefix + /usr/bin/cmake -P",
			cmd:  "cd /build && /usr/bin/cmake -P /src/scripts/gen.cmake",
			want: true,
		},
		{
			name: "cd prefix + cmake -D... -P (libpng pnglibconf shape)",
			cmd:  "cd /build && /usr/bin/cmake -DOUTPUT=pnglibconf.h -P /build/scripts/gensrc.cmake",
			want: true,
		},
		{
			name: "cmake -D + -P with bare cmake name",
			cmd:  "cmake -DFOO=bar -DBAZ=qux -P /tmp/build/script.cmake",
			want: true,
		},
		{
			name: "env wrapper + cmake -P",
			cmd:  "env SOURCE_DATE_EPOCH=0 /usr/bin/cmake -P /src/scripts/gen.cmake",
			want: true,
		},
		{
			name: "cmake -E (not script mode)",
			cmd:  "/usr/bin/cmake -E touch /build/marker",
			want: false,
		},
		{
			name: "non-cmake driver",
			cmd:  "/usr/bin/python3 scripts/gen.py /build/x.h",
			want: false,
		},
		{
			name: "cd-prefixed non-cmake driver",
			cmd:  "cd /build && /usr/bin/python3 scripts/gen.py",
			want: false,
		},
		{
			name: "cmake without -P",
			cmd:  "/usr/bin/cmake --build /build --target foo",
			want: false,
		},
		{
			name: "cmake with -P-like flag value (not the script-mode flag)",
			cmd:  "/usr/bin/cmake -DOPTION=-P /src/scripts/gen.cmake",
			want: false,
		},
		{
			// Locks that iteration past the `-P` token doesn't matter once
			// the script-mode signal is observed — additional flags after
			// the script path (cmake honours `--debug-output` as a
			// cache-affecting flag in script mode) must still trip the
			// refusal.
			name: "cmake -P with trailing flag",
			cmd:  "/usr/bin/cmake -P /build/scripts/gen.cmake --debug-output",
			want: true,
		},
		{
			name: "empty",
			cmd:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := usesCmakeScriptMode(tc.cmd)
			if got != tc.want {
				t.Errorf("usesCmakeScriptMode(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestGenruleNameForRelativizesAbsoluteBuildOutput(t *testing.T) {
	build := &ninja.Build{
		Outputs: []string{"/tmp/convert-element-build-1806770363/pkg/gen/output.cpp"},
	}

	got := genruleNameFor(build, "/tmp/convert-element-build-1806770363")
	want := "gen_pkg_gen_output_cpp"
	if got != want {
		t.Fatalf("genruleNameFor() = %q, want %q", got, want)
	}
}
