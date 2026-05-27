package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_HeaderCommentsCollapseNewlines pins the LLVM-surfaced
// behaviour: a HeaderComments line carrying embedded newlines (as
// LLVM's `option(LLVM_BUILD_BENCHMARKS "Add LLVM ...\ntargets. If
// OFF ...")` HELPSTRING shape produces) must collapse to a single
// `# <text>\n` line. Before the fix the second line landed without
// the `# ` prefix, breaking the BUILD with a bare-syntax token.
func TestEmit_HeaderCommentsCollapseNewlines(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		HeaderComments: []string{
			"plain line",
			"  - LLVM_BUILD_BENCHMARKS = OFF (Add LLVM benchmark targets to the list of default\ntargets. If OFF, benchmarks still could be built using Benchmarks target.)",
			"line with \r\n CRLF",
		},
		Targets: []ir.Target{{Name: "x", Kind: ir.KindCCLibrary, Srcs: []string{"x.c"}}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(out)
	// Every header-comment line must start with "# " — no bare
	// continuation lines.
	for i, line := range strings.Split(body, "\n") {
		if line == "" {
			break // hit blank separator before rule emission
		}
		if strings.HasPrefix(line, "load(") || strings.HasPrefix(line, "package(") {
			break
		}
		if !strings.HasPrefix(line, "#") {
			t.Errorf("line %d emitted without `#` prefix (bare continuation?): %q\nfull body:\n%s",
				i+1, line, body)
		}
	}
	// The collapsed form keeps the option name + value visible on
	// a single line.
	want := "# - LLVM_BUILD_BENCHMARKS = OFF (Add LLVM benchmark targets to the list of default targets. If OFF, benchmarks still could be built using Benchmarks target.)"
	if !strings.Contains(body, want) {
		t.Errorf("expected collapsed line %q; body:\n%s", want, body)
	}
}
