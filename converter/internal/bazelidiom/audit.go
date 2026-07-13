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
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchainfeature"
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
	// Cross-rule check: non-C/C++/ASM language sources (Fortran) in a
	// cc_* rule — Bazel's cc rules compile by file extension and can't
	// build these, so the target is unbuildable as emitted.
	findings = append(findings, auditNonCCLanguageSources(rule, target, call)...)
	return findings
}

// fortranSrcExts are the source extensions Bazel's cc_* rules do NOT
// know how to compile. cmake happily puts these in a target the
// converter lowers to cc_library/cc_binary (cmake drives a Fortran
// compiler per-source by LANGUAGE; Bazel cc rules dispatch purely on
// extension and have no Fortran action), so the emitted rule would fail
// at build time. OpenBLAS's reference-LAPACK targets (LAPACK_OVERRIDES
// etc., ~3274 `.f` srcs) are the canonical case. ASM (.S/.s) is
// deliberately excluded — rules_cc's default toolchain DOES assemble
// those.
var fortranSrcExts = map[string]bool{
	".f": true, ".f90": true, ".f95": true, ".f03": true, ".f08": true,
	".for": true, ".ftn": true, ".fpp": true,
}

// auditNonCCLanguageSources fires on a cc_library / cc_binary / cc_test
// whose srcs include Fortran sources. Bazel's cc rules can't compile
// them, so this is a hard build-time failure the converter emitted
// silently — surfacing it points the operator at the missing
// Fortran-ruleset wiring (rules_fortran or a foreign_cc build) rather
// than a confusing downstream "no compiler for .f" error.
func auditNonCCLanguageSources(rule, target string, call *build.CallExpr) []Finding {
	switch rule {
	case "cc_library", "cc_binary", "cc_test":
	default:
		return nil
	}
	hits := flatListContains(call, "srcs", func(s string) bool {
		dot := strings.LastIndex(s, ".")
		if dot < 0 {
			return false
		}
		return fortranSrcExts[strings.ToLower(s[dot:])]
	})
	if len(hits) == 0 {
		return nil
	}
	sample := hits[0]
	more := ""
	if len(hits) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(hits)-1)
	}
	return []Finding{{
		Rule:   rule,
		Target: target,
		Code:   "non-cc-language-source",
		Message: rule + " has Fortran source(s) in srcs (e.g. " + sample + more +
			") — Bazel's cc rules compile by file extension and can't build these. The converter normally retags a Fortran target to a fortran_library (//rules:fortran.bzl, which drives the cc toolchain's own gfortran driver) and a mixed C+Fortran target gains a private `<name>_fortran` sibling the cc_* target deps on; a finding here means some Fortran source slipped through that retag. Fix the retag (lower's retagFortranTargets), or hand-route the source to a fortran_library",
	}}
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
		return "pch-speed-not-replicated",
			"target uses target_precompile_headers — the forced-include semantics are preserved (the converter expands the declared PCH header list into -include copts), but the compile-SPEED effect of actual precompilation is not (Bazel cc_library has no native PCH attribute). Optional: wire real precompilation operator-side per docs/operator-toolchain-features.md"
	case tag == "cmake-codegen-fortran-target":
		// Informational-only now: the converter lowers Fortran targets to a
		// buildable fortran_library (//rules:fortran.bzl) that drives the cc
		// toolchain's own gfortran driver, so this is no longer an
		// operator-action gap (it builds, given gfortran in the cc toolchain —
		// the GNU default). The tag is kept for provenance/grep-ability; it
		// doesn't surface as an idiom finding.
		return "", ""
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
		// Informational only: the lifter now emits the native
		// effect (`-rdynamic` in linkopts) for ENABLE_EXPORTS, so
		// no operator action is required. The tag remains for
		// auditability; like cmake-codegen-version it signals no
		// gap, so return no finding.
		return "", ""
	case strings.HasPrefix(tag, "cmake-codegen-language-override="):
		lang := strings.TrimPrefix(tag, "cmake-codegen-language-override=")
		return "language-override-needs-split",
			"target has set_source_files_properties(... LANGUAGE " + lang + ") on at least one source — Bazel cc_library compiles each source by its file extension; the override is silently dropped. Fix: rename the source to a " + lang + "-conventional extension or split the affected sources into a separate cc_library"
	}
	return "", ""
}

// auditCmakeElidedTags surfaces the audit-eligible link/include
// silent-drop tags that signal operator action gaps: the #219
// prefix-include drop and the unresolved-link-arm gap (a direct
// target_link_libraries arm that resolved to no imports-manifest
// import and isn't a toolchain system lib).
//
// The pre-existing cmake-elided-* tags (build-dir-source,
// missing-source, compiler-artifact) are intentionally not
// scanned here — those are file-existence-level filtering
// signals, not operator-action gaps. A future PR can fold
// them in if/when the audit-framework taxonomy grows to
// cover that family.
func auditCmakeElidedTags(rule, target string, call *build.CallExpr) []Finding {
	tags := flatListContains(call, "tags", func(s string) bool {
		return strings.HasPrefix(s, "cmake-unresolved-link-arm=") ||
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

// elidedTagToFinding maps an audit-eligible link/include silent-drop
// tag (see auditCmakeElidedTags) to an audit finding (code, message).
// Returns ("", "") for tags outside the audit-eligible subset,
// matching codegenTagToFinding's shape.
func elidedTagToFinding(tag string) (code, msg string) {
	switch {
	case strings.HasPrefix(tag, "cmake-unresolved-link-arm="):
		arm := strings.TrimPrefix(tag, "cmake-unresolved-link-arm=")
		return "unresolved-link-arm",
			"target's target_link_libraries names " + arm + " which resolves to no imports-manifest import and isn't a toolchain system lib — a manifest-producer (harvest/export) gap; the dep is missing from `deps` and the BUILD will link-fail. Fix: harvest the library at its producing element (so an exports.json entry maps its name/path to a real cc_import/cc_library label), or add it to the imports manifest"
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

// looksLikeFeatureFlag and featureForRawFlag delegate to the
// shared toolchainfeature package so the audit-side detection
// and the lower-side rewrite stay in lockstep — adding a new
// raw-flag mapping is a single edit visible to both consumers.
func looksLikeFeatureFlag(flag string) bool { return toolchainfeature.LooksLikeFeatureFlag(flag) }

func featureForRawFlag(flag string) string { return toolchainfeature.Feature(flag) }

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
	lc = strings.TrimSuffix(lc, "_enabled")
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
	textualHdrs := listAttrLen(call, "textual_hdrs")
	if srcs > 0 || hdrs > 0 || textualHdrs > 0 {
		return nil
	}
	// A deps-only cc_library is an idiomatic transparent
	// re-export: the wrapper exposes the union of its deps'
	// interfaces. Bazel accepts this shape; the convertor
	// produces it as the wrapper for multi-language splits
	// (LLVMSupport -> :LLVMSupport_c + :LLVMSupport_cxx) and
	// as the parent of cross-target hdrs-strip cases. The
	// audit shouldn't surface a finding for these — the
	// "converter refused everything" diagnostic only applies
	// when there's truly nothing to expose.
	if listAttrLen(call, "deps") > 0 || listAttrLen(call, "implementation_deps") > 0 {
		return nil
	}
	// Suppress when the target is a trace-synthesized INTERFACE
	// library marker. Some projects (abseil's `config`,
	// `pretty_function`) declare INTERFACE libraries deliberately
	// empty — they serve as cmake-side namespace anchors with no
	// content. The `cmake-codegen-interface-library-from-trace`
	// tag signals this; emitting the empty-cc-library finding for
	// them would surface noise without operator-actionable signal.
	if flatListContains(call, "tags", func(s string) bool {
		return s == "cmake-codegen-interface-library-from-trace"
	}) != nil {
		return nil
	}
	return []Finding{{
		Rule:    rule,
		Target:  target,
		Code:    "empty-cc-library",
		Message: "cc_library has no srcs, hdrs, or deps — typically means the converter refused everything for this target; expected an upstream lowerer to produce sources or a deliberate INTERFACE_LIBRARY (cc_library with hdrs only) when the target is header-only",
	}}
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

// auditCCBinaryOrTest fires on cc_binary / cc_test with no srcs. In this
// repo's emitted output that usually means the converter didn't recover the
// target's sources (compileGroups[].sourceIndexes) — it is not a Bazel hard
// error, since a deps-only binary builds fine when a dep supplies the entry
// point. The multi-language structural-split *wrapper* (lower.go's
// splitCompileGroups) is exactly that valid case — deps-only by design, its
// sources in the sibling "<name>_<lang>" sub-libraries it deps on — so it is
// exempted; a srcs-less target with no split-sibling dep still fires.
func auditCCBinaryOrTest(rule, target string, call *build.CallExpr) []Finding {
	var findings []Finding
	if listAttrLen(call, "srcs") == 0 && !depsContainLanguageSplitLib(target, call) {
		findings = append(findings, Finding{
			Rule:    rule,
			Target:  target,
			Code:    "empty-srcs",
			Message: rule + " has no srcs — likely the converter didn't recover this target's sources (compileGroups[].sourceIndexes); a deps-only binary builds only when a dep supplies the entry point (e.g. a multi-language split wrapper, which this audit exempts)",
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

// languageSplitSuffixes are the per-language sub-library name suffixes the
// multi-language structural split synthesizes (lower.go's langSuffix): its
// explicit cases (c, cxx, objc, objcxx, fortran, asm) plus common languages
// it lowercases via the strings.ToLower(lang) fallback (cuda). Not exhaustive
// — keep in sync with langSuffix and extend it when a new language's split
// wrapper surfaces as an empty-srcs false positive.
var languageSplitSuffixes = []string{"c", "cxx", "objc", "objcxx", "fortran", "asm", "cuda"}

// depsContainLanguageSplitLib reports whether the rule's deps include a
// sibling sub-library named "<target>_<lang>" — i.e. this is the deps-only
// wrapper of a multi-language structural split, not a target that lost its
// sources. Uses the concat-aware flatListContains so a `[flat] + select({...})`
// deps shape (per-platform deps) is handled too: the unconditional
// split-sibling deps live in the flat list.
func depsContainLanguageSplitLib(target string, call *build.CallExpr) bool {
	return len(flatListContains(call, "deps", func(label string) bool {
		return isLanguageSplitSibling(target, label)
	})) > 0
}

// isLanguageSplitSibling reports whether label names the wrapper's own split
// sub-library "<target>_<lang>" (optionally "_<n>" for the multi-compile-
// group-per-language case). Only a package-relative (":name") or root-package
// ("//:name") label qualifies — the split emits the sub-libs in the wrapper's
// package or hoisted to root, so a "//other:<target>_<lang>" in an unrelated
// package is a name coincidence, not a split sibling, and must not exempt.
func isLanguageSplitSibling(target, label string) bool {
	name, ok := splitSiblingLocalName(label)
	if !ok || !strings.HasPrefix(name, target+"_") {
		return false
	}
	rest := name[len(target)+1:]
	for _, s := range languageSplitSuffixes {
		if rest == s {
			return true
		}
		// "<lang>_<n>" for the multi-compile-group-per-language case: the
		// tail after the language must be all digits, so an ordinary dep
		// like ":foo_cxx_runtime" is not treated as a split sibling.
		if strings.HasPrefix(rest, s+"_") && isAllDigits(rest[len(s)+1:]) {
			return true
		}
	}
	return false
}

// splitSiblingLocalName returns the target name of a dep label only when the
// label is package-relative (":name" or bare "name") or in the root package
// ("//:name") — the only places splitCompileGroups puts a wrapper's own split
// sub-libs. Returns ok=false for another package ("//pkg:name") or an external
// repo ("@repo//..."), so a name coincidence there can't exempt.
func splitSiblingLocalName(label string) (string, bool) {
	if strings.HasPrefix(label, "//:") {
		return label[len("//:"):], true
	}
	if strings.HasPrefix(label, "//") || strings.HasPrefix(label, "@") {
		return "", false
	}
	return strings.TrimPrefix(label, ":"), true
}

// isAllDigits reports whether s is non-empty and entirely ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
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
