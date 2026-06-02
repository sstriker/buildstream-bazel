package bazel

import (
	"strings"
	"testing"

	"github.com/bazelbuild/buildtools/build"
)

// markGeneratedHeaderDeps must mark the wrapper dep even when `deps` renders
// as `[flat] + select({...})` (a *build.BinaryExpr) — the shape Bazel uses
// when a consumer also carries per-platform deps. The wrapper label lives in
// the flat-list operand; the old plain-ListExpr-only path missed it and
// gazelle could then strip the unmarked edge.
func TestMarkGeneratedHeaderDeps_BinaryExprDeps(t *testing.T) {
	flat := &build.ListExpr{List: []build.Expr{
		&build.StringExpr{Value: "//inc:" + generatedIncludesName},
	}}
	sel := &build.CallExpr{X: &build.Ident{Name: "select"}, List: []build.Expr{
		&build.DictExpr{List: []*build.KeyValueExpr{{
			Key:   &build.StringExpr{Value: "//conditions:default"},
			Value: &build.ListExpr{List: []build.Expr{&build.StringExpr{Value: "//y:z"}}},
		}}},
	}}
	call := &build.CallExpr{X: &build.Ident{Name: "cc_library"}, List: []build.Expr{
		&build.AssignExpr{LHS: &build.Ident{Name: "deps"}, Op: "=", RHS: &build.BinaryExpr{Op: "+", X: flat, Y: sel}},
	}}
	markGeneratedHeaderDeps(call)

	if !hasKeepSuffix(flat.List[0].(*build.StringExpr).Comment().Suffix) {
		t.Error("wrapper dep inside `[flat] + select(...)` did not receive # keep")
	}
	// A non-wrapper dep (in the select arm) must NOT be marked.
	other := sel.List[0].(*build.DictExpr).List[0].Value.(*build.ListExpr).List[0].(*build.StringExpr)
	if hasKeepSuffix(other.Comment().Suffix) {
		t.Error("a non-wrapper dep was incorrectly # keep'd")
	}
}

// The reserved wrapper name must sit outside headerLibName's `<dir>_headers`
// output space, so it can never collide with a synthesized include-root
// header library — a rename back into that space would reintroduce the
// duplicate-target bug.
func TestGeneratedIncludesName_OutsideHeaderLibScheme(t *testing.T) {
	if strings.HasSuffix(generatedIncludesName, "_headers") {
		t.Errorf("generatedIncludesName %q ends in _headers; collides with headerLibName output space", generatedIncludesName)
	}
	if headerLibName("generated") == generatedIncludesName {
		t.Errorf("generatedIncludesName collides with headerLibName(%q)", "generated")
	}
}
