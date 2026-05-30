package lower

import (
	"reflect"
	"testing"

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
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
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
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
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
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
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
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1; got %d", len(got))
	}
	if len(got[0].Deps) != 0 {
		t.Errorf("PRIVATE deps should not route on INTERFACE lib; got %v", got[0].Deps)
	}
}
