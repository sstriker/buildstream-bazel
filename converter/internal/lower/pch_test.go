package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestLowerTarget_PCH_TagsTarget confirms targets with
// target_precompile_headers get the cmake-codegen-pch tag so
// operators can grep for the gap.
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
	for _, tgt := range pkg.Targets {
		if tgt.Name == "foo" {
			if !stringSliceContains(tgt.Tags, "cmake-codegen-pch") {
				t.Errorf("expected cmake-codegen-pch tag; got %v", tgt.Tags)
			}
			return
		}
	}
	t.Fatal("foo not in pkg.Targets")
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
