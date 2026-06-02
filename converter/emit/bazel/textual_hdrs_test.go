package bazel

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// A cc_library with TextualHdrs renders the textual_hdrs attribute (the
// idiom for generated tablegen .inc fragments — included but not compiled
// as modular headers).
func TestTextualHdrsRender(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:        "generated_headers",
		Kind:        ir.KindCCLibrary,
		TextualHdrs: []string{"llvm/IR/Attributes.inc", "llvm/IR/IntrinsicEnums.inc"},
		Includes:    []string{"."},
	}}}
	out, err := Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `textual_hdrs = [`) ||
		!strings.Contains(got, `"llvm/IR/Attributes.inc"`) ||
		!strings.Contains(got, `"llvm/IR/IntrinsicEnums.inc"`) {
		t.Errorf("expected textual_hdrs list, got:\n%s", got)
	}
}
