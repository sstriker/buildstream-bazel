package lower

import "testing"

// TestDetectInPlaceOutputRenames covers the source-tree-input ==
// build-tree-output collision detection: an output whose source-tree
// form is also a source gets a non-shadowing rename; everything else is
// left alone (nil map).
func TestDetectInPlaceOutputRenames(t *testing.T) {
	tests := []struct {
		name     string
		outs     []string
		srcs     []string
		umbrella string
		want     map[string]string
	}{
		{
			name: "collision no umbrella",
			outs: []string{"version.txt"},
			srcs: []string{"version.txt"},
			want: map[string]string{"version.txt": "version.txt.gen"},
		},
		{
			name:     "collision under umbrella",
			outs:     []string{"lib/Remarks/Remarks.exports"},
			srcs:     []string{"llvm/lib/Remarks/Remarks.exports"},
			umbrella: "llvm",
			want:     map[string]string{"lib/Remarks/Remarks.exports": "lib/Remarks/Remarks.exports.gen"},
		},
		{
			name: "no collision — distinct output",
			outs: []string{"generated.txt"},
			srcs: []string{"version.txt"},
			want: nil,
		},
		{
			name: "no collision — output not among srcs",
			outs: []string{"a.h", "b.h"},
			srcs: []string{"c.h"},
			want: nil,
		},
		{
			name:     "umbrella mismatch is not a collision",
			outs:     []string{"lib/x.txt"},
			srcs:     []string{"lib/x.txt"}, // would need "llvm/lib/x.txt" to match
			umbrella: "llvm",
			want:     nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectInPlaceOutputRenames(tc.outs, tc.srcs, tc.umbrella)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("rename[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestRenameRawCmdBuildOutputs confirms the build-output occurrence is
// renamed (keeping its buildDir prefix, so the later strip yields the
// renamed token) while the source-tree input occurrence — sharing the
// same basename but a different absolute prefix — is untouched. This is
// the disambiguation that stops input and output collapsing to one token.
func TestRenameRawCmdBuildOutputs(t *testing.T) {
	cmd := "cmake -E copy /src/proj/version.txt /build/proj/version.txt"
	got := renameRawCmdBuildOutputs(cmd, "/build/proj", map[string]string{"version.txt": "version.txt.gen"})
	want := "cmake -E copy /src/proj/version.txt /build/proj/version.txt.gen"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestRenameRawCmdBuildOutputs_NoOp confirms an empty rename map / empty
// buildDir leaves the cmd byte-identical (the common, non-in-place path).
func TestRenameRawCmdBuildOutputs_NoOp(t *testing.T) {
	cmd := "cp /build/a.txt /build/b.txt"
	if got := renameRawCmdBuildOutputs(cmd, "/build", nil); got != cmd {
		t.Errorf("nil renames should be a no-op; got %q", got)
	}
	if got := renameRawCmdBuildOutputs(cmd, "", map[string]string{"a.txt": "a.txt.gen"}); got != cmd {
		t.Errorf("empty buildDir should be a no-op; got %q", got)
	}
}
