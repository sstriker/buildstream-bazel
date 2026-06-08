package lower

import "testing"

// TestResolveElementSourceRoot pins the --element-source-root override
// validation (and the forced label-root resolution it drives): the value must
// be an absolute path that is an ancestor of (or equal to) the cmake source
// root. This exercises the FORCED override path independent of the
// workspace-root escape auto-detection.
func TestResolveElementSourceRoot(t *testing.T) {
	cases := []struct {
		name      string
		esr       string
		cmakeSrc  string
		want      string
		wantError bool
	}{
		{
			name:     "ancestor of cmakeSrc (cuda-samples shape: repo over sample)",
			esr:      "/repo",
			cmakeSrc: "/repo/cpp/0_Introduction/vectorAdd",
			want:     "/repo",
		},
		{
			name:     "equal to cmakeSrc",
			esr:      "/repo/src",
			cmakeSrc: "/repo/src",
			want:     "/repo/src",
		},
		{
			name:     "cleanable but still an ancestor",
			esr:      "/repo/build/../",
			cmakeSrc: "/repo/sub",
			want:     "/repo",
		},
		{
			name:     "dir element merely starting with .. is not an escape",
			esr:      "/repo",
			cmakeSrc: "/repo/..weird/sub",
			want:     "/repo",
		},
		{
			name:      "relative path is rejected",
			esr:       "repo/sub",
			cmakeSrc:  "/repo/sub",
			wantError: true,
		},
		{
			name:      "non-ancestor (sibling) is rejected",
			esr:       "/repo/other",
			cmakeSrc:  "/repo/sample",
			wantError: true,
		},
		{
			name:      "descendant of cmakeSrc is rejected (wrong direction)",
			esr:       "/repo/sample/sub",
			cmakeSrc:  "/repo/sample",
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveElementSourceRoot(tc.esr, tc.cmakeSrc)
			if tc.wantError {
				if err == nil {
					t.Fatalf("resolveElementSourceRoot(%q,%q) = %q, nil; want error", tc.esr, tc.cmakeSrc, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveElementSourceRoot(%q,%q) unexpected error: %v", tc.esr, tc.cmakeSrc, err)
			}
			if got != tc.want {
				t.Errorf("resolveElementSourceRoot(%q,%q) = %q; want %q", tc.esr, tc.cmakeSrc, got, tc.want)
			}
		})
	}
}
