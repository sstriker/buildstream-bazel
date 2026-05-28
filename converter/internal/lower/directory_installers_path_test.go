package lower

import "testing"

// TestProjectToSourceRoot_TopLevelRelative pins the fix for the
// double-prefix bug surfaced by gtest + LLVM survey: cmake records
// installer paths as top-level-source-relative, not per-directory
// source-relative. Joining with dirSrc double-prefixes the path
// when the install() lives in a subdir CMakeLists.
func TestProjectToSourceRoot_TopLevelRelative(t *testing.T) {
	cases := []struct {
		name     string
		p        string
		dirSrc   string
		cmakeSrc string
		want     string
	}{
		{
			name:     "gtest dir installer — relative path is top-level",
			p:        "googletest/include",
			dirSrc:   "/src/googletest",
			cmakeSrc: "/src",
			want:     "googletest/include",
		},
		{
			name:     "fmt-shape — top-level CMakeLists path resolves the same",
			p:        "include/fmt/args.h",
			dirSrc:   "/src/.",
			cmakeSrc: "/src",
			want:     "include/fmt/args.h",
		},
		{
			name:     "LLVM tools subdir — path is top-level-relative not subdir",
			p:        "include/llvm-c/lto.h",
			dirSrc:   "/src/tools/lto",
			cmakeSrc: "/src",
			want:     "include/llvm-c/lto.h",
		},
		{
			name:     "absolute path inside cmakeSrc",
			p:        "/src/include/foo.h",
			dirSrc:   "/src/.",
			cmakeSrc: "/src",
			want:     "include/foo.h",
		},
		{
			name:     "path outside cmakeSrc returns empty",
			p:        "/elsewhere/foo.h",
			dirSrc:   "/src/.",
			cmakeSrc: "/src",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectToSourceRoot(tc.p, tc.dirSrc, tc.cmakeSrc)
			if got != tc.want {
				t.Errorf("projectToSourceRoot(p=%q, dirSrc=%q, cmakeSrc=%q) = %q; want %q",
					tc.p, tc.dirSrc, tc.cmakeSrc, got, tc.want)
			}
		})
	}
}
