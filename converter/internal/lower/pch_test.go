package lower

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// pchReply builds a Reply with one declaring target ("core") whose CXX
// compile group carries the three codemodel precompileHeaders shapes (an
// absolute source-tree path, the angle form, the verbatim-quoted form) plus
// the `-include <build>/CMakeFiles/core.dir/cmake_pch.hxx` fragment cmake
// emits, and one REUSE_FROM consumer ("user") whose PrecompileHeaders is
// null — its PCH arrives ONLY via the owner-dir fragment, the shape cmake
// produces for target_precompile_headers(user REUSE_FROM core).
func pchReply() *fileapi.Reply {
	return &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/b"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Id: "core::@", Name: "core"},
					{Id: "user::@", Name: "user"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"core::@": {
				Name: "core",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "core.cpp", CompileGroupIndex: 0},
					{Path: "/src/pch.h"},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
					CompileCommandFragments: []fileapi.CommandFragment{
						{Fragment: "-O2 -Winvalid-pch -include /b/CMakeFiles/core.dir/cmake_pch.hxx"},
					},
					PrecompileHeaders: []fileapi.CompilePCH{
						{Header: "/src/pch.h"},
						{Header: "<vector>"},
						{Header: `"other.h"`},
					},
				}},
			},
			"user::@": {
				Name: "user",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "user.cpp", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
					CompileCommandFragments: []fileapi.CommandFragment{
						{Fragment: "-O2 -Winvalid-pch -include /b/CMakeFiles/core.dir/cmake_pch.hxx"},
					},
				}},
			},
		},
	}
}

func pchFindTarget(t *testing.T, pkg *ir.Package, name string) *ir.Target {
	t.Helper()
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == name {
			return &pkg.Targets[i]
		}
	}
	t.Fatalf("%s not in pkg.Targets", name)
	return nil
}

// TestLowerTarget_PCH_ForcedIncludeLift confirms the declaring target's
// cmake_pch `-include` pair is replaced by the declared header list, in
// order, with each codemodel entry shape mapped onto a workable `-include`
// argument: source-tree absolute → element-relative (and staged), angle →
// bare name riding the include search chain, verbatim-quoted → unquoted.
func TestLowerTarget_PCH_ForcedIncludeLift(t *testing.T) {
	pkg, err := ToIR(pchReply(), &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	core := pchFindTarget(t, pkg, "core")
	want := []string{"-include", "pch.h", "-include", "vector", "-include", "other.h"}
	var got []string
	for i := 0; i < len(core.Copts); i++ {
		if core.Copts[i] == "-include" && i+1 < len(core.Copts) {
			got = append(got, core.Copts[i], core.Copts[i+1])
			i++
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forced-include copts = %v, want %v (full copts: %v)", got, want, core.Copts)
	}
	for _, c := range core.Copts {
		if strings.Contains(c, "cmake_pch") {
			t.Errorf("cmake_pch artifact leaked into copts: %v", core.Copts)
		}
	}
	if !stringSliceContains(core.Tags, "cmake-codegen-pch") {
		t.Errorf("expected cmake-codegen-pch tag; got %v", core.Tags)
	}
	if !stringSliceContains(core.Hdrs, "pch.h") {
		t.Errorf("source-tree PCH header not staged in Hdrs: %v", core.Hdrs)
	}
}

// TestLowerTarget_PCH_ReuseFromConsumer confirms the REUSE_FROM shape: the
// consumer's codemodel PrecompileHeaders is null, so the lift must resolve
// the owning target from the fragment's CMakeFiles/<owner>.dir segment and
// expand the OWNER's declared list — plus stage the source-tree headers the
// consumer doesn't carry in its own Sources, and tag it (before the lift
// this shape lost its forced include silently, with no tag at all).
func TestLowerTarget_PCH_ReuseFromConsumer(t *testing.T) {
	pkg, err := ToIR(pchReply(), &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	user := pchFindTarget(t, pkg, "user")
	joined := strings.Join(user.Copts, " ")
	if !strings.Contains(joined, "-include pch.h") {
		t.Errorf("REUSE_FROM consumer missing owner's forced include; copts = %v", user.Copts)
	}
	if strings.Contains(joined, "cmake_pch") {
		t.Errorf("cmake_pch artifact leaked into consumer copts: %v", user.Copts)
	}
	if !stringSliceContains(user.Tags, "cmake-codegen-pch") {
		t.Errorf("REUSE_FROM consumer should carry the PCH tag; got %v", user.Tags)
	}
	if !stringSliceContains(user.Hdrs, "pch.h") {
		t.Errorf("owner's source-tree PCH header not staged on consumer: %v", user.Hdrs)
	}
}

// TestLowerTarget_PCH_TagsTarget confirms targets with
// target_precompile_headers get the cmake-codegen-pch tag even without a
// cmake_pch compile fragment (codemodel-declared shape alone).
func TestLowerTarget_PCH_TagsTarget(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX",
					PrecompileHeaders: []fileapi.CompilePCH{
						{Header: "stdafx.h"},
					},
				}},
			},
		},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "foo::@", Name: "foo"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	foo := pchFindTarget(t, pkg, "foo")
	if !stringSliceContains(foo.Tags, "cmake-codegen-pch") {
		t.Errorf("expected cmake-codegen-pch tag; got %v", foo.Tags)
	}
}

// TestLowerTarget_NoPCH_NoTag confirms targets without
// target_precompile_headers don't get the tag (no false-positive).
func TestLowerTarget_NoPCH_NoTag(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX",
				}},
			},
		},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "foo::@", Name: "foo"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "foo" && stringSliceContains(tgt.Tags, "cmake-codegen-pch") {
			t.Errorf("unexpected cmake-codegen-pch tag: %v", tgt.Tags)
		}
	}
}

// TestPCHIncludeArg pins the per-entry mapping of codemodel
// precompileHeaders shapes onto `-include` arguments.
func TestPCHIncludeArg(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantArg   string
		wantStage string
	}{
		{"source-tree absolute", "/src/inc/pch.h", "inc/pch.h", "inc/pch.h"},
		{"angle system header", "<vector>", "vector", ""},
		{"verbatim quoted", `"other.h"`, "other.h", ""},
		{"build-dir generated", "/b/gen/config.h", "gen/config.h", ""},
		{"out-of-tree absolute", "/usr/include/zlib.h", "/usr/include/zlib.h", ""},
		{"bare relative (unresolved genex)", "dbg.h", "dbg.h", ""},
		{"empty", "", "", ""},
	}
	c := pchLiftCtx{cmakeSrc: "/src", cmakeBuild: "/b"}
	for _, tc := range cases {
		arg, stage := c.includeArg(tc.header)
		if arg != tc.wantArg || stage != tc.wantStage {
			t.Errorf("%s: includeArg(%q) = (%q, %q), want (%q, %q)",
				tc.name, tc.header, arg, stage, tc.wantArg, tc.wantStage)
		}
	}
}

// TestPCHIncludeArg_Reanchor confirms the umbrella-promotion reanchor is
// applied to source-tree entries (LLVM shape: labels rooted above cmakeSrc).
func TestPCHIncludeArg_Reanchor(t *testing.T) {
	c := pchLiftCtx{
		cmakeSrc:   "/src",
		cmakeBuild: "/b",
		reanchor:   func(rel string) string { return "llvm/" + rel },
	}
	arg, stage := c.includeArg("/src/pch.h")
	if arg != "llvm/pch.h" || stage != "llvm/pch.h" {
		t.Errorf("includeArg with reanchor = (%q, %q), want (llvm/pch.h, llvm/pch.h)", arg, stage)
	}
}

// TestPCHIncludeArg_PackagePath confirms in-element `-include` arguments
// carry the exec-root form when the element lands in a subpackage
// (--bazel-package-path): compile actions run from the exec root and copts
// are verbatim — unlike the `includes` attribute, which Bazel
// package-prefixes — so a bare element-relative path would fail to resolve
// (SDL's `src/SDL_internal.h` under the build lens's elements/sdl mount).
// The staged hdr stays element-relative (it's a package-level Hdrs entry).
func TestPCHIncludeArg_PackagePath(t *testing.T) {
	c := pchLiftCtx{cmakeSrc: "/src", cmakeBuild: "/b", pkgPath: "elements/sdl"}
	arg, stage := c.includeArg("/src/src/SDL_internal.h")
	if arg != "elements/sdl/src/SDL_internal.h" || stage != "src/SDL_internal.h" {
		t.Errorf("includeArg with pkgPath = (%q, %q), want (elements/sdl/src/SDL_internal.h, src/SDL_internal.h)", arg, stage)
	}
	if arg, _ := c.includeArg("/b/gen/config.h"); arg != "elements/sdl/gen/config.h" {
		t.Errorf("build-dir includeArg with pkgPath = %q, want elements/sdl/gen/config.h", arg)
	}
	// Out-of-tree absolutes and angle/verbatim/bare entries never get the
	// package prefix — they don't name in-element files.
	if arg, _ := c.includeArg("/usr/include/zlib.h"); arg != "/usr/include/zlib.h" {
		t.Errorf("out-of-tree includeArg with pkgPath = %q, want /usr/include/zlib.h", arg)
	}
	if arg, _ := c.includeArg("<vector>"); arg != "vector" {
		t.Errorf("angle includeArg with pkgPath = %q, want vector", arg)
	}
}

// TestFilterPCHCoptArm pins the multi-config arm cleanup: the per-config
// cmake_pch path token is always stripped; the bare `-include` flag is
// stripped only when nothing in the arm could be its argument (a genuine
// per-config forced include keeps its pair).
func TestFilterPCHCoptArm(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"pch pair dropped",
			[]string{"-include", "/b/CMakeFiles/t.dir/Debug/cmake_pch.hxx"},
			nil,
		},
		{
			"pch dropped, real forced include kept",
			[]string{"-include", "/b/CMakeFiles/t.dir/Debug/cmake_pch.hxx", "config_debug.h"},
			[]string{"-include", "config_debug.h"},
		},
		{
			"no pch: untouched",
			[]string{"-O0", "-include", "config_debug.h"},
			[]string{"-O0", "-include", "config_debug.h"},
		},
		{
			"pch artifact without flag",
			[]string{"-O0", "/b/CMakeFiles/t.dir/Release/cmake_pch.h"},
			[]string{"-O0"},
		},
	}
	for _, tc := range cases {
		in := append([]string(nil), tc.in...)
		got := filterPCHCoptArm(in)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: filterPCHCoptArm(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestLowerMultiConfigDeltas_StripsPCHArmTokens runs the arm cleanup through
// the real fold: per-config cmake_pch paths differ per cell (the
// `<Config>/cmake_pch.hxx` segment), so without the filter they'd land as
// raw convert-time build-dir tokens in the //config:* copts select arms.
func TestLowerMultiConfigDeltas_StripsPCHArmTokens(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	byCfg := map[string]map[string]fileapi.Target{
		"foo::@": {
			"Release": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX",
					CompileCommandFragments: []fileapi.CommandFragment{
						{Fragment: "-O3 -include /b/CMakeFiles/foo.dir/Release/cmake_pch.hxx"},
					},
				}},
			},
			"Debug": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX",
					CompileCommandFragments: []fileapi.CommandFragment{
						{Fragment: "-O0 -include /b/CMakeFiles/foo.dir/Debug/cmake_pch.hxx"},
					},
				}},
			},
		},
	}
	lowerMultiConfigDeltas(pkg, byCfg, []string{"Release", "Debug"}, "/src", "/b", nil)
	copts := pkg.Targets[0].PerPlatform["copts"]
	if copts == nil {
		t.Fatalf("copts arms missing: %v", pkg.Targets[0].PerPlatform)
	}
	for cell, arm := range copts {
		for _, v := range arm {
			if strings.Contains(v, "cmake_pch") || v == "-include" {
				t.Errorf("%s arm leaked PCH machinery: %v", cell, arm)
			}
		}
	}
	if got := copts["//config:release"]; len(got) != 1 || got[0] != "-O3" {
		t.Errorf("Release arm = %v, want [-O3]", got)
	}
}
