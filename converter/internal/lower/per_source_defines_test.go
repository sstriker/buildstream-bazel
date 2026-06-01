package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestShouldSplitCompileGroups_MultiLanguage covers the legacy
// case: two CGs with different languages always split.
func TestShouldSplitCompileGroups_MultiLanguage(t *testing.T) {
	tgt := &fileapi.Target{
		CompileGroups: []fileapi.CompileGroup{
			{Language: "C"},
			{Language: "CXX"},
		},
	}
	if !shouldSplitCompileGroups(tgt) {
		t.Errorf("multi-language target should split")
	}
}

// TestShouldSplitCompileGroups_SingleCG never splits — nothing to
// partition.
func TestShouldSplitCompileGroups_SingleCG(t *testing.T) {
	tgt := &fileapi.Target{
		CompileGroups: []fileapi.CompileGroup{
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "FOO=1"}}},
		},
	}
	if shouldSplitCompileGroups(tgt) {
		t.Errorf("single-CG target should not split")
	}
}

// TestShouldSplitCompileGroups_SameLangSameDefines confirms
// duplicate-shape CGs (cmake's degenerate emit) DO NOT split.
func TestShouldSplitCompileGroups_SameLangSameDefines(t *testing.T) {
	tgt := &fileapi.Target{
		CompileGroups: []fileapi.CompileGroup{
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "FOO=1"}}},
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "FOO=1"}}},
		},
	}
	if shouldSplitCompileGroups(tgt) {
		t.Errorf("duplicate-shape CGs should not trigger split")
	}
}

// TestShouldSplitCompileGroups_SameLangDifferentDefines covers
// the per-source-defines case Phase 1 task 3 surfaces: two C-CGs
// with different Defines (from set_source_files_properties).
func TestShouldSplitCompileGroups_SameLangDifferentDefines(t *testing.T) {
	tgt := &fileapi.Target{
		CompileGroups: []fileapi.CompileGroup{
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "FOO=1"}}},
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "BAR=2"}}},
		},
	}
	if !shouldSplitCompileGroups(tgt) {
		t.Errorf("CGs with differing defines should trigger split")
	}
}

// TestShouldSplitCompileGroups_SameLangDifferentFlags covers the
// other split trigger: same defines but differing CompileCommandFragments.
func TestShouldSplitCompileGroups_SameLangDifferentFlags(t *testing.T) {
	tgt := &fileapi.Target{
		CompileGroups: []fileapi.CompileGroup{
			{
				Language:                "C",
				CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O3"}},
			},
			{
				Language:                "C",
				CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O0"}},
			},
		},
	}
	if !shouldSplitCompileGroups(tgt) {
		t.Errorf("CGs with differing flags should trigger split")
	}
}

func TestIntSuffix(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{42, "42"},
		{100, "100"},
	}
	for _, c := range cases {
		if got := intSuffix(c.in); got != c.want {
			t.Errorf("intSuffix(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReclassifyHeaderOnlySources_MovesToHdrs(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c", "decl_only.h", "bar.c"},
			Hdrs: []string{"foo.h"},
		}},
	}
	reclassifyHeaderOnlySources(pkg, map[string]bool{"decl_only.h": true})
	wantSrcs := []string{"foo.c", "bar.c"}
	if len(pkg.Targets[0].Srcs) != len(wantSrcs) {
		t.Fatalf("Srcs: %v", pkg.Targets[0].Srcs)
	}
	if !stringSliceContains(pkg.Targets[0].Hdrs, "decl_only.h") {
		t.Errorf("decl_only.h should be in hdrs; got %v", pkg.Targets[0].Hdrs)
	}
}

func TestReclassifyHeaderOnlySources_DedupesExisting(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"decl_only.h"},
			Hdrs: []string{"decl_only.h"}, // already there
		}},
	}
	reclassifyHeaderOnlySources(pkg, map[string]bool{"decl_only.h": true})
	if len(pkg.Targets[0].Hdrs) != 1 {
		t.Errorf("Hdrs should not have dupe: %v", pkg.Targets[0].Hdrs)
	}
	if len(pkg.Targets[0].Srcs) != 0 {
		t.Errorf("Srcs should be empty: %v", pkg.Targets[0].Srcs)
	}
}

func TestCMakeTruthy(t *testing.T) {
	for _, v := range []string{"TRUE", "true", "ON", "on", "YES", "yes", "1", "Y", "y"} {
		if !cmakeTruthy(v) {
			t.Errorf("cmakeTruthy(%q) = false; want true", v)
		}
	}
	for _, v := range []string{"FALSE", "OFF", "NO", "0", "N", "", "anything"} {
		if cmakeTruthy(v) {
			t.Errorf("cmakeTruthy(%q) = true; want false", v)
		}
	}
}

func TestCollectObjectDepends_SingleProperty(t *testing.T) {
	calls := []shadowSourceFilePropertiesCallStub{
		{
			Files: []string{"foo.c"},
			Properties: []shadowSourceFilePropertyStub{
				{Name: "OBJECT_DEPENDS", Value: "extra.h;more.h"},
			},
		},
	}
	got := collectObjectDepends(toShadowCalls(calls))
	if len(got["foo.c"]) != 2 || got["foo.c"][0] != "extra.h" || got["foo.c"][1] != "more.h" {
		t.Errorf("OBJECT_DEPENDS for foo.c: %v", got["foo.c"])
	}
}

func TestAddObjectDependsHeaders_AppendsToHdrs(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
			Hdrs: []string{"foo.h"},
		}},
	}
	byPath := map[string][]string{"foo.c": {"extra.h", "more.h"}}
	addObjectDependsHeaders(pkg, byPath)
	if len(pkg.Targets[0].Hdrs) != 3 {
		t.Errorf("Hdrs: %v want 3 entries", pkg.Targets[0].Hdrs)
	}
}

func TestAddObjectDependsHeaders_DedupesExisting(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
			Hdrs: []string{"foo.h", "extra.h"},
		}},
	}
	byPath := map[string][]string{"foo.c": {"extra.h", "more.h"}}
	addObjectDependsHeaders(pkg, byPath)
	if len(pkg.Targets[0].Hdrs) != 3 {
		t.Errorf("Hdrs len: %d, want 3 (dedup)", len(pkg.Targets[0].Hdrs))
	}
}

// Lightweight shims so we can build SourceFilePropertiesCall
// values in test code without importing the shadow package
// here (the lower package already imports it for production
// code — these reduce test boilerplate).
type shadowSourceFilePropertiesCallStub struct {
	Files      []string
	Properties []shadowSourceFilePropertyStub
}

type shadowSourceFilePropertyStub struct {
	Name, Value string
}

func toShadowCalls(in []shadowSourceFilePropertiesCallStub) []shadow.SourceFilePropertiesCall {
	out := make([]shadow.SourceFilePropertiesCall, len(in))
	for i, c := range in {
		out[i].Files = c.Files
		out[i].Properties = make([]shadow.SourceFileProperty, len(c.Properties))
		for j, p := range c.Properties {
			out[i].Properties[j] = shadow.SourceFileProperty{Name: p.Name, Value: p.Value}
		}
	}
	return out
}

func TestCollectLanguageOverrides_PerSourceForce(t *testing.T) {
	calls := []shadowSourceFilePropertiesCallStub{
		{
			Files: []string{"foo.c"},
			Properties: []shadowSourceFilePropertyStub{
				{Name: "LANGUAGE", Value: "CXX"},
			},
		},
	}
	got := collectLanguageOverrides(toShadowCalls(calls))
	if got["foo.c"] != "CXX" {
		t.Errorf("foo.c LANGUAGE: %q want CXX", got["foo.c"])
	}
}

func TestTagLanguageOverrides_TagsAffectedTargets(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c", "bar.c"},
		}},
	}
	byPath := map[string]string{"foo.c": "CXX"}
	tagLanguageOverrides(pkg, byPath)
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-language-override=CXX") {
		t.Errorf("Tags: %v should include language-override", pkg.Targets[0].Tags)
	}
}

// TestCollectGeneratedSources covers the GENERATED property
// (Phase 1 slice 1c): a source marked GENERATED TRUE surfaces in the
// set; non-truthy / absent values don't.
func TestCollectGeneratedSources(t *testing.T) {
	calls := []shadowSourceFilePropertiesCallStub{
		{
			Files: []string{"gen.c", "gen.h"},
			Properties: []shadowSourceFilePropertyStub{
				{Name: "GENERATED", Value: "TRUE"},
			},
		},
		{
			Files: []string{"plain.c"},
			Properties: []shadowSourceFilePropertyStub{
				{Name: "GENERATED", Value: "FALSE"},
			},
		},
	}
	got := collectGeneratedSources(toShadowCalls(calls))
	if !got["gen.c"] || !got["gen.h"] {
		t.Errorf("GENERATED sources should include gen.c, gen.h; got %v", got)
	}
	if got["plain.c"] {
		t.Errorf("GENERATED FALSE should not be in the set; got %v", got)
	}
}

// TestCollectPerSourceCompileDefinitions covers the
// COMPILE_DEFINITIONS decode: semicolon-split, empties dropped.
func TestCollectPerSourceCompileDefinitions(t *testing.T) {
	calls := []shadowSourceFilePropertiesCallStub{
		{
			Files: []string{"foo.c"},
			Properties: []shadowSourceFilePropertyStub{
				{Name: "COMPILE_DEFINITIONS", Value: "FOO=1;BAR;"},
			},
		},
	}
	got := collectPerSourceCompileDefinitions(toShadowCalls(calls))
	want := []string{"FOO=1", "BAR"}
	if len(got["foo.c"]) != len(want) {
		t.Fatalf("foo.c defines: got %v want %v", got["foo.c"], want)
	}
	for i, w := range want {
		if got["foo.c"][i] != w {
			t.Errorf("foo.c defines[%d]: got %q want %q", i, got["foo.c"][i], w)
		}
	}
}

// TestApplyPerSourceCompileDefinitions_UniformFolds covers the
// tractable case: every source carries the same COMPILE_DEFINITIONS,
// so they fold into the target's defines.
func TestApplyPerSourceCompileDefinitions_UniformFolds(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:    "foo",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"a.c", "b.c"},
			Defines: []string{"PRE"},
		}},
	}
	byPath := map[string][]string{
		"a.c": {"SHARED=1"},
		"b.c": {"SHARED=1"},
	}
	applyPerSourceCompileDefinitions(pkg, byPath)
	if !stringSliceContains(pkg.Targets[0].Defines, "SHARED=1") {
		t.Errorf("uniform define should fold into Defines; got %v", pkg.Targets[0].Defines)
	}
	if !stringSliceContains(pkg.Targets[0].Defines, "PRE") {
		t.Errorf("pre-existing define should be preserved; got %v", pkg.Targets[0].Defines)
	}
	for _, tag := range pkg.Targets[0].Tags {
		if tag == "cmake-per-source-compile-definitions-divergent" {
			t.Errorf("uniform case should NOT tag divergent; tags %v", pkg.Targets[0].Tags)
		}
	}
}

// TestApplyPerSourceCompileDefinitions_SingleSource covers the common
// trivial case: one source with COMPILE_DEFINITIONS folds in.
func TestApplyPerSourceCompileDefinitions_SingleSource(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"only.c"},
		}},
	}
	byPath := map[string][]string{"only.c": {"ONLY=2"}}
	applyPerSourceCompileDefinitions(pkg, byPath)
	if !stringSliceContains(pkg.Targets[0].Defines, "ONLY=2") {
		t.Errorf("single-source define should fold; got %v", pkg.Targets[0].Defines)
	}
}

// TestApplyPerSourceCompileDefinitions_DivergentTags covers the
// limitation: when sources within one target carry DIFFERENT defines,
// a single cc_library can't express it — we tag instead of folding
// (which would over-define the other sources).
func TestApplyPerSourceCompileDefinitions_DivergentTags(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"a.c", "b.c"},
		}},
	}
	byPath := map[string][]string{
		"a.c": {"AONLY=1"},
		"b.c": {"BONLY=1"},
	}
	applyPerSourceCompileDefinitions(pkg, byPath)
	if stringSliceContains(pkg.Targets[0].Defines, "AONLY=1") ||
		stringSliceContains(pkg.Targets[0].Defines, "BONLY=1") {
		t.Errorf("divergent defines must NOT fold; got %v", pkg.Targets[0].Defines)
	}
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-per-source-compile-definitions-divergent") {
		t.Errorf("divergent case should tag; tags %v", pkg.Targets[0].Tags)
	}
}

// TestApplyPerSourceCompileDefinitions_PartialIsDivergent covers the
// case where one source carries a define and another doesn't — the
// undefined source is an empty set, so folding would leak the define
// to it. Treated as divergent (tag, no fold).
func TestApplyPerSourceCompileDefinitions_PartialIsDivergent(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"a.c", "b.c"},
		}},
	}
	byPath := map[string][]string{"a.c": {"AONLY=1"}}
	applyPerSourceCompileDefinitions(pkg, byPath)
	if stringSliceContains(pkg.Targets[0].Defines, "AONLY=1") {
		t.Errorf("partial define must NOT fold (would leak to b.c); got %v", pkg.Targets[0].Defines)
	}
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-per-source-compile-definitions-divergent") {
		t.Errorf("partial case should tag divergent; tags %v", pkg.Targets[0].Tags)
	}
}

// TestLowerTarget_GeneratedSourceNotElided covers Phase 1 slice 1c's
// GENERATED handling: a source the trace marked GENERATED that isn't
// on disk must NOT be elided as a missing source (it's expected to be
// produced by a generator); the surviving cc_library keeps it in srcs
// and the target is tagged cmake-declared-generated-source. A
// genuinely-missing (non-GENERATED) source is still elided.
func TestLowerTarget_GeneratedSourceNotElided(t *testing.T) {
	hostSrc := t.TempDir()
	// Only real.c exists on disk; gen.c (GENERATED) and gone.c
	// (plain) are absent.
	if err := os.WriteFile(filepath.Join(hostSrc, "real.c"), []byte("int r(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tgt := &fileapi.Target{
		Name: "foo",
		Type: "STATIC_LIBRARY",
		Sources: []fileapi.TargetSource{
			{Path: "real.c", CompileGroupIndex: 0},
			{Path: "gen.c", CompileGroupIndex: 0},
			{Path: "gone.c", CompileGroupIndex: 0},
		},
		CompileGroups: []fileapi.CompileGroup{{
			Language:      "C",
			SourceIndexes: []int{0, 1, 2},
		}},
	}
	cc := &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}}
	generated := map[string]bool{"gen.c": true}

	irt, err := lowerTarget(tgt, hostSrc, "/build", hostSrc, "", true, nil, cc,
		map[string]string{}, map[string]bool{}, nil, nil,
		map[string]bool{}, nil, nil, nil, nil, nil,
		map[string]string{}, map[string][]string{}, nil, "", generated, nil)
	if err != nil {
		t.Fatalf("lowerTarget: %v", err)
	}
	if irt == nil {
		t.Fatal("lowerTarget returned nil")
	}
	if !stringSliceContains(irt.Srcs, "real.c") {
		t.Errorf("real.c should be in srcs; got %v", irt.Srcs)
	}
	if !stringSliceContains(irt.Srcs, "gen.c") {
		t.Errorf("GENERATED gen.c should be kept (not elided); got %v", irt.Srcs)
	}
	if stringSliceContains(irt.Srcs, "gone.c") {
		t.Errorf("plain missing gone.c should be elided; got %v", irt.Srcs)
	}
	if !stringSliceContains(irt.Tags, "cmake-declared-generated-source") {
		t.Errorf("expected cmake-declared-generated-source tag; got %v", irt.Tags)
	}
	if !stringSliceContains(irt.Tags, "cmake-elided-missing-source") {
		t.Errorf("expected cmake-elided-missing-source tag for gone.c; got %v", irt.Tags)
	}
}

func TestTagLanguageOverrides_OneTagPerLanguage(t *testing.T) {
	// Multiple sources forced to same language → single tag.
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"a.c", "b.c"},
		}},
	}
	byPath := map[string]string{"a.c": "CXX", "b.c": "CXX"}
	tagLanguageOverrides(pkg, byPath)
	count := 0
	for _, tag := range pkg.Targets[0].Tags {
		if tag == "cmake-codegen-language-override=CXX" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 tag; got %d (Tags: %v)", count, pkg.Targets[0].Tags)
	}
}
