package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// TestLowerTarget_BareSystemLibLinkFlags covers the theme-2 producer
// gap: bare system-library links cmake emits as NON-absolute
// `libraries`-role command fragments (`-lm` from
// target_link_libraries(foo m), `-lpthread`/`-pthread` from
// Threads::Threads, `-ldl` from ${CMAKE_DL_LIBS}). These have no
// in-codebase dep to carry them, so they must reach linkopts rather
// than being dropped as in-codebase target refs. A bare archive name
// (`libfoo.a`) still stays elided — it's a sibling target already
// routed to deps.
func TestLowerTarget_BareSystemLibLinkFlags(t *testing.T) {
	target := &fileapi.Target{
		Name: "consumer",
		Type: "STATIC_LIBRARY",
		Link: &fileapi.TargetLink{
			Language: "C",
			CommandFragments: []fileapi.CommandFragment{
				{Fragment: "-lm", Role: "libraries"},
				{Fragment: "-lpthread", Role: "libraries"},
				{Fragment: "-pthread", Role: "libraries"},
				{Fragment: "-ldl", Role: "libraries"},
				// Bare archive name — an in-codebase sibling, NOT a flag.
				{Fragment: "libsibling.a", Role: "libraries"},
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
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got := findTarget(pkg, "consumer")
	if got == nil {
		t.Fatal("consumer not in pkg.Targets")
	}
	for _, want := range []string{"-lm", "-lpthread", "-pthread", "-ldl"} {
		if !stringSliceContains(got.LinkOpts, want) {
			t.Errorf("LinkOpts should contain %q (bare system-lib link); got %v", want, got.LinkOpts)
		}
	}
	// The bare archive name must NOT be lifted to a linkopt.
	for _, unwanted := range []string{"libsibling.a", "-lsibling.a", "sibling.a"} {
		if stringSliceContains(got.LinkOpts, unwanted) {
			t.Errorf("LinkOpts should not contain %q (in-codebase archive, routed to deps); got %v", unwanted, got.LinkOpts)
		}
	}
}

// TestLowerTarget_BuildDirRpathDropped covers the SHARED-consumer rpath
// leak: cmake's Ninja generator adds the auto build-tree rpath
// (`-Wl,-rpath,<convert-time build dir>`) so the just-built executable
// finds its sibling .so — and files it under the "libraries" role, NOT
// "flags". That role bypasses appendLinkFlagTokens' reanchor, so the
// dead convert-time absolute path used to leak into every shared
// consumer's linkopts. routeNonAbsLibraryFragment now runs flag-shaped
// "libraries" fragments through reanchorLinkOptToken: a rpath INSIDE the
// build dir is dropped (Bazel carries the .so in runfiles with its own
// $ORIGIN rpath), while a source-relative rpath is legitimate runtime
// metadata and stays.
func TestLowerTarget_BuildDirRpathDropped(t *testing.T) {
	const buildDir = "/tmp/convert-element-build-test"
	target := &fileapi.Target{
		Name: "app",
		Type: "EXECUTABLE",
		Link: &fileapi.TargetLink{
			Language: "C",
			CommandFragments: []fileapi.CommandFragment{
				// The cmake-mis-filed build-tree rpath — must be dropped.
				{Fragment: "-Wl,-rpath," + buildDir, Role: "libraries"},
				// A legitimate runtime-metadata rpath outside the build
				// dir — must survive.
				{Fragment: "-Wl,-rpath,/opt/vendor/lib", Role: "libraries"},
			},
		},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": *target},
		Codemodel: fileapi.Codemodel{
			Paths:          fileapi.CodemodelPaths{Build: buildDir, Source: "/tmp/src"},
			Configurations: []fileapi.Configuration{{Name: "Release", Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}}}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{BuildDir: buildDir})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got := findTarget(pkg, "app")
	if got == nil {
		t.Fatal("app not in pkg.Targets")
	}
	if stringSliceContains(got.LinkOpts, "-Wl,-rpath,"+buildDir) {
		t.Errorf("LinkOpts must not leak the convert-time build-dir rpath; got %v", got.LinkOpts)
	}
	if !stringSliceContains(got.LinkOpts, "-Wl,-rpath,/opt/vendor/lib") {
		t.Errorf("LinkOpts should keep the source-external runtime rpath; got %v", got.LinkOpts)
	}
}

// TestLowerTarget_BareLinkFlagProducerRedirect covers the producer-
// element precedence on the bare `-l<name>` path: when an imports
// manifest claims the lib name via link_libraries, the `-l<name>`
// fragment redirects to the producer's Bazel label instead of linking
// the host `-l<name>` — same precedence the absolute system-lib lift
// applies.
func TestLowerTarget_BareLinkFlagProducerRedirect(t *testing.T) {
	target := &fileapi.Target{
		Name: "consumer",
		Type: "STATIC_LIBRARY",
		Link: &fileapi.TargetLink{
			Language: "C",
			CommandFragments: []fileapi.CommandFragment{
				{Fragment: "-lfoo", Role: "libraries"},
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
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: imports})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got := findTarget(pkg, "consumer")
	if got == nil {
		t.Fatal("consumer not in pkg.Targets")
	}
	if !stringSliceContains(got.Deps, "//elements/foo:foo") {
		t.Errorf("Deps should include //elements/foo:foo via -lfoo producer redirect; got %v", got.Deps)
	}
	if stringSliceContains(got.LinkOpts, "-lfoo") {
		t.Errorf("LinkOpts should not contain -lfoo (redirected to the producer element); got %v", got.LinkOpts)
	}
}
