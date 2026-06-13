package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

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
