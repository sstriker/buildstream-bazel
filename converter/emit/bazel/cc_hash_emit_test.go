package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// A KindCCHash target renders the cc_hash rule + its load line.
func TestEmitCCHash(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:       "gen_vtkSocketCommunicatorHash_h",
			Kind:       ir.KindCCHash,
			Tags:       []string{"cmake-codegen-cc-hash"},
			Visibility: []string{"//visibility:private"},
			CCHash: &ir.CCHashSpec{
				Src:       "vtkSocketCommunicator.cxx",
				Name:      "vtkSocketCommunicatorHash",
				Algorithm: "SHA256",
				OutHeader: "vtkSocketCommunicatorHash.h",
			},
		}},
	}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	s := string(out)
	t.Logf("rendered:\n%s", s)

	for _, want := range []string{
		`load("@rules_buildstream_bazel//rules:cc_hash.bzl", "cc_hash")`,
		`cc_hash(`,
		`name = "gen_vtkSocketCommunicatorHash_h"`,
		`src = "vtkSocketCommunicator.cxx"`,
		`define_name = "vtkSocketCommunicatorHash"`,
		`algorithm = "SHA256"`,
		`out_header = "vtkSocketCommunicatorHash.h"`,
		`tool = "//tools:cc-hash"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}
