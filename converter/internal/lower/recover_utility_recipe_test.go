package lower

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// utilityReply builds a minimal codemodel reply/configuration with one UTILITY
// target plus optional sibling (EXECUTABLE) targets — the latter populate the
// walk's sibling-target fence.
func utilityReply(utilityName string, siblings ...string) (*fileapi.Reply, fileapi.Configuration) {
	r := &fileapi.Reply{Targets: map[string]fileapi.Target{
		"util": {Name: utilityName, Type: "UTILITY"},
	}}
	cfg := fileapi.Configuration{Targets: []fileapi.ConfigTargetRef{{Id: "util", Name: utilityName}}}
	for i, s := range siblings {
		id := fmt.Sprintf("sib%d", i)
		r.Targets[id] = fileapi.Target{Name: s, Type: "EXECUTABLE"}
		cfg.Targets = append(cfg.Targets, fileapi.ConfigTargetRef{Id: id, Name: s})
	}
	return r, cfg
}

// TestRecoverUtilityRecipeCommands recovers the custom command that produces an
// include()d `.cmake` recipe reached through a UTILITY (add_custom_target)
// target's ninja deps, registering it in cc.OutToGenrule — the link lowerTarget's
// UTILITY skip would otherwise leave missing, so the #730 causal attribution can
// complete the chain. The phony→recipe walk mirrors the add_custom_target(gen
// DEPENDS recipe.cmake) shape.
func TestRecoverUtilityRecipeCommands(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen/recipe.cmake: CUSTOM_COMMAND
  COMMAND = python gen.py -o gen/recipe.cmake
build gen_recipe: phony gen/recipe.cmake
`)
	cc := newCodegenContext()
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: filepath.Join(cmakeBuild, "gen/recipe.cmake"), File: "/src/CMakeLists.txt", Line: 7},
	}
	r, cfg := utilityReply("gen_recipe")

	recoverUtilityRecipeCommands(r, cfg, g, cc, cmakeSrc, cmakeBuild)

	name, ok := cc.OutToGenrule["gen/recipe.cmake"]
	if !ok {
		t.Fatalf("recipe not registered in OutToGenrule: %v", cc.OutToGenrule)
	}
	if len(cc.Genrules) != 1 || cc.Genrules[0].Name != name {
		t.Fatalf("want one recovered genrule named %q; got %+v", name, cc.Genrules)
	}
	if cc.Genrules[0].GenruleOuts[0] != "gen/recipe.cmake" {
		t.Errorf("genrule outs = %v; want [gen/recipe.cmake]", cc.Genrules[0].GenruleOuts)
	}

	// The whole chain: a target_sources()'d generated source the recipe pulled
	// in (no producing ninja edge of its own) now resolves to the recovered
	// genrule via the OUTPUT->include->target_sources tie.
	relOut, tied, ok := cc.adoptIncludedRecipeOutput(filepath.Join(cmakeBuild, "gen/foo.cpp"), cmakeBuild, "")
	if !ok || tied != name || relOut != "gen/foo.cpp" {
		t.Fatalf("chain tie: got (%q,%q,%v), want (gen/foo.cpp, %q, true)", relOut, tied, ok, name)
	}
}

// TestRecoverUtilityRecipeCommands_NotIncludedNoOp is the strictly-additive
// guard: a UTILITY custom command whose output nothing include()s is left
// untouched — no dead genrule, nothing in OutToGenrule.
func TestRecoverUtilityRecipeCommands_NotIncludedNoOp(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen/recipe.cmake: CUSTOM_COMMAND
  COMMAND = python gen.py -o gen/recipe.cmake
build gen_recipe: phony gen/recipe.cmake
`)
	cc := newCodegenContext()
	// No include() of the recipe — nothing to gate the recovery on.
	r, cfg := utilityReply("gen_recipe")

	recoverUtilityRecipeCommands(r, cfg, g, cc, cmakeSrc, cmakeBuild)

	if len(cc.OutToGenrule) != 0 || len(cc.Genrules) != 0 {
		t.Fatalf("expected no recovery without an include(); OutToGenrule=%v Genrules=%v",
			cc.OutToGenrule, cc.Genrules)
	}
}

// TestRecoverUtilityRecipeCommands_StopsAtSiblingTarget confirms the walk fences
// at sibling-target boundaries (every codemodel target name): a recipe reached
// only by crossing into another target's node is NOT recovered, so a depended-on
// tool's or library's own edges aren't pulled in.
func TestRecoverUtilityRecipeCommands_StopsAtSiblingTarget(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen/recipe.cmake: CUSTOM_COMMAND
  COMMAND = python gen.py -o gen/recipe.cmake
build sibling_tool: phony gen/recipe.cmake
build gen_recipe: phony sibling_tool
`)
	cc := newCodegenContext()
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: filepath.Join(cmakeBuild, "gen/recipe.cmake"), File: "/src/CMakeLists.txt"},
	}
	// gen_recipe reaches the recipe only THROUGH sibling_tool, which is itself a
	// target name — the walk must stop there.
	r, cfg := utilityReply("gen_recipe", "sibling_tool")

	recoverUtilityRecipeCommands(r, cfg, g, cc, cmakeSrc, cmakeBuild)

	if _, ok := cc.OutToGenrule["gen/recipe.cmake"]; ok {
		t.Errorf("recipe behind a sibling target should not be recovered: %v", cc.OutToGenrule)
	}
}
