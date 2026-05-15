package lower

import "testing"

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
