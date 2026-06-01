package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// A $<TARGET_FILE:t> in an add_custom_command argv resolves (in build.ninja)
// to the target's build-dir-relative artifact path, which rewriteToolFromTarget
// already lifts to $(location :t) + a tools dep. The audit tag confirms that
// portable resolution happened.
func TestLowerStandaloneCustomCommands_GenexTargetFileResolved(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen.h: CUSTOM_COMMAND
  COMMAND = bin/tool --emit gen.h
`)
	artifactToName := map[string]string{"bin/tool": "tool"}
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{{
			Outputs:  []string{"gen.h"},
			Commands: [][]string{{"$<TARGET_FILE:tool>", "--emit", "gen.h"}},
		}},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", artifactToName, ctx)
	if len(got) != 1 {
		t.Fatalf("want 1 genrule; got %d (%v)", len(got), got)
	}
	if !strings.Contains(got[0].GenruleCmd, "$(location :tool)") {
		t.Errorf("cmd should lift bin/tool to $(location :tool); got %q", got[0].GenruleCmd)
	}
	if !containsTool(got[0].GenruleTools, ":tool") {
		t.Errorf("tools should carry :tool; got %v", got[0].GenruleTools)
	}
	if !hasTag(got[0].Tags, cmdGenexResolvedTag) {
		t.Errorf("want %s; got %v", cmdGenexResolvedTag, got[0].Tags)
	}
	if hasTag(got[0].Tags, cmdGenexUnresolvedTag) {
		t.Errorf("should not be unresolved; got %v", got[0].Tags)
	}
}

// $<TARGET_OBJECTS:t> resolves to the object-library's .o file list, which
// rewriteToolFromTarget does NOT lift (artifactToName carries final artifacts,
// not object lists) — so the resolved paths bake as non-portable literals. The
// audit tag must surface that as unresolved residue.
func TestLowerStandaloneCustomCommands_GenexTargetObjectsUnresolved(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build manifest.txt: CUSTOM_COMMAND
  COMMAND = list-objs CMakeFiles/objlib.dir/a.o CMakeFiles/objlib.dir/b.o > manifest.txt
`)
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{{
			Outputs:  []string{"manifest.txt"},
			Commands: [][]string{{"list-objs", "$<TARGET_OBJECTS:objlib>", ">", "manifest.txt"}},
		}},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", nil, ctx)
	if len(got) != 1 {
		t.Fatalf("want 1 genrule; got %d", len(got))
	}
	if !hasTag(got[0].Tags, cmdGenexUnresolvedTag) {
		t.Errorf("TARGET_OBJECTS not lifted to a label → want %s; got %v", cmdGenexUnresolvedTag, got[0].Tags)
	}
}

// A command carrying only value genexes ($<CONFIG> etc.) has no path-portability
// hazard for a single configure — the resolved value bakes correctly — so it
// classifies as resolved.
func TestLowerStandaloneCustomCommands_GenexValueOnlyResolved(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build tag.txt: CUSTOM_COMMAND
  COMMAND = emit Release > tag.txt
`)
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{{
			Outputs:  []string{"tag.txt"},
			Commands: [][]string{{"emit", "$<CONFIG>", ">", "tag.txt"}},
		}},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", nil, ctx)
	if len(got) != 1 {
		t.Fatalf("want 1 genrule; got %d", len(got))
	}
	if !hasTag(got[0].Tags, cmdGenexResolvedTag) {
		t.Errorf("value-only genex should classify resolved; got %v", got[0].Tags)
	}
}

// A genex-free add_custom_command gets no genex audit tag — the legacy shape is
// byte-stable.
func TestLowerStandaloneCustomCommands_NoGenexNoTag(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build out.txt: CUSTOM_COMMAND
  COMMAND = echo hi > out.txt
`)
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{{
			Outputs:  []string{"out.txt"},
			Commands: [][]string{{"echo", "hi", ">", "out.txt"}},
		}},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", nil, ctx)
	if len(got) != 1 {
		t.Fatalf("want 1 genrule; got %d", len(got))
	}
	if hasTag(got[0].Tags, cmdGenexResolvedTag) || hasTag(got[0].Tags, cmdGenexUnresolvedTag) {
		t.Errorf("genex-free command should carry no genex tag; got %v", got[0].Tags)
	}
}

// A command mixing a liftable full-path op ($<TARGET_FILE:tool> → bin/tool →
// $(location :tool)) with a derived op for the SAME target ($<TARGET_FILE_DIR:
// tool> → bare `bin`, never lifted) must NOT be classified resolved just
// because "tool" landed in tools via the full-path op — the _DIR occurrence
// baked a non-portable literal.
func TestLowerStandaloneCustomCommands_GenexMixedFamilyOpsUnresolved(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen.h: CUSTOM_COMMAND
  COMMAND = bin/tool --out-dir bin --emit gen.h
`)
	artifactToName := map[string]string{"bin/tool": "tool"}
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{{
			Outputs:  []string{"gen.h"},
			Commands: [][]string{{"$<TARGET_FILE:tool>", "--out-dir", "$<TARGET_FILE_DIR:tool>", "--emit", "gen.h"}},
		}},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", artifactToName, ctx)
	if len(got) != 1 {
		t.Fatalf("want 1 genrule; got %d", len(got))
	}
	// The full-path op still lifts (so :tool is in tools)...
	if !containsTool(got[0].GenruleTools, ":tool") {
		t.Errorf("full-path TARGET_FILE should still lift to :tool; got %v", got[0].GenruleTools)
	}
	// ...but the $<TARGET_FILE_DIR:tool> occurrence baked a bare `bin`, so the
	// command must classify unresolved, not resolved.
	if !hasTag(got[0].Tags, cmdGenexUnresolvedTag) {
		t.Errorf("mixed full-path + derived op should be %s; got %v", cmdGenexUnresolvedTag, got[0].Tags)
	}
	if hasTag(got[0].Tags, cmdGenexResolvedTag) {
		t.Errorf("must not be %s when a derived op baked; got %v", cmdGenexResolvedTag, got[0].Tags)
	}
}

func containsTool(tools []string, want string) bool {
	for _, t := range tools {
		if t == want {
			return true
		}
	}
	return false
}
