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

// Data FILE-path entries (consumer-attributed generated artifacts — VTK's
// wrap-hierarchy .args/.data response files) must be rewritten per package
// like srcs: package-relative when the consumer's own package owns the file,
// a cross-package label (with the producer publicized) when another package
// does. Pre-fix they passed through verbatim as element-root-relative paths,
// which Bazel resolved against the consumer's package — 80 missing inputs on
// VTK's IOInfovis. Target refs (":x") keep their deps-style relabel.
func TestEmitSplit_DataFilePaths_RewrittenPerPackage(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			// Producer of a generated non-cc artifact in gen/.
			{
				Name:         "gen_args",
				Kind:         ir.KindWriteFile,
				WriteFileOut: "gen/CMakeFiles/mod-hierarchy.Release.args",
			},
			// A real cc target making gen/ a package.
			{Name: "genlib", Kind: ir.KindCCLibrary, Srcs: []string{"gen/genlib.cpp"}},
			// Cross-package consumer: data names the OTHER package's artifact
			// (element-root-relative, the lower-side attribution shape).
			{
				Name: "consumer",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"lib/consumer.cpp"},
				Data: []string{"gen/CMakeFiles/mod-hierarchy.Release.args"},
			},
			// Same-package consumer: data names its OWN package's artifact.
			{
				Name: "selfuser",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"gen/selfuser.cpp"},
				Data: []string{"gen/CMakeFiles/mod-hierarchy.Release.args"},
			},
		},
		SubPackages: map[string]string{
			"gen_args": "gen",
			"genlib":   "gen",
			"consumer": "lib",
			"selfuser": "gen",
		},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	gen := string(tree["gen"])
	lib := string(tree["lib"])

	// Cross-package consumer: full label into the owning package.
	if !strings.Contains(lib, `"//elements/p/gen:CMakeFiles/mod-hierarchy.Release.args"`) {
		t.Errorf("consumer data should carry the cross-package label:\n%s", lib)
	}
	if strings.Contains(lib, `"gen/CMakeFiles/mod-hierarchy.Release.args"`) {
		t.Errorf("consumer data still carries the verbatim element-relative path:\n%s", lib)
	}
	// Same-package consumer: package-relative path.
	if !strings.Contains(gen, `data = ["CMakeFiles/mod-hierarchy.Release.args"]`) {
		t.Errorf("same-package data should be package-relative:\n%s", gen)
	}
	// The producer must be publicized for the cross-package data ref.
	if !strings.Contains(gen, "gen_args") {
		t.Fatalf("producer missing from gen package:\n%s", gen)
	}
	genArgsRule := gen[strings.Index(gen, `name = "gen_args"`):]
	if end := strings.Index(genArgsRule, ")"); end > 0 {
		genArgsRule = genArgsRule[:end]
	}
	if strings.Contains(genArgsRule, `//visibility:private`) {
		t.Errorf("cross-package-consumed producer should be publicized:\n%s", genArgsRule)
	}
}

// A cmake_configure_file output consumed cross-package must be reached through
// the producing rule's output label (//pkg:out), NOT exports_files()'d. The
// configure_file / file(GENERATE) lift produces its `out` at build time just
// like a genrule or write_file, so it belongs in splitPlan.genOuts — without
// that, splitFileLabel records a bogus exports_files() for the generated file,
// which both misrepresents it as a source and fails to load (the package
// already declares the same label as a generated output).
func TestEmitSplit_ConfigureFileOut_CrossPackageLabelNotExportsFiles(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			// gen/ holds the configure_file output plus a real cc target that
			// makes gen/ a Bazel package (deepestPkg).
			{Name: "genlib", Kind: ir.KindCCLibrary, Srcs: []string{"gen/genlib.cpp"}},
			{
				Name:               "gen_config_h",
				Kind:               ir.KindCMakeConfigureFile,
				CMakeConfigureFile: &ir.CMakeConfigureFileSpec{Out: "gen/config.h", Template: "gen/config.h.in"},
			},
			// A consumer in lib/ that lists the generated header (element-root
			// relative) in hdrs — the cross-package case.
			{Name: "consumer", Kind: ir.KindCCLibrary, Srcs: []string{"lib/consumer.cpp"}, Hdrs: []string{"gen/config.h"}},
		},
		SubPackages: map[string]string{"genlib": "gen", "consumer": "lib"},
	}
	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit: %v", err)
	}
	gen := string(tree["gen"])
	lib := string(tree["lib"])

	// The producing package must NOT exports_files() the generated header.
	if strings.Contains(gen, "exports_files") {
		t.Errorf("gen/ package must not exports_files() the configure_file-generated header:\n%s", gen)
	}
	// The producing rule is still emitted in gen/.
	if !strings.Contains(gen, `name = "gen_config_h"`) {
		t.Errorf("gen/ package missing the cmake_configure_file rule:\n%s", gen)
	}
	// The consumer reaches the generated header through the producing rule's
	// cross-package output label, not a path or an exports_files reference.
	if !strings.Contains(lib, `hdrs = ["//elements/p/gen:config.h"]`) {
		t.Errorf("consumer should reference the generated header by cross-package label:\n%s", lib)
	}
}
