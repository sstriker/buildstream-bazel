package lower

import (
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestAdoptIncludedRecipeOutput: a generated source no ninja edge produces is
// attributed to the codegen genrule whose recipe .cmake (its declared OUTPUT)
// the project include()s — the OUTPUT -> include tie. The source is added to
// that genrule's outs and indexed in OutToGenrule.
func TestAdoptIncludedRecipeOutput(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	cc.Genrules = []ir.Target{{
		Name:        "gen_recipe",
		Kind:        ir.KindGenrule,
		GenruleOuts: []string{"gen/recipe.cmake"},
	}}
	cc.OutToGenrule = map[string]string{"gen/recipe.cmake": "gen_recipe"}
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: filepath.Join(buildDir, "gen/recipe.cmake"), File: "/src/CMakeLists.txt", Line: 10},
	}

	relOut, name, ok := cc.adoptIncludedRecipeOutput(filepath.Join(buildDir, "gen/foo.cpp"), buildDir, "")
	if !ok {
		t.Fatal("expected attribution to the included-recipe codegen genrule")
	}
	if relOut != "gen/foo.cpp" || name != "gen_recipe" {
		t.Fatalf("got (%q, %q), want (gen/foo.cpp, gen_recipe)", relOut, name)
	}
	if cc.OutToGenrule["gen/foo.cpp"] != "gen_recipe" {
		t.Errorf("OutToGenrule not updated: %v", cc.OutToGenrule)
	}
	// The source is now a declared output of the genrule (sorted in).
	g := cc.Genrules[0]
	found := false
	for _, o := range g.GenruleOuts {
		if o == "gen/foo.cpp" {
			found = true
		}
	}
	if !found {
		t.Errorf("gen/foo.cpp not added to genrule outs: %v", g.GenruleOuts)
	}
}

// TestAdoptIncludedRecipeOutput_MultiCandidateDeclines: with 2+ include()d
// recipe codegens and no consumer scope to disambiguate, attribution declines
// (caller keeps its existing behavior) — strictly additive, never a wrong guess.
func TestAdoptIncludedRecipeOutput_MultiCandidateDeclines(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	cc.Genrules = []ir.Target{
		{Name: "g1", Kind: ir.KindGenrule, GenruleOuts: []string{"a/r1.cmake"}},
		{Name: "g2", Kind: ir.KindGenrule, GenruleOuts: []string{"b/r2.cmake"}},
	}
	cc.OutToGenrule = map[string]string{"a/r1.cmake": "g1", "b/r2.cmake": "g2"}
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: filepath.Join(buildDir, "a/r1.cmake"), File: "/src/a/CMakeLists.txt"},
		{Path: filepath.Join(buildDir, "b/r2.cmake"), File: "/src/b/CMakeLists.txt"},
	}
	if _, _, ok := cc.adoptIncludedRecipeOutput(filepath.Join(buildDir, "x/foo.cpp"), buildDir, ""); ok {
		t.Error("expected decline with ambiguous candidates and no consumer scope")
	}
	// With the consumer's defining file in b/, the b/ recipe codegen is chosen.
	relOut, name, ok := cc.adoptIncludedRecipeOutput(filepath.Join(buildDir, "x/foo.cpp"), buildDir, "/src/b/CMakeLists.txt")
	if !ok || name != "g2" || relOut != "x/foo.cpp" {
		t.Fatalf("scope match: got (%q,%q,%v), want (x/foo.cpp, g2, true)", relOut, name, ok)
	}
}

// TestAdoptIncludedRecipeOutput_DirectTargetSourcesTie: when the trace records the
// target_sources() that added the source — and the File that ran it is a recovered
// recipe .cmake — the source is tied DIRECTLY to that recipe's genrule, with no
// consumerDefFile and even when several recipe codegens are present (the case the
// include-scope heuristic declines). This is the exact source->recipe signal.
func TestAdoptIncludedRecipeOutput_DirectTargetSourcesTie(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	cc.Genrules = []ir.Target{
		{Name: "g1", Kind: ir.KindGenrule, GenruleOuts: []string{"a/r1.cmake"}},
		{Name: "g2", Kind: ir.KindGenrule, GenruleOuts: []string{"b/r2.cmake"}},
	}
	cc.OutToGenrule = map[string]string{"a/r1.cmake": "g1", "b/r2.cmake": "g2"}
	// Two recipes are include()d (ambiguous for the scope heuristic), but the
	// trace says r2 (g2) is the one whose target_sources() added b/foo.c.
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: filepath.Join(buildDir, "a/r1.cmake"), File: "/src/a/CMakeLists.txt"},
		{Path: filepath.Join(buildDir, "b/r2.cmake"), File: "/src/b/CMakeLists.txt"},
	}
	cc.TargetSourcesCalls = []shadow.TargetSourcesCall{
		{Target: "app", Sources: []string{filepath.Join(buildDir, "b/foo.c")}, File: filepath.Join(buildDir, "b/r2.cmake")},
	}

	// consumerDefFile empty + ambiguous candidates: the include-scope heuristic
	// alone would decline, but the direct target_sources tie resolves it to g2.
	relOut, name, ok := cc.adoptIncludedRecipeOutput(filepath.Join(buildDir, "b/foo.c"), buildDir, "")
	if !ok || name != "g2" || relOut != "b/foo.c" {
		t.Fatalf("direct tie: got (%q,%q,%v), want (b/foo.c, g2, true)", relOut, name, ok)
	}
	if cc.OutToGenrule["b/foo.c"] != "g2" {
		t.Errorf("OutToGenrule not updated to g2: %v", cc.OutToGenrule)
	}
	found := false
	for _, o := range cc.Genrules[1].GenruleOuts {
		if o == "b/foo.c" {
			found = true
		}
	}
	if !found {
		t.Errorf("b/foo.c not added to g2 outs: %v", cc.Genrules[1].GenruleOuts)
	}
}

// TestOrdinarySource_RecipeTieBeforeBake: a build-dir source cmake did NOT flag
// IsGenerated (e.g. target_sources()'d from a deferred include() of a generated
// recipe) reaches the ORDINARY-source path. Before baking it as static bytes, the
// path must try the OUTPUT->include recipe tie — wiring it to the recipe's genrule
// instead of freezing it. This mirrors the IsGenerated branch's tie so the
// generated bit cmake missed doesn't cause a stale bake.
func TestOrdinarySource_RecipeTieBeforeBake(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"

	cc := newCodegenContext()
	// A recovered codegen genrule whose declared OUTPUT is a recipe .cmake the
	// project include()s; the recipe's target_sources() pulled in gen/foo.c.
	cc.Genrules = []ir.Target{{
		Name: "gen_recipe", Kind: ir.KindGenrule, GenruleOuts: []string{"gen/recipe.cmake"},
	}}
	cc.OutToGenrule = map[string]string{"gen/recipe.cmake": "gen_recipe"}
	cc.IncludeCalls = []shadow.IncludeCall{
		{Path: filepath.Join(cmakeBuild, "gen/recipe.cmake"), File: "/src/CMakeLists.txt", Line: 5},
	}

	lc := targetLowerCtx{cc: cc, cmakeSrc: cmakeSrc, cmakeBuild: cmakeBuild}
	irt := &ir.Target{Name: "app", Kind: ir.KindCCLibrary}
	st := &sourceWalkState{srcEmitPath: map[int]string{}}
	// IsGenerated=false (the bug condition); under the build dir, not on disk.
	src := fileapi.TargetSource{Path: filepath.Join(cmakeBuild, "gen/foo.c")}

	recoverOrElideBuildDirSource(irt, src, "gen/foo.c", 0, st, true, lc)

	if !st.consumesCodegen {
		t.Error("recipe-tied ordinary source not recognized as codegen (it was baked/elided?)")
	}
	if cc.OutToGenrule["gen/foo.c"] != "gen_recipe" {
		t.Errorf("gen/foo.c not tied to the recipe genrule: %v", cc.OutToGenrule)
	}
	if len(irt.Srcs) != 1 || irt.Srcs[0] != "gen/foo.c" {
		t.Errorf("srcs = %v; want [gen/foo.c] wired to gen_recipe, not baked", irt.Srcs)
	}
	// gen/foo.c is now a declared output of the recipe's genrule.
	found := false
	for _, o := range cc.Genrules[0].GenruleOuts {
		if o == "gen/foo.c" {
			found = true
		}
	}
	if !found {
		t.Errorf("gen/foo.c not added to the recipe genrule outs: %v", cc.Genrules[0].GenruleOuts)
	}
}
