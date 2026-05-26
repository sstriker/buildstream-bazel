package lower

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
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

// TestLowerTarget_LinkOpts_FlagsRouted covers the wider linkopts
// fix: Link.CommandFragments with role=flags / libraryPath /
// frameworkPath / frameworks now route into irt.LinkOpts rather
// than being silently dropped.
func TestLowerTarget_LinkOpts_FlagsRouted(t *testing.T) {
	target := &fileapi.Target{
		Name: "app",
		Type: "EXECUTABLE",
		Link: &fileapi.TargetLink{
			Language: "C",
			CommandFragments: []fileapi.CommandFragment{
				{Fragment: "-Wl,--as-needed", Role: "flags"},
				{Fragment: "/opt/lib", Role: "libraryPath"},
				{Fragment: "/Library/Frameworks", Role: "frameworkPath"},
				{Fragment: "Cocoa", Role: "frameworks"},
			},
		},
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
		wantContains := []string{
			"-Wl,--as-needed",
			"-L/opt/lib",
			"-F/Library/Frameworks",
			"-framework",
			"Cocoa",
		}
		joined := strings.Join(tgt.LinkOpts, " ")
		for _, w := range wantContains {
			if !strings.Contains(joined, w) {
				t.Errorf("LinkOpts missing %q; got %v", w, tgt.LinkOpts)
			}
		}
		return
	}
	t.Fatal("app not in pkg.Targets")
}

// TestApplyProbeGenexProperties_BuildRpathToLinkopts covers the
// BUILD_RPATH lift: each semicolon-separated entry becomes a
// `-Wl,-rpath,<path>` linkopt.
func TestApplyProbeGenexProperties_BuildRpathToLinkopts(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "app",
			Kind: ir.KindCCBinary,
		}},
	}
	probes := []cmakerunGenexProbeStub{{
		Name: "app",
		Properties: map[string]string{
			"BUILD_RPATH": "$ORIGIN/../lib;/opt/foo/lib",
		},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	wantLinks := []string{"-Wl,-rpath,$ORIGIN/../lib", "-Wl,-rpath,/opt/foo/lib"}
	if !reflect.DeepEqual(pkg.Targets[0].LinkOpts, wantLinks) {
		t.Errorf("LinkOpts: %v want %v", pkg.Targets[0].LinkOpts, wantLinks)
	}
}

// TestApplyProbeGenexProperties_PIC covers POSITION_INDEPENDENT_CODE
// → features=["pic"] / ["-pic"] routing.
func TestApplyProbeGenexProperties_PIC(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "on", Kind: ir.KindCCLibrary},
		{Name: "off", Kind: ir.KindCCLibrary},
	}}
	probes := []cmakerunGenexProbeStub{
		{Name: "on", Properties: map[string]string{"POSITION_INDEPENDENT_CODE": "TRUE"}},
		{Name: "off", Properties: map[string]string{"POSITION_INDEPENDENT_CODE": "FALSE"}},
	}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Features, "pic") {
		t.Errorf("on.Features: %v want [pic]", pkg.Targets[0].Features)
	}
	if !stringSliceContains(pkg.Targets[1].Features, "-pic") {
		t.Errorf("off.Features: %v want [-pic]", pkg.Targets[1].Features)
	}
}

// TestApplyProbeGenexProperties_VisibilityPreset covers
// {CXX,C}_VISIBILITY_PRESET → -fvisibility=<v> copt.
func TestApplyProbeGenexProperties_VisibilityPreset(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "hide", Kind: ir.KindCCLibrary},
	}}
	probes := []cmakerunGenexProbeStub{{
		Name: "hide",
		Properties: map[string]string{
			"CXX_VISIBILITY_PRESET": "hidden",
		},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Copts, "-fvisibility=hidden") {
		t.Errorf("Copts: %v should contain -fvisibility=hidden", pkg.Targets[0].Copts)
	}
}

// Test stubs to build GenexProbe values without re-importing
// cmakerun in this test file (keeps the test deps minimal).
type cmakerunGenexProbeStub struct {
	Name       string
	Properties map[string]string
}

func toProbeSlice(in []cmakerunGenexProbeStub) []cmakerun.GenexProbe {
	out := make([]cmakerun.GenexProbe, len(in))
	for i, p := range in {
		out[i].Name = p.Name
		out[i].Properties = p.Properties
	}
	return out
}
