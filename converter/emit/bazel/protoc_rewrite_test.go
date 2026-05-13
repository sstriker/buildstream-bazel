package bazel

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bazelbuild/buildtools/build"
	"github.com/sstriker/cmake-to-bazel/converter/ir"
)

// TestProtocFlow_RegressionGuard exercises the documented
// protobuf example from
// `docs/design/operator-gazelle-step.md` end-to-end:
//
//  1. A `protoc`-style add_custom_command in CMake lowers to
//     an `ir.Target` with `Kind = KindGenrule`, a `cmd`
//     containing `protoc`, and `.pb.cc` / `.pb.h` outputs.
//  2. `bazel.Emit` renders the genrule with a Phase-7a
//     `# keep` marker (whole-rule suffix on the closing
//     paren).
//  3. The operator's flow: remove the `# keep` marker, run a
//     gazelle-style rewriter that pattern-matches on `protoc`
//     in the cmd and emits canonical
//     `proto_library` + `cc_proto_library` rules.
//
// This test exercises (1)-(3) entirely in-process. The
// rewriter step uses a small reference helper
// (rewriteProtocGenrule) built on the same buildtools-AST
// library a real gazelle extension would use — so the test
// covers both the converter's emission shape AND the operator
// rewrite-pattern remaining feasible against our output.
//
// If the test fails, either:
//   - Phase 7a's `# keep` injection broke (the marker is
//     missing or attached to the wrong node), OR
//   - The genrule emission shape changed in a way that
//     breaks the documented protoc pattern (cmd, srcs, outs
//     attribute names/forms), OR
//   - The buildtools-AST rewriter API drifted.
//
// All three are real regression risks the doc's protoc
// example depends on; this test pins the contract.
func TestProtocFlow_RegressionGuard(t *testing.T) {
	pkg := &ir.Package{
		Name: "myelem",
		Targets: []ir.Target{{
			Name: "myelem_proto_gen",
			Kind: ir.KindGenrule,
			// Mirror the cmd shape a real CMake
			// `protobuf_generate_cpp()` produces: a protoc
			// invocation that consumes the `.proto` source and
			// emits `.pb.cc` + `.pb.h` siblings.
			Srcs:        []string{"myelem.proto"},
			GenruleOuts: []string{"myelem.pb.cc", "myelem.pb.h"},
			GenruleCmd:  "$(location @protobuf//:protoc) --cpp_out=$(@D) $(location myelem.proto)",
		}},
	}
	emitted, err := Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// ─── Stage 1: converter emission shape ───
	// Phase 7a's contract: every genrule gets a trailing
	// `# keep` marker on the closing-paren line.
	if !strings.Contains(string(emitted), ")  # keep") {
		t.Errorf("emitted BUILD missing genrule # keep marker:\n%s", emitted)
	}
	// The protoc cmd must be preserved verbatim — operator
	// rewriters pattern-match on substrings of it.
	if !strings.Contains(string(emitted), "protoc") {
		t.Errorf("emitted BUILD missing `protoc` in genrule cmd:\n%s", emitted)
	}
	// The .pb.cc / .pb.h outputs are the gazelle-side
	// signal that selects proto_library + cc_proto_library
	// rewriting over generic rules.
	if !strings.Contains(string(emitted), `"myelem.pb.cc"`) ||
		!strings.Contains(string(emitted), `"myelem.pb.h"`) {
		t.Errorf("emitted BUILD missing .pb.cc/.pb.h outs:\n%s", emitted)
	}

	// ─── Stage 2: operator removes # keep ───
	// Real operator workflow is a manual edit; here we
	// simulate it by parsing + dropping the suffix comment.
	f, err := build.Parse("BUILD.bazel", emitted)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		if ident, ok := call.X.(*build.Ident); !ok || ident.Name != "genrule" {
			continue
		}
		call.Comment().Suffix = nil
	}

	// ─── Stage 3: operator-side rewrite ───
	// rewriteProtocGenrule is the reference rewriter — a
	// 30-line buildtools-AST walk that mirrors what a real
	// gazelle extension would do for the protoc pattern.
	rewritten := rewriteProtocGenrule(t, f)
	body := build.Format(rewritten)

	// Assert the rewritten file has the expected canonical
	// shape: proto_library + cc_proto_library, NO genrule.
	if strings.Contains(string(body), "genrule(") {
		t.Errorf("rewritten BUILD still has a genrule:\n%s", body)
	}
	for _, want := range []string{
		`proto_library(`,
		`name = "myelem_proto"`,
		`srcs = ["myelem.proto"]`,
		`cc_proto_library(`,
		`name = "myelem_cc_proto"`,
		`deps = [":myelem_proto"]`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("rewritten BUILD missing %q\n%s", want, body)
		}
	}
}

// rewriteProtocGenrule implements the operator-side rewrite
// the operator-gazelle-step doc describes: walk f, find every
// genrule WITHOUT a `# keep` marker whose `cmd` attribute
// references `protoc`, and replace it with a
// `proto_library(name="<base>_proto", srcs=[<.proto>])` +
// `cc_proto_library(name="<base>_cc_proto", deps=[":<base>_proto"])`
// pair.
//
// Kept inline in the test file (not promoted to its own
// package) because this is the REFERENCE pattern for
// operators, not production code we ship. Operators with
// gazelle_proto / a custom plugin will use those instead;
// this helper exists to prove the rewrite is feasible
// against our emission shape.
func rewriteProtocGenrule(t *testing.T, f *build.File) *build.File {
	t.Helper()
	var newStmts []build.Expr
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			newStmts = append(newStmts, stmt)
			continue
		}
		ident, ok := call.X.(*build.Ident)
		if !ok || ident.Name != "genrule" {
			newStmts = append(newStmts, stmt)
			continue
		}
		// Don't touch genrules the operator left # keep'd.
		hasKeep := false
		for _, c := range call.Comment().Suffix {
			if strings.TrimSpace(c.Token) == "# keep" {
				hasKeep = true
				break
			}
		}
		if hasKeep {
			newStmts = append(newStmts, stmt)
			continue
		}
		name := stringArg(call, "name")
		cmd := stringArg(call, "cmd")
		srcs := stringListArg(call, "srcs")
		if name == "" || cmd == "" || !strings.Contains(cmd, "protoc") {
			newStmts = append(newStmts, stmt)
			continue
		}
		// Strip a single `_proto_gen` / `_gen` suffix so
		// the rewritten label reads naturally; otherwise
		// reuse the genrule name as the proto_library base.
		base := strings.TrimSuffix(strings.TrimSuffix(name, "_proto_gen"), "_gen")
		newStmts = append(newStmts,
			mkRuleCall("proto_library", []ruleAttr{
				{key: "name", value: fmt.Sprintf("%s_proto", base)},
				{key: "srcs", listValue: srcs},
			}),
			mkRuleCall("cc_proto_library", []ruleAttr{
				{key: "name", value: fmt.Sprintf("%s_cc_proto", base)},
				{key: "deps", listValue: []string{fmt.Sprintf(":%s_proto", base)}},
			}),
		)
	}
	f.Stmt = newStmts
	return f
}

// stringArg / stringListArg / mkRuleCall / ruleAttr —
// minimal buildtools-AST helpers for the rewriter. Mirrors
// the cmd/build-cc-index/main.go style of construction so
// operators can copy-paste from either reference.

type ruleAttr struct {
	key       string
	value     string   // scalar string-RHS attrs
	listValue []string // list-of-strings RHS attrs
}

func mkRuleCall(kind string, attrs []ruleAttr) *build.CallExpr {
	args := make([]build.Expr, 0, len(attrs))
	for _, a := range attrs {
		var rhs build.Expr
		if a.listValue != nil {
			items := make([]build.Expr, 0, len(a.listValue))
			for _, s := range a.listValue {
				items = append(items, &build.StringExpr{Value: s})
			}
			rhs = &build.ListExpr{List: items}
		} else {
			rhs = &build.StringExpr{Value: a.value}
		}
		args = append(args, &build.AssignExpr{
			LHS: &build.Ident{Name: a.key},
			Op:  "=",
			RHS: rhs,
		})
	}
	return &build.CallExpr{
		X:    &build.Ident{Name: kind},
		List: args,
	}
}

func stringArg(call *build.CallExpr, attr string) string {
	for _, a := range call.List {
		assign, ok := a.(*build.AssignExpr)
		if !ok {
			continue
		}
		ident, ok := assign.LHS.(*build.Ident)
		if !ok || ident.Name != attr {
			continue
		}
		if s, ok := assign.RHS.(*build.StringExpr); ok {
			return s.Value
		}
	}
	return ""
}

func stringListArg(call *build.CallExpr, attr string) []string {
	for _, a := range call.List {
		assign, ok := a.(*build.AssignExpr)
		if !ok {
			continue
		}
		ident, ok := assign.LHS.(*build.Ident)
		if !ok || ident.Name != attr {
			continue
		}
		list, ok := assign.RHS.(*build.ListExpr)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(list.List))
		for _, item := range list.List {
			if s, ok := item.(*build.StringExpr); ok {
				out = append(out, s.Value)
			}
		}
		return out
	}
	return nil
}

// TestProtocFlow_KeepMarkerProtectsGenrule covers the
// other side of the contract: a genrule WITH a `# keep`
// marker (the default emission state) survives an attempted
// rewrite untouched. This is the "literal-CMake fidelity
// wins by default" guarantee Phase 7a's keep markers buy us.
func TestProtocFlow_KeepMarkerProtectsGenrule(t *testing.T) {
	pkg := &ir.Package{
		Name: "myelem",
		Targets: []ir.Target{{
			Name:        "myelem_proto_gen",
			Kind:        ir.KindGenrule,
			Srcs:        []string{"myelem.proto"},
			GenruleOuts: []string{"myelem.pb.cc", "myelem.pb.h"},
			GenruleCmd:  "protoc --cpp_out=$(@D) $(location myelem.proto)",
		}},
	}
	emitted, _ := Emit(pkg)
	f, err := build.Parse("BUILD.bazel", emitted)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Don't strip # keep markers — simulating the default
	// operator state where no genrule has been explicitly
	// opted into rewriting.
	rewritten := rewriteProtocGenrule(t, f)
	body := build.Format(rewritten)
	// The genrule must still be there.
	if !strings.Contains(string(body), "genrule(") {
		t.Errorf("rewriter clobbered a # keep'd genrule:\n%s", body)
	}
	// The proto_library must NOT be there.
	if strings.Contains(string(body), "proto_library(") {
		t.Errorf("rewriter emitted proto_library against operator's # keep:\n%s", body)
	}
}
