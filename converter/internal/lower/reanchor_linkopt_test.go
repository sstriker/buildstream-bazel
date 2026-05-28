package lower

import "testing"

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
