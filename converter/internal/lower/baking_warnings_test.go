package lower

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
)

func TestApplyBakeInPolicy_WarnEmitsPerTaggedRule(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "configured_h", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-lifted"}},
		{Name: "stamp_v", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-execute-process", "cmake-codegen-execute-process-hoisted"}},
		{Name: "regular_lib", Kind: ir.KindCCLibrary},
		{Name: "hash_h", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-cmake-script-lift"}},
	}}
	var buf bytes.Buffer
	if err := applyBakeInPolicy(pkg, &buf, convmode.BakeInWarn); err != nil {
		t.Fatalf("warn policy should not error; got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "convert-time-baked output(s)") {
		t.Errorf("missing header; got:\n%s", out)
	}
	for _, want := range []string{"configured_h", "stamp_v", "hash_h"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing rule %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "regular_lib") {
		t.Errorf("regular_lib has no bake tag but appears in warning:\n%s", out)
	}
}

func TestApplyBakeInPolicy_NilSinkSilent(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "x", Tags: []string{"cmake-codegen-lifted"}},
	}}
	if err := applyBakeInPolicy(pkg, nil, convmode.BakeInWarn); err != nil {
		t.Fatalf("nil sink + warn should not error; got %v", err)
	}
}

func TestApplyBakeInPolicy_NoTaggedRulesNoOutput(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib"}}}
	var buf bytes.Buffer
	if err := applyBakeInPolicy(pkg, &buf, convmode.BakeInWarn); err != nil {
		t.Fatalf("warn policy should not error on empty bake set; got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for pkg with no bake tags; got %q", buf.String())
	}
}

func TestApplyBakeInPolicy_DedupesSameRuleReason(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "x", Tags: []string{"cmake-codegen-lifted", "cmake-codegen-lifted"}},
	}}
	var buf bytes.Buffer
	if err := applyBakeInPolicy(pkg, &buf, convmode.BakeInWarn); err != nil {
		t.Fatalf("warn policy should not error; got %v", err)
	}
	if got := strings.Count(buf.String(), "- x:"); got != 1 {
		t.Errorf("entries for x: got %d, want 1; output:\n%s", got, buf.String())
	}
}

func TestApplyBakeInPolicy_AllowSuppresses(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "configured_h", Tags: []string{"cmake-codegen-lifted"}},
	}}
	var buf bytes.Buffer
	if err := applyBakeInPolicy(pkg, &buf, convmode.BakeInAllow); err != nil {
		t.Fatalf("allow should not error; got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("allow should emit no inventory; got %q", buf.String())
	}
}

func TestApplyBakeInPolicy_RejectReturnsErrorAndStillEmitsInventory(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "configured_h", Tags: []string{"cmake-codegen-lifted"}},
		{Name: "stamp_v", Tags: []string{"cmake-codegen-execute-process"}},
	}}
	var buf bytes.Buffer
	err := applyBakeInPolicy(pkg, &buf, convmode.BakeInReject)
	if err == nil {
		t.Fatalf("reject should error when bake tags are present")
	}
	if !strings.Contains(err.Error(), "--bake-in=reject") {
		t.Errorf("error should name the policy; got %v", err)
	}
	if !strings.Contains(err.Error(), "configured_h") {
		t.Errorf("error should embed the per-rule inventory; got %v", err)
	}
	if !strings.Contains(buf.String(), "configured_h") {
		t.Errorf("reject should also write the inventory to sink; got %q", buf.String())
	}
}

func TestApplyBakeInPolicy_RejectNoBakeIsNoOp(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib"}}}
	var buf bytes.Buffer
	if err := applyBakeInPolicy(pkg, &buf, convmode.BakeInReject); err != nil {
		t.Fatalf("reject on empty bake set should not error; got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for pkg with no bake tags; got %q", buf.String())
	}
}

func TestApplyBakeInPolicy_EmptyPolicyResolvesToWarn(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "x", Tags: []string{"cmake-codegen-lifted"}},
	}}
	var buf bytes.Buffer
	if err := applyBakeInPolicy(pkg, &buf, ""); err != nil {
		t.Fatalf("empty policy should not error; got %v", err)
	}
	if !strings.Contains(buf.String(), "x:") {
		t.Errorf("empty policy should emit warn-style inventory; got %q", buf.String())
	}
}
