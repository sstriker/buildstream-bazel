package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerInterfaceLibraries_RoutesInterfaceLinkLibsAsDeps pins
// the abseil-shape fix: trace-based INTERFACE libraries get their
// target_link_libraries INTERFACE arms routed to deps. Without
// this, deps-only INTERFACE wrappers (abseil's absl_check ⇒
// absl::log_internal_check_impl) emit as empty cc_library.
func TestLowerInterfaceLibraries_RoutesInterfaceLinkLibsAsDeps(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "absl_check", Type: "INTERFACE"},
			{Name: "log_internal_check_impl", Type: "INTERFACE"},
			// ALIAS: absl::log_internal_check_impl → log_internal_check_impl
			{Name: "absl::log_internal_check_impl", Type: "ALIAS",
				Aliases: []string{"log_internal_check_impl"}},
		},
		Links: []shadow.TargetLinkCall{
			{
				Target: "absl_check",
				Groups: []shadow.TargetLinkGroup{
					{Visibility: "INTERFACE", Libs: []string{"absl::log_internal_check_impl"}},
				},
			},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 2 {
		t.Fatalf("want 2 interface libs (absl_check + log_internal_check_impl); got %d", len(got))
	}
	// First target alphabetically is absl_check.
	if got[0].Name != "absl_check" {
		t.Errorf("first target = %q; want absl_check", got[0].Name)
	}
	want := []string{":log_internal_check_impl"}
	if !reflect.DeepEqual(got[0].Deps, want) {
		t.Errorf("deps = %v; want %v", got[0].Deps, want)
	}
}

// TestLowerInterfaceLibraries_SplitsSemicolonJoinedLibs: a single Libs entry
// that is itself a `;`-joined cmake list — abseil's absl_cc_library expands a
// quoted "${..._DEPS}" variable into one target_link_libraries arg — must
// split into one dep per lib, not a single bogus
// `:absl_config;absl_int128;…` label Bazel can't resolve (the abseil
// random_internal_pcg_engine build-lens failure).
func TestLowerInterfaceLibraries_SplitsSemicolonJoinedLibs(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "pcg_engine", Type: "INTERFACE"},
		},
		Links: []shadow.TargetLinkCall{
			{
				Target: "pcg_engine",
				Groups: []shadow.TargetLinkGroup{
					{Visibility: "INTERFACE", Libs: []string{"absl::config;absl::int128;absl::type_traits"}},
				},
			},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib; got %d", len(got))
	}
	want := []string{":absl_config", ":absl_int128", ":absl_type_traits"}
	if !reflect.DeepEqual(got[0].Deps, want) {
		t.Errorf("deps = %v; want %v (the ;-list must split)", got[0].Deps, want)
	}
}

func TestLowerInterfaceLibraries_NamespacedLibWithoutRecordedAliasSanitizes(t *testing.T) {
	// Consumer references `Pkg::Foo` but no ALIAS in trace —
	// sanitize to :Pkg_Foo (the alias-target lift will emit the
	// matching alias rule for the consumer to resolve against).
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "consumer", Type: "INTERFACE"},
		},
		Links: []shadow.TargetLinkCall{
			{
				Target: "consumer",
				Groups: []shadow.TargetLinkGroup{
					{Visibility: "PUBLIC", Libs: []string{"Pkg::Foo"}},
				},
			},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1; got %d", len(got))
	}
	want := []string{":Pkg_Foo"}
	if !reflect.DeepEqual(got[0].Deps, want) {
		t.Errorf("deps = %v; want %v", got[0].Deps, want)
	}
}

func TestLowerInterfaceLibraries_DropsLinkFlagAndGenexTokens(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "iface", Type: "INTERFACE"},
		},
		Links: []shadow.TargetLinkCall{
			{
				Target: "iface",
				Groups: []shadow.TargetLinkGroup{
					{Visibility: "INTERFACE", Libs: []string{
						"real_dep",
						"-pthread",            // link flag, not a dep
						"-Wl,--as-needed",     // ditto
						"$<TARGET_OBJECTS:x>", // genex placeholder
						"",                    // empty token
					}},
				},
			},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1; got %d", len(got))
	}
	want := []string{":real_dep"}
	if !reflect.DeepEqual(got[0].Deps, want) {
		t.Errorf("deps = %v; want %v", got[0].Deps, want)
	}
}

func TestLowerInterfaceLibraries_PrivateScopeNotRouted(t *testing.T) {
	// PRIVATE deps on INTERFACE libs don't make semantic sense
	// (the lib has no own .cc files); skip per cmake docs.
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "iface", Type: "INTERFACE"},
		},
		Links: []shadow.TargetLinkCall{
			{
				Target: "iface",
				Groups: []shadow.TargetLinkGroup{
					{Visibility: "PRIVATE", Libs: []string{"private_dep"}},
				},
			},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1; got %d", len(got))
	}
	if len(got[0].Deps) != 0 {
		t.Errorf("PRIVATE deps should not route on INTERFACE lib; got %v", got[0].Deps)
	}
}

// TestLowerInterfaceLibraries_DotIncludeIsRootWalk: a literal "." (or "./")
// INTERFACE include dir — `target_include_directories(lib INTERFACE .)`
// recorded verbatim by --trace-expand — denotes the package root, like "". It
// must set RootInclude (so split can restore include_prefix) and must NOT leak
// "." into the includes slice (normDir(".") → "" would synthesize a bogus root
// header lib under --split-packages). Matches the codemodel path's
// rel=="" || rel=="." handling.
func TestLowerInterfaceLibraries_DotIncludeIsRootWalk(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "hdrlib", Type: "INTERFACE"},
		},
		Includes: []shadow.TargetIncludeCall{
			{Target: "hdrlib", Groups: []shadow.TargetIncludeGroup{
				{Visibility: "INTERFACE", Dirs: []string{"."}},
			}},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	var lib *ir.Target
	for i := range got {
		if got[i].Name == "hdrlib" {
			lib = &got[i]
		}
	}
	if lib == nil {
		t.Fatalf("hdrlib not emitted: %+v", got)
	}
	if !lib.RootInclude {
		t.Errorf("RootInclude = false; want true for a \".\" INTERFACE include dir")
	}
	for _, inc := range lib.Includes {
		if inc == "." || inc == "" || inc == "./" {
			t.Errorf("includes leaked a root-dir entry %q: %v", inc, lib.Includes)
		}
	}
}
