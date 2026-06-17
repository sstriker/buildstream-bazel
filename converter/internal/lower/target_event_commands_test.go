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

// TestLowerTargetEventCommands_UnresolvedGenexSkipped: a POST_BUILD command that
// references an unresolved $<TARGET_FILE:…> genex (cmake --trace-expand does NOT
// expand genexes) must NOT emit a genrule with the literal genex (a broken rule)
// — it's skipped + warned, byproduct left for the breadcrumb to surface.
func TestLowerTargetEventCommands_UnresolvedGenexSkipped(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{{
		Target:     "producer",
		Event:      "POST_BUILD",
		Commands:   [][]string{{"cp", "$<TARGET_FILE:producer>", "/tmp/build/producer_copy.a"}},
		ByProducts: []string{"/tmp/build/producer_copy.a"},
	}}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "producer_post_build" {
			t.Errorf("emitted a genrule for an unresolved-genex command (would be broken): %+v", cc.Genrules[i])
		}
	}
	if cc.OutToGenrule["producer_copy.a"] != "" {
		t.Errorf("byproduct must not be registered when the command is skipped: %v", cc.OutToGenrule)
	}
	if !strings.Contains(warn.String(), "unresolved generator expression") {
		t.Errorf("expected unresolved-genex skip warning; got %q", warn.String())
	}
}

// TestLowerTargetEventCommands_InferredOutputs: a command that declares no
// BYPRODUCTS but writes a build-dir file via a compiler `-o <path>` or a `> <file>`
// redirect has that output inferred from the command line (best-effort) rather
// than being dropped as a pure side-effect.
func TestLowerTargetEventCommands_InferredOutputs(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{
		{
			Target:   "comp",
			Event:    "PRE_LINK",
			Commands: [][]string{{"/usr/bin/cc", "-c", "/src/extra.c", "-o", "/tmp/build/extra.o"}},
		},
		{
			Target:   "redir",
			Event:    "POST_BUILD",
			Commands: [][]string{{"/bin/gen", "--emit", ">", "/tmp/build/manifest.txt"}},
		},
	}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	// -o operand inferred as an output, wired through OutToGenrule.
	if cc.OutToGenrule["extra.o"] != "comp_pre_link" {
		t.Errorf("extra.o not inferred/registered to comp_pre_link: %v", cc.OutToGenrule)
	}
	// > redirect target inferred as an output.
	if cc.OutToGenrule["manifest.txt"] != "redir_post_build" {
		t.Errorf("manifest.txt not inferred/registered to redir_post_build: %v", cc.OutToGenrule)
	}
	// The emitted genrule preserves the redirect with the operand anchored to
	// $(RULEDIR) — genrule cmd runs under bash, so the redirect produces the out.
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "redir_post_build" &&
			!strings.Contains(cc.Genrules[i].GenruleCmd, "> $(RULEDIR)/manifest.txt") {
			t.Errorf("redirect not anchored in genrule cmd: %q", cc.Genrules[i].GenruleCmd)
		}
	}
	// Inferred genrules carry the audit tag distinguishing them from declared ones.
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "comp_pre_link" &&
			!stringSliceContains(cc.Genrules[i].Tags, "cmake-codegen-target-event-inferred-output") {
			t.Errorf("inferred genrule missing audit tag: %v", cc.Genrules[i].Tags)
		}
	}
	if !strings.Contains(warn.String(), "inferred output") {
		t.Errorf("expected inferred-output breadcrumb; got %q", warn.String())
	}
}

// TestInferTargetEventOutputs_Variants: the redirect/-o inference accepts plain
// `>` / `1>` and a standalone `-o`, and rejects the append/stderr variants,
// `$`-bearing operands, and out-of-tree paths.
func TestInferTargetEventOutputs_Variants(t *testing.T) {
	const buildDir = "/tmp/build"
	cases := []struct {
		name string
		argv []string
		want []string
	}{
		{"dash-o", []string{"cc", "-o", "/tmp/build/a.o", "x.c"}, []string{"a.o"}},
		{"redirect", []string{"gen", ">", "/tmp/build/b.txt"}, []string{"b.txt"}},
		{"redirect-1", []string{"gen", "1>", "/tmp/build/c.txt"}, []string{"c.txt"}},
		{"append-excluded", []string{"gen", ">>", "/tmp/build/d.txt"}, nil},
		{"stderr-excluded", []string{"gen", "2>", "/tmp/build/e.txt"}, nil},
		{"genex-operand-excluded", []string{"cc", "-o", "$<TARGET_FILE:t>"}, nil},
		{"var-operand-excluded", []string{"cc", "-o", "${OUT}"}, nil},
		{"out-of-tree-excluded", []string{"cc", "-o", "/other/place/f.o"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferTargetEventOutputs([][]string{tc.argv}, buildDir)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("inferTargetEventOutputs(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}
