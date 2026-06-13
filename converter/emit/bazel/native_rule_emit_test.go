package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmitNativeRule renders the generic native-rule substrate the codegen
// recognizer registry builds on: a proto_library + cc_proto_library pair (the
// shape the protoc-cpp recognizer will emit). Asserts the data-driven loads are
// emitted (one per distinct .bzl, symbols grouped) and each rule renders with
// its attrs.
func TestEmitNativeRule(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			{
				Name: "foo_proto",
				Kind: ir.KindNativeRule,
				NativeRule: &ir.NativeRuleSpec{
					Kind:     "proto_library",
					LoadFrom: "@protobuf//bazel:proto_library.bzl",
					Attrs: []ir.NativeAttr{
						{Name: "srcs", List: []string{"foo.proto"}},
						{Name: "visibility", List: []string{"//visibility:public"}},
					},
				},
			},
			{
				Name: "foo_cc_proto",
				Kind: ir.KindNativeRule,
				NativeRule: &ir.NativeRuleSpec{
					Kind:     "cc_proto_library",
					LoadFrom: "@protobuf//bazel:cc_proto_library.bzl",
					Attrs: []ir.NativeAttr{
						{Name: "deps", List: []string{":foo_proto"}},
						{Name: "visibility", List: []string{"//visibility:public"}},
					},
				},
			},
		},
	}
	out, err := bazel.EmitWithOptions(pkg, bazel.Options{BazelPackagePath: "pkg"})
	if err != nil {
		t.Fatalf("EmitWithOptions: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		`load("@protobuf//bazel:cc_proto_library.bzl", "cc_proto_library")`,
		`load("@protobuf//bazel:proto_library.bzl", "proto_library")`,
		`proto_library(`,
		`name = "foo_proto"`,
		`srcs = ["foo.proto"]`,
		`cc_proto_library(`,
		`name = "foo_cc_proto"`,
		`deps = [":foo_proto"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted BUILD missing %q:\n%s", want, got)
		}
	}
}

// TestEmitNativeRule_BuiltinNoLoad: a NativeRule with no LoadFrom (a built-in
// rule) emits the rule but no load line for it.
func TestEmitNativeRule_BuiltinNoLoad(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			{
				Name: "g",
				Kind: ir.KindNativeRule,
				NativeRule: &ir.NativeRuleSpec{
					Kind:  "some_builtin_rule",
					Attrs: []ir.NativeAttr{{Name: "out", Str: "x.txt"}},
				},
			},
		},
	}
	out, err := bazel.EmitWithOptions(pkg, bazel.Options{BazelPackagePath: "pkg"})
	if err != nil {
		t.Fatalf("EmitWithOptions: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `some_builtin_rule(`) || !strings.Contains(got, `out = "x.txt"`) {
		t.Errorf("built-in native rule not rendered:\n%s", got)
	}
	if strings.Contains(got, `load(`) && strings.Contains(got, "some_builtin_rule") &&
		strings.Contains(got[:strings.Index(got, "some_builtin_rule(")], `load("`) {
		// crude: there should be no load whose symbol is some_builtin_rule
		if strings.Contains(got, `"some_builtin_rule")`) {
			t.Errorf("built-in rule should not get a load line:\n%s", got)
		}
	}
}
