package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestLowerTarget_LTO_ArchiveSide covers STATIC_LIBRARY with
// TargetArchive.LTO set — INTERPROCEDURAL_OPTIMIZATION enabled
// on a static library.
func TestLowerTarget_LTO_ArchiveSide(t *testing.T) {
	target := &fileapi.Target{
		Name:    "foo",
		Type:    "STATIC_LIBRARY",
		Archive: &fileapi.TargetArchive{LTO: true},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"foo::@": *target},
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
	var fooTarget *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "foo" {
			fooTarget = &pkg.Targets[i]
		}
	}
	if fooTarget == nil {
		t.Fatal("foo not in pkg.Targets")
	}
	if !reflect.DeepEqual(fooTarget.Features, []string{"lto"}) {
		t.Errorf("Features: got %v want [lto]", fooTarget.Features)
	}
}

// TestLowerTarget_LTO_LinkSide covers EXECUTABLE / SHARED with
// TargetLink.LTO set.
func TestLowerTarget_LTO_LinkSide(t *testing.T) {
	target := &fileapi.Target{
		Name: "app",
		Type: "EXECUTABLE",
		Link: &fileapi.TargetLink{Language: "CXX", LTO: true},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "app" {
			if !reflect.DeepEqual(tgt.Features, []string{"lto"}) {
				t.Errorf("Features: got %v want [lto]", tgt.Features)
			}
			return
		}
	}
	t.Fatal("app not in pkg.Targets")
}

// TestLowerTarget_NoLTO_NoFeatures confirms targets without
// IPO leave Features empty.
func TestLowerTarget_NoLTO_NoFeatures(t *testing.T) {
	target := &fileapi.Target{
		Name: "plain",
		Type: "STATIC_LIBRARY",
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"plain::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "plain::@", Name: "plain"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "plain" && len(tgt.Features) != 0 {
			t.Errorf("plain target should have empty Features; got %v", tgt.Features)
		}
	}
}

// TestLowerTarget_Frameworks_AppendsFFlag covers the macOS
// `-F <path>` lift: CompileGroup.Frameworks entries become
// -F<path> copts.
func TestLowerTarget_Frameworks_AppendsFFlag(t *testing.T) {
	target := &fileapi.Target{
		Name: "app",
		Type: "EXECUTABLE",
		CompileGroups: []fileapi.CompileGroup{{
			Language: "CXX",
			Frameworks: []fileapi.CompileFramework{
				{Path: "/Library/Frameworks"},
				{Path: "/System/Library/Frameworks", IsSystem: true},
			},
		}},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name != "app" {
			continue
		}
		hasLib := false
		hasSys := false
		for _, c := range tgt.Copts {
			if c == "-F/Library/Frameworks" {
				hasLib = true
			}
			if c == "-F/System/Library/Frameworks" {
				hasSys = true
			}
		}
		if !hasLib || !hasSys {
			t.Errorf("expected -F flags for both framework paths; got %v", tgt.Copts)
		}
		return
	}
	t.Fatal("app not in pkg.Targets")
}
