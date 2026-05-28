package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestLowerMultiConfigDeltas_PopulatesPerPlatform(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
		}},
	}
	byCfg := map[string]map[string]fileapi.Target{
		"foo::@": {
			"Release": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines:  []fileapi.CompileDefine{{Define: "NDEBUG"}},
				}},
			},
			"Debug": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines:  []fileapi.CompileDefine{{Define: "DEBUG=1"}},
				}},
			},
		},
	}
	lowerMultiConfigDeltas(pkg, byCfg, []string{"Release", "Debug"}, "", "")
	tgt := pkg.Targets[0]
	if tgt.PerPlatform == nil {
		t.Fatalf("PerPlatform should be populated; got nil")
	}
	defines := tgt.PerPlatform["defines"]
	if defines == nil {
		t.Fatalf("defines arm missing: %v", tgt.PerPlatform)
	}
	if got := defines["//config:release"]; len(got) != 1 || got[0] != "NDEBUG" {
		t.Errorf("Release arm: %v", got)
	}
	if got := defines["//config:debug"]; len(got) != 1 || got[0] != "DEBUG=1" {
		t.Errorf("Debug arm: %v", got)
	}
}

func TestLowerMultiConfigDeltas_StripsCompileFragmentPrefix(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	byCfg := map[string]map[string]fileapi.Target{
		"foo::@": {
			"Release": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language:                "C",
					CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O3"}},
				}},
			},
			"Debug": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language:                "C",
					CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O0"}},
				}},
			},
		},
	}
	lowerMultiConfigDeltas(pkg, byCfg, []string{"Release", "Debug"}, "", "")
	copts := pkg.Targets[0].PerPlatform["copts"]
	// "-O3" should not appear as "C|-O3" in the select arm — the
	// language disambiguator is for configfold's bookkeeping, not
	// for emit.
	if got := copts["//config:release"]; len(got) != 1 || got[0] != "-O3" {
		t.Errorf("Release copts: %v", got)
	}
}

func TestLowerMultiConfigDeltas_SkipsSanitizerConfigs(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	byCfg := map[string]map[string]fileapi.Target{
		"foo::@": {
			"Release": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines:  []fileapi.CompileDefine{{Define: "RELEASE=1"}},
				}},
			},
			"ASan": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines:  []fileapi.CompileDefine{{Define: "ASAN_BUILD=1"}},
				}},
			},
		},
	}
	// Only Release survives the sanitizer filter; len(configs) == 1
	// → no useful Partition; PerPlatform stays nil.
	lowerMultiConfigDeltas(pkg, byCfg, []string{"Release", "ASan"}, "", "")
	if pkg.Targets[0].PerPlatform != nil {
		t.Errorf("sanitizer-only multi-config should not populate PerPlatform; got %v",
			pkg.Targets[0].PerPlatform)
	}
}

func TestLowerMultiConfigDeltas_SkipsEmptyByConfig(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}},
	}
	lowerMultiConfigDeltas(pkg, nil, []string{"Release"}, "", "")
	if pkg.Targets[0].PerPlatform != nil {
		t.Errorf("single-config / empty byConfig should be a no-op")
	}
}

func TestLowerMultiConfigDeltas_TargetMissingFromPackage(t *testing.T) {
	// Phantom target case: codemodel has the target but pkg.Targets
	// doesn't (e.g. dropped during lowering). The Phase 5 helper
	// shouldn't add it — the missing-from-pkg condition gates the
	// match.
	pkg := &ir.Package{Targets: []ir.Target{}}
	byCfg := map[string]map[string]fileapi.Target{
		"ghost::@": {
			"Release": fileapi.Target{
				Name: "ghost",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines:  []fileapi.CompileDefine{{Define: "X"}},
				}},
			},
			"Debug": fileapi.Target{
				Name: "ghost",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines:  []fileapi.CompileDefine{{Define: "Y"}},
				}},
			},
		},
	}
	lowerMultiConfigDeltas(pkg, byCfg, []string{"Release", "Debug"}, "", "")
	if len(pkg.Targets) != 0 {
		t.Errorf("ghost target shouldn't be added; got %v", pkg.Targets)
	}
}
