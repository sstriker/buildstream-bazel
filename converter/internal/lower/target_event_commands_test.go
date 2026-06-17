package lower

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerTargetEventCommands: a PRE_LINK/POST_BUILD command with BYPRODUCTS
// becomes a genrule producing those byproducts, registered in OutToGenrule so a
// consumer resolves; a command with no byproducts is warned and dropped.
func TestLowerTargetEventCommands(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{
		{
			Target:     "foo",
			Event:      "PRE_LINK",
			Commands:   [][]string{{"/cmake", "-E", "touch", "/tmp/build/foo_stamp.h"}},
			ByProducts: []string{"/tmp/build/foo_stamp.h"},
		},
		{
			Target:   "bar",
			Event:    "POST_BUILD",
			Commands: [][]string{{"/cmake", "-E", "echo", "done"}}, // no byproducts
		},
	}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	// PRE_LINK byproduct → genrule + OutToGenrule registration.
	if cc.OutToGenrule["foo_stamp.h"] != "foo_pre_link" {
		t.Errorf("foo_stamp.h not registered to foo_pre_link: %v", cc.OutToGenrule)
	}
	found := false
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "foo_pre_link" {
			found = true
			if len(cc.Genrules[i].GenruleOuts) != 1 || cc.Genrules[i].GenruleOuts[0] != "foo_stamp.h" {
				t.Errorf("foo_pre_link outs = %v, want [foo_stamp.h]", cc.Genrules[i].GenruleOuts)
			}
			if !stringSliceContains(cc.Genrules[i].Tags, "cmake-codegen-target-event-command") {
				t.Errorf("missing audit tag: %v", cc.Genrules[i].Tags)
			}
			// output anchored to $(RULEDIR)
			if !strings.Contains(cc.Genrules[i].GenruleCmd, "$(RULEDIR)/foo_stamp.h") {
				t.Errorf("cmd not output-anchored: %q", cc.Genrules[i].GenruleCmd)
			}
		}
	}
	if !found {
		t.Fatal("foo_pre_link genrule not synthesized")
	}
	// No-byproduct command → no genrule, and a warning.
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "bar_post_build" {
			t.Error("bar POST_BUILD has no byproducts; should not synthesize a genrule")
		}
	}
	if !strings.Contains(warn.String(), "bar") || !strings.Contains(warn.String(), "no recoverable BYPRODUCTS") {
		t.Errorf("expected no-byproducts warning for bar; got %q", warn.String())
	}
}
