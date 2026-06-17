package lower

import (
	"path/filepath"
	"testing"

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
