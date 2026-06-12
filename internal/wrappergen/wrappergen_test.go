package wrappergen

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

func sampleManifest() *manifest.Imports {
	return &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "greet",
			Exports: []*manifest.Export{
				{
					CMakeTarget:       "Greeter::core",
					BazelLabel:        "//old/prebuilts:core",
					LinkPaths:         []string{"/opt/prefix/lib/libcore.a"},
					InterfaceIncludes: []string{"include"},
					Deps:              []string{"//old/prebuilts:base", "@sys//:z"},
				},
				{
					CMakeTarget: "Greeter::base",
					BazelLabel:  "//old/prebuilts:base",
					LinkPaths:   []string{"/opt/prefix/lib/libbase.a"},
				},
				{
					CMakeTarget:       "Greeter::headers",
					BazelLabel:        "//old/prebuilts:headers",
					InterfaceIncludes: []string{"include"},
				},
			},
		}},
	}
}

// TestGenerate_WrapperShape pins the synthesized BUILD: cc_import per
// archived export (prefix-relative path, kind by extension), the
// consumer-facing cc_library wrapper with the archive + remapped
// closure as REAL deps, header-only exports as glob-driven wrappers
// with no import, and cross-element closure labels passing through.
func TestGenerate_WrapperShape(t *testing.T) {
	build, _, err := Generate(sampleManifest(), "prebuilts/greet", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	for _, want := range []string{
		`cc_import(
    name = "core_archive",
    static_library = "lib/libcore.a",`,
		`cc_library(
    name = "core",`,
		`        ":core_archive",
        "//prebuilts/greet:base",
        "@sys//:z",`,
		`cc_library(
    name = "headers",`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("BUILD missing:\n%s\n--- got ---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"headers_archive"`) {
		t.Errorf("header-only export must not get a cc_import:\n%s", s)
	}
	if strings.Contains(s, "//old/prebuilts:base") {
		t.Errorf("closure label naming another export's OLD label must remap to its wrapper:\n%s", s)
	}
}

// TestGenerate_ManifestRewrite pins the consume-and-clear half of the
// Export.Deps invariant: labels repoint at the wrappers, Deps clears,
// the fragment-redirect keys (link_paths) survive.
func TestGenerate_ManifestRewrite(t *testing.T) {
	_, out, err := Generate(sampleManifest(), "prebuilts/greet", "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*manifest.Export{}
	for _, ex := range out.Elements[0].Exports {
		byName[ex.CMakeTarget] = ex
	}
	core := byName["Greeter::core"]
	if core.BazelLabel != "//prebuilts/greet:core" {
		t.Errorf("core label = %q, want the wrapper", core.BazelLabel)
	}
	if len(core.Deps) != 0 {
		t.Errorf("Deps must CLEAR after generation (invariant): %v", core.Deps)
	}
	if len(core.LinkPaths) != 1 || core.LinkPaths[0] != "/opt/prefix/lib/libcore.a" {
		t.Errorf("link_paths must survive (fragment redirects key on them): %v", core.LinkPaths)
	}
	// The input manifest must stay untouched (deep copy).
	in := sampleManifest()
	_, _, _ = Generate(in, "prebuilts/greet", "")
	if in.Elements[0].Exports[0].BazelLabel != "//old/prebuilts:core" || len(in.Elements[0].Exports[0].Deps) != 2 {
		t.Error("Generate mutated its input manifest")
	}
}

// TestGenerate_NameCollision: wrapper names are consumer-facing; a
// post-sanitization collision is an error, not a silent overwrite.
func TestGenerate_NameCollision(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "x",
			Exports: []*manifest.Export{
				{CMakeTarget: "A::foo+bar", BazelLabel: "//a:1"},
				{CMakeTarget: "B::foo_bar", BazelLabel: "//b:2"},
			},
		}},
	}
	if _, _, err := Generate(im, "p", ""); err == nil {
		t.Error("colliding wrapper names must error")
	}
}
