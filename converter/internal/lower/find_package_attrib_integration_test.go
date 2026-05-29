package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// TestLowerTarget_LinkLibraryRedirect covers B: a host-resolved link
// fragment (/usr/lib/.../libfoo.so) with no find_package attribution
// and no <Pkg>::<Pkg> manifest entry — the variable-only Find module
// case. When a producer element claims the lib name via link_libraries,
// the fragment redirects to the producer's label instead of linking the
// host -lfoo.
func TestLowerTarget_LinkLibraryRedirect(t *testing.T) {
	target := &fileapi.Target{
		Name: "consumer",
		Type: "STATIC_LIBRARY",
		Link: &fileapi.TargetLink{
			Language: "C",
			CommandFragments: []fileapi.CommandFragment{
				{Fragment: "/usr/lib/x86_64-linux-gnu/libfoo.so", Role: "libraries"},
			},
		},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"consumer::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "consumer::@", Name: "consumer"}},
			}},
		},
	}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "foo",
			Exports: []*manifest.Export{{
				CMakeTarget:   "Foo::foo",
				BazelLabel:    "//elements/foo:foo",
				LinkLibraries: []string{"foo"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// No ConfigureLog / CMakeVars: find_package attribution misses, so
	// the fragment reaches the systemLibName fallback where the B
	// redirect fires.
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: imports})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var found *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "consumer" {
			found = &pkg.Targets[i]
		}
	}
	if found == nil {
		t.Fatal("consumer not in pkg.Targets")
	}
	if !stringSliceContains(found.Deps, "//elements/foo:foo") {
		t.Errorf("Deps should include //elements/foo:foo via link-library redirect; got %v", found.Deps)
	}
	if stringSliceContains(found.LinkOpts, "-lfoo") {
		t.Errorf("LinkOpts should not contain -lfoo (redirected to the producer element); got %v", found.LinkOpts)
	}
}

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

// TestLowerTarget_FindPackageAttrib_ManifestProvided_AttributionMissed
// covers the dual to TestLowerTarget_FindPackageAttrib_NoManifest:
// the imports manifest IS provided (operator opted into
// find_package attribution) BUT neither attribution source can
// recover the package name — configureLog has no find_package-v1
// event AND cmakeVars carries no `<Pkg>_FOUND`. findPackageAttrib
// returns nil, .Lookup() returns "", and the link fragment would
// otherwise fall through to the generic cmake-elided-link-fragment
// tag without any find_package-specific signal. The new
// cmake-codegen-find-package-attribution-missed tag fires here so
// the audit framework can surface the gap with the right operator
// remediation (re-run with --dump-vars=true OR add the lib's
// link-path to the manifest entry).
func TestLowerTarget_FindPackageAttrib_ManifestProvided_AttributionMissed(t *testing.T) {
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
	// Manifest carries a ZLIB::ZLIB entry — but with no
	// link_libraries / link_paths binding, so the manifest's
	// per-path index won't find /usr/lib/.../libz.so. The
	// per-cmake-target index will still resolve ZLIB::ZLIB if
	// the lower could attribute the path back to ZLIB. With
	// neither configureLog nor cmakeVars feeding
	// findPackageAttrib, that attribution can't happen — so
	// the path falls through to the elided + attribution-
	// missed tag pair.
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
		// Deliberately empty — the dual case.
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
	if !stringSliceContains(found.Tags, "cmake-codegen-find-package-attribution-missed=libz.so") {
		t.Errorf("Tags should include attribution-missed; got %v", found.Tags)
	}
	// The cmake-elided-link-fragment tag previously fired
	// alongside the attribution-missed tag, but the system-lib
	// lift now routes /usr/lib*-paths to `-lz` linkopts
	// instead of eliding (the linker resolves system libs via
	// the toolchain's default library search path). The
	// attribution-missed tag still fires because the operator
	// asked for the imports-manifest path AND find_package
	// missed — the lift doesn't replace that signal, it only
	// upgrades the elision shape.
	if !stringSliceContains(found.LinkOpts, "-lz") {
		t.Errorf("LinkOpts should include -lz post system-lib lift; got %v", found.LinkOpts)
	}
}
