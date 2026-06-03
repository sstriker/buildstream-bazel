package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmitSplit_GenexIncludeNoCrash is the split-mode backstop for the glog
// finding: a target carrying an unresolved-genex include dir
// (`$<TARGET_PROPERTY:…>`, the glog_test INTERFACE-library shape) must not
// abort the split emit by synthesizing a header lib named "$<…>_headers"
// (an invalid Bazel identifier). planSplit skips the genex include root, so
// EmitSplit succeeds and no genex leaks into the emitted BUILDs. (ToIR's
// dropGenexIncludeDirs strips these upstream; this guards the emit layer
// directly so a future ordering change can't reintroduce the abort.)
func TestEmitSplit_GenexIncludeNoCrash(t *testing.T) {
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{
			{
				Name:     "lib",
				Kind:     ir.KindCCLibrary,
				Srcs:     []string{"lib.cpp"},
				Hdrs:     []string{"foo.h"},
				Includes: []string{"$<TARGET_PROPERTY:other,INCLUDE_DIRECTORIES>"},
			},
		},
	}

	tree, err := bazel.EmitSplit(pkg, bazel.Options{BazelPackagePath: "elements/p"})
	if err != nil {
		t.Fatalf("EmitSplit aborted on an unresolved-genex include dir: %v", err)
	}
	for dir, b := range tree {
		if strings.Contains(string(b), "$<") {
			t.Errorf("emitted BUILD for %q leaked an unresolved genex:\n%s", dir, b)
		}
	}
}
