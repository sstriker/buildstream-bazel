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
func astTargetStmts(t ir.Target, opts Options) ([]build.Expr, bool, error) {
	one := func(c *build.CallExpr) ([]build.Expr, bool, error) { return []build.Expr{c}, true, nil }
	oneErr := func(c *build.CallExpr, err error) ([]build.Expr, bool, error) {
		if err != nil {
			return nil, true, err
		}
		return []build.Expr{c}, true, nil
	}
	switch t.Kind {
	case ir.KindAlias:
		return one(aliasExpr(t))
	case ir.KindBoolFlag:
		return one(boolFlagExpr(t))
	case ir.KindConfigSetting:
		return one(configSettingExpr(t))
	case ir.KindPickFile:
		return one(pickFileExpr(t))
	case ir.KindCCHash:
		return oneErr(ccHashExpr(t))
	case ir.KindFilegroup:
		return oneErr(filegroupExpr(t))
	case ir.KindShBinary:
		return one(shBinaryExpr(t))
	case ir.KindCCImport:
		return one(ccImportExpr(t))
	case ir.KindGenrule:
		return one(genruleExpr(t))
	case ir.KindNativeRule:
		return oneErr(nativeRuleExpr(t))
	case ir.KindCCEmbed:
		return oneErr(ccEmbedExpr(t))
	case ir.KindWriteFile:
		return one(writeFileExpr(t))
	case ir.KindPkgFiles:
		return one(pkgFilesExpr(t))
	case ir.KindCMakeConfigureFile:
		return oneErr(cmakeConfigureFileExpr(t))
	case ir.KindCCLibrary, ir.KindCCInterface, ir.KindCCBinary, ir.KindCCTest,
		ir.KindCudaLibrary, ir.KindCudaBinary, ir.KindCudaTest, ir.KindFortranLibrary:
		call, err := ccTargetCall(t, opts)
		if err != nil {
			return nil, true, err
		}
		stmts := []build.Expr{call}
		if t.SharedLibName != "" {
			stmts = append(stmts, sharedLibraryExpr(t))
		}
		return stmts, true, nil
	}
	return nil, false, nil
}

// genruleExpr is the AST form of emitGenrule: name, optional srcs, outs, cmd
// (a single quoted string), optional tools/tags/visibility.
func genruleExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("genrule")
	r.SetAttr("name", strExpr(t.Name))
	setListIfNonEmpty(r, "srcs", sortedCopy(t.Srcs))
	r.SetAttr("outs", strListExpr(sortedCopy(t.GenruleOuts)))
	r.SetAttr("cmd", strExpr(t.GenruleCmd))
	setListIfNonEmpty(r, "tools", sortedCopy(t.GenruleTools))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// nativeRuleExpr renders a generic native rule from its NativeRuleSpec: the
// rule kind, `name` from the target, then each spec attr (scalar string or
// string list). build.Format re-sorts attrs to buildifier's canonical order, so
// the spec's attr order doesn't matter. The load() for the rule is emitted
// separately (see emitNativeRuleLoads).
func nativeRuleExpr(t ir.Target) (*build.CallExpr, error) {
	s := t.NativeRule
	if s == nil || s.Kind == "" {
		return nil, fmt.Errorf("emit native rule %q: nil/kindless NativeRule spec", t.Name)
	}
	call, r := newCall(s.Kind)
	r.SetAttr("name", strExpr(t.Name))
	for _, a := range s.Attrs {
		switch {
		case a.List != nil:
			r.SetAttr(a.Name, strListExpr(a.List))
		case a.Ident != "":
			r.SetAttr(a.Name, &build.Ident{Name: a.Ident})
		default:
			r.SetAttr(a.Name, strExpr(a.Str))
		}
	}
	return call, nil
}

// ccEmbedExpr is the AST form of emitCCEmbed: scalar src/symbol/out_*, the
// True-only binary/nul_terminate flags, optional export_*, the fixed tool, and
// tags/visibility.
func ccEmbedExpr(t ir.Target) (*build.CallExpr, error) {
	s := t.CCEmbed
	if s == nil {
		return nil, fmt.Errorf("emit cc_embed %q: nil CCEmbed spec", t.Name)
	}
	call, r := newCall("cc_embed")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("src", strExpr(s.Src))
	r.SetAttr("symbol", strExpr(s.Symbol))
	r.SetAttr("out_header", strExpr(s.OutHeader))
	r.SetAttr("out_source", strExpr(s.OutSource))
	if s.Binary {
		r.SetAttr("binary", boolIdent(true))
	}
	if s.NulTerminate {
		r.SetAttr("nul_terminate", boolIdent(true))
	}
	if s.ExportSymbol != "" {
		r.SetAttr("export_symbol", strExpr(s.ExportSymbol))
	}
	if s.ExportHeader != "" {
		r.SetAttr("export_header", strExpr(s.ExportHeader))
	}
	r.SetAttr("tool", strExpr("//tools:cc-embed"))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call, nil
}

// newCall starts a rule CallExpr of the given kind and its Rule helper.
func newCall(kind string) (*build.CallExpr, *build.Rule) {
	call := &build.CallExpr{X: &build.Ident{Name: kind}}
	return call, build.NewRule(call)
}

func strExpr(s string) build.Expr { return &build.StringExpr{Value: s} }

// strListExpr builds a string list. Layout (inline vs multi-line) is left
// entirely to build.Format's width rule — the AST-direct emitter does not
// reproduce the old text renderers' arbitrary thresholds (strList's >60,
// indentArmList's always-multi-line); buildifier owns it, so the output is
// what `buildifier --mode=fix` produces by construction.
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
		entries = append(entries, &build.KeyValueExpr{Key: strExpr(k), Value: strListExpr(sel[k])})
	}
	entries = append(entries, &build.KeyValueExpr{
		Key:   strExpr("//conditions:default"),
		Value: strListExpr(defaultArm),
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

// selectCall builds `select({...})`. The dict is always multi-line: the text
// renderers (attrExpr, renderConfigDictSelect) open every select with a
// newline after `{`, which buildifier keeps. (attrExpr selects carry ≥2 arms
// and would multi-line anyway; a single-arm config select needs the force.)
func selectCall(entries []*build.KeyValueExpr) build.Expr {
	return &build.CallExpr{
		X:    &build.Ident{Name: "select"},
		List: []build.Expr{&build.DictExpr{List: entries}},
	}
}

// configDictSelectExpr is the AST form of renderConfigDictSelect: a
// `select({"//config:x": {dict}, …})` with NO default arm, each arm a
// strDict.
func configDictSelectExpr(perConfig map[string]map[string]string) build.Expr {
	labels := sliceutil.SortedKeys(perConfig)
	entries := make([]*build.KeyValueExpr, 0, len(labels))
	for _, l := range labels {
		entries = append(entries, &build.KeyValueExpr{Key: strExpr(l), Value: strDictExpr(perConfig[l])})
	}
	return selectCall(entries)
}

// cmakeConfigureFileExpr is the AST form of emitCMakeConfigureFile.
func cmakeConfigureFileExpr(t ir.Target) (*build.CallExpr, error) {
	s := t.CMakeConfigureFile
	if s == nil {
		return nil, fmt.Errorf("emit cmake_configure_file %q: nil CMakeConfigureFile spec", t.Name)
	}
	call, r := newCall("cmake_configure_file")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("out", strExpr(s.Out))
	if s.Template != "" {
		r.SetAttr("template", strExpr(s.Template))
	} else {
		r.SetAttr("content", strExpr(s.Content))
	}
	r.SetAttr("values", strDictExpr(s.Values))
	if len(s.StampValues) > 0 {
		r.SetAttr("stamp_values", strDictExpr(s.StampValues))
	}
	if len(s.GenexValuesPerConfig) > 0 {
		r.SetAttr("genex_values", configDictSelectExpr(s.GenexValuesPerConfig))
	} else if len(s.GenexValues) > 0 {
		r.SetAttr("genex_values", strDictExpr(s.GenexValues))
	}
	setStrIfNonEmpty(r, "genex_context", s.GenexContext)
	if len(s.TargetFiles) > 0 {
		r.SetAttr("target_files", strDictExpr(s.TargetFiles))
	}
	if len(s.TargetObjects) > 0 {
		r.SetAttr("target_objects", strDictExpr(s.TargetObjects))
	}
	r.SetAttr("tool", strExpr(s.Tool))
	if s.AtOnly {
		r.SetAttr("at_only", boolIdent(true))
	}
	if s.CopyOnly {
		r.SetAttr("copy_only", boolIdent(true))
	}
	if s.EscapeQuotes {
		r.SetAttr("escape_quotes", boolIdent(true))
	}
	setStrIfNonEmpty(r, "newline_style", s.NewlineStyle)
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call, nil
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
	r.SetAttr("flag_values", &build.DictExpr{List: []*build.KeyValueExpr{
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

// setStrIfNonEmpty SetAttrs a string attribute only when non-empty (the
// templates' `{{- if .X}}` guard around scalar string attributes).
func setStrIfNonEmpty(r *build.Rule, key, s string) {
	if s != "" {
		r.SetAttr(key, strExpr(s))
	}
}

// concatExprs is the AST form of concatListExprs: nil-aware `a + b`.
func concatExprs(a, b build.Expr) build.Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		return &build.BinaryExpr{Op: "+", X: a, Y: b}
	}
}

// strDictExpr builds a {k: v} dict. Layout is left to build.Format (see strListExpr).
func strDictExpr(m map[string]string) build.Expr {
	keys := sliceutil.SortedKeys(m)
	entries := make([]*build.KeyValueExpr, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, &build.KeyValueExpr{Key: strExpr(k), Value: strExpr(m[k])})
	}
	return &build.DictExpr{List: entries}
}

// ccViewToCall renders a ccView (its attr-expr fields already AST) into the
// cc-family rule CallExpr — the AST form of ccRuleTmpl. Attribute presence
// mirrors the template's `{{- if}}` guards (nil expr / empty string / empty
// list / false bool => omitted); order is left to build.Format's NamePriority.
func ccViewToCall(v ccView) *build.CallExpr {
	call, r := newCall(v.RuleKind)
	r.SetAttr("name", strExpr(v.Name))
	setIfNonNil(r, "srcs", v.SrcsExpr)
	setIfNonNil(r, "module_srcs", v.ModuleSrcsExpr)
	setIfNonNil(r, "hdrs", v.HdrsExpr)
	setIfNonNil(r, "textual_hdrs", v.TextualHdrsExpr)
	setIfNonNil(r, "includes", v.IncludesExpr)
	setStrIfNonEmpty(r, "include_prefix", v.IncludePrefix)
	setStrIfNonEmpty(r, "strip_include_prefix", v.StripIncludePrefix)
	setIfNonNil(r, "copts", v.CoptsExpr)
	setIfNonNil(r, "defines", v.DefinesExpr)
	setIfNonNil(r, "local_defines", v.LocalDefinesExpr)
	setIfNonNil(r, "linkopts", v.LinkoptsExpr)
	// rules_cuda's HOST-side link flags (lower's partitionCudaLinkopts fills
	// this only for KindCuda* targets; plain `linkopts` there is the DEVICE
	// link, and the cuda_binary/cuda_test macros drop it from the host link).
	setIfNonNil(r, "host_linkopts", v.HostLinkoptsExpr)
	setIfNonNil(r, "additional_linker_inputs", v.AdditionalLinkerInputsExpr)
	setIfNonNil(r, "deps", v.DepsExpr)
	setIfNonNil(r, "dynamic_deps", v.DynamicDepsExpr)
	setIfNonNil(r, "implementation_deps", v.ImplementationDepsExpr)
	setListIfNonEmpty(r, "data", v.Data)
	setListIfNonEmpty(r, "args", v.Args)
	if len(v.Env) > 0 {
		r.SetAttr("env", strDictExpr(v.Env))
	}
	setStrIfNonEmpty(r, "timeout", v.Timeout)
	if v.Linkstatic {
		r.SetAttr("linkstatic", boolIdent(true))
	}
	if v.Alwayslink {
		r.SetAttr("alwayslink", boolIdent(true))
	}
	// rules_cuda's relocatable-device-code attr (lower sets CudaRdc only on
	// KindCuda* targets, from the ninja device-link edge).
	if v.CudaRdc {
		r.SetAttr("rdc", boolIdent(true))
	}
	setListIfNonEmpty(r, "features", v.Features)
	setListIfNonEmpty(r, "tags", v.Tags)
	setListIfNonEmpty(r, "visibility", v.Visibility)
	return call
}

// sharedLibraryExpr is the AST form of emitSharedLibrary: the cc_shared_library
// companion wrapping a SHARED/MODULE library's static impl into a real .so.
func sharedLibraryExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("cc_shared_library")
	r.SetAttr("name", strExpr(t.Name+"_shared"))
	r.SetAttr("shared_lib_name", strExpr(t.SharedLibName))
	if dd := sortedCopy(t.SharedLibDynamicDeps); len(dd) > 0 {
		// emitSharedLibrary renders dynamic_deps multi-line (newline after `[`).
		r.SetAttr("dynamic_deps", strListExpr(dd))
	}
	// additional_linker_inputs pins files the user_link_flags reference via
	// $(location ...) (the version-map etc.). Sorted: a pure input set.
	if ali := sortedCopy(t.SharedLibAdditionalLinkerInputs); len(ali) > 0 {
		r.SetAttr("additional_linker_inputs", strListExpr(ali))
	}
	// user_link_flags reaches the .so LINK (soname etc.) — order-preserving,
	// not sorted, since linker flag order is semantic.
	if len(t.SharedLibUserLinkFlags) > 0 {
		r.SetAttr("user_link_flags", strListExpr(t.SharedLibUserLinkFlags))
	}
	r.SetAttr("deps", strListExpr([]string{":" + t.Name}))
	return call
}

// writeFileExpr is the AST form of emitWriteFile: name, out, content (an
// ORDERED list — not sorted — or a per-config select over the bake bodies),
// newline, optional tags/visibility.
func writeFileExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("write_file")
	r.SetAttr("name", strExpr(t.Name))
	r.SetAttr("out", strExpr(t.WriteFileOut))
	var content build.Expr
	if len(t.WriteFileContentByConfig) > 0 {
		sel := make(map[string][]string, len(t.WriteFileContentByConfig)+1)
		for label, lines := range t.WriteFileContentByConfig {
			sel[label] = lines
		}
		if _, ok := sel["//conditions:default"]; !ok {
			sel["//conditions:default"] = t.WriteFileContent
		}
		content = attrExprAST(nil, sel)
	} else {
		content = strListExpr(t.WriteFileContent)
	}
	r.SetAttr("content", content)
	newline := t.WriteFileNewline
	if newline == "" {
		newline = "unix"
	}
	r.SetAttr("newline", strExpr(newline))
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// pkgFilesExpr is the AST form of emitPkgFiles.
func pkgFilesExpr(t ir.Target) *build.CallExpr {
	call, r := newCall("pkg_files")
	r.SetAttr("name", strExpr(t.Name))
	srcs, strip := pkgFilesSrcsExprAST(t)
	if srcs == nil {
		srcs = &build.ListExpr{} // template renders the always-present srcs as [] when empty
	}
	r.SetAttr("srcs", srcs)
	setStrIfNonEmpty(r, "prefix", t.PkgPrefix)
	setIfNonNil(r, "renames", pkgRenamesExpr(t.PkgRenames))
	setIfNonNil(r, "strip_prefix", strip)
	setListIfNonEmpty(r, "tags", sortedCopy(t.Tags))
	setListIfNonEmpty(r, "visibility", nonDefaultVisibility(t.Visibility))
	return call
}

// pkgFilesSrcsExprAST is the AST form of pkgFilesSrcsExpr: a plain/select list
// or glob, plus the optional strip_prefix.from_pkg(...) call for the glob case.
func pkgFilesSrcsExprAST(t ir.Target) (srcs build.Expr, strip build.Expr) {
	if !t.PkgSrcsGlob {
		return attrExprAST(sortedCopy(t.Srcs), perPlatformAttr(t, "srcs")), nil
	}
	patterns := make([]string, 0, len(t.Srcs))
	for _, d := range sortedCopy(t.Srcs) {
		patterns = append(patterns, d+"/**")
	}
	srcs = globExpr(patterns)
	if t.PkgStripPrefix != "" {
		strip = &build.CallExpr{
			X:    &build.DotExpr{X: &build.Ident{Name: "strip_prefix"}, Name: "from_pkg"},
			List: []build.Expr{strExpr(t.PkgStripPrefix)},
		}
	}
	return srcs, strip
}

// pkgRenamesExpr is the AST form of pkgFilesRenamesExpr — an always-multi-line
// {src: dst} dict (the text renderer opens with a newline after `{`).
func pkgRenamesExpr(renames map[string]string) build.Expr {
	if len(renames) == 0 {
		return nil
	}
	keys := sliceutil.SortedKeys(renames)
	entries := make([]*build.KeyValueExpr, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, &build.KeyValueExpr{Key: strExpr(k), Value: strExpr(renames[k])})
	}
	return &build.DictExpr{List: entries}
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
