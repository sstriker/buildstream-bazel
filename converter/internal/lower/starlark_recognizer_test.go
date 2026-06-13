package lower

import (
	"os"
	"path/filepath"
	"testing"
)

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

// A non-matching driver: Match declines.
func TestStarlarkRecognizer_MatchDeclines(t *testing.T) {
	r := loadStarFromString(t, "protoc.star", protocStar)
	if r.Match(CodegenCommand{Driver: "flatc", Args: []string{"--cpp"}}) {
		t.Error("flatc should not match the protoc recognizer")
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
