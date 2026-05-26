package lower

import (
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

// TestShouldSplitCompileGroups_LegacyAlias confirms the
// shouldSplitMultiLanguage backward-compat alias forwards
// to the new gate.
func TestShouldSplitCompileGroups_LegacyAlias(t *testing.T) {
	tgt := &fileapi.Target{
		CompileGroups: []fileapi.CompileGroup{
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "FOO=1"}}},
			{Language: "C", Defines: []fileapi.CompileDefine{{Define: "BAR=2"}}},
		},
	}
	if !shouldSplitMultiLanguage(tgt) {
		t.Errorf("legacy alias should defer to shouldSplitCompileGroups")
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
