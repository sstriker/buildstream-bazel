package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestClassifyCMakeETar pins the mode-aware bucketing: create lifts
// (BucketCMakeE/tar), extract refuses (→ unspecified-outputs rescue),
// list benign-skips (tar_list noop).
func TestClassifyCMakeETar(t *testing.T) {
	cases := []struct {
		mode   string
		bucket Bucket
		op     string
	}{
		{"cf", BucketCMakeE, "tar"},
		{"czf", BucketCMakeE, "tar"},
		{"cjf", BucketCMakeE, "tar"},
		{"xf", BucketRefuse, ""},
		{"xzf", BucketRefuse, ""},
		{"tf", BucketCMakeE, "tar_list"},
	}
	for _, tc := range cases {
		v := Classify(shadow.ExecuteProcessCall{
			Commands: [][]string{{"cmake", "-E", "tar", tc.mode, "/build/a.tar", "/src/x"}},
		})
		if v.Bucket != tc.bucket || (tc.op != "" && v.CMakeEOp != tc.op) {
			t.Errorf("tar %s: bucket=%v op=%q; want %v/%q (%s)", tc.mode, v.Bucket, v.CMakeEOp, tc.bucket, tc.op, v.Reason)
		}
	}
}

// TestLiftCMakeETarCreate pins the pkg_tar emission: source inputs
// become labels, a produced input wires to its producer, the archive
// registers in OutToGenrule, and the compression flag maps to
// `extension`. An unresolvable input refuses (no silent member drop).
func TestLiftCMakeETarCreate(t *testing.T) {
	run := func(argv ...string) (*codegenContext, []executeProcessRefusal) {
		cc := newCodegenContext()
		cc.OutToGenrule["gen/made.txt"] = "exec_made" // a recovered producer
		_, refusals := recoverExecuteProcess(
			[]shadow.ExecuteProcessCall{{File: "/src/CMakeLists.txt", Line: 3, Commands: [][]string{argv}}},
			"/src", "/src", "", "/build", false, nil, nil, cc)
		return cc, refusals
	}

	t.Run("create-gzip", func(t *testing.T) {
		cc, refusals := run("cmake", "-E", "tar", "czf", "/build/bundle.tar.gz", "/src/a.txt", "/build/gen/made.txt")
		if len(refusals) != 0 {
			t.Fatalf("refusals: %+v", refusals)
		}
		g := cc.Genrules[0]
		if g.Kind != ir.KindNativeRule || g.NativeRule == nil || g.NativeRule.Kind != "pkg_tar" {
			t.Fatalf("expected pkg_tar native rule, got %+v", g)
		}
		attr := map[string]ir.NativeAttr{}
		for _, a := range g.NativeRule.Attrs {
			attr[a.Name] = a
		}
		if attr["out"].Str != "bundle.tar.gz" {
			t.Errorf("out = %q; want bundle.tar.gz", attr["out"].Str)
		}
		if attr["extension"].Str != "tar.gz" {
			t.Errorf("extension = %q; want tar.gz", attr["extension"].Str)
		}
		if strings.Join(attr["srcs"].List, ",") != ":exec_made,a.txt" {
			t.Errorf("srcs = %v; want [:exec_made a.txt] (source label + produced :label)", attr["srcs"].List)
		}
		if cc.OutToGenrule["bundle.tar.gz"] != g.Name {
			t.Errorf("archive not registered in OutToGenrule: %v", cc.OutToGenrule)
		}
		if g.NativeRule.LoadFrom != "@rules_pkg//pkg:tar.bzl" {
			t.Errorf("LoadFrom = %q", g.NativeRule.LoadFrom)
		}
	})

	t.Run("unresolvable-input-refuses", func(t *testing.T) {
		_, refusals := run("cmake", "-E", "tar", "cf", "/build/x.tar", "/build/unrecovered.txt")
		if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "neither a source-tree file nor a recovered build output") {
			t.Fatalf("an unresolvable member must refuse the lift: %+v", refusals)
		}
	})

	t.Run("uncompressed", func(t *testing.T) {
		cc, _ := run("cmake", "-E", "tar", "cf", "/build/plain.tar", "/src/a.txt")
		for _, a := range cc.Genrules[0].NativeRule.Attrs {
			if a.Name == "extension" {
				t.Errorf("uncompressed tar must omit extension (defaults to tar); got %q", a.Str)
			}
		}
	})
}

// TestNativeRuleOuts pins the kind-agnostic producer-output accessor the
// nested re-home now uses (out scalar + outs list).
func TestNativeRuleOuts(t *testing.T) {
	spec := &ir.NativeRuleSpec{Attrs: []ir.NativeAttr{
		{Name: "out", Str: "a.tar"},
		{Name: "outs", List: []string{"x.pb.h", "x.pb.cc"}},
		{Name: "srcs", List: []string{"in"}},
	}}
	got := nativeRuleOuts(spec)
	if strings.Join(got, ",") != "a.tar,x.pb.h,x.pb.cc" {
		t.Errorf("nativeRuleOuts = %v; want [a.tar x.pb.h x.pb.cc]", got)
	}
	if nativeRuleOuts(nil) != nil {
		t.Error("nil spec must yield nil")
	}
}
