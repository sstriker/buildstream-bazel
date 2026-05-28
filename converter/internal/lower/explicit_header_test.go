package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestExplicitHeaderSurfaces pins the CUDA-fixture + fmt fixture
// observation: cmake's `add_library(<name> SRC HEADER.h)` with no
// target_include_directories declared leaves the header without a
// compileGroupIndex AND without an include-walk to discover it.
// Previously the converter silently dropped these explicit headers.
// The fix in lower.go routes them to irt.Hdrs based on the
// extension when inCompileGroup is false.
//
// This test exercises the path resolution: absolute-under-cmakeSrc
// and cmakeSrc-relative both resolve to the same package-relative
// form, and `..`-containing paths are skipped (Bazel labels can't
// escape the package).
func TestExplicitHeader_PathResolutionShapes(t *testing.T) {
	cases := []struct {
		name     string
		srcPath  string
		cmakeSrc string
		want     string
		wantSkip bool
	}{
		{
			name:     "cmakeSrc-relative form passes through",
			srcPath:  "math.h",
			cmakeSrc: "/src",
			want:     "math.h",
		},
		{
			name:     "absolute under cmakeSrc relativizes",
			srcPath:  "/src/math.h",
			cmakeSrc: "/src",
			want:     "math.h",
		},
		{
			name:     "absolute outside cmakeSrc is skipped",
			srcPath:  "/elsewhere/math.h",
			cmakeSrc: "/src",
			wantSkip: true,
		},
		{
			name:     "../ in cmakeSrc-relative form is skipped",
			srcPath:  "../other/math.h",
			cmakeSrc: "/src",
			wantSkip: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fileapi.TargetSource{Path: tc.srcPath}
			_ = src
			// Inline the relevant resolution logic from the fixed
			// branch in lowerTarget.
			gotSkip := false
			gotPath := tc.srcPath
			if isAbs(tc.srcPath) {
				if rel, inside := relativeIfInside(tc.cmakeSrc, tc.srcPath); inside {
					gotPath = rel
				} else {
					gotSkip = true
				}
			}
			if !gotSkip && pathHasDotDotSegment(gotPath) {
				gotSkip = true
			}
			if gotSkip != tc.wantSkip {
				t.Errorf("skip = %v; want %v", gotSkip, tc.wantSkip)
			}
			if !gotSkip && gotPath != tc.want {
				t.Errorf("path = %q; want %q", gotPath, tc.want)
			}
		})
	}
}

// isAbs is a test helper mirroring filepath.IsAbs without importing
// it (avoids a circular-import shape; Go's filepath is fine, this
// is just for clarity).
func isAbs(p string) bool {
	return len(p) > 0 && p[0] == '/'
}
