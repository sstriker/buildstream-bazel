package lower

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
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

// recipeNinja is the add_custom_target(gen_recipe DEPENDS gen/recipe.cmake) ninja
// shape shared by the cases below: a UTILITY phony depending on a CUSTOM_COMMAND
// that produces gen/recipe.cmake.
func recipeNinja(t *testing.T) *ninja.Graph {
	t.Helper()
	return mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen/recipe.cmake: CUSTOM_COMMAND
  COMMAND = python gen.py -o gen/recipe.cmake
build gen_recipe: phony gen/recipe.cmake
`)
}

// TestRecoverUtilityRecipeCommands_OuterInclude is the cross-boundary
// superbuild case: a recipe produced by THIS (nested) build's UTILITY is
// include()d by an OUTER configure, so the nested build's OWN trace carries no
// include() of it (cc.IncludeCalls empty). cc.OuterRecipeIncludes — the ancestor
// recipe paths threaded down — must still drive the recovery. The ancestor path
// is absolute and inside this build dir; one outside it is a harmless no-op.
func TestRecoverUtilityRecipeCommands_OuterInclude(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"
	cc := newCodegenContext()
	// No nested-trace include() of the recipe; the include lived in the outer.
	cc.OuterRecipeIncludes = []string{
		filepath.Join(cmakeBuild, "gen/recipe.cmake"), // inside this build dir → drives recovery
		"/other/elsewhere.cmake",                      // outside → ignored
	}
	r, cfg := utilityReply("gen_recipe")

	recoverUtilityRecipeCommands(r, cfg, recipeNinja(t), cc, cmakeSrc, cmakeBuild)

	if _, ok := cc.OutToGenrule["gen/recipe.cmake"]; !ok {
		t.Fatalf("outer-included nested recipe not recovered via OuterRecipeIncludes: %v", cc.OutToGenrule)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("want exactly one recovered genrule (the elsewhere path must no-op); got %+v", cc.Genrules)
	}
}

// TestAccumulateRecipeIncludes folds the inherited ancestor recipes with this
// build's own include()/target_sources recipes, deduped and filtered to `.cmake`.
func TestAccumulateRecipeIncludes(t *testing.T) {
	cc := newCodegenContext()
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: "/b/own.cmake"},
		{Path: "/b/not-a-recipe.txt"}, // non-.cmake dropped
	}
	cc.TargetSourcesCalls = []shadow.TargetSourcesCall{
		{Recipe: "/b/own.cmake"}, // dup of the include() above
		{Recipe: "/b/ts.cmake"},
	}
	got := accumulateRecipeIncludes([]string{"/anc/a.cmake", "/anc/a.cmake"}, cc)
	want := []string{"/anc/a.cmake", "/b/own.cmake", "/b/ts.cmake"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
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

// TestRecipeStem pins the per-configure-stable normalization: a trailing
// `[-_.]<hex-run>` token is stripped when the run has a digit OR is length >= 2
// (so divergent counters AND all-letter hashes map to one stem), distinct stems
// stay distinct, and a single hex LETTER is kept as a meaningful name suffix.
func TestRecipeStem(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gen/recipe-0.cmake", "gen/recipe.cmake"},     // single-digit counter
		{"gen/recipe-3.cmake", "gen/recipe.cmake"},     // divergent counter
		{"gen/recipe-1a2f.cmake", "gen/recipe.cmake"},  // mixed hash
		{"gen/foo-cafe.cmake", "gen/foo.cmake"},        // all-letter hash, len>=2 (no-digit gap closed)
		{"gen/foo-deadbeef.cmake", "gen/foo.cmake"},    // longer all-letter hash
		{"gen/module_a-0.cmake", "gen/module_a.cmake"}, // counter strips, _a kept
		{"gen/module_b-0.cmake", "gen/module_b.cmake"},
		{"recipe.cmake", "recipe.cmake"}, // no token
		// A single hex LETTER (len 1, no digit) is a name suffix, not a hash, so
		// module_a / module_b stay DISTINCT (the would-be mispair window).
		{"gen/module_a.cmake", "gen/module_a.cmake"},
		{"gen/module_b.cmake", "gen/module_b.cmake"},
		{"gen/recipe-stable.cmake", "gen/recipe-stable.cmake"}, // non-hex run, not stripped
	}
	for _, c := range cases {
		if got := recipeStem(c.in); got != c.want {
			t.Errorf("recipeStem(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSourceLabelPrefix: the label-root-relative source prefix is the nested
// source subdir when the label root sits above cmakeSrc, else empty.
func TestSourceLabelPrefix(t *testing.T) {
	if got := sourceLabelPrefix("/proj", "/proj/sub"); got != "sub" {
		t.Errorf("nested prefix = %q, want sub", got)
	}
	if got := sourceLabelPrefix("/proj", "/proj"); got != "" {
		t.Errorf("equal-root prefix = %q, want empty", got)
	}
	if got := sourceLabelPrefix("", "/proj"); got != "" {
		t.Errorf("no-root prefix = %q, want empty", got)
	}
}

// TestRecoverUtilityRecipeCommands_DivergentStemPair is the superbuild
// hash-unstable case: the outer trace's recipe (recipe-0) and target_sources
// gen_src diverge from the re-configured graph's recipe edge (recipe-3). The
// stem pairing recovers the recipe-3 edge declaring the STABLE gen_src (not the
// recipe), so the consumer wires via OutToGenrule.
func TestRecoverUtilityRecipeCommands_DivergentStemPair(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build gen/recipe-3.cmake: CUSTOM_COMMAND sub/gen.py
  COMMAND = python3 sub/gen.py gen 3
build gen_recipe: phony gen/recipe-3.cmake
`)
	cc := newCodegenContext()
	// Outer trace: include recipe-0 (diverges from the graph's recipe-3) and a
	// target_sources of gen_src attributed to recipe-0.
	cc.OuterRecipeIncludes = []string{filepath.Join(cmakeBuild, "gen/recipe-0.cmake")}
	cc.OuterTargetSources = []shadow.TargetSourcesCall{{
		Target:  "app",
		Recipe:  filepath.Join(cmakeBuild, "gen/recipe-0.cmake"),
		Sources: []string{filepath.Join(cmakeBuild, "gen/gen_src.c")},
	}}
	r, cfg := utilityReply("gen_recipe")

	recoverUtilityRecipeCommands(r, cfg, g, cc, cmakeSrc, cmakeBuild)

	name, ok := cc.OutToGenrule["gen/gen_src.c"]
	if !ok {
		t.Fatalf("divergent recipe gen_src not recovered: OutToGenrule=%v", cc.OutToGenrule)
	}
	// The recovered genrule declares the STABLE gen_src, not the unstable recipe.
	if _, isRecipe := cc.OutToGenrule["gen/recipe-3.cmake"]; isRecipe {
		t.Errorf("unstable recipe should not be a declared output: %v", cc.OutToGenrule)
	}
	var found *ir.Target
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == name {
			found = &cc.Genrules[i]
		}
	}
	if found == nil || len(found.GenruleOuts) != 1 || found.GenruleOuts[0] != "gen/gen_src.c" {
		t.Fatalf("genrule %q outs wrong: %+v", name, found)
	}
}
