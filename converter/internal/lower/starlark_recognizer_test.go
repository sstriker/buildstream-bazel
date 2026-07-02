package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// stdoutGenStar is an operator recognizer for a STDOUT generator (`sgen in > out`)
// — a tool with no native Bazel rule, so lower() returns a genrule(...) rather
// than a native_rule(...). It supplies the OUTPUT_FILE basename (fed as
// cmd.discovered_outputs on the execute_process path) as both the genrule out and
// the derived output.
const stdoutGenStar = `
def match(cmd):
    return cmd.driver == "sgen"

def lower(cmd):
    out = cmd.discovered_outputs[0]
    return result(
        targets = [
            genrule(
                name = "sgen_gen",
                cmd = "$(location //tools:sgen) $(SRCS) > $@",
                outs = [out],
                srcs = cmd.srcs,
                tools = ["//tools:sgen"],
            ),
        ],
        consumer_deps = [":sgen_gen"],
        derived_outputs = [out],
    )
`

// TestStarlarkRecognizer_GenruleBuiltin pins the genrule(...) builtin: an operator
// recognizer for a stdout generator lowers to an ir.KindGenrule target carrying
// the cmd / outs / srcs / tools it declared.
func TestStarlarkRecognizer_GenruleBuiltin(t *testing.T) {
	r := loadStarFromString(t, "sgen.star", stdoutGenStar)
	cmd := CodegenCommand{Driver: "sgen", Srcs: []string{"in.x"}, DiscoveredOutputs: []string{"out.h"}}
	if !r.Match(cmd) {
		t.Fatal("expected Match to claim sgen")
	}
	res, err := r.Lower(cmd)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(res.Targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(res.Targets))
	}
	g := res.Targets[0]
	if g.Kind != ir.KindGenrule || g.Name != "sgen_gen" {
		t.Fatalf("want a KindGenrule named sgen_gen, got kind=%v name=%q", g.Kind, g.Name)
	}
	if g.GenruleCmd != "$(location //tools:sgen) $(SRCS) > $@" {
		t.Errorf("cmd = %q", g.GenruleCmd)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "out.h" {
		t.Errorf("outs = %v, want [out.h]", g.GenruleOuts)
	}
	if len(g.Srcs) != 1 || g.Srcs[0] != "in.x" {
		t.Errorf("srcs = %v, want [in.x]", g.Srcs)
	}
	if len(g.GenruleTools) != 1 || g.GenruleTools[0] != "//tools:sgen" {
		t.Errorf("tools = %v, want [//tools:sgen]", g.GenruleTools)
	}
	if len(res.DerivedOutputs) != 1 || res.DerivedOutputs[0] != "out.h" {
		t.Errorf("derived = %v, want [out.h]", res.DerivedOutputs)
	}
}

// TestStarlarkRecognizer_GenruleRequiresOuts: a genrule(...) with no outs is a
// declaration error (surfaced from lower()).
func TestStarlarkRecognizer_GenruleRequiresOuts(t *testing.T) {
	src := `
def match(cmd):
    return True
def lower(cmd):
    return result(targets = [genrule(name = "g", cmd = "x > $@", outs = [])], derived_outputs = ["x"])
`
	r := loadStarFromString(t, "bad.star", src)
	if _, err := r.Lower(CodegenCommand{Driver: "t"}); err == nil {
		t.Fatal("a genrule with no outs must error")
	}
}

// TestStarlarkTarget_NonStringConstructSurfaces: a recognizer struct whose
// _construct field is present but not a string is malformed — starlarkTarget must
// surface that directly rather than silently falling through to a confusing
// native_rule error.
func TestStarlarkTarget_NonStringConstructSurfaces(t *testing.T) {
	st := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"_construct": starlark.MakeInt(1),
	})
	if _, err := starlarkTarget(st); err == nil {
		t.Fatal("a non-string _construct must surface an error, not fall through")
	}
}

// TestStarlarkGenruleTarget_NonStringCmdSurfaces: a genrule struct whose cmd is
// present but not a string surfaces the type error, not the generic
// "requires a non-empty name and cmd" message that hides the real problem.
func TestStarlarkGenruleTarget_NonStringCmdSurfaces(t *testing.T) {
	st := starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"_construct": starlark.String("genrule"),
		"name":       starlark.String("g"),
		"cmd":        starlark.MakeInt(1),
	})
	_, err := starlarkTarget(st)
	if err == nil {
		t.Fatal("a non-string cmd must surface a type error")
	}
	if !strings.Contains(err.Error(), "cmd") {
		t.Errorf("error should name the cmd field; got %v", err)
	}
}

// TestStarlarkRecognizer_ScalarOutsRejected: passing a bare string where a list
// is expected (outs = "gen.h") must fail fast with a clear error, not silently
// iterate the string into single characters.
func TestStarlarkRecognizer_ScalarOutsRejected(t *testing.T) {
	src := `
def match(cmd):
    return True
def lower(cmd):
    return result(targets = [genrule(name = "g", cmd = "x > $@", outs = "gen.h")], derived_outputs = ["gen.h"])
`
	r := loadStarFromString(t, "scalar.star", src)
	_, err := r.Lower(CodegenCommand{Driver: "t"})
	if err == nil {
		t.Fatal("a scalar string for outs must error, not split into characters")
	}
	if !strings.Contains(err.Error(), "list") {
		t.Errorf("error should hint the list requirement; got %v", err)
	}
}

const protocStar = `
def match(cmd):
    return cmd.driver.startswith("protoc") and any([a.startswith("--cpp_out") for a in cmd.args])

def lower(cmd):
    base = cmd.srcs[0].rsplit("/", 1)[-1][:-len(".proto")]
    return result(
        targets = [
            native_rule("proto_library", base + "_proto",
                        load_from = "@protobuf//bazel:proto_library.bzl",
                        attrs = {"srcs": [base + ".proto"]}),
            native_rule("cc_proto_library", base + "_cc_proto",
                        load_from = "@protobuf//bazel:cc_proto_library.bzl",
                        attrs = {"deps": [":" + base + "_proto"]}),
        ],
        consumer_deps = [":" + base + "_cc_proto"],
        derived_outputs = [base + ".pb.cc", base + ".pb.h"],
    )
`

func loadStarFromString(t *testing.T, name, src string) CodegenRecognizer {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := LoadStarlarkRecognizers([]string{p})
	if err != nil {
		t.Fatalf("LoadStarlarkRecognizers: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 recognizer, got %d", len(recs))
	}
	return recs[0]
}

func TestStarlarkRecognizer_MatchAndLower(t *testing.T) {
	r := loadStarFromString(t, "protoc.star", protocStar)
	if got := r.Name(); got != "starlark:protoc" {
		t.Errorf("Name = %q, want starlark:protoc", got)
	}
	cmd := CodegenCommand{
		Driver: "protoc",
		Args:   []string{"--cpp_out=.", "foo.proto"},
		Srcs:   []string{"foo.proto"},
		Outs:   []string{"foo.pb.cc", "foo.pb.h"},
		Pkg:    "pkg",
	}
	if !r.Match(cmd) {
		t.Fatal("expected Match to claim protoc --cpp_out")
	}
	res, err := r.Lower(cmd)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(res.Targets))
	}
	pl, cc := res.Targets[0], res.Targets[1]
	if pl.Name != "foo_proto" || pl.NativeRule == nil || pl.NativeRule.Kind != "proto_library" {
		t.Errorf("proto_library target wrong: %+v", pl)
	}
	if pl.NativeRule.LoadFrom != "@protobuf//bazel:proto_library.bzl" {
		t.Errorf("proto_library LoadFrom = %q", pl.NativeRule.LoadFrom)
	}
	if got := attrList(t, pl, "srcs"); len(got) != 1 || got[0] != "foo.proto" {
		t.Errorf("proto_library srcs = %v", got)
	}
	if cc.Name != "foo_cc_proto" || cc.NativeRule.Kind != "cc_proto_library" {
		t.Errorf("cc_proto_library target wrong: %+v", cc)
	}
	if got := attrList(t, cc, "deps"); len(got) != 1 || got[0] != ":foo_proto" {
		t.Errorf("cc_proto_library deps = %v", got)
	}
	if len(res.ConsumerDeps) != 1 || res.ConsumerDeps[0] != ":foo_cc_proto" {
		t.Errorf("ConsumerDeps = %v", res.ConsumerDeps)
	}
}

// A content-derived-output recognizer reads cmd.discovered_outputs (the set the
// generic genrule would declare) and returns the ".h" subset — the use case
// CodegenCommand.DiscoveredOutputs exists for, exercised end-to-end through the
// Starlark bridge.
func TestStarlarkRecognizer_ReadsDiscoveredOutputs(t *testing.T) {
	const star = `
def match(cmd):
    return cmd.driver == "mygen"

def lower(cmd):
    hdrs = [o for o in cmd.discovered_outputs if o.endswith(".h")]
    return result(
        targets = [native_rule("mygen_lib", "mygen_rule", attrs = {"srcs": hdrs})],
        derived_outputs = hdrs,
    )
`
	r := loadStarFromString(t, "mygen.star", star)
	cmd := CodegenCommand{
		Driver:            "mygen",
		Srcs:              []string{"in.x"},
		DiscoveredOutputs: []string{"a.h", "a.cc", "b.h"},
	}
	if !r.Match(cmd) {
		t.Fatal("mygen recognizer should match")
	}
	res, err := r.Lower(cmd)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if got := res.DerivedOutputs; len(got) != 2 || got[0] != "a.h" || got[1] != "b.h" {
		t.Errorf("derived_outputs from discovered_outputs = %v, want [a.h b.h]", got)
	}
}

// Parity with the Go built-ins: a recognizer can place its rule via
// result(sub_package=…) and branch on cmd.sibling_cpp_proto — the two fields
// protoc/grpc built-ins use that the bridge previously didn't expose.
func TestStarlarkRecognizer_SubPackageAndSiblingProto(t *testing.T) {
	const star = `
def match(cmd):
    return cmd.driver == "protoc"

def lower(cmd):
    base = cmd.srcs[0].rsplit("/", 1)[-1][:-len(".proto")]
    dir = cmd.srcs[0].rsplit("/", 1)[0] if "/" in cmd.srcs[0] else ""
    tgts = [native_rule("proto_library", base + "_proto", attrs = {"srcs": [base + ".proto"]})]
    # Only emit cc_proto when a sibling cpp call isn't already producing it.
    if not cmd.sibling_cpp_proto:
        tgts.append(native_rule("cc_proto_library", base + "_cc_proto", attrs = {"deps": [":" + base + "_proto"]}))
    return result(targets = tgts, derived_outputs = [base + ".pb.cc", base + ".pb.h"], sub_package = dir)
`
	r := loadStarFromString(t, "p.star", star)
	res, err := r.Lower(CodegenCommand{Driver: "protoc", Srcs: []string{"sub/a.proto"}, SiblingCppProto: true})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if res.SubPackage != "sub" {
		t.Errorf("sub_package = %q, want sub", res.SubPackage)
	}
	if len(res.Targets) != 1 || res.Targets[0].Name != "a_proto" {
		t.Errorf("sibling_cpp_proto=true should suppress the cc_proto target; got %+v", res.Targets)
	}
}

// A bool attr (grpc_only = True) renders as a bare identifier (NativeAttr.Ident),
// which cc_grpc_library needs.
func TestStarlarkRecognizer_BoolAttrIsIdent(t *testing.T) {
	const star = `
def match(cmd):
    return cmd.driver == "protoc"

def lower(cmd):
    return result(
        targets = [native_rule("cc_grpc_library", "g_cc_grpc",
                               load_from = "@grpc//bazel:cc_grpc_library.bzl",
                               attrs = {"grpc_only": True, "deps": [":g_cc_proto"]})],
        derived_outputs = ["g.grpc.pb.cc"],
    )
`
	r := loadStarFromString(t, "g.star", star)
	res, err := r.Lower(CodegenCommand{Driver: "protoc", Srcs: []string{"g.proto"}})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var ident string
	for _, a := range res.Targets[0].NativeRule.Attrs {
		if a.Name == "grpc_only" {
			ident = a.Ident
		}
	}
	if ident != "True" {
		t.Errorf("grpc_only should render as bare identifier True; got Ident=%q", ident)
	}
}

// A non-matching driver: Match declines.
func TestStarlarkRecognizer_MatchDeclines(t *testing.T) {
	r := loadStarFromString(t, "protoc.star", protocStar)
	if r.Match(CodegenCommand{Driver: "flatc", Args: []string{"--cpp"}}) {
		t.Error("flatc should not match the protoc recognizer")
	}
}

// Supply mode (execute_process shape): empty Outs → no cross-check, the script's
// derived_outputs are returned for the caller to corroborate on-disk.
func TestStarlarkRecognizer_SupplyMode(t *testing.T) {
	r := loadStarFromString(t, "protoc.star", protocStar)
	res, err := r.Lower(CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=."}, Srcs: []string{"foo.proto"}})
	if err != nil {
		t.Fatalf("Lower supply mode: %v", err)
	}
	if got := res.DerivedOutputs; len(got) != 2 || got[0] != "foo.pb.cc" || got[1] != "foo.pb.h" {
		t.Errorf("DerivedOutputs = %v, want [foo.pb.cc foo.pb.h]", got)
	}
}

// The host runs the output-authority cross-check: a derived-vs-recorded
// mismatch surfaces as a Lower error (→ genrule fallback).
func TestStarlarkRecognizer_OutputCrossCheck(t *testing.T) {
	r := loadStarFromString(t, "protoc.star", protocStar)
	cmd := CodegenCommand{
		Driver: "protoc",
		Args:   []string{"--cpp_out=.", "foo.proto"},
		Srcs:   []string{"foo.proto"},
		Outs:   []string{"foo.pb.cc", "foo.pb.h", "foo.unexpected.h"},
		Pkg:    "pkg",
	}
	if _, err := r.Lower(cmd); err == nil {
		t.Error("expected a cross-check error on the extra recorded output")
	}
}

// recognizeCodegenWith consults operator recognizers after the built-ins.
func TestRecognizeCodegenWith_OperatorRecognizerFires(t *testing.T) {
	const genPbStar = `
def match(cmd):
    return cmd.driver == "gen_pb"

def lower(cmd):
    return result(
        targets = [native_rule("proto_library", "foo_proto",
                               load_from = "@protobuf//bazel:proto_library.bzl",
                               attrs = {"srcs": ["foo.proto"]})],
        consumer_deps = [":foo_cc_proto"],
        derived_outputs = ["foo.pb.cc", "foo.pb.h"],
    )
`
	r := loadStarFromString(t, "gen_pb.star", genPbStar)
	cmd := CodegenCommand{Driver: "gen_pb", Srcs: []string{"foo.proto"}, Outs: []string{"foo.pb.cc", "foo.pb.h"}}
	res, matched, err := recognizeCodegenWith([]CodegenRecognizer{r}, cmd)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Name != "foo_proto" {
		t.Errorf("targets = %+v", res.Targets)
	}
	// A command no recognizer claims still falls through.
	if _, matched, _ := recognizeCodegenWith([]CodegenRecognizer{r}, CodegenCommand{Driver: "other"}); matched {
		t.Error("unclaimed command should not match")
	}
}

// A missing match/lower function is a hard load error.
func TestLoadStarlarkRecognizers_MissingFunc(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.star")
	if err := os.WriteFile(p, []byte("def match(cmd):\n    return True\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStarlarkRecognizers([]string{p}); err == nil {
		t.Error("expected an error for a recognizer missing lower()")
	}
}
