package lower

import "testing"

// anchorGenruleOutputsToRuledir anchors the output AND its multi-component
// parent dir, so a make_directory/mkdir of the output's parent lands under
// $(RULEDIR) in lockstep with the output write — the parent dir is created
// where the file is actually written, not as a stray dir in the sandbox cwd.
func TestAnchorGenruleOutputsToRuledir_ParentDir(t *testing.T) {
	cmd := "mkdir -p gen/myproj && cp gen/gen.inc.in gen/myproj/gen.inc"
	got := anchorGenruleOutputsToRuledir(cmd, []string{"gen/myproj/gen.inc"})
	want := "mkdir -p $(RULEDIR)/gen/myproj && cp gen/gen.inc.in $(RULEDIR)/gen/myproj/gen.inc"
	if got != want {
		t.Errorf("parent-dir anchor:\n got  %q\n want %q", got, want)
	}
}

// A single-component parent ("gen") is NOT anchored: it would corrupt a
// slash-less sibling token (a `gen.inc.in` src) by substring match, and it
// maps to the package's $(RULEDIR) root post-split (no subdir → no mkdir).
func TestAnchorGenruleOutputsToRuledir_SingleComponentParentSafe(t *testing.T) {
	cmd := "cp gen.inc.in gen/out.inc"
	got := anchorGenruleOutputsToRuledir(cmd, []string{"gen/out.inc"})
	want := "cp gen.inc.in $(RULEDIR)/gen/out.inc"
	if got != want {
		t.Errorf("single-component parent must stay un-anchored:\n got  %q\n want %q", got, want)
	}
}
