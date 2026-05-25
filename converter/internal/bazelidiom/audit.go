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
	switch rule {
	case "cc_library":
		return auditCCLibrary(rule, target, call)
	case "cc_import":
		return auditCCImport(rule, target, call)
	case "cc_binary", "cc_test":
		return auditCCBinaryOrTest(rule, target, call)
	}
	return nil
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
	if listAttrLen(call, "srcs") == 0 {
		return []Finding{{
			Rule:    rule,
			Target:  target,
			Code:    "empty-srcs",
			Message: rule + " has no srcs — Bazel will reject at build time; check whether the converter recovered every source for this target's compileGroups[].sourceIndexes",
		}}
	}
	return nil
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
