package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
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
