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
// cmake_pch `-include` pair is rewritten IN PLACE to the synthesized
// mirror header's path (positional fidelity: the forced include keeps
// cmake's argv position relative to other flags), the mirror rule is
// registered with `#pragma GCC system_header` plus the declared
// #includes in order, and the mirror is staged into srcs.
func TestLowerTarget_PCH_ForcedIncludeLift(t *testing.T) {
	pkg, err := ToIR(pchReply(), &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	core := pchFindTarget(t, pkg, "core")
	want := []string{"-include", "cmake_pch/core/cmake_pch.hxx"}
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
		if strings.Contains(c, "CMakeFiles") {
			t.Errorf("cmake_pch build-dir artifact leaked into copts: %v", core.Copts)
		}
	}
	if !stringSliceContains(core.Tags, "cmake-codegen-pch") {
		t.Errorf("expected cmake-codegen-pch tag; got %v", core.Tags)
	}
	if !stringSliceContains(core.Srcs, "cmake_pch/core/cmake_pch.hxx") {
		t.Errorf("mirror header not staged in srcs: %v", core.Srcs)
	}
	mirror := pchFindTarget(t, pkg, "pch_cmake_pch_core_cmake_pch_hxx")
	if mirror.Kind != ir.KindWriteFile || mirror.WriteFileOut != "cmake_pch/core/cmake_pch.hxx" {
		t.Fatalf("mirror rule shape: kind=%v out=%q", mirror.Kind, mirror.WriteFileOut)
	}
	body := strings.Join(mirror.WriteFileContent, "\n")
	iSys := strings.Index(body, "#pragma GCC system_header")
	iPch := strings.Index(body, `#include "pch.h"`)
	iVec := strings.Index(body, "#include <vector>")
	iOther := strings.Index(body, `#include "other.h"`)
	if !(iSys >= 0 && iSys < iPch && iPch < iVec && iVec < iOther) {
		t.Errorf("mirror body must carry system_header then the declared includes in order; got:\n%s", body)
	}
	if !stringSliceContains(mirror.Tags, "cmake-codegen-pch") {
		t.Errorf("mirror rule missing the pch facet: %v", mirror.Tags)
	}
}

// TestLowerTarget_PCH_ReuseFromConsumer confirms the REUSE_FROM shape: the
// consumer's codemodel PrecompileHeaders is null, so the lift must resolve
// the owning target from the fragment's CMakeFiles/<owner>.dir segment and
// force-include the OWNER's mirror — sharing the owner's registered rule,
// not duplicating it — plus stage the mirror AND the source-tree headers
// it references (the consumer doesn't carry them in its own Sources), and
// tag it (before the lift this shape lost its forced include silently).
// The staging slot is SRCS, not hdrs: the header is a compile input of
// this rule's own TUs only, and hdrs would export it to dependents (the
// include-over-grant shape).
func TestLowerTarget_PCH_ReuseFromConsumer(t *testing.T) {
	pkg, err := ToIR(pchReply(), &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	user := pchFindTarget(t, pkg, "user")
	joined := strings.Join(user.Copts, " ")
	if !strings.Contains(joined, "-include cmake_pch/core/cmake_pch.hxx") {
		t.Errorf("REUSE_FROM consumer missing the owner's mirror include; copts = %v", user.Copts)
	}
	if strings.Contains(joined, "CMakeFiles") {
		t.Errorf("cmake_pch build-dir artifact leaked into consumer copts: %v", user.Copts)
	}
	if !stringSliceContains(user.Tags, "cmake-codegen-pch") {
		t.Errorf("REUSE_FROM consumer should carry the PCH tag; got %v", user.Tags)
	}
	for _, want := range []string{"cmake_pch/core/cmake_pch.hxx", "pch.h"} {
		if !stringSliceContains(user.Srcs, want) {
			t.Errorf("consumer srcs missing staged %q: %v", want, user.Srcs)
		}
	}
	if stringSliceContains(user.Hdrs, "pch.h") {
		t.Errorf("staged PCH header must not be exported via hdrs: %v", user.Hdrs)
	}
	// Exactly ONE mirror rule serves both the owner and the consumer.
	n := 0
	for _, tgt := range pkg.Targets {
		if tgt.WriteFileOut == "cmake_pch/core/cmake_pch.hxx" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("mirror rule count = %d, want exactly 1 (shared)", n)
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

// TestPCHIncludeLine pins the per-entry mapping of codemodel
// precompileHeaders shapes onto the mirror header's #include lines.
func TestPCHIncludeLine(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantLine  string
		wantStage string
	}{
		{"source-tree absolute", "/src/inc/pch.h", `#include "inc/pch.h"`, "inc/pch.h"},
		{"angle system header", "<vector>", "#include <vector>", ""},
		{"verbatim quoted", `"other.h"`, `#include "other.h"`, ""},
		{"build-dir generated", "/b/gen/config.h", `#include "gen/config.h"`, ""},
		{"out-of-tree absolute", "/usr/include/zlib.h", `#include "/usr/include/zlib.h"`, ""},
		{"bare relative (unresolved genex)", "dbg.h", `#include "dbg.h"`, ""},
		{"empty", "", "", ""},
	}
	c := pchLiftCtx{cmakeSrc: "/src", cmakeBuild: "/b"}
	for _, tc := range cases {
		line, stage := c.includeLine(tc.header)
		if line != tc.wantLine || stage != tc.wantStage {
			t.Errorf("%s: includeLine(%q) = (%q, %q), want (%q, %q)",
				tc.name, tc.header, line, stage, tc.wantLine, tc.wantStage)
		}
	}
}

// TestPCHIncludeLine_Reanchor confirms the umbrella-promotion reanchor is
// applied to source-tree entries (LLVM shape: labels rooted above cmakeSrc).
func TestPCHIncludeLine_Reanchor(t *testing.T) {
	c := pchLiftCtx{
		cmakeSrc:   "/src",
		cmakeBuild: "/b",
		reanchor:   func(rel string) string { return "llvm/" + rel },
	}
	line, stage := c.includeLine("/src/pch.h")
	if line != `#include "llvm/pch.h"` || stage != "llvm/pch.h" {
		t.Errorf("includeLine with reanchor = (%q, %q), want (#include \"llvm/pch.h\", llvm/pch.h)", line, stage)
	}
}

// TestPCHIncludeLine_PackagePath confirms in-element mirror #includes carry
// the exec-root form when the element lands in a subpackage
// (--bazel-package-path): compile actions run from the exec root and the
// mirror's quote-includes resolve relative to it (SDL's
// `src/SDL_internal.h` under the build lens's elements/sdl mount). The
// staged hdr stays element-relative (it's a package-level srcs entry).
func TestPCHIncludeLine_PackagePath(t *testing.T) {
	c := pchLiftCtx{cmakeSrc: "/src", cmakeBuild: "/b", pkgPath: "elements/sdl"}
	line, stage := c.includeLine("/src/src/SDL_internal.h")
	if line != `#include "elements/sdl/src/SDL_internal.h"` || stage != "src/SDL_internal.h" {
		t.Errorf("includeLine with pkgPath = (%q, %q)", line, stage)
	}
	if line, _ := c.includeLine("/b/gen/config.h"); line != `#include "elements/sdl/gen/config.h"` {
		t.Errorf("build-dir includeLine with pkgPath = %q", line)
	}
	// Out-of-tree absolutes and angle/verbatim/bare entries never get the
	// package prefix — they don't name in-element files.
	if line, _ := c.includeLine("/usr/include/zlib.h"); line != `#include "/usr/include/zlib.h"` {
		t.Errorf("out-of-tree includeLine with pkgPath = %q", line)
	}
	if line, _ := c.includeLine("<vector>"); line != "#include <vector>" {
		t.Errorf("angle includeLine with pkgPath = %q", line)
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
	lowerMultiConfigDeltas(pkg, byCfg, []string{"Release", "Debug"}, "/src", "/b", nil, nil)
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

// TestSplitCompileFragments_ForcedIncludePairs pins the atomic-pair
// handling: (a) repeated NON-PCH forced includes keep both pairs in
// order — the generic per-flag dedup used to swallow the second
// `-include` and leave its argument as an orphan bare token; (b) the
// cmake_pch pair stays IN PLACE (positional fidelity) and is surfaced
// via pchArtifacts; (c) identical pairs dedup at pair granularity.
func TestSplitCompileFragments_ForcedIncludePairs(t *testing.T) {
	frags := []fileapi.CommandFragment{
		{Fragment: "-include first.h -O2 -include /b/CMakeFiles/core.dir/cmake_pch.hxx -include second.h -include first.h"},
	}
	copts, _, arts := splitCompileFragments(frags)
	want := []string{
		"-include", "first.h",
		"-O2",
		"-include", "/b/CMakeFiles/core.dir/cmake_pch.hxx",
		"-include", "second.h",
	}
	if !reflect.DeepEqual(copts, want) {
		t.Errorf("copts = %v, want %v", copts, want)
	}
	if len(arts) != 1 || arts[0] != "/b/CMakeFiles/core.dir/cmake_pch.hxx" {
		t.Errorf("pchArtifacts = %v, want the cmake_pch path", arts)
	}
}

// TestLowerTarget_PCH_PositionalMirror confirms the mirror's -include
// lands at the cmake_pch pair's ORIGINAL argv position — a target that
// also adds its own non-PCH forced include keeps cmake's forced-include
// processing order (one forced header may depend on the other's macros).
func TestLowerTarget_PCH_PositionalMirror(t *testing.T) {
	r := pchReply()
	core := r.Targets["core::@"]
	core.CompileGroups[0].CompileCommandFragments = []fileapi.CommandFragment{
		{Fragment: "-include /b/CMakeFiles/core.dir/cmake_pch.hxx -include own_forced.h -O2"},
	}
	r.Targets["core::@"] = core
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pchFindTarget(t, pkg, "core")
	joined := strings.Join(tgt.Copts, " ")
	iMirror := strings.Index(joined, "-include cmake_pch/core/cmake_pch.hxx")
	iOwn := strings.Index(joined, "-include own_forced.h")
	if !(iMirror >= 0 && iOwn >= 0 && iMirror < iOwn) {
		t.Errorf("mirror include must PRECEDE the target's own forced include (cmake's order); copts = %v", tgt.Copts)
	}
}

// TestPerConfigPCHArms covers the config-varying declared list: the
// baseline mirror (primary list) moves into the primary config's arm,
// each divergent config gets its own mirror in its arm, and every
// mirror stages into srcs. The non-divergent case (same list, only the
// artifact path differing per config) must stay entirely on the
// baseline.
func TestPerConfigPCHArms(t *testing.T) {
	mk := func(cfg string, headers ...string) fileapi.Target {
		var entries []fileapi.CompilePCH
		for _, h := range headers {
			entries = append(entries, fileapi.CompilePCH{Header: h})
		}
		return fileapi.Target{
			Name: "foo",
			CompileGroups: []fileapi.CompileGroup{{
				Language:          "CXX",
				PrecompileHeaders: entries,
				CompileCommandFragments: []fileapi.CommandFragment{
					{Fragment: "-include /b/CMakeFiles/foo.dir/" + cfg + "/cmake_pch.hxx"},
				},
			}},
		}
	}
	newCtx := func() pchLiftCtx {
		return pchLiftCtx{cmakeSrc: "/src", cmakeBuild: "/b", cc: newCodegenContext()}
	}

	// Divergent lists: Debug adds dbg.h.
	pch := newCtx()
	baseRel, _ := pch.ensureMirror("foo", "CXX", "", []fileapi.CompilePCH{{Header: `"common.h"`}})
	tgt := &ir.Target{Name: "foo", Kind: ir.KindCCLibrary,
		Copts: []string{"-include", baseRel, "-O2"}, Srcs: []string{baseRel}}
	views := map[string]fileapi.Target{
		"Release": mk("Release", `"common.h"`),
		"Debug":   mk("Debug", `"common.h"`, `"dbg.h"`),
	}
	perConfigPCHArms(tgt, views, []string{"Release", "Debug"}, map[string]map[string]fileapi.Target{"foo::@": views}, map[string]string{"foo": "foo::@"}, pch)
	if strings.Contains(strings.Join(tgt.Copts, " "), "-include") {
		t.Errorf("baseline pair must move into the arms; copts = %v", tgt.Copts)
	}
	arms := tgt.PerPlatform["copts"]
	if got := strings.Join(arms["//config:release"], " "); got != "-include cmake_pch/foo/cmake_pch.hxx" {
		t.Errorf("release arm = %q, want the baseline mirror pair", got)
	}
	if got := strings.Join(arms["//config:debug"], " "); got != "-include cmake_pch/foo/Debug/cmake_pch.hxx" {
		t.Errorf("debug arm = %q, want the per-config mirror pair", got)
	}
	if !stringSliceContains(tgt.Srcs, "cmake_pch/foo/Debug/cmake_pch.hxx") {
		t.Errorf("debug mirror not staged: %v", tgt.Srcs)
	}
	if _, ok := pch.cc.OutToGenrule["cmake_pch/foo/Debug/cmake_pch.hxx"]; !ok {
		t.Error("debug mirror rule not registered")
	}

	// Non-divergent lists: same headers, config-varying artifact path only.
	pch2 := newCtx()
	base2, _ := pch2.ensureMirror("foo", "CXX", "", []fileapi.CompilePCH{{Header: `"common.h"`}})
	tgt2 := &ir.Target{Name: "foo", Kind: ir.KindCCLibrary, Copts: []string{"-include", base2}}
	views2 := map[string]fileapi.Target{
		"Release": mk("Release", `"common.h"`),
		"Debug":   mk("Debug", `"common.h"`),
	}
	perConfigPCHArms(tgt2, views2, []string{"Release", "Debug"}, map[string]map[string]fileapi.Target{"foo::@": views2}, map[string]string{"foo": "foo::@"}, pch2)
	if len(tgt2.PerPlatform) != 0 {
		t.Errorf("non-divergent lists must not produce arms: %v", tgt2.PerPlatform)
	}
	if !reflect.DeepEqual(tgt2.Copts, []string{"-include", base2}) {
		t.Errorf("non-divergent baseline must stay; copts = %v", tgt2.Copts)
	}
}

// TestLowerTarget_PCH_CreatorGroupExcluded is the test-binary shape: a
// PCH-declaring EXECUTABLE doesn't split compile groups, and cmake's
// codemodel lists the PCH-CREATOR group (the cmake_pch.hxx.cxx compile,
// fragments carrying `-x c++-header`) FIRST. Selecting it as the
// primary group used to leak `-x c++-header` into the rule's copts —
// compiling every project TU as a header. The creator group must be
// skipped (firstRealCompileGroup), its machinery sources dropped
// silently (no misleading elided-source audit tag), and the real
// group's mirror lift ride as usual.
func TestLowerTarget_PCH_CreatorGroupExcluded(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/b"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "unit_test::@1", Name: "unit_test"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"unit_test::@1": {
				Name: "unit_test",
				Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{
					{Path: "/b/CMakeFiles/unit_test.dir/cmake_pch.hxx.cxx", CompileGroupIndex: 0},
					{Path: "test_main.cpp", CompileGroupIndex: 1},
					{Path: "/b/CMakeFiles/unit_test.dir/cmake_pch.hxx"},
				},
				CompileGroups: []fileapi.CompileGroup{
					{
						Language:      "CXX",
						SourceIndexes: []int{0},
						CompileCommandFragments: []fileapi.CommandFragment{
							{Fragment: "-Winvalid-pch -x c++-header -include /b/CMakeFiles/unit_test.dir/cmake_pch.hxx"},
						},
						PrecompileHeaders: []fileapi.CompilePCH{{Header: "/src/tpch.h"}},
					},
					{
						Language:      "CXX",
						SourceIndexes: []int{1},
						CompileCommandFragments: []fileapi.CommandFragment{
							{Fragment: "-Winvalid-pch -include /b/CMakeFiles/unit_test.dir/cmake_pch.hxx"},
						},
						PrecompileHeaders: []fileapi.CompilePCH{{Header: "/src/tpch.h"}},
					},
				},
			},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pchFindTarget(t, pkg, "unit_test")
	joined := strings.Join(tgt.Copts, " ")
	if strings.Contains(joined, "c++-header") || strings.Contains(joined, "-x ") {
		t.Errorf("PCH-creator fragments leaked into copts: %v", tgt.Copts)
	}
	if !strings.Contains(joined, "-include cmake_pch/unit_test/cmake_pch.hxx") {
		t.Errorf("real group's mirror include missing: %v", tgt.Copts)
	}
	for _, s := range tgt.Srcs {
		if strings.Contains(s, "CMakeFiles") {
			t.Errorf("PCH machinery source leaked into srcs: %v", tgt.Srcs)
		}
	}
	if stringSliceContains(tgt.Tags, "cmake-elided-build-dir-source") {
		t.Errorf("machinery sources must skip silently, not stamp the elided audit tag: %v", tgt.Tags)
	}
	// Creator + one real group must NOT split: one rule, no _cxx_0 subs.
	for _, other := range pkg.Targets {
		if strings.HasPrefix(other.Name, "unit_test_cxx") {
			t.Errorf("creator group forced a spurious split: %s", other.Name)
		}
	}
}

// TestLowerTarget_PCH_ReuseFromEdgeDrops pins the REUSE_FROM dependency
// routing: cmake records the owner as a plain target dependency (no
// backtrace), detectable only as "the dep is the owner of a cmake_pch
// artifact my fragments force-include" (pchReuseFromOwners). The edge
// DROPS: the consumer's real input is the owner's mirror FILE (staged
// via srcs), deps would be illegal for an executable-kind owner, and
// even data poisons (a cc_test owner is implicitly testonly, which a
// non-test consumer can't reference).
func TestLowerTarget_PCH_ReuseFromEdgeDrops(t *testing.T) {
	parent := 0
	g := fileapi.BacktraceGraph{
		Commands: []string{"target_precompile_headers"},
		Files:    []string{"CMakeLists.txt"},
		Nodes: []fileapi.BacktraceNode{
			{File: 0, Line: 0, Command: -1},
			{File: 0, Line: 9, Command: 0, Parent: &parent},
		},
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/b"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Id: "unit_test::@1", Name: "unit_test"},
					{Id: "tool::@1", Name: "tool"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"unit_test::@1": {
				Name: "unit_test", Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{{Path: "test_main.cpp", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX", SourceIndexes: []int{0},
					PrecompileHeaders: []fileapi.CompilePCH{{Header: "/src/tpch.h"}},
				}},
			},
			"tool::@1": {
				Name: "tool", Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{{Path: "tool_main.cpp", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX", SourceIndexes: []int{0},
					CompileCommandFragments: []fileapi.CommandFragment{
						{Fragment: "-include /b/CMakeFiles/unit_test.dir/cmake_pch.hxx"},
					},
				}},
				Dependencies:   []fileapi.TargetDependency{{Id: "unit_test::@1", Backtrace: 1}},
				BacktraceGraph: g,
			},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tool := pchFindTarget(t, pkg, "tool")
	if stringSliceContains(tool.Deps, ":unit_test") {
		t.Errorf("REUSE_FROM owner must not land in deps (illegal for executable kinds): %v", tool.Deps)
	}
	if stringSliceContains(tool.Data, ":unit_test") {
		t.Errorf("REUSE_FROM owner must not land in data either (testonly poisoning from a cc_test owner): %v", tool.Data)
	}
	if !strings.Contains(strings.Join(tool.Copts, " "), "-include cmake_pch/unit_test/cmake_pch.hxx") {
		t.Errorf("REUSE_FROM consumer missing the owner's mirror: %v", tool.Copts)
	}
}
