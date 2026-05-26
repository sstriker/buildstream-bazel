package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// TestLowerTarget_FindPackageAttrib_ManifestHit covers the case
// where a Link.CommandFragments path resolves via the
// find_package(X) attribution AND the imports manifest has an
// entry for `<Pkg>::<Pkg>` — the manifest's label is emitted as
// a real dep.
func TestLowerTarget_FindPackageAttrib_ManifestHit(t *testing.T) {
	target := &fileapi.Target{
		Name: "iostreams",
		Type: "STATIC_LIBRARY",
		Link: &fileapi.TargetLink{
			Language: "CXX",
			CommandFragments: []fileapi.CommandFragment{
				{Fragment: "/usr/lib/x86_64-linux-gnu/libz.so", Role: "libraries"},
			},
		},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"iostreams::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "iostreams::@", Name: "iostreams"}},
			}},
		},
	}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "zlib",
			Exports: []*manifest.Export{{
				CMakeTarget: "ZLIB::ZLIB",
				BazelLabel:  "@zlib//:zlib",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{
		Imports: imports,
		ConfigureLog: []fileapi.Event{{
			Kind: "find_package-v1",
			Found: &fileapi.EventFindPackageFound{
				IsFound: true, Package: "ZLIB",
			},
		}},
		CMakeVars: map[string]string{
			"ZLIB_LIBRARIES": "/usr/lib/x86_64-linux-gnu/libz.so",
		},
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var found *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "iostreams" {
			found = &pkg.Targets[i]
		}
	}
	if found == nil {
		t.Fatal("iostreams not in pkg.Targets")
	}
	if !stringSliceContains(found.Deps, "@zlib//:zlib") {
		t.Errorf("Deps should include @zlib//:zlib; got %v", found.Deps)
	}
	for _, tag := range found.Tags {
		if tag == "cmake-codegen-find-package-fallback=ZLIB=libz.so" {
			t.Errorf("manifest hit should not also emit fallback tag: %v", found.Tags)
		}
	}
}

// TestLowerTarget_FindPackageAttrib_NoManifest covers the
// fallback path: find_package attributes the lib to ZLIB but
// the imports manifest has no entry — emit a
// cmake-codegen-find-package-fallback tag so the gap is
// visible to the operator.
func TestLowerTarget_FindPackageAttrib_NoManifest(t *testing.T) {
	target := &fileapi.Target{
		Name: "iostreams",
		Type: "STATIC_LIBRARY",
		Link: &fileapi.TargetLink{
			Language: "CXX",
			CommandFragments: []fileapi.CommandFragment{
				{Fragment: "/usr/lib/x86_64-linux-gnu/libz.so", Role: "libraries"},
			},
		},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"iostreams::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "iostreams::@", Name: "iostreams"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{
		ConfigureLog: []fileapi.Event{{
			Kind: "find_package-v1",
			Found: &fileapi.EventFindPackageFound{
				IsFound: true, Package: "ZLIB",
			},
		}},
		CMakeVars: map[string]string{
			"ZLIB_LIBRARIES": "/usr/lib/x86_64-linux-gnu/libz.so",
		},
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var found *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "iostreams" {
			found = &pkg.Targets[i]
		}
	}
	if found == nil {
		t.Fatal("iostreams not in pkg.Targets")
	}
	if !stringSliceContains(found.Tags, "cmake-codegen-find-package-fallback=ZLIB=libz.so") {
		t.Errorf("Tags should include find-package fallback; got %v", found.Tags)
	}
}
