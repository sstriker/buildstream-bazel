package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// A cc_library consumer that needs a generated `.inc` (recorded on
// CodegenHeaderConsumers) gets a # keep'd deps edge on a synthesized
// generated_headers wrapper in the producing package; the wrapper carries
// the .inc in textual_hdrs with includes=["."] and a whole-rule keep.
func TestEmitSplit_GeneratedHeaderWrapper(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			// A real target in gen/ makes it a Bazel package (deepestPkg).
			{Name: "genlib", Kind: ir.KindCCLibrary, Srcs: []string{"gen/genlib.cpp"}},
			{Name: "consumer", Kind: ir.KindCCLibrary, Srcs: []string{"lib/consumer.cpp"}},
		},
		SubPackages: map[string]string{"genlib": "gen", "consumer": "lib"},
		CodegenHeaderConsumers: map[string][]string{
			"consumer": {"gen/myproj/gen.inc"},
		},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	gen := string(tree["gen"])
	lib := string(tree["lib"])

	for _, want := range []string{
		`name = "generated_headers"`,
		`textual_hdrs = ["myproj/gen.inc"]`,
		`includes = ["."]`,
	} {
		if !strings.Contains(gen, want) {
			t.Errorf("gen/ package missing %q:\n%s", want, gen)
		}
	}
	// Whole-rule keep on the synthesized wrapper — gazelle has no on-disk
	// srcs to reconcile it against and would otherwise delete it.
	if !strings.Contains(gen, ")  # keep") {
		t.Errorf("wrapper missing whole-rule keep marker:\n%s", gen)
	}
	// The consumer depends on the cross-package wrapper, with a per-item
	// keep so a gazelle pass (which can't resolve the generated .inc)
	// preserves the edge.
	if !strings.Contains(lib, "/gen:generated_headers") {
		t.Errorf("consumer missing cross-package wrapper dep:\n%s", lib)
	}
	if !strings.Contains(lib, `:generated_headers",  # keep`) {
		t.Errorf("consumer wrapper dep missing per-item # keep:\n%s", lib)
	}
}
