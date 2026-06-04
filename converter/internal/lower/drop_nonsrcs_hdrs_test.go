package lower

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestDropNonSrcsHeadersFromCcExecutables: cc_binary / cc_test have no
// hdrs/textual_hdrs slot, so the emitter folds their Hdrs into srcs — and a
// file whose extension Bazel rejects in srcs then fails the rule's
// srcs-extension check at analysis: a header ext srcs rejects (.def/.gen,
// "misplaced here") or a non-code artifact (.pc pkg-config, an
// extension-less config script, "does not produce any srcs files"). The
// pass drops exactly those from executable targets (srcs-valid files like
// .h/.inc stay, order preserved), leaves cc_library untouched (hdrs accepts
// the wider set), and breadcrumbs the drop. Reproduces libxml2's
// testrecurse/testlimits blockers (codegen/ranges.def, libxml-2.0.pc).
func TestDropNonSrcsHeadersFromCcExecutables(t *testing.T) {
	mkHdrs := func() []string {
		return []string{
			"codegen/ranges.def", "foo.gen", "libxml-2.0.pc", "xml2-config",
			"config.h", "codegen/charset.inc",
		}
	}
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "atest", Kind: ir.KindCCTest, Srcs: []string{"t.c"}, Hdrs: mkHdrs()},
		{Name: "abin", Kind: ir.KindCCBinary, Srcs: []string{"m.c"}, Hdrs: mkHdrs()},
		{Name: "alib", Kind: ir.KindCCLibrary, Srcs: []string{"l.c"}, Hdrs: mkHdrs()},
	}}
	var warn bytes.Buffer
	dropNonSrcsHeadersFromCcExecutables(pkg, &warn)

	// .def/.gen/.pc and the extension-less script dropped from the
	// executables; .h/.inc kept in original order.
	wantKept := []string{"config.h", "codegen/charset.inc"}
	for _, name := range []string{"atest", "abin"} {
		if got := findTarget(pkg, name).Hdrs; !reflect.DeepEqual(got, wantKept) {
			t.Errorf("%s Hdrs = %v, want %v", name, got, wantKept)
		}
	}
	// cc_library is untouched — `hdrs` accepts the wider set (e.g. .def).
	if got := findTarget(pkg, "alib").Hdrs; !reflect.DeepEqual(got, mkHdrs()) {
		t.Errorf("alib Hdrs = %v, want unchanged %v", got, mkHdrs())
	}
	// Breadcrumb names the count, the executable targets, and the dropped files.
	out := warn.String()
	for _, sub := range []string{
		"dropped 8 non-srcs file(s) from 2 cc_binary/cc_test target(s)",
		"atest", "abin", "ranges.def", "foo.gen", "libxml-2.0.pc", "xml2-config",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("breadcrumb missing %q\n%s", sub, out)
		}
	}
}

// TestDropNonSrcsHeadersFromCcExecutables_NoWarnNoDropNoOp: a clean
// executable (only srcs-valid headers) is untouched and emits no breadcrumb;
// nil writer is safe.
func TestDropNonSrcsHeadersFromCcExecutables_NoWarnNoDropNoOp(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "clean", Kind: ir.KindCCTest, Srcs: []string{"t.c"}, Hdrs: []string{"a.h", "b.inc"}},
	}}
	dropNonSrcsHeadersFromCcExecutables(pkg, nil) // nil writer must not panic
	if got, want := findTarget(pkg, "clean").Hdrs, []string{"a.h", "b.inc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clean Hdrs = %v, want unchanged %v", got, want)
	}
}
