package bazel

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/bazelbuild/buildtools/build"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
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
// the text template, or (nil, false, nil) for kinds still on the text path.
// The leading/provenance comments are attached by the caller (targetStmts). An
// error mirrors the producer-bug guards the text emitters raise (e.g. a
// filegroup that sets both explicit srcs and a glob).
func astTargetCall(t ir.Target) (*build.CallExpr, bool, error) {
	switch t.Kind {
	case ir.KindAlias:
		return aliasExpr(t), true, nil
	case ir.KindBoolFlag:
		return boolFlagExpr(t), true, nil
	case ir.KindConfigSetting:
		return configSettingExpr(t), true, nil
	case ir.KindPickFile:
		return pickFileExpr(t), true, nil
	case ir.KindCCHash:
		call, err := ccHashExpr(t)
		return call, true, err
	case ir.KindFilegroup:
		call, err := filegroupExpr(t)
		return call, true, err
	case ir.KindShBinary:
		return shBinaryExpr(t), true, nil
	case ir.KindCCImport:
		return ccImportExpr(t), true, nil
	}
	return nil, false, nil
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

// armListExpr is strListExpr for a select() arm: the text path renders arm
// lists via indentArmList, which always opens with a newline after `[`, so
// buildifier keeps them multi-line. A NON-empty arm therefore forces
// multi-line; an empty arm stays the inline `[]` indentArmList also emits.
func armListExpr(vs []string) build.Expr {
	le := &build.ListExpr{List: make([]build.Expr, len(vs))}
	for i, v := range vs {
		le.List[i] = strExpr(v)
	}
	le.ForceMultiLine = len(vs) > 0
	return le
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

// setIfNonNil SetAttrs an attribute only when the value is present (nil =
// omit), mirroring the templates' `{{- if .XExpr}}` guards around
// attrExpr/scalarAttrExpr-derived attributes.
func setIfNonNil(r *build.Rule, key string, e build.Expr) {
	if e != nil {
		r.SetAttr(key, e)
	}
}

// attrExprAST is the AST form of attrExpr: a list attribute that is a plain
// list, a select({...}), or `list + select({...})`. Returns nil when empty
// (the attribute is omitted). Since both this AST and a parse of attrExpr's
// string are canonicalized by build.Format, only the STRUCTURE must match, not
// the string spacing.
func attrExprAST(flat []string, sel map[string][]string) build.Expr {
	hasSel := len(sel) > 0
	hasFlat := len(flat) > 0
	if !hasSel && !hasFlat {
		return nil
	}
	if !hasSel {
		return strListExpr(flat)
	}
	selExpr := selectListExpr(sel)
	if hasFlat {
		return &build.BinaryExpr{Op: "+", X: strListExpr(flat), Y: selExpr}
	}
	return selExpr
}

// selectListExpr builds `select({k: [..], …, "//conditions:default": [..]})`
// for a list-valued per-platform attribute, matching attrExpr's key order
// (sorted non-default keys, then the default arm last and exactly once).
func selectListExpr(sel map[string][]string) build.Expr {
	defaultArm := sel["//conditions:default"]
	keys := make([]string, 0, len(sel))
	for k := range sel {
		if k != "//conditions:default" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	entries := make([]*build.KeyValueExpr, 0, len(keys)+1)
	for _, k := range keys {
		entries = append(entries, &build.KeyValueExpr{Key: strExpr(k), Value: armListExpr(sel[k])})
	}
	entries = append(entries, &build.KeyValueExpr{
		Key:   strExpr("//conditions:default"),
		Value: armListExpr(defaultArm),
	})
	return selectCall(entries)
}

// scalarAttrExprAST is the AST form of scalarAttrExpr: a scalar attribute that
// is a quoted string or a select({k: "v", …, "//conditions:default": None}).
// Returns nil when empty.
func scalarAttrExprAST(flat string, sel map[string]string) build.Expr {
	hasSel := len(sel) > 0
	if !hasSel && flat == "" {
		return nil
	}
	if !hasSel {
		return strExpr(flat)
	}
	keys := sliceutil.SortedKeys(sel)
	entries := make([]*build.KeyValueExpr, 0, len(keys)+1)
	for _, k := range keys {
		entries = append(entries, &build.KeyValueExpr{Key: strExpr(k), Value: strExpr(sel[k])})
	}
	entries = append(entries, &build.KeyValueExpr{
		Key:   strExpr("//conditions:default"),
		Value: &build.Ident{Name: "None"},
	})
	return selectCall(entries)
}

func selectCall(entries []*build.KeyValueExpr) build.Expr {
	return &build.CallExpr{
		X:    &build.Ident{Name: "select"},
		List: []build.Expr{&build.DictExpr{List: entries}},
	}
}

// globExpr builds `glob(["pat", …])`.
func globExpr(patterns []string) build.Expr {
	return &build.CallExpr{X: &build.Ident{Name: "glob"}, List: []build.Expr{strListExpr(patterns)}}
}

// configSettingExpr is the AST form of emitConfigSetting: name, a single-entry
// flag_values dict, optional visibility (no tags).
func configSettingExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("config_setting")
	r.SetAttr("name", strExpr(t.Name))
	// ForceMultiLine: the template renders the single-entry flag_values dict
	// across lines; buildifier keeps a source-multi-line dict multi-line, so
	// the AST must force it (a 1-entry dict otherwise inlines).
	r.SetAttr("flag_values", &build.DictExpr{ForceMultiLine: true, List: []*build.KeyValueExpr{
		{Key: strExpr(t.ConfigSettingFlag), Value: strExpr(t.ConfigSettingValue)},
	}})
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// pickFileExpr is the AST form of emitPickFile: name, src, path, optional
// tags + visibility.
func pickFileExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("pick_file")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("src", strExpr(t.PickSrc))
	r.SetAttr("path", strExpr(t.PickPath))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// ccHashExpr is the AST form of emitCCHash.
func ccHashExpr(t ir.Target) (*build.CallExpr, error) {
	s := t.CCHash
	if s == nil {
		return nil, fmt.Errorf("emit cc_hash %q: nil CCHash spec", t.Name)
	}
	call, r := newCall("cc_hash")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("src", strExpr(s.Src))
	r.SetAttr("define_name", strExpr(s.Name))
	r.SetAttr("algorithm", strExpr(s.Algorithm))
	r.SetAttr("out_header", strExpr(s.OutHeader))
	r.SetAttr("tool", strExpr("//tools:cc-hash"))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call, nil
}

// filegroupExpr is the AST form of emitFilegroup: srcs as a plain/select list
// or glob(...); optional output_group, tags, visibility.
func filegroupExpr(t ir.Target) (*build.CallExpr, error) {
	call, r := newCall("filegroup")
	r.SetAttr("name", strExpr(t.Name))
	if len(t.FilegroupGlob) > 0 {
		if len(t.Srcs) > 0 {
			return nil, fmt.Errorf("filegroup %q sets both explicit srcs and FilegroupGlob; they are mutually exclusive", t.Name)
		}
		r.SetAttr("srcs", globExpr(sortedCopy(t.FilegroupGlob)))
	} else {
		setIfNonNil(r, "srcs", attrExprAST(sortedCopy(t.Srcs), perPlatformAttr(t, "srcs")))
	}
	if t.FilegroupOutputGroup != "" {
		r.SetAttr("output_group", strExpr(t.FilegroupOutputGroup))
	}
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call, nil
}

// shBinaryExpr is the AST form of emitShBinary.
func shBinaryExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("sh_binary")
	r.SetAttr("name", strExpr(t.Name))
	setIfNonNil(r, "srcs", attrExprAST(sortedCopy(t.Srcs), perPlatformAttr(t, "srcs")))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// ccImportExpr is the AST form of emitCCImport.
func ccImportExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("cc_import")
	r.SetAttr("name", strExpr(t.Name))
	setIfNonNil(r, "static_library", scalarAttrExprAST(t.StaticLibrary, perPlatformScalarAttr(t, "static_library")))
	setIfNonNil(r, "shared_library", scalarAttrExprAST(t.SharedLibrary, perPlatformScalarAttr(t, "shared_library")))
	setIfNonNil(r, "hdrs", attrExprAST(sortedCopy(t.Hdrs), perPlatformAttr(t, "hdrs")))
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
