package lower

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// discoveredSubsetRecognizer models a tool whose outputs are derived from input
// CONTENTS: it can't predict them from a naming convention, so it returns the
// ".h" subset of the converter's DiscoveredOutputs as its DerivedOutputs.
type discoveredSubsetRecognizer struct{}

func (discoveredSubsetRecognizer) Name() string                  { return "test-discovered-subset" }
func (discoveredSubsetRecognizer) Match(cmd CodegenCommand) bool { return cmd.Driver == "mygen" }
func (discoveredSubsetRecognizer) Lower(cmd CodegenCommand) (CodegenResult, error) {
	var outs []string
	for _, o := range cmd.DiscoveredOutputs {
		if strings.HasSuffix(o, ".h") {
			outs = append(outs, o)
		}
	}
	return CodegenResult{
		DerivedOutputs: outs,
		ConsumerDeps:   []string{":mygen_rule"},
		Targets:        []ir.Target{{Name: "mygen_rule", Kind: ir.KindGenrule, GenruleOuts: outs}},
	}, nil
}

// TestRecognizeOrGenrule_FeedsDiscoveredOutputs: the chokepoint feeds the
// recognizer the genrule fallback's outs as DiscoveredOutputs, so a
// content-derived-output recognizer can return a subset.
func TestRecognizeOrGenrule_FeedsDiscoveredOutputs(t *testing.T) {
	fallback := ir.Target{
		Name: "g", Kind: ir.KindGenrule,
		GenruleOuts: []string{"a.cc", "a.h", "b.h"},
		GenruleCmd:  "mygen in.x",
	}
	cmd := CodegenCommand{Driver: "mygen", Args: []string{"in.x"}}
	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	cc.ExtraRecognizers = []CodegenRecognizer{discoveredSubsetRecognizer{}}
	tgts, recognized := recognizeOrGenrule(cc, cmd, fallback)
	if !recognized || len(tgts) != 1 {
		t.Fatalf("recognizer should claim via discovered outputs; recognized=%v tgts=%+v", recognized, tgts)
	}
	if got := tgts[0].GenruleOuts; len(got) != 2 || got[0] != "a.h" || got[1] != "b.h" {
		t.Errorf("recognizer should return the .h subset of discovered outs; got %v", got)
	}
}

// On the execute_process path the converter discovers the on-disk files under
// the tool's --*_out dir and feeds them as DiscoveredOutputs (output-dir-
// relative); the recognizer returns the subset, which the caller anchors +
// corroborates on disk.
func TestLiftRecognizedExecuteProcess_FeedsDiscoveredOutputs(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.x", "spec\n")
	writeTree(t, hostBuild, "gen/a.h", "A")
	writeTree(t, hostBuild, "gen/a.cc", "AC")
	writeTree(t, hostBuild, "gen/b.h", "B")
	call := argvCall(hostSrc, "mygen", "--mygen_out="+filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.x"))
	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	cc.ExtraRecognizers = []CodegenRecognizer{discoveredSubsetRecognizer{}}
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	got := make([]string, len(outs))
	for i, o := range outs {
		got[i] = o.RelOutput
	}
	if len(got) != 2 || got[0] != "gen/a.h" || got[1] != "gen/b.h" {
		t.Fatalf("recognizer should claim the .h subset under the out dir; got %v", got)
	}
	if len(cc.Genrules) != 1 || cc.Genrules[0].Name != "mygen_rule" {
		t.Fatalf("recognizer's native rule should be emitted; got %+v", cc.Genrules)
	}
}

// TestRecognizeOrGenrule_DedupsSameInputAcrossOutDirs: the SAME input run into
// two output dirs is ONE canonical native rule — the second invocation reuses it
// (no duplicate target) and wires its outputs to the existing consumer dep.
func TestRecognizeOrGenrule_DedupsSameInputAcrossOutDirs(t *testing.T) {
	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	cc.ExtraRecognizers = []CodegenRecognizer{discoveredSubsetRecognizer{}}

	fb1 := ir.Target{Name: "g1", Kind: ir.KindGenrule, GenruleOuts: []string{"gen1/a.h", "gen1/a.cc"}}
	cmd1 := CodegenCommand{Driver: "mygen", Srcs: []string{"in.x"}, Outs: []string{"gen1/a.h"}}
	t1, ok1 := recognizeOrGenrule(cc, cmd1, fb1)
	if !ok1 || len(t1) != 1 || t1[0].Name != "mygen_rule" {
		t.Fatalf("first invocation should emit the native rule; ok=%v t=%+v", ok1, t1)
	}

	fb2 := ir.Target{Name: "g2", Kind: ir.KindGenrule, GenruleOuts: []string{"gen2/a.h", "gen2/a.cc"}}
	cmd2 := CodegenCommand{Driver: "mygen", Srcs: []string{"in.x"}, Outs: []string{"gen2/a.h"}}
	t2, ok2 := recognizeOrGenrule(cc, cmd2, fb2)
	if !ok2 {
		t.Fatal("second (same input) is still recognized, not a genrule fallback")
	}
	if len(t2) != 0 {
		t.Errorf("the SAME input in a second out dir must NOT re-emit a duplicate rule; got %+v", t2)
	}
	if cc.OutToNativeConsumerDep["gen2/a.h"] != "mygen_rule" {
		t.Errorf("the deduped invocation's output should wire to the existing rule; got %q", cc.OutToNativeConsumerDep["gen2/a.h"])
	}
}

// TestRecognizeOrGenrule_DifferentInputsDifferentPackagesBothEmit: distinct
// inputs landing in distinct packages both emit (no false dedup, no collision).
func TestRecognizeOrGenrule_DifferentInputsDifferentPackagesBothEmit(t *testing.T) {
	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	cc.ExtraRecognizers = []CodegenRecognizer{discoveredSubsetRecognizer{}}

	_, ok1 := recognizeOrGenrule(cc,
		CodegenCommand{Driver: "mygen", Srcs: []string{"a.x"}, Outs: []string{"gen1/a.h"}},
		ir.Target{Name: "g1", Kind: ir.KindGenrule, GenruleOuts: []string{"gen1/a.h"}})
	t2, ok2 := recognizeOrGenrule(cc,
		CodegenCommand{Driver: "mygen", Srcs: []string{"b.x"}, Outs: []string{"gen2/b.h"}},
		ir.Target{Name: "g2", Kind: ir.KindGenrule, GenruleOuts: []string{"gen2/b.h"}})
	if !ok1 || !ok2 || len(t2) != 1 {
		t.Fatalf("distinct inputs in distinct packages both emit; ok1=%v ok2=%v t2=%+v", ok1, ok2, t2)
	}
}

// TestRecognizeOrGenrule_DifferentInputsCollidingNameFallBack: two DIFFERENT
// inputs that would emit the same rule name in the same package (a recognizer
// naming bug) — the second falls back to the generic genrule rather than emit a
// load-breaking duplicate.
func TestRecognizeOrGenrule_DifferentInputsCollidingNameFallBack(t *testing.T) {
	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	cc.ExtraRecognizers = []CodegenRecognizer{discoveredSubsetRecognizer{}}

	_, ok1 := recognizeOrGenrule(cc,
		CodegenCommand{Driver: "mygen", Srcs: []string{"a.x"}, Outs: []string{"gen/a.h"}},
		ir.Target{Name: "g1", Kind: ir.KindGenrule, GenruleOuts: []string{"gen/a.h"}})
	if !ok1 {
		t.Fatal("first invocation emits")
	}
	fb2 := ir.Target{Name: "g2", Kind: ir.KindGenrule, GenruleOuts: []string{"gen/b.h"}}
	t2, ok2 := recognizeOrGenrule(cc,
		CodegenCommand{Driver: "mygen", Srcs: []string{"b.x"}, Outs: []string{"gen/b.h"}}, fb2)
	if ok2 {
		t.Fatalf("a different input colliding on rule name must fall back; got ok=%v", ok2)
	}
	if len(t2) != 1 || t2[0].Name != "g2" {
		t.Errorf("fallback should be the generic genrule; got %+v", t2)
	}
}

// TestRecognizeOrGenrule_FidelityMismatch: a recognizer that MATCHES the tool
// but whose derived outputs disagree with cmake's recorded ones refuses (a loud
// build-time stub) under --fidelity=strict, and falls back to the genrule under
// best-effort.
func TestRecognizeOrGenrule_FidelityMismatch(t *testing.T) {
	fallback := ir.Target{
		Name: "g", Kind: ir.KindGenrule,
		GenruleOuts: []string{"foo.pb.cc", "foo.pb.h", "foo.weird.h"},
		GenruleCmd:  "protoc --cpp_out=. foo.proto",
	}
	// protoc claims it (--cpp_out) but cmake recorded an extra output the cpp
	// convention doesn't predict → matched-but-non-standard.
	cmd := CodegenCommand{
		Driver: "protoc", Args: []string{"--cpp_out=."}, Srcs: []string{"foo.proto"},
		Outs: []string{"foo.pb.cc", "foo.pb.h", "foo.weird.h"},
	}
	strict := newCodegenContext()
	strict.RecognizeCodegen = true
	strict.Fidelity = "strict"
	tgts, recognized := recognizeOrGenrule(strict, cmd, fallback)
	if recognized {
		t.Error("a non-standard claim is not a recognized native lower")
	}
	if len(tgts) != 1 || !strings.Contains(tgts[0].GenruleCmd, "exit 1") {
		t.Errorf("strict should emit a refusal stub, got %+v", tgts)
	}

	best := newCodegenContext()
	best.RecognizeCodegen = true
	best.Fidelity = "best-effort"
	tgts, _ = recognizeOrGenrule(best, cmd, fallback)
	if len(tgts) != 1 || tgts[0].GenruleCmd != fallback.GenruleCmd {
		t.Errorf("best-effort should fall back to the genrule, got %+v", tgts)
	}
}

// TestProtocCppRecognizer_SupplyMode: the execute_process shape passes no
// recorded Outs, so the recognizer SUPPLIES the derived set without a
// cross-check (the caller corroborates on-disk).
func TestProtocCppRecognizer_SupplyMode(t *testing.T) {
	res, matched, err := recognizeCodegen(CodegenCommand{
		Driver: "protoc", Args: []string{"--cpp_out=."}, Srcs: []string{"foo.proto"},
	})
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	if got := res.DerivedOutputs; len(got) != 2 || got[0] != "foo.pb.cc" || got[1] != "foo.pb.h" {
		t.Errorf("DerivedOutputs = %v, want [foo.pb.cc foo.pb.h]", got)
	}
}

// TestRewriteNativeRuleConsumers: a cc target listing a recognized codegen
// output as a src/hdr has it stripped and a direct deps edge to the native rule
// added.
func TestRewriteNativeRuleConsumers(t *testing.T) {
	cc := newCodegenContext()
	cc.OutToNativeConsumerDep["foo.pb.cc"] = "foo_cc_proto"
	cc.OutToNativeConsumerDep["foo.pb.h"] = "foo_cc_proto"
	pkg := &ir.Package{Targets: []ir.Target{{
		Name: "use_foo", Kind: ir.KindCCLibrary,
		Srcs: []string{"foo.pb.cc", "use_foo.cc"}, Hdrs: []string{"foo.pb.h"}, Deps: []string{"//x:y"},
	}}}
	rewriteNativeRuleConsumers(pkg, cc)
	tgt := pkg.Targets[0]
	if len(tgt.Srcs) != 1 || tgt.Srcs[0] != "use_foo.cc" {
		t.Errorf("Srcs = %v, want [use_foo.cc]", tgt.Srcs)
	}
	if len(tgt.Hdrs) != 0 {
		t.Errorf("Hdrs = %v, want []", tgt.Hdrs)
	}
	if !slices.Contains(tgt.Deps, ":foo_cc_proto") {
		t.Errorf("Deps = %v, want to contain :foo_cc_proto", tgt.Deps)
	}
}

// TestProtoImportLabels: a .proto's source-tree imports map to the proto_library
// labels the recognizer's deps need; well-known types (not under cmakeSrc) drop.
func TestProtoImportLabels(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"pkg/a/a.proto": "syntax=\"proto3\";\n",
		"pkg/b/b.proto": "syntax=\"proto3\";\nimport \"pkg/a/a.proto\";\nimport \"google/protobuf/any.proto\";\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outs := []string{"pkg/b/b.pb.cc"} // proto_path = source root → no rebase
	got := protoImportLabels([]string{"pkg/b/b.proto"}, outs, dir, "")
	if len(got) != 1 || got[0] != "//pkg/a:a_proto" {
		t.Errorf("protoImportLabels = %v, want [//pkg/a:a_proto] (well-known any.proto dropped)", got)
	}
	// With an element package prefix.
	got = protoImportLabels([]string{"pkg/b/b.proto"}, outs, dir, "elements/x")
	if len(got) != 1 || got[0] != "//elements/x/pkg/a:a_proto" {
		t.Errorf("protoImportLabels(prefixed) = %v", got)
	}
}

// TestProtoPathRoot: the proto_path root is recovered from the mismatch between
// the proto's source path and its canonical (output-derived) name.
func TestProtoPathRoot(t *testing.T) {
	cases := []struct {
		proto string
		outs  []string
		want  string
	}{
		{"foo.proto", []string{"foo.pb.cc", "foo.pb.h"}, ""},             // root proto, proto_path = source root
		{"pkg/a/a.proto", []string{"pkg/a/a.pb.cc", "pkg/a/a.pb.h"}, ""}, // subdir proto, proto_path = source root
		{"proto/dep.proto", []string{"dep.pb.cc", "dep.pb.h"}, "proto"},  // rebased proto_path = proto/
		{"x/y/m.proto", []string{"m.pb.cc"}, "x/y"},                      // rebased two levels
	}
	for _, c := range cases {
		if got := protoPathRoot(c.proto, c.outs); got != c.want {
			t.Errorf("protoPathRoot(%q, %v) = %q, want %q", c.proto, c.outs, got, c.want)
		}
	}
}

func TestOutputClaimed(t *testing.T) {
	cc := newCodegenContext()
	if cc.outputClaimed("x") {
		t.Error("x should be unclaimed")
	}
	cc.OutToGenrule["g"] = "gen"
	cc.OutToNativeConsumerDep["n"] = "lib"
	if !cc.outputClaimed("g") || !cc.outputClaimed("n") {
		t.Error("g (genrule) and n (native) should both be claimed")
	}
}

func attrList(t *testing.T, tgt ir.Target, name string) []string {
	t.Helper()
	if tgt.NativeRule == nil {
		t.Fatalf("target %q has nil NativeRule", tgt.Name)
	}
	for _, a := range tgt.NativeRule.Attrs {
		if a.Name == name {
			return a.List
		}
	}
	return nil
}

// TestProtocCppRecognizer_Match: fires on protoc --cpp_out, not on a
// grpc-only protoc invocation or a non-protoc tool.
func TestProtocCppRecognizer_Match(t *testing.T) {
	r := protocCppRecognizer{}
	cases := []struct {
		name string
		cmd  CodegenCommand
		want bool
	}{
		{"cpp", CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=.", "foo.proto"}}, true},
		{"cpp-eq-dir", CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=gen", "foo.proto"}}, true},
		{"grpc-only", CodegenCommand{Driver: "protoc", Args: []string{"--grpc_out=.", "foo.proto"}}, false},
		{"not-protoc", CodegenCommand{Driver: "flatc", Args: []string{"--cpp_out=.", "x.fbs"}}, false},
	}
	for _, c := range cases {
		if got := r.Match(c.cmd); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestProtocCppRecognizer_Lower: a standard protoc --cpp_out command lowers to
// proto_library + cc_proto_library, with the derived-output cross-check passing
// and the consumer dep pointing at the cc_proto_library.
func TestProtocCppRecognizer_Lower(t *testing.T) {
	cmd := CodegenCommand{
		Driver: "protoc",
		Args:   []string{"--proto_path=.", "--cpp_out=.", "foo.proto"},
		Srcs:   []string{"foo.proto"},
		Outs:   []string{"foo.pb.cc", "foo.pb.h"},
		Pkg:    "pkg",
	}
	res, matched, err := recognizeCodegen(cmd)
	if !matched || err != nil {
		t.Fatalf("recognizeCodegen matched=%v err=%v", matched, err)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("want 2 targets, got %d: %+v", len(res.Targets), res.Targets)
	}
	pl, cc := res.Targets[0], res.Targets[1]
	if pl.Name != "foo_proto" || pl.NativeRule.Kind != "proto_library" {
		t.Errorf("proto_library target wrong: %+v", pl)
	}
	if got := attrList(t, pl, "srcs"); len(got) != 1 || got[0] != "foo.proto" {
		t.Errorf("proto_library srcs = %v, want [foo.proto]", got)
	}
	if cc.Name != "foo_cc_proto" || cc.NativeRule.Kind != "cc_proto_library" {
		t.Errorf("cc_proto_library target wrong: %+v", cc)
	}
	if got := attrList(t, cc, "deps"); len(got) != 1 || got[0] != ":foo_proto" {
		t.Errorf("cc_proto_library deps = %v, want [:foo_proto]", got)
	}
	if len(res.ConsumerDeps) != 1 || res.ConsumerDeps[0] != ":foo_cc_proto" {
		t.Errorf("ConsumerDeps = %v, want [:foo_cc_proto]", res.ConsumerDeps)
	}
}

// TestProtocCppRecognizer_ImportDeps: resolved proto import deps thread onto the
// proto_library's deps (the cross-package edge).
func TestProtocCppRecognizer_ImportDeps(t *testing.T) {
	cmd := CodegenCommand{
		Driver:    "protoc",
		Args:      []string{"--cpp_out=.", "b.proto"},
		Srcs:      []string{"b.proto"},
		Outs:      []string{"b.pb.cc", "b.pb.h"},
		Pkg:       "pkg/b",
		ProtoDeps: []string{"//pkg/a:a_proto"},
	}
	res, matched, err := recognizeCodegen(cmd)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	if got := attrList(t, res.Targets[0], "deps"); len(got) != 1 || got[0] != "//pkg/a:a_proto" {
		t.Errorf("proto_library deps = %v, want [//pkg/a:a_proto]", got)
	}
}

// TestProtocCppRecognizer_OutputCrossCheck: a derived-vs-recorded output
// mismatch makes Lower return an error (matched-but-non-standard), so the
// dispatch can refuse (strict) / fall back (best-effort).
func TestProtocCppRecognizer_OutputCrossCheck(t *testing.T) {
	cmd := CodegenCommand{
		Driver: "protoc",
		Args:   []string{"--cpp_out=.", "foo.proto"},
		Srcs:   []string{"foo.proto"},
		// Non-standard: an extra recorded output the cpp convention doesn't predict.
		Outs: []string{"foo.pb.cc", "foo.pb.h", "foo.weird.h"},
		Pkg:  "pkg",
	}
	_, matched, err := recognizeCodegen(cmd)
	if !matched {
		t.Fatalf("expected the recognizer to MATCH (protoc --cpp_out)")
	}
	if err == nil {
		t.Errorf("expected a cross-check error on the extra recorded output")
	}
}

// TestGrpcCppRecognizer_Match: fires on a COMBINED protoc --cpp_out+--grpc_out,
// not on cpp-only, grpc-only, or non-protoc.
func TestGrpcCppRecognizer_Match(t *testing.T) {
	r := grpcCppRecognizer{}
	cases := []struct {
		name string
		cmd  CodegenCommand
		want bool
	}{
		{"combined", CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=.", "--grpc_out=.", "foo.proto"}}, true},
		{"cpp-only", CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=.", "foo.proto"}}, false},
		{"grpc-only", CodegenCommand{Driver: "protoc", Args: []string{"--grpc_out=.", "foo.proto"}}, false},
		{"not-protoc", CodegenCommand{Driver: "flatc", Args: []string{"--cpp_out=.", "--grpc_out=.", "x.fbs"}}, false},
	}
	for _, c := range cases {
		if got := r.Match(c.cmd); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestGrpcCppRecognizer_Lower: a combined command lowers to proto_library +
// cc_proto_library + cc_grpc_library(grpc_only=True), consumer dep → the
// cc_grpc_library, and the 4-output set cross-checked.
func TestGrpcCppRecognizer_Lower(t *testing.T) {
	cmd := CodegenCommand{
		Driver: "protoc",
		Args:   []string{"--cpp_out=.", "--grpc_out=.", "svc.proto"},
		Srcs:   []string{"svc.proto"},
		Outs:   []string{"svc.pb.cc", "svc.pb.h", "svc.grpc.pb.cc", "svc.grpc.pb.h"},
		Pkg:    "pkg",
	}
	res, matched, err := recognizeCodegen(cmd)
	if !matched || err != nil {
		t.Fatalf("recognizeCodegen matched=%v err=%v", matched, err)
	}
	if len(res.Targets) != 3 {
		t.Fatalf("want 3 targets (proto, cc_proto, cc_grpc), got %d: %+v", len(res.Targets), res.Targets)
	}
	pl, cc, grpc := res.Targets[0], res.Targets[1], res.Targets[2]
	if pl.NativeRule.Kind != "proto_library" || pl.Name != "svc_proto" {
		t.Errorf("target 0 = %+v, want proto_library svc_proto", pl)
	}
	if cc.NativeRule.Kind != "cc_proto_library" || cc.Name != "svc_cc_proto" {
		t.Errorf("target 1 = %+v, want cc_proto_library svc_cc_proto", cc)
	}
	if grpc.NativeRule.Kind != "cc_grpc_library" || grpc.Name != "svc_cc_grpc" {
		t.Fatalf("target 2 = %+v, want cc_grpc_library svc_cc_grpc", grpc)
	}
	if grpc.NativeRule.LoadFrom != "@grpc//bazel:cc_grpc_library.bzl" {
		t.Errorf("cc_grpc_library LoadFrom = %q", grpc.NativeRule.LoadFrom)
	}
	if got := attrList(t, grpc, "srcs"); len(got) != 1 || got[0] != ":svc_proto" {
		t.Errorf("cc_grpc_library srcs = %v, want [:svc_proto]", got)
	}
	if got := attrList(t, grpc, "deps"); len(got) != 1 || got[0] != ":svc_cc_proto" {
		t.Errorf("cc_grpc_library deps = %v, want [:svc_cc_proto]", got)
	}
	var grpcOnly string
	for _, a := range grpc.NativeRule.Attrs {
		if a.Name == "grpc_only" {
			grpcOnly = a.Ident
		}
	}
	if grpcOnly != "True" {
		t.Errorf("grpc_only = %q (Ident), want True", grpcOnly)
	}
	if len(res.ConsumerDeps) != 1 || res.ConsumerDeps[0] != ":svc_cc_grpc" {
		t.Errorf("ConsumerDeps = %v, want [:svc_cc_grpc]", res.ConsumerDeps)
	}
}

// TestGrpcCppRecognizer_OutputCrossCheck: a combined command whose recorded
// outputs omit a grpc output is non-standard → Lower errors (so the dispatch
// refuses/falls back per fidelity).
func TestGrpcCppRecognizer_OutputCrossCheck(t *testing.T) {
	cmd := CodegenCommand{
		Driver: "protoc",
		Args:   []string{"--cpp_out=.", "--grpc_out=.", "svc.proto"},
		Srcs:   []string{"svc.proto"},
		Outs:   []string{"svc.pb.cc", "svc.pb.h"}, // missing the .grpc.pb.* pair
		Pkg:    "pkg",
	}
	_, matched, err := recognizeCodegen(cmd)
	if !matched {
		t.Fatal("expected the grpc recognizer to MATCH the combined command")
	}
	if err == nil {
		t.Error("expected a cross-check error for the missing grpc outputs")
	}
}

// TestGrpcOnlyRecognizer_Match: fires on a grpc-only protoc call (--grpc_out,
// NO --cpp_out) ONLY when a sibling cpp call exists (SiblingCppProto); declines
// without the sibling, on a combined call, and on non-protoc.
func TestGrpcOnlyRecognizer_Match(t *testing.T) {
	r := grpcOnlyRecognizer{}
	cases := []struct {
		name string
		cmd  CodegenCommand
		want bool
	}{
		{"grpc-only+sibling", CodegenCommand{Driver: "protoc", Args: []string{"--grpc_out=.", "svc.proto"}, SiblingCppProto: true}, true},
		{"grpc-only-no-sibling", CodegenCommand{Driver: "protoc", Args: []string{"--grpc_out=.", "svc.proto"}}, false},
		{"combined", CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=.", "--grpc_out=.", "svc.proto"}, SiblingCppProto: true}, false},
		{"cpp-only", CodegenCommand{Driver: "protoc", Args: []string{"--cpp_out=.", "svc.proto"}, SiblingCppProto: true}, false},
		{"not-protoc", CodegenCommand{Driver: "flatc", Args: []string{"--grpc_out=.", "x.fbs"}, SiblingCppProto: true}, false},
	}
	for _, c := range cases {
		if got := r.Match(c.cmd); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestGrpcOnlyRecognizer_Lower: emits ONLY cc_grpc_library referencing the
// sibling cpp call's :svc_proto/:svc_cc_proto (no re-emitted proto rules),
// grpc_only=True, consumer dep → the cc_grpc_library, derived = grpc outs only.
func TestGrpcOnlyRecognizer_Lower(t *testing.T) {
	cmd := CodegenCommand{
		Driver:          "protoc",
		Args:            []string{"--grpc_out=.", "svc.proto"},
		Srcs:            []string{"svc.proto"},
		Outs:            []string{"svc.grpc.pb.cc", "svc.grpc.pb.h"},
		Pkg:             "pkg",
		SiblingCppProto: true,
	}
	res, matched, err := recognizeCodegen(cmd)
	if !matched || err != nil {
		t.Fatalf("recognizeCodegen matched=%v err=%v", matched, err)
	}
	if len(res.Targets) != 1 {
		t.Fatalf("want exactly 1 target (cc_grpc_library, no re-emitted proto rules), got %d: %+v", len(res.Targets), res.Targets)
	}
	g := res.Targets[0]
	if g.Name != "svc_cc_grpc" || g.NativeRule.Kind != "cc_grpc_library" {
		t.Fatalf("target = %+v, want cc_grpc_library svc_cc_grpc", g)
	}
	if got := attrList(t, g, "srcs"); len(got) != 1 || got[0] != ":svc_proto" {
		t.Errorf("srcs = %v, want [:svc_proto] (the sibling proto_library)", got)
	}
	if got := attrList(t, g, "deps"); len(got) != 1 || got[0] != ":svc_cc_proto" {
		t.Errorf("deps = %v, want [:svc_cc_proto] (the sibling cc_proto_library)", got)
	}
	if len(res.ConsumerDeps) != 1 || res.ConsumerDeps[0] != ":svc_cc_grpc" {
		t.Errorf("ConsumerDeps = %v, want [:svc_cc_grpc]", res.ConsumerDeps)
	}
	if len(res.DerivedOutputs) != 2 || res.DerivedOutputs[0] != "svc.grpc.pb.cc" {
		t.Errorf("DerivedOutputs = %v, want the grpc pair only", res.DerivedOutputs)
	}
}

// TestGrpcOnlyRecognizer_NoSiblingFallsThrough: without a sibling cpp call the
// grpc-only recognizer declines, so no recognizer claims it (genrule fallback).
func TestGrpcOnlyRecognizer_NoSiblingFallsThrough(t *testing.T) {
	cmd := CodegenCommand{
		Driver: "protoc", Args: []string{"--grpc_out=.", "svc.proto"},
		Srcs: []string{"svc.proto"}, Outs: []string{"svc.grpc.pb.cc", "svc.grpc.pb.h"},
	}
	if _, matched, _ := recognizeCodegen(cmd); matched {
		t.Error("a grpc-only call with no sibling cpp proto must NOT be recognized (stays a genrule)")
	}
}
