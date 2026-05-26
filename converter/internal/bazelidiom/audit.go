// Package bazelidiom audits emitted BUILD bytes for known
// anti-patterns the cmake converter sometimes produces — Phase 7 of
// the generator-parity uplift (ROADMAP.md).
//
// The pass is observational, not prescriptive: each finding is a
// human-readable note explaining what the converter emitted and what
// the Bazel-idiomatic form would have been. Callers decide whether
// to surface findings as warnings, fail the build, or store them
// for audit-gate consumption. The package itself returns plain
// data — no I/O, no policy.
//
// Why audit instead of rewrite: many anti-patterns are downstream
// signals of upstream gaps (a cc_library with no srcs and no hdrs
// usually means the converter refused everything; the right fix is
// teaching the converter to lower the refused content, not
// rewriting the emit). The audit pass turns the gap into an
// inspectable artifact so the upstream fix prioritization is
// data-driven.
package bazelidiom

import (
	"fmt"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

// Finding is one audit observation.
type Finding struct {
	// Rule is the Bazel rule kind (e.g. "cc_library", "cc_import")
	// the finding fires on. Empty when the finding is file-level.
	Rule string
	// Target is the rule's name attribute. Empty when the finding
	// is file-level or the rule has no name.
	Target string
	// Code is a stable identifier for the finding kind — usable as
	// an audit-gate filter or allowlist key.
	Code string
	// Message is the human-readable description, including the
	// recommended Bazel-idiomatic form.
	Message string
}

// String formats a finding as `<rule>(<target>): <code>: <message>`
// for stderr surfacing.
func (f Finding) String() string {
	var prefix string
	switch {
	case f.Rule != "" && f.Target != "":
		prefix = fmt.Sprintf("%s(%s): ", f.Rule, f.Target)
	case f.Rule != "":
		prefix = f.Rule + ": "
	case f.Target != "":
		prefix = f.Target + ": "
	}
	return prefix + f.Code + ": " + f.Message
}

// Audit parses body as a BUILD.bazel file and returns findings.
// Returns nil + nil when the body is empty; returns an error when
// the body fails to parse (caller should treat this as a hard
// failure — emit produces parseable bytes by construction).
func Audit(body []byte) ([]Finding, error) {
	if len(body) == 0 {
		return nil, nil
	}
	f, err := build.Parse("BUILD.bazel", body)
	if err != nil {
		return nil, fmt.Errorf("bazelidiom: parse BUILD: %w", err)
	}
	var findings []Finding
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		rule := callName(call)
		if rule == "" {
			continue
		}
		target := stringAttr(call, "name")
		findings = append(findings, auditRule(rule, target, call)...)
	}
	return findings, nil
}

// auditRule dispatches per-rule-kind checks.
func auditRule(rule, target string, call *build.CallExpr) []Finding {
	var findings []Finding
	switch rule {
	case "cc_library":
		findings = append(findings, auditCCLibrary(rule, target, call)...)
	case "cc_import":
		findings = append(findings, auditCCImport(rule, target, call)...)
	case "cc_binary", "cc_test":
		findings = append(findings, auditCCBinaryOrTest(rule, target, call)...)
	}
	// Cross-rule check: sanitizer-shaped selects on copts / linkopts
	// should lower to --features instead. Fires on any rule that
	// accepts those attributes.
	findings = append(findings, auditSanitizerSelects(rule, target, call)...)
	// Cross-rule check: raw -fPIC / -flto / -fsanitize=... in
	// copts / linkopts — cc_toolchain features are the canonical
	// home for these flags.
	findings = append(findings, auditRawCompileFlags(rule, target, call)...)
	// Cross-rule check: surface known cmake-codegen-* tags that
	// signal operator action (PCH wiring, Qt host-tool genrules,
	// etc.) so the audit pass shows the same gaps as the gaps
	// doc's queued list.
	findings = append(findings, auditCmakeCodegenTags(rule, target, call)...)
	// Cross-rule check: surface cmake-elided-* tags that signal
	// operator action (currently the #219 prefix-include and #220
	// link-fragment silent-drops). Sibling to the codegen scan
	// above; kept as a separate function so the elision-vs-
	// codegen distinction stays clear in the tag taxonomy.
	findings = append(findings, auditCmakeElidedTags(rule, target, call)...)
	return findings
}

// auditCmakeCodegenTags surfaces cmake-codegen-* tags that signal
// operator action gaps. Each known tag maps to a finding kind and
// a recommendation; unknown cmake-codegen-* tags pass through
// silently (forward-compat with future tag additions).
func auditCmakeCodegenTags(rule, target string, call *build.CallExpr) []Finding {
	tags := flatListContains(call, "tags", func(s string) bool {
		return strings.HasPrefix(s, "cmake-codegen-")
	})
	if len(tags) == 0 {
		return nil
	}
	var findings []Finding
	for _, tag := range tags {
		code, msg := codegenTagToFinding(tag)
		if code == "" {
			continue
		}
		findings = append(findings, Finding{
			Rule:    rule,
			Target:  target,
			Code:    code,
			Message: msg,
		})
	}
	return findings
}

// codegenTagToFinding maps a cmake-codegen-* tag to an audit
// finding (code, message). Returns ("", "") for tags that don't
// signal operator action (informational-only tags like
// cmake-codegen-version=…).
func codegenTagToFinding(tag string) (code, msg string) {
	switch {
	case tag == "cmake-codegen-pch":
		return "pch-toolchain-feature-needed",
			"target declares target_precompile_headers — Bazel cc_library has no native PCH attribute; wire via cc_toolchain pch feature for the actual PCH effect"
	case tag == "cmake-codegen-qt-automoc":
		return "qt-automoc-host-tool-needed",
			"target has AUTOMOC=TRUE — cmake's generator runs moc as part of `cmake --build`; Bazel doesn't, so moc-generated sources are missing. Wrap moc as a host-tool genrule in a kind:bazel override or use a rules_qt module"
	case tag == "cmake-codegen-qt-autouic":
		return "qt-autouic-host-tool-needed",
			"target has AUTOUIC=TRUE — same as automoc but for the uic-generated header source"
	case tag == "cmake-codegen-qt-autorcc":
		return "qt-autorcc-host-tool-needed",
			"target has AUTORCC=TRUE — same as automoc but for the rcc-generated resource source"
	case tag == "cmake-codegen-enable-exports":
		return "enable-exports-toolchain-feature-needed",
			"target has ENABLE_EXPORTS=TRUE (executables exporting symbols) — Bazel cc_binary has no native attribute; wire via cc_toolchain feature"
	case strings.HasPrefix(tag, "cmake-codegen-language-override="):
		lang := strings.TrimPrefix(tag, "cmake-codegen-language-override=")
		return "language-override-needs-split",
			"target has set_source_files_properties(... LANGUAGE " + lang + ") on at least one source — Bazel cc_library compiles each source by its file extension; the override is silently dropped. Fix: rename the source to a " + lang + "-conventional extension or split the affected sources into a separate cc_library"
	case strings.HasPrefix(tag, "cmake-codegen-find-package-fallback="):
		rest := strings.TrimPrefix(tag, "cmake-codegen-find-package-fallback=")
		return "find-package-dep-unresolved",
			"target links a library resolved by find_package(" + rest + ") that has no imports-manifest entry — the dep is missing from `deps` and the BUILD will link-fail. Fix: add the package's namespaced target (e.g. `Pkg::Pkg`) to the imports manifest mapping to a real cc_import/cc_library label"
	}
	return "", ""
}

// auditCmakeElidedTags surfaces cmake-elided-* tags that signal
// operator action gaps. Sibling to auditCmakeCodegenTags;
// scoped to the new audit-eligible elision tags (the #219
// prefix-include and #220 link-fragment silent-drops).
//
// The pre-existing cmake-elided-* tags (build-dir-source,
// missing-source, compiler-artifact) are intentionally not
// scanned here — those are file-existence-level filtering
// signals, not operator-action gaps. A future PR can fold
// them in if/when the audit-framework taxonomy grows to
// cover that family.
func auditCmakeElidedTags(rule, target string, call *build.CallExpr) []Finding {
	tags := flatListContains(call, "tags", func(s string) bool {
		return strings.HasPrefix(s, "cmake-elided-link-fragment=") ||
			strings.HasPrefix(s, "cmake-elided-prefix-include=")
	})
	if len(tags) == 0 {
		return nil
	}
	var findings []Finding
	for _, tag := range tags {
		code, msg := elidedTagToFinding(tag)
		if code == "" {
			continue
		}
		findings = append(findings, Finding{
			Rule:    rule,
			Target:  target,
			Code:    code,
			Message: msg,
		})
	}
	return findings
}

// elidedTagToFinding maps a cmake-elided-* tag (in the audit-
// eligible subset — see auditCmakeElidedTags) to an audit
// finding (code, message). Returns ("", "") for tags that don't
// match the audit-eligible subset, matching codegenTagToFinding's
// shape.
func elidedTagToFinding(tag string) (code, msg string) {
	switch {
	case strings.HasPrefix(tag, "cmake-elided-link-fragment="):
		path := strings.TrimPrefix(tag, "cmake-elided-link-fragment=")
		return "unresolved-link-fragment",
			"target links an absolute-path library (" + path + ") that's neither in the imports manifest nor attributable to a find_package call — the dep is missing from `deps` and the BUILD will link-fail. Fix: add the library to the imports manifest, or declare the producing element as a kind:bazel / kind:cmake dep"
	case strings.HasPrefix(tag, "cmake-elided-prefix-include="):
		path := strings.TrimPrefix(tag, "cmake-elided-prefix-include=")
		return "unresolved-prefix-include",
			"target's compile-group lists an include path under the synth-prefix tree (" + path + ") that the producing element should be providing through a cc_library dep, not as a raw include. Fix: ensure the consuming target has the producing element on its `deps` (typically via find_package's namespaced target in the imports manifest); the cross-element headers flow through hdrs propagation, not includes"
	}
	return "", ""
}

// auditRawCompileFlags fires when copts / linkopts carry raw flag
// values that have first-class Bazel-toolchain features. cc_toolchain
// features are the canonical home for -fsanitize=address / -fPIC /
// -flto etc.; per-rule emission of those flags forks the toolchain's
// shape per target rather than declaring once.
//
// Strict literal-match (not via select() — that's the
// sanitizer-select-not-feature check). Catches the case where the
// converter (or operator-edited BUILD) baked the flag in directly.
func auditRawCompileFlags(rule, target string, call *build.CallExpr) []Finding {
	var findings []Finding
	for _, attr := range []string{"copts", "linkopts"} {
		hits := flatListContains(call, attr, looksLikeFeatureFlag)
		for _, flag := range hits {
			feat := featureForRawFlag(flag)
			if feat == "" {
				continue
			}
			findings = append(findings, Finding{
				Rule:    rule,
				Target:  target,
				Code:    "raw-toolchain-feature-flag",
				Message: "raw " + flag + " in " + attr + " — prefer features = [\"" + feat + "\"] so the cc_toolchain owns the flag set; per-rule emission forks toolchain shape per target",
			})
		}
	}
	return findings
}

// looksLikeFeatureFlag identifies raw flag strings that have a
// first-class cc_toolchain feature equivalent. Conservative —
// matches the common cmake-derived patterns; unrelated flags
// pass through silently.
func looksLikeFeatureFlag(flag string) bool {
	switch flag {
	case "-fPIC", "-fpic", "-flto":
		return true
	}
	if strings.HasPrefix(flag, "-fsanitize=") {
		return true
	}
	return false
}

// featureForRawFlag maps a raw compile/link flag to the
// cc_toolchain feature name that owns it (per the
// SANITIZER_FEATURES convention in
// examples/sanitizer-features/toolchain/features.bzl).
func featureForRawFlag(flag string) string {
	switch flag {
	case "-fPIC", "-fpic":
		return "pic"
	case "-flto":
		return "lto"
	case "-fsanitize=address":
		return "asan"
	case "-fsanitize=thread":
		return "tsan"
	case "-fsanitize=memory":
		return "msan"
	case "-fsanitize=undefined":
		return "ubsan"
	case "-fsanitize=leak":
		return "lsan"
	}
	return ""
}

// flatListContains returns flat-list literal entries in the named
// attribute that match the predicate. Skips select() arms (covered
// separately by auditSanitizerSelects); concat-with-select-arms
// scans the literal halves.
func flatListContains(call *build.CallExpr, attr string, match func(string) bool) []string {
	for _, arg := range call.List {
		bin, ok := arg.(*build.AssignExpr)
		if !ok {
			continue
		}
		key, ok := bin.LHS.(*build.Ident)
		if !ok || key.Name != attr {
			continue
		}
		return collectLiteralStrings(bin.RHS, match)
	}
	return nil
}

// collectLiteralStrings walks list literals + concat halves,
// returning the strings matching the predicate. Doesn't descend
// into select() arms.
func collectLiteralStrings(e build.Expr, match func(string) bool) []string {
	var out []string
	switch v := e.(type) {
	case *build.ListExpr:
		for _, item := range v.List {
			if s, ok := item.(*build.StringExpr); ok && match(s.Value) {
				out = append(out, s.Value)
			}
		}
	case *build.BinaryExpr:
		if v.Op == "+" {
			out = append(out, collectLiteralStrings(v.X, match)...)
			out = append(out, collectLiteralStrings(v.Y, match)...)
		}
	}
	return out
}

// auditSanitizerSelects fires when a rule's copts / linkopts / defines
// is a select() whose arms mention sanitizer-shaped config_setting
// names. The Bazel-idiomatic form is to define the sanitizer flags
// in a cc_toolchain feature and route via --features=<name>, not
// hand-roll the per-config select.
func auditSanitizerSelects(rule, target string, call *build.CallExpr) []Finding {
	var findings []Finding
	for _, attr := range []string{"copts", "linkopts", "defines"} {
		if names := selectKeysMatching(call, attr, looksLikeSanitizerConfig); len(names) > 0 {
			findings = append(findings, Finding{
				Rule:    rule,
				Target:  target,
				Code:    "sanitizer-select-not-feature",
				Message: "select() on " + attr + " keys " + strings.Join(names, ", ") + " match sanitizer/instrumentation patterns; Bazel-idiomatic form is a cc_toolchain feature (--features=asan / =tsan / =lto / …) rather than a per-rule select. Phase 5 of the generator-parity uplift maps these names automatically when multi-config is on; this finding fires on hand-rolled selects that bypassed the mapping.",
			})
		}
	}
	return findings
}

// looksLikeSanitizerConfig matches config_setting labels whose path
// component contains a known sanitizer / instrumentation marker.
// Conservative on purpose — false positives surface as informational
// findings rather than rewrites, so the cost of catching unrelated
// labels is low.
func looksLikeSanitizerConfig(label string) bool {
	lc := strings.ToLower(label)
	// Strip label syntax (`//config:asan` → `asan`).
	if i := strings.LastIndex(lc, ":"); i >= 0 {
		lc = lc[i+1:]
	}
	if i := strings.LastIndex(lc, "/"); i >= 0 {
		lc = lc[i+1:]
	}
	if strings.HasSuffix(lc, "_enabled") {
		lc = strings.TrimSuffix(lc, "_enabled")
	}
	switch lc {
	case "asan", "tsan", "msan", "ubsan", "lsan", "coverage", "lto":
		return true
	}
	return false
}

// selectKeysMatching returns the select() arm keys whose label
// matches the predicate. Returns nil when the attribute isn't
// present, isn't a select(), or no arm matches.
func selectKeysMatching(call *build.CallExpr, attr string, match func(string) bool) []string {
	for _, arg := range call.List {
		bin, ok := arg.(*build.AssignExpr)
		if !ok {
			continue
		}
		key, ok := bin.LHS.(*build.Ident)
		if !ok || key.Name != attr {
			continue
		}
		return collectSelectKeys(bin.RHS, match)
	}
	return nil
}

// collectSelectKeys walks an expression for select() arms and
// returns the keys whose label matches the predicate. Recurses
// through binary `+` concat (covers `[…] + select(…)` shapes).
func collectSelectKeys(e build.Expr, match func(string) bool) []string {
	var out []string
	switch v := e.(type) {
	case *build.BinaryExpr:
		if v.Op == "+" {
			out = append(out, collectSelectKeys(v.X, match)...)
			out = append(out, collectSelectKeys(v.Y, match)...)
		}
	case *build.CallExpr:
		if id, ok := v.X.(*build.Ident); ok && id.Name == "select" && len(v.List) > 0 {
			dict, ok := v.List[0].(*build.DictExpr)
			if !ok {
				return nil
			}
			for _, entry := range dict.List {
				str, ok := entry.Key.(*build.StringExpr)
				if !ok {
					continue
				}
				if match(str.Value) {
					out = append(out, str.Value)
				}
			}
		}
	}
	return out
}

// auditCCLibrary fires on empty cc_library (no srcs, no hdrs) —
// usually a sign the converter refused everything for a target
// and emitted a placeholder.
func auditCCLibrary(rule, target string, call *build.CallExpr) []Finding {
	srcs := listAttrLen(call, "srcs")
	hdrs := listAttrLen(call, "hdrs")
	if srcs == 0 && hdrs == 0 {
		return []Finding{{
			Rule:    rule,
			Target:  target,
			Code:    "empty-cc-library",
			Message: "cc_library has no srcs and no hdrs — typically means the converter refused everything for this target; expected an upstream lowerer to produce sources or a deliberate INTERFACE_LIBRARY (cc_library with hdrs only) when the target is header-only",
		}}
	}
	return nil
}

// auditCCImport fires on cc_import with no static_library and no
// shared_library — produces an unusable rule that consumers can't
// link against.
func auditCCImport(rule, target string, call *build.CallExpr) []Finding {
	hasStatic := hasNonEmptyStringAttr(call, "static_library")
	hasShared := hasNonEmptyStringAttr(call, "shared_library")
	if !hasStatic && !hasShared {
		return []Finding{{
			Rule:    rule,
			Target:  target,
			Code:    "empty-cc-import",
			Message: "cc_import has neither static_library nor shared_library — consumers can't link against this rule; check whether the underlying cmake target's install rule populated NameOnDisk + InstallDest correctly",
		}}
	}
	return nil
}

// auditCCBinaryOrTest fires on cc_binary / cc_test with no srcs —
// Bazel rejects these at build time, so it's worth catching at
// emit-audit time with a clearer message.
func auditCCBinaryOrTest(rule, target string, call *build.CallExpr) []Finding {
	var findings []Finding
	if listAttrLen(call, "srcs") == 0 {
		findings = append(findings, Finding{
			Rule:    rule,
			Target:  target,
			Code:    "empty-srcs",
			Message: rule + " has no srcs — Bazel will reject at build time; check whether the converter recovered every source for this target's compileGroups[].sourceIndexes",
		})
	}
	// cc_test with no srcs AND no deps that could provide a
	// test entry — likely a misconfigured test target. Bazel
	// will error at build time, but the audit catches it earlier
	// with attribution to the upstream cmake declaration.
	if rule == "cc_test" {
		if listAttrLen(call, "srcs") == 0 && listAttrLen(call, "deps") == 0 {
			findings = append(findings, Finding{
				Rule:    rule,
				Target:  target,
				Code:    "test-with-no-entry",
				Message: "cc_test has no srcs and no deps — the test rule has no entry point; verify the upstream cmake add_test() recorded a real binary, or that the test was meant to be filtered out",
			})
		}
	}
	return findings
}

// callName returns the function-call identifier name (the rule
// kind for a top-level BUILD call expression like
// `cc_library(name = ...)`).
func callName(call *build.CallExpr) string {
	ident, ok := call.X.(*build.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}

// stringAttr returns the string-literal value of the named keyword
// argument, or "" when the argument is missing or non-string.
func stringAttr(call *build.CallExpr, name string) string {
	for _, arg := range call.List {
		bin, ok := arg.(*build.AssignExpr)
		if !ok {
			continue
		}
		key, ok := bin.LHS.(*build.Ident)
		if !ok || key.Name != name {
			continue
		}
		str, ok := bin.RHS.(*build.StringExpr)
		if !ok {
			return ""
		}
		return str.Value
	}
	return ""
}

// hasNonEmptyStringAttr reports whether the named keyword argument
// is set to a non-empty string literal.
func hasNonEmptyStringAttr(call *build.CallExpr, name string) bool {
	return stringAttr(call, name) != ""
}

// listAttrLen returns the number of elements in the named list-
// valued keyword argument. Returns 0 when the argument is missing,
// non-list, or empty. select() / concat expressions count their
// declared elements heuristically — a select() with non-empty arms
// is counted as len(arms-with-content) > 0.
func listAttrLen(call *build.CallExpr, name string) int {
	for _, arg := range call.List {
		bin, ok := arg.(*build.AssignExpr)
		if !ok {
			continue
		}
		key, ok := bin.LHS.(*build.Ident)
		if !ok || key.Name != name {
			continue
		}
		return countListLike(bin.RHS)
	}
	return 0
}

// countListLike counts the elements in a list / concat / select.
// A list literal counts its elements; a concat sums its parts; a
// select counts an arm as 1 if it has non-empty content. Unknown
// shapes count as 1 (conservative — treat the attr as "has
// something" rather than "empty").
func countListLike(e build.Expr) int {
	switch v := e.(type) {
	case *build.ListExpr:
		return len(v.List)
	case *build.BinaryExpr:
		if v.Op == "+" {
			return countListLike(v.X) + countListLike(v.Y)
		}
	case *build.CallExpr:
		// select({...}): count arms with non-empty values.
		if id, ok := v.X.(*build.Ident); ok && id.Name == "select" && len(v.List) > 0 {
			dict, ok := v.List[0].(*build.DictExpr)
			if !ok {
				return 1
			}
			n := 0
			for _, entry := range dict.List {
				if countListLike(entry.Value) > 0 {
					n++
				}
			}
			return n
		}
	}
	return 1
}

// FormatFindings renders findings as a multi-line stderr message.
// Returns "" when findings is empty so callers can use the result
// as a conditional Fprintf body.
func FormatFindings(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, f := range findings {
		sb.WriteString(f.String())
		sb.WriteString("\n")
	}
	return sb.String()
}
