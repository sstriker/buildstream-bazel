package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// A cc_library consumer that needs a generated `.inc` (recorded on
// CodegenHeaderConsumers) gets a # keep'd deps edge on a synthesized
// generated_includes wrapper in the producing package; the wrapper carries
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
		`name = "generated_includes"`,
		`textual_hdrs = ["myproj/gen.inc"]`,
		`includes = ["."]`,
		`"cmake-codegen-generated-includes"`,
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
	if !strings.Contains(lib, "/gen:generated_includes") {
		t.Errorf("consumer missing cross-package wrapper dep:\n%s", lib)
	}
	if !strings.Contains(lib, `:generated_includes",  # keep`) {
		t.Errorf("consumer wrapper dep missing per-item # keep:\n%s", lib)
	}
}

// When a producing package already declares a real target with the reserved
// wrapper name, EmitSplit fails fast with a clear error rather than emit a
// duplicate Bazel target (load error).
func TestEmitSplit_GeneratedHeaderWrapper_NameCollision(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			{Name: "genlib", Kind: ir.KindCCLibrary, Srcs: []string{"gen/genlib.cpp"}},
			// A real project target colliding with the reserved name, in the
			// same package the wrapper would be synthesized into.
			{Name: "generated_includes", Kind: ir.KindCCLibrary, Srcs: []string{"gen/user.cpp"}},
			{Name: "consumer", Kind: ir.KindCCLibrary, Srcs: []string{"lib/consumer.cpp"}},
		},
		SubPackages: map[string]string{
			"genlib":             "gen",
			"generated_includes": "gen",
			"consumer":           "lib",
		},
		CodegenHeaderConsumers: map[string][]string{
			"consumer": {"gen/myproj/gen.inc"},
		},
	}
	if _, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"}); err == nil {
		t.Fatal("expected a reserved-name collision error, got nil")
	} else if !strings.Contains(err.Error(), "generated_includes") {
		t.Errorf("collision error should name the reserved target, got: %v", err)
	}
}

// A protoc-style genrule whose outputs live under a single-component output
// root (grpc's `gens/`) moves INTO that `gens` package under --split-packages.
// split-emit must then (A) shrink the cmd's `$(RULEDIR)/gens` output-dir arg to
// `$(RULEDIR)` (the package IS gens, so $(RULEDIR) already points at it) and
// (B) relabel the bare in-element tool `grpc_cpp_plugin` (a cc_binary in the
// element root, referenced via $(execpath)) to its cross-package label.
func TestEmitSplit_CodegenGenrule_OutputRootAndToolRelabel(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			{Name: "grpc_cpp_plugin", Kind: ir.KindCCBinary, Srcs: []string{"plugin.cc"}},
			{
				Name:         "gen_gens_x_grpc_pb_cc",
				Kind:         ir.KindGenrule,
				GenruleOuts:  []string{"gens/src/proto/x.grpc.pb.cc", "gens/src/proto/x.grpc.pb.h"},
				GenruleCmd:   "$(execpath @protobuf//:protoc) --grpc_out=:$(RULEDIR)/gens --cpp_out=$(RULEDIR)/gens --plugin=protoc-gen-grpc=$(execpath grpc_cpp_plugin) -I elements/p elements/p/src/proto/x.proto",
				GenruleTools: []string{"@protobuf//:protoc", "grpc_cpp_plugin"},
				Srcs:         []string{"src/proto/x.proto"},
				Visibility:   []string{"//visibility:private"},
			},
			// Consumer compiles the generated .cc and carries the gens output
			// root on includes (the #4 wiring) — which makes `gens` an include
			// root → its own package, so the genrule moves into it.
			{
				Name:     "consumer",
				Kind:     ir.KindCCLibrary,
				Srcs:     []string{"gens/src/proto/x.grpc.pb.cc"},
				Hdrs:     []string{"gens/src/proto/x.grpc.pb.h"},
				Includes: []string{"gens"},
			},
		},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	gens, ok := tree["gens"]
	if !ok {
		t.Fatalf("no gens package emitted; got %v", keysOf(tree))
	}
	body := string(gens)
	// (A) output-root shrunk: no doubled $(RULEDIR)/gens remains.
	if strings.Contains(body, "$(RULEDIR)/gens") {
		t.Errorf("output-root not shrunk (still $(RULEDIR)/gens):\n%s", body)
	}
	if !strings.Contains(body, "--cpp_out=$(RULEDIR) ") {
		t.Errorf("--cpp_out not anchored to bare $(RULEDIR):\n%s", body)
	}
	// (B) bare tool relabeled cross-package in both tools + cmd; the absolute
	// @protobuf//:protoc is left as-is.
	if !strings.Contains(body, "$(execpath //elements/p:grpc_cpp_plugin)") {
		t.Errorf("bare tool not relabeled in cmd:\n%s", body)
	}
	if !strings.Contains(body, `"//elements/p:grpc_cpp_plugin"`) {
		t.Errorf("bare tool not relabeled in tools:\n%s", body)
	}
	if !strings.Contains(body, "$(execpath @protobuf//:protoc)") {
		t.Errorf("absolute tool ref should be preserved:\n%s", body)
	}
}

// A codegen TOOL (grpc_cpp_plugin) that carries cmake's global -I<build>/gens
// must NOT get a dep on the gens/ include-root header lib it transitively
// produces — that's a cycle (tool → gens_headers → generated reflection.pb.h →
// genrule → tool). A normal consumer of the same include root still gets it.
func TestEmitSplit_CodegenTool_NoSelfOutputRootHeaderDep(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			// The codegen tool: a genrule tool that also (via cmake's global
			// include) lists gens as an include root.
			{Name: "grpc_cpp_plugin", Kind: ir.KindCCBinary, Srcs: []string{"plugin.cc"}, Includes: []string{"gens"}},
			{
				Name:         "gen_x",
				Kind:         ir.KindGenrule,
				GenruleOuts:  []string{"gens/src/proto/x.grpc.pb.cc", "gens/src/proto/x.grpc.pb.h"},
				GenruleCmd:   "$(execpath grpc_cpp_plugin) -o $(RULEDIR) elements/p/x.proto",
				GenruleTools: []string{"grpc_cpp_plugin"},
				Visibility:   []string{"//visibility:private"},
			},
			// A legitimate consumer of the gens include root (also makes gens a
			// header-lib root).
			{Name: "consumer", Kind: ir.KindCCLibrary, Srcs: []string{"gens/src/proto/x.grpc.pb.cc"}, Hdrs: []string{"gens/src/proto/x.grpc.pb.h"}, Includes: []string{"gens"}},
		},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	root := string(tree[""])
	// grpc_cpp_plugin (in root) must NOT depend on gens:gens_headers.
	plugin := sliceRule(root, "grpc_cpp_plugin")
	if strings.Contains(plugin, "gens:gens_headers") || strings.Contains(plugin, "/gens:gens_headers") {
		t.Errorf("codegen tool grpc_cpp_plugin should NOT dep its own output-root header lib:\n%s", plugin)
	}
	// The legitimate consumer SHOULD depend on it.
	consumer := sliceRule(root, "consumer")
	if !strings.Contains(consumer, ":gens_headers") {
		t.Errorf("consumer should dep gens header lib:\n%s", consumer)
	}
}

// sliceRule returns the substring of body for the rule named n (best-effort:
// from its `name = "n"` line to the next blank line).
func sliceRule(body, n string) string {
	marker := "name = \"" + n + "\""
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	// back up to the rule start (preceding "(\n")
	start := strings.LastIndex(body[:i], "(\n")
	if start < 0 {
		start = i
	}
	rest := body[start:]
	if j := strings.Index(rest, "\n)"); j >= 0 {
		return rest[:j]
	}
	return rest
}
