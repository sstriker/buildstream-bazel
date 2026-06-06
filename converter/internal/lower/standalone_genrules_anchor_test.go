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

// A cd-stripped WORKING_DIRECTORY-relative output (curl's
// `perl mk-lib1521.pl < curl.h lib1521.c`, declared out tests/libtest/lib1521.c)
// appears in the cmd only by its workdir-relative suffix; the suffix fallback
// must still anchor it to $(RULEDIR)/.
func TestAnchorGenruleOutputsToRuledir_CdStrippedSuffix(t *testing.T) {
	cmd := "perl elements/curl/tests/libtest/mk-lib1521.pl < elements/curl/include/curl/curl.h lib1521.c"
	got := anchorGenruleOutputsToRuledir(cmd, []string{"tests/libtest/lib1521.c"})
	want := "perl elements/curl/tests/libtest/mk-lib1521.pl < elements/curl/include/curl/curl.h $(RULEDIR)/lib1521.c"
	if got != want {
		t.Errorf("cd-stripped suffix output must anchor:\n got  %q\n want %q", got, want)
	}
}

// The suffix fallback anchors the LONGEST present suffix: a nested
// workdir-relative output (out a/b/sub/foo.c, cmd carries sub/foo.c) anchors
// sub/foo.c, not the bare basename foo.c.
func TestAnchorGenruleOutputsToRuledir_LongestSuffix(t *testing.T) {
	cmd := "tool -o sub/foo.c input"
	got := anchorGenruleOutputsToRuledir(cmd, []string{"a/b/sub/foo.c"})
	want := "tool -o $(RULEDIR)/sub/foo.c input"
	if got != want {
		t.Errorf("longest present suffix must anchor:\n got  %q\n want %q", got, want)
	}
}

// When the full build-dir-relative form IS present, the fallback is a no-op —
// the basename is NOT additionally anchored (no churn for the standalone shape).
func TestAnchorGenruleOutputsToRuledir_FullFormNoFallback(t *testing.T) {
	cmd := "tool gen/myproj/out.inc"
	got := anchorGenruleOutputsToRuledir(cmd, []string{"gen/myproj/out.inc"})
	want := "tool $(RULEDIR)/gen/myproj/out.inc"
	if got != want {
		t.Errorf("full-form output anchors once:\n got  %q\n want %q", got, want)
	}
}
