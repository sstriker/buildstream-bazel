package bazel

import (
	"bytes"
	"strings"

	"github.com/bazelbuild/buildtools/build"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// AST-direct rule emitters. The text->AST migration (ROADMAP "AST-direct BUILD
// emit") converts the per-kind emit* functions from text/template to direct
// buildtools-AST construction, so the assembled File skips the build.Parse the
// text path pays. Each converted kind has a sibling byte-identity test
// (ast_emit_test.go) asserting its AST output Formats identically to the
// template's text -> Parse -> Format output; the render gates pin the same
// invariant end-to-end. Attribute ORDER is left to build.Format's rewriter
// (tables.NamePriority) exactly as the template path relied on, so SetAttr
// order here is irrelevant.

// astTargetCall returns the AST CallExpr for a kind that has been migrated off
// the text template, or (nil, false) for kinds still on the text path. The
// leading/provenance comments are attached by the caller (targetStmts).
func astTargetCall(t ir.Target) (*build.CallExpr, bool) {
	switch t.Kind {
	case ir.KindAlias:
		return aliasExpr(t), true
	case ir.KindBoolFlag:
		return boolFlagExpr(t), true
	}
	return nil, false
}

// newCall starts a rule CallExpr of the given kind and its Rule helper.
func newCall(kind string) (*build.CallExpr, *build.Rule) {
	call := &build.CallExpr{X: &build.Ident{Name: kind}}
	return call, build.NewRule(call)
}

func strExpr(s string) build.Expr { return &build.StringExpr{Value: s} }

func strListExpr(vs []string) build.Expr {
	items := make([]build.Expr, len(vs))
	for i, v := range vs {
		items[i] = strExpr(v)
	}
	return &build.ListExpr{List: items}
}

// boolIdent renders a Starlark True/False literal (a bare Ident, matching how
// buildtools parses the template's `{{.Default}}` token).
func boolIdent(b bool) build.Expr {
	if b {
		return &build.Ident{Name: "True"}
	}
	return &build.Ident{Name: "False"}
}

// setListIfNonEmpty SetAttrs a list attribute only when non-empty, mirroring
// the templates' `{{- if .X}}` omission of empty list attributes.
func setListIfNonEmpty(r *build.Rule, key string, vs []string) {
	if len(vs) > 0 {
		r.SetAttr(key, strListExpr(vs))
	}
}

// aliasExpr is the AST form of emitAlias (aliasTmpl): name, actual, optional
// tags + visibility.
func aliasExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("alias")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("actual", strExpr(t.AliasActual))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// boolFlagExpr is the AST form of emitBoolFlag (boolFlagTmpl): name,
// build_setting_default (True/False literal), optional tags + visibility.
func boolFlagExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("bool_flag")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("build_setting_default", boolIdent(t.BoolFlagDefault))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// leadingCommentTokens renders a target's leading author comment + provenance
// breadcrumb (gated by opts) as buildtools Before-comments, reusing the exact
// text generators so the comment bytes match the text path; build.Format then
// reproduces them identically (proven by the spike + the per-kind gates).
func leadingCommentTokens(t ir.Target, opts Options) []build.Comment {
	var cbuf bytes.Buffer
	if opts.EmitSourceComments {
		emitLeadingComment(&cbuf, t.LeadingComment)
	}
	if opts.EmitProvenance {
		emitTargetProvenance(&cbuf, t)
	}
	if cbuf.Len() == 0 {
		return nil
	}
	var out []build.Comment
	for _, line := range strings.Split(strings.TrimRight(cbuf.String(), "\n"), "\n") {
		out = append(out, build.Comment{Token: line})
	}
	return out
}
