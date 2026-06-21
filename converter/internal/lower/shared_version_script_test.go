package lower

import (
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestSharedVersionScript_RoutedToWrapper covers the symbol version-script
// half of cc_shared_library fidelity. cmake records a
// target_link_options(-Wl,--version-script,<src>.map) as a "flags" link
// fragment. Under faithful-SHARED emit the target renders a cc_shared_library
// wrapper that produces the real .so, so the version-script must land on the
// WRAPPER's user_link_flags (as an unquoted $(location ...) — user_link_flags
// is not shell-tokenised) with the map staged via the wrapper's
// additional_linker_inputs — NOT dropped, and NOT on the impl cc_library
// (whose linkopts would propagate the script to every consumer). The default
// static-collapse emit still drops it.
func TestSharedVersionScript_RoutedToWrapper(t *testing.T) {
	src := t.TempDir()
	mapPath := filepath.Join(src, "greet.map")
	vsFlag := "-Wl,--version-script," + mapPath

	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"greet::@": {
			Name:       "greet",
			Type:       "SHARED_LIBRARY",
			NameOnDisk: "libgreet.so.1",
			Link: &fileapi.TargetLink{
				Language:         "C",
				CommandFragments: []fileapi.CommandFragment{{Fragment: vsFlag, Role: "flags"}},
			},
		}},
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: src, Build: filepath.Join(src, "b")},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "greet::@", Name: "greet"}},
			}},
		},
	}

	t.Run("faithful-shared routes to wrapper", func(t *testing.T) {
		pkg, err := ToIR(reply, &ninja.Graph{}, Options{EmitSharedLibraries: true, HostSourceRoot: src})
		if err != nil {
			t.Fatalf("ToIR: %v", err)
		}
		got := findTarget(pkg, "greet")
		if got == nil {
			t.Fatal("greet not in pkg.Targets")
		}
		wantFlag := "-Wl,--version-script,$(location greet.map)"
		if !stringSliceContains(got.SharedLibUserLinkFlags, wantFlag) {
			t.Errorf("SharedLibUserLinkFlags should carry %q (unquoted $(location)); got %v", wantFlag, got.SharedLibUserLinkFlags)
		}
		if !stringSliceContains(got.SharedLibAdditionalLinkerInputs, "greet.map") {
			t.Errorf("SharedLibAdditionalLinkerInputs should stage greet.map; got %v", got.SharedLibAdditionalLinkerInputs)
		}
		// Must NOT land on the impl cc_library (would propagate to consumers).
		for _, lo := range got.LinkOpts {
			if lo == wantFlag || lo == vsFlag {
				t.Errorf("impl LinkOpts must not carry the version-script (propagates to consumers); got %v", got.LinkOpts)
			}
		}
		for _, ali := range got.AdditionalLinkerInputs {
			if ali == "greet.map" {
				t.Errorf("impl AdditionalLinkerInputs must not stage the wrapper's map; got %v", got.AdditionalLinkerInputs)
			}
		}
	})

	t.Run("default static-collapse drops it", func(t *testing.T) {
		pkg, err := ToIR(reply, &ninja.Graph{}, Options{HostSourceRoot: src})
		if err != nil {
			t.Fatalf("ToIR: %v", err)
		}
		got := findTarget(pkg, "greet")
		if got == nil {
			t.Fatal("greet not in pkg.Targets")
		}
		if len(got.SharedLibUserLinkFlags) != 0 || len(got.SharedLibAdditionalLinkerInputs) != 0 {
			t.Errorf("default emit renders no wrapper; want no shared-lib flags, got user=%v inputs=%v",
				got.SharedLibUserLinkFlags, got.SharedLibAdditionalLinkerInputs)
		}
		for _, lo := range got.LinkOpts {
			if lo == vsFlag || lo == "-Wl,--version-script,$(location greet.map)" {
				t.Errorf("default emit must drop the version-script (harmful on a static cc_library); got %v", got.LinkOpts)
			}
		}
	})
}
