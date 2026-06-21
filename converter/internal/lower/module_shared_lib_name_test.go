package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestSharedLibName_ModuleKeepsExactName covers the MODULE-vs-SHARED .so
// naming split under faithful-SHARED emit. A SHARED library with an
// unversioned on-disk name collides with its impl cc_library's implicit
// lib<name>.so dynamic output when a consumer carries both the impl (deps)
// and :<lib>_shared (dynamic_deps), so it gets a `.so.1` soversion suffix.
// A MODULE has no link consumer pulling that impl, so there's no collision
// to dodge — and a module is dlopen'd by EXACT filename, so it must keep
// cmake's exact `libplugin.so`. Suffixing it would break
// dlopen("libplugin.so").
func TestSharedLibName_ModuleKeepsExactName(t *testing.T) {
	reply := func(typ, nameOnDisk string) *fileapi.Reply {
		const id = "lib::@"
		return &fileapi.Reply{
			Targets: map[string]fileapi.Target{id: {
				Name:       "lib",
				Type:       typ,
				NameOnDisk: nameOnDisk,
				Link:       &fileapi.TargetLink{Language: "C"},
			}},
			Codemodel: fileapi.Codemodel{
				Configurations: []fileapi.Configuration{{
					Name:    "Release",
					Targets: []fileapi.ConfigTargetRef{{Id: id, Name: "lib"}},
				}},
			},
		}
	}

	cases := []struct {
		name       string
		targetType string
		nameOnDisk string
		want       string
	}{
		{"module unversioned keeps exact name", "MODULE_LIBRARY", "liblib.so", "liblib.so"},
		{"shared unversioned gets soversion suffix", "SHARED_LIBRARY", "liblib.so", "liblib.so.1"},
		{"shared versioned keeps cmake name", "SHARED_LIBRARY", "liblib.so.2.3", "liblib.so.2.3"},
		{"module versioned keeps cmake name", "MODULE_LIBRARY", "liblib.so.2", "liblib.so.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkg, err := ToIR(reply(tc.targetType, tc.nameOnDisk), &ninja.Graph{}, Options{EmitSharedLibraries: true})
			if err != nil {
				t.Fatalf("ToIR: %v", err)
			}
			got := findTarget(pkg, "lib")
			if got == nil {
				t.Fatal("lib not in pkg.Targets")
			}
			if got.SharedLibName != tc.want {
				t.Errorf("SharedLibName = %q, want %q", got.SharedLibName, tc.want)
			}
		})
	}
}
