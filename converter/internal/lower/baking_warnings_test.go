package lower

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestWarnConvertTimeBaking_EmitsPerTaggedRule(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "configured_h", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-lifted"}},
		{Name: "stamp_v", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-execute-process", "cmake-codegen-execute-process-hoisted"}},
		{Name: "regular_lib", Kind: ir.KindCCLibrary},
		{Name: "hash_h", Kind: ir.KindGenrule, Tags: []string{"cmake-codegen-cmake-script-lift"}},
	}}
	var buf bytes.Buffer
	warnConvertTimeBaking(pkg, &buf)
	out := buf.String()
	// Header with count: three of the four rules carry baked
	// tags, but stamp_v carries TWO tags ⇒ two entries, so 4
	// distinct (name, reason) pairs.
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

func TestWarnConvertTimeBaking_NilSinkSilent(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "x", Tags: []string{"cmake-codegen-lifted"}},
	}}
	// Must not panic when sink is nil.
	warnConvertTimeBaking(pkg, nil)
}

func TestWarnConvertTimeBaking_NoTaggedRulesNoOutput(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{Name: "lib"}}}
	var buf bytes.Buffer
	warnConvertTimeBaking(pkg, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for pkg with no bake tags; got %q", buf.String())
	}
}

func TestWarnConvertTimeBaking_DedupesSameRuleReason(t *testing.T) {
	// A rule carrying the same bake tag twice should still
	// produce a single entry (shouldn't happen in practice but
	// defensive).
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "x", Tags: []string{"cmake-codegen-lifted", "cmake-codegen-lifted"}},
	}}
	var buf bytes.Buffer
	warnConvertTimeBaking(pkg, &buf)
	if got := strings.Count(buf.String(), "- x:"); got != 1 {
		t.Errorf("entries for x: got %d, want 1; output:\n%s", got, buf.String())
	}
}
