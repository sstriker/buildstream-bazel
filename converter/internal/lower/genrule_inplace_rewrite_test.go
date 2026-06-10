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
			// The contract is a nil map (not empty-non-nil) when there's
			// no collision — assert nilness explicitly so an empty map
			// can't silently satisfy the no-collision cases.
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want nil map (no collision), got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want %v, got nil", tc.want)
			}
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

// TestRenameInPlaceOutputsRawCmd covers stage 1 of the two-stage rename:
// on the RAW cmd the buildDir-prefixed output operand and the bare
// cwd-relative form are renamed, while the absolute SOURCE-dir operand —
// whose trailing component is the same token — is protected by the
// '/'-boundary guard. Without this stage, rewriteGenruleCmd collapses
// input and output to one token and the anchor pass turns the cmd into a
// self-copy of the renamed output.
func TestRenameInPlaceOutputsRawCmd(t *testing.T) {
	renames := map[string]string{"version.txt": "version.txt.gen"}
	for _, tc := range []struct {
		name, cmd, want string
	}{
		{
			name: "explicit-path copy (the fixture shape)",
			cmd:  "cd /b && cmake -E copy /s/version.txt /b/version.txt",
			want: "cd /b && cmake -E copy /s/version.txt /b/version.txt.gen",
		},
		{
			name: "bare cwd-relative output token",
			cmd:  "gen.py /s/version.txt > version.txt",
			want: "gen.py /s/version.txt > version.txt.gen",
		},
		{
			name: "relative source ref stays protected",
			cmd:  "tool ../src/version.txt version.txt",
			want: "tool ../src/version.txt version.txt.gen",
		},
	} {
		if got := renameInPlaceOutputsRawCmd(tc.cmd, renames, "/b"); got != tc.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestRenameAnchoredGenruleOutputs_Idempotent confirms stage 2 leaves an
// occurrence stage 1 already renamed alone (no `x.gen.gen`).
func TestRenameAnchoredGenruleOutputs_Idempotent(t *testing.T) {
	cmd := "cp version.txt $(RULEDIR)/version.txt.gen"
	got := renameAnchoredGenruleOutputs(cmd, map[string]string{"version.txt": "version.txt.gen"})
	if got != cmd {
		t.Errorf("already-renamed token re-suffixed:\ngot  %q\nwant %q", got, cmd)
	}
}

// TestRenameAnchoredGenruleOutputs confirms the rename keys on the anchored
// $(RULEDIR)/<out> output form (renaming it to its `.gen` sibling) while the
// source-tree input occurrence — sharing the same basename but carrying the
// element package path, NOT $(RULEDIR) — is left untouched. This is the
// LLVM Remarks.exports shape: cmake reads the source `Remarks.exports` and
// writes a build-tree `Remarks.exports`, which collide as one Bazel label
// until the output is renamed.
func TestRenameAnchoredGenruleOutputs(t *testing.T) {
	cmd := `python3 -c "..." < elements/llvm/tools/remarks-shlib/Remarks.exports > $(RULEDIR)/tools/remarks-shlib/Remarks.exports`
	got := renameAnchoredGenruleOutputs(cmd, map[string]string{
		"tools/remarks-shlib/Remarks.exports": "tools/remarks-shlib/Remarks.exports.gen",
	})
	want := `python3 -c "..." < elements/llvm/tools/remarks-shlib/Remarks.exports > $(RULEDIR)/tools/remarks-shlib/Remarks.exports.gen`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestRenameAnchoredGenruleOutputs_NoOp confirms an empty rename map / empty
// cmd leaves the cmd byte-identical (the common, non-in-place path).
func TestRenameAnchoredGenruleOutputs_NoOp(t *testing.T) {
	cmd := "tool -o $(RULEDIR)/a.txt"
	if got := renameAnchoredGenruleOutputs(cmd, nil); got != cmd {
		t.Errorf("nil renames should be a no-op; got %q", got)
	}
	if got := renameAnchoredGenruleOutputs("", map[string]string{"a.txt": "a.txt.gen"}); got != "" {
		t.Errorf("empty cmd should be a no-op; got %q", got)
	}
}

// TestRenameAnchoredGenruleOutputs_OverlapSafe confirms longest-first
// matching: an output that is a textual prefix of another ($(RULEDIR)/x vs
// $(RULEDIR)/x.inc) renames each independently without the shorter mangling
// the longer, and the `.gen` replacement isn't itself re-matched.
func TestRenameAnchoredGenruleOutputs_OverlapSafe(t *testing.T) {
	cmd := "tool -o $(RULEDIR)/x.inc -d $(RULEDIR)/x"
	got := renameAnchoredGenruleOutputs(cmd, map[string]string{
		"x":     "x.gen",
		"x.inc": "x.inc.gen",
	})
	want := "tool -o $(RULEDIR)/x.inc.gen -d $(RULEDIR)/x.gen"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
