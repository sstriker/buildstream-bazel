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

func TestApplyProbeGenexProperties_VisibilityInlinesHidden(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib", Kind: ir.KindCCLibrary}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "lib",
		Properties: map[string]string{"VISIBILITY_INLINES_HIDDEN": "TRUE"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Copts, "-fvisibility-inlines-hidden") {
		t.Errorf("Copts: %v should contain -fvisibility-inlines-hidden", pkg.Targets[0].Copts)
	}
}

func TestApplyProbeGenexProperties_EnableExports(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "app", Kind: ir.KindCCBinary}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "app",
		Properties: map[string]string{"ENABLE_EXPORTS": "1"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-enable-exports") {
		t.Errorf("Tags: %v should include cmake-codegen-enable-exports", pkg.Targets[0].Tags)
	}
}

func TestApplyProbeGenexProperties_VersionTags(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib", Kind: ir.KindCCLibrary}}}
	probes := []cmakerunGenexProbeStub{{
		Name: "lib",
		Properties: map[string]string{
			"VERSION":   "1.2.3",
			"SOVERSION": "1",
		},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-version=1.2.3") {
		t.Errorf("Tags: %v should include version", pkg.Targets[0].Tags)
	}
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-soversion=1") {
		t.Errorf("Tags: %v should include soversion", pkg.Targets[0].Tags)
	}
}

func TestApplyProbeGenexProperties_QtToggles(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "qtwidget", Kind: ir.KindCCLibrary},
	}}
	probes := []cmakerunGenexProbeStub{{
		Name: "qtwidget",
		Properties: map[string]string{
			"AUTOMOC": "TRUE",
			"AUTOUIC": "TRUE",
			"AUTORCC": "TRUE",
		},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	for _, want := range []string{
		"cmake-codegen-qt-automoc",
		"cmake-codegen-qt-autouic",
		"cmake-codegen-qt-autorcc",
	} {
		if !stringSliceContains(pkg.Targets[0].Tags, want) {
			t.Errorf("Tags %v missing %q", pkg.Targets[0].Tags, want)
		}
	}
}

func TestApplyProbeGenexProperties_ExcludeFromAll(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "tool", Kind: ir.KindCCBinary}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "tool",
		Properties: map[string]string{"EXCLUDE_FROM_ALL": "TRUE"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Tags, "manual") {
		t.Errorf("Tags: %v want manual", pkg.Targets[0].Tags)
	}
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-exclude-from-all") {
		t.Errorf("Tags: %v want cmake-codegen-exclude-from-all", pkg.Targets[0].Tags)
	}
}

func TestApplyProbeGenexProperties_MSVCRuntimeLibrary(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib", Kind: ir.KindCCLibrary}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "lib",
		Properties: map[string]string{"MSVC_RUNTIME_LIBRARY": "MultiThreadedDLL"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Tags, "cmake-codegen-msvc-runtime=MultiThreadedDLL") {
		t.Errorf("Tags: %v want msvc-runtime", pkg.Targets[0].Tags)
	}
}

func TestApplyProbeGenexProperties_JobPools(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib", Kind: ir.KindCCLibrary}}}
	probes := []cmakerunGenexProbeStub{{
		Name: "lib",
		Properties: map[string]string{
			"JOB_POOL_COMPILE": "compile-heavy",
			"JOB_POOL_LINK":    "link-heavy",
		},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	for _, want := range []string{
		"cmake-codegen-job-pool-compile=compile-heavy",
		"cmake-codegen-job-pool-link=link-heavy",
	} {
		if !stringSliceContains(pkg.Targets[0].Tags, want) {
			t.Errorf("Tags: %v missing %q", pkg.Targets[0].Tags, want)
		}
	}
}

func TestApplyProbeGenexProperties_CXXExtensions_RewritesStdFlag(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:  "lib",
		Kind:  ir.KindCCLibrary,
		Copts: []string{"-std=c++17", "-O2"},
	}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "lib",
		Properties: map[string]string{"CXX_EXTENSIONS": "ON"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Copts, "-std=gnu++17") {
		t.Errorf("Copts: %v should contain -std=gnu++17", pkg.Targets[0].Copts)
	}
	if stringSliceContains(pkg.Targets[0].Copts, "-std=c++17") {
		t.Errorf("Copts: %v should NOT still contain -std=c++17", pkg.Targets[0].Copts)
	}
}

func TestApplyProbeGenexProperties_CExtensions_RewritesCStd(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:  "lib",
		Kind:  ir.KindCCLibrary,
		Copts: []string{"-std=c11"},
	}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "lib",
		Properties: map[string]string{"C_EXTENSIONS": "TRUE"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Copts, "-std=gnu11") {
		t.Errorf("Copts: %v should contain -std=gnu11", pkg.Targets[0].Copts)
	}
}

func TestApplyProbeGenexProperties_CExtensions_DoesNotMatchCxx(t *testing.T) {
	// C_EXTENSIONS=ON must NOT rewrite -std=c++17 to -std=gnu++17.
	// The C/CXX rewrites are driven by distinct properties.
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:  "lib",
		Kind:  ir.KindCCLibrary,
		Copts: []string{"-std=c++17"},
	}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "lib",
		Properties: map[string]string{"C_EXTENSIONS": "TRUE"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Copts, "-std=c++17") {
		t.Errorf("Copts: %v should preserve -std=c++17 (C_EXTENSIONS doesn't apply)", pkg.Targets[0].Copts)
	}
}

func TestApplyProbeGenexProperties_ExtensionsOff_NoRewrite(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:  "lib",
		Kind:  ir.KindCCLibrary,
		Copts: []string{"-std=c++17"},
	}}}
	probes := []cmakerunGenexProbeStub{{
		Name:       "lib",
		Properties: map[string]string{"CXX_EXTENSIONS": "OFF"},
	}}
	applyProbeGenexProperties(pkg, toProbeSlice(probes))
	if !stringSliceContains(pkg.Targets[0].Copts, "-std=c++17") {
		t.Errorf("Copts: %v should preserve strict -std=c++17 (CXX_EXTENSIONS=OFF)", pkg.Targets[0].Copts)
	}
}

func TestLowerTarget_Sysroot_Tag(t *testing.T) {
	target := &fileapi.Target{
		Name: "cross",
		Type: "STATIC_LIBRARY",
		CompileGroups: []fileapi.CompileGroup{{
			Language: "C",
			Sysroot: &struct {
				Path string `json:"path"`
			}{Path: "/opt/cross/sysroot-aarch64"},
		}},
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"cross::@": *target},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "cross::@", Name: "cross"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "cross" {
			if !stringSliceContains(tgt.Tags, "cmake-codegen-sysroot=/opt/cross/sysroot-aarch64") {
				t.Errorf("missing sysroot tag; got %v", tgt.Tags)
			}
			return
		}
	}
	t.Fatal("cross not found")
}
