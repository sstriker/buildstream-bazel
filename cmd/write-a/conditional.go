package main

// BuildStream (?): per-arch conditional blocks → project-B select().
//
// Real .bst files declare per-arch variable overrides via the `(?):`
// directive inside `variables:` blocks:
//
//   variables:
//     arch_options: ''
//     (?):
//     - target_arch == "x86_64":
//         arch_options: "--enable-sse2 --enable-avx"
//     - target_arch == "aarch64":
//         arch_options: "--enable-neon"
//
// 81 of 1 092 FDSDK elements (7 %) declare these. They control the
// shape of compiler flags, install destinations, and command lists
// per CPU architecture.
//
// At write-a time we don't pick an arch — that would bake host-arch
// into a cross-compile-capable system. Instead we extract the
// (?): block into a structured form and let the pipeline handler
// emit `cmd = select({...})` over `@platforms//cpu:*` so Bazel
// resolves per target platform at build time.
//
// extractConditionalsFromVariables walks a yaml.Node tree, finds the
// (?): block inside the top-level `variables:` map (the only place
// FDSDK puts them), parses each branch's expression, and removes
// the (?): key from the tree so the subsequent struct-decode step
// doesn't choke on the unhandled shape. Returns the parsed branches.
//
// Expression syntax recognized:
//
//	<var> == "X" / <var> == 'X'
//	<var> != "X"               (target_arch only — closed-set complement)
//	<var> in ("X", "Y", ...)
//	<var> == "X" or <var> == "Y" or ...   (consistent LHS variable)
//	<var> != "X" and <var> != "Y" and ... (same-LHS intersection;
//	                                       the FDSDK negation-chain shape)
//	(... any of the above ...)            (a single layer of outer parens)
//
// Mixed-LHS chains, parens at intermediate positions, and
// other unrecognized syntax return ("", nil) — the caller skips
// the branch (no select() entry emitted; static dispatch falls
// through). Branches with empty Varname (unrecognized expression)
// survive in the conditionalBranch list as-is so future parser
// extensions can reach them.
//
// Historic note (this comment block once read v1-only):
//   target_arch == "X"
//   target_arch == "X" or target_arch == "Y" or ...
//   target_arch in ("X", "Y", ...)
//   target_arch != "X"        (matches every supported-arch except X)
// Expressions referencing other variables (host_arch, build_arch,
// custom %{variant}) and richer combinators (and / parentheses)
// are not yet supported — branches with unrecognized expressions
// produce no arches and are silently skipped (a real-FDSDK miss
// surfaces as a missing `cmd` branch in the rendered select(),
// which is preferable to baking in a wrong default).

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
	"gopkg.in/yaml.v3"
)

// supportedArches enumerates the CPU arches write-a renders
// `cmd = select({...})` branches for. Each entry maps to a Bazel
// constraint label; FDSDK uses these arch identifiers in its
// target_arch == "..." expressions. The set mirrors FDSDK's
// project.conf arch.yml branches.
var supportedArches = []string{
	"x86_64",
	"aarch64",
	"i686",
	"ppc64le",
	"riscv64",
	"loongarch64",
}

// archConstraintLabel returns the Bazel @platforms//cpu:* label
// each supported arch lowers to. i686 is a Bazel-side rename of
// "x86_32"; everything else matches the BuildStream-side identifier.
func archConstraintLabel(arch string) string {
	switch arch {
	case "i686":
		return "@platforms//cpu:x86_32"
	default:
		return "@platforms//cpu:" + arch
	}
}

// conditionalBranch is one entry in a (?): block's list.
//
// Single-LHS branches (the dominant shape) populate Varname +
// Arches: the LHS variable the expression dispatches on plus the
// values the branch matches. `target_arch`-keyed single-LHS
// branches lower to project-B select() over @platforms//cpu:*;
// other LHS variables get statically evaluated against
// staticDispatchVars at graph-load time and folded into the
// parent variable layer.
//
// Mixed-LHS `and`-chains
// (`target_arch == X and bootstrap_build_arch == Y`) populate
// Constraints: a slice with one entry per LHS variable, each
// carrying the values that variable must take for the branch
// to fire. All constraints in the slice must match for the
// branch to apply (intersection semantics). Varname/Arches stay
// empty in this case so existing single-LHS callers cleanly
// skip mixed-LHS branches; multi-LHS-aware callers
// (branchMatchesTuple, dispatchSpaceForElement) iterate
// Constraints.
//
// Overrides is the yaml.Node carrying the variable-overrides /
// partial-pipelineCfg the branch contributes when matching.
type conditionalBranch struct {
	Varname     string
	Arches      []string
	Constraints []conditionalConstraint
	Overrides   *yaml.Node
}

// conditionalConstraint is one (Varname, Values) pair inside a
// mixed-LHS branch's Constraints slice. The branch matches a
// dispatch tuple iff every constraint's Varname maps to a value
// in its Values set.
type conditionalConstraint struct {
	Varname string
	Values  []string
}

// varEqualsRE matches `<var> == "X"` (with single-quote alternative).
// Whitespace tolerant. Captures (var, value).
var varEqualsRE = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_-]*)\s*==\s*['"]([^'"]+)['"]\s*$`)

// varNotEqualsRE matches `<var> != "X"`.
var varNotEqualsRE = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_-]*)\s*!=\s*['"]([^'"]+)['"]\s*$`)

// varInRE matches `<var> in ("X", "Y", "Z")`.
var varInRE = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_-]*)\s+in\s+\(([^)]+)\)\s*$`)

// archInTupleEntryRE extracts quoted arch names from the tuple
// body of varInRE.
var archInTupleEntryRE = regexp.MustCompile(`['"]([^'"]+)['"]`)

// parseArchExpression returns the LHS variable and the set of
// supported arches a BuildStream conditional expression matches.
// Most FDSDK uses target_arch on the LHS; bootstrap_build_arch /
// host_arch / build_arch and a few custom dispatch variables also
// appear (see flags.yml). Recognized syntax:
//
//	<var> == "X"
//	<var> != "X"
//	<var> in ("X", "Y", ...)
//	<var> == "X" or <var> == "Y" or ...   (consistent LHS variable)
//
// Unrecognized syntax / mixed-LHS or-chains return ("", nil) — the
// caller skips the branch (no select() entry emitted; static
// dispatch falls through).
//
// Arches matching is filtered to supportedArches for the LHS that
// drives Bazel select() lowering (target_arch); for other LHS
// variables the arches set returns the literal RHS values
// (bootstrap_build_arch / host_arch values aren't constrained to
// the @platforms//cpu:* set).
func parseArchExpression(expr string) (varname string, arches []string) {
	expr = stripOuterParens(strings.TrimSpace(expr))
	// `or`-joined chain — union the parts (same-LHS required; mixed-LHS
	// chains return ("", nil)).
	if strings.Contains(expr, " or ") && !strings.Contains(expr, " and ") {
		return parseArchChain(strings.Split(expr, " or "), false)
	}
	// `and`-joined chain — same-LHS intersection. The most common FDSDK
	// shape is the negation chain `var != "X" and var != "Y"` (the != handler
	// returns each part's complement set; intersection then gives the arches
	// excluded by both). Mixed-LHS and-chains require multi-dimensional
	// constraint dispatch — not yet implemented; they surface as ("", nil).
	if strings.Contains(expr, " and ") && !strings.Contains(expr, " or ") {
		return parseArchChain(strings.Split(expr, " and "), true)
	}
	if m := varEqualsRE.FindStringSubmatch(expr); m != nil {
		varname = m[1]
		v := m[2]
		if varname == "target_arch" && !isSupported(v) {
			return varname, nil
		}
		return varname, []string{v}
	}
	if m := varNotEqualsRE.FindStringSubmatch(expr); m != nil {
		varname = m[1]
		notv := m[2]
		// !=  only well-defined for target_arch — we know the
		// closed set @platforms//cpu:* allows. For other vars,
		// can't enumerate the complement; leave nil.
		if varname != "target_arch" {
			return varname, nil
		}
		out := make([]string, 0, len(supportedArches))
		for _, a := range supportedArches {
			if a != notv {
				out = append(out, a)
			}
		}
		return varname, out
	}
	if m := varInRE.FindStringSubmatch(expr); m != nil {
		varname = m[1]
		var out []string
		for _, em := range archInTupleEntryRE.FindAllStringSubmatch(m[2], -1) {
			v := em[1]
			if varname == "target_arch" && !isSupported(v) {
				continue
			}
			out = append(out, v)
		}
		return varname, out
	}
	return "", nil
}

// stripOuterParens removes a single layer of matched outer parens wrapping the
// whole expression (the common case where authors wrap for readability). Parens
// at intermediate positions (precedence-grouping inside an or/and chain) are
// left intact: an expression like "(a) or (b)", whose outer parens DON'T span
// the whole string, is returned unchanged so the trim doesn't munge two
// unrelated groups.
func stripOuterParens(expr string) string {
	if !strings.HasPrefix(expr, "(") || !strings.HasSuffix(expr, ")") {
		return expr
	}
	depth := 0
	for i, c := range expr {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return expr // unbalanced: a ')' with no matching '('
			}
			if depth == 0 && i != len(expr)-1 {
				return expr // closing paren doesn't span the whole expr
			}
		}
	}
	if depth != 0 {
		return expr // unbalanced: leftover '(' (e.g. "((a)")
	}
	return strings.TrimSpace(expr[1 : len(expr)-1])
}

// parseArchChain parses an `or`/`and`-joined arch expression by recursing on
// each part. Every part must share the same LHS variable (mixed-LHS chains
// return ("", nil)). With intersect=false the parts' arch sets are unioned (the
// `or` case); with intersect=true they're intersected (the `and` case — the
// first part seeds the accumulator).
func parseArchChain(parts []string, intersect bool) (string, []string) {
	var lhs string
	var out []string
	first := true
	for _, p := range parts {
		v, vs := parseArchExpression(strings.TrimSpace(p))
		if v == "" {
			return "", nil
		}
		if lhs == "" {
			lhs = v
		} else if lhs != v {
			return "", nil
		}
		switch {
		case !intersect:
			out = mergeArches(out, vs)
		case first:
			out = vs
		default:
			out = intersectArches(out, vs)
		}
		first = false
	}
	return lhs, out
}

func isSupported(arch string) bool {
	for _, a := range supportedArches {
		if a == arch {
			return true
		}
	}
	return false
}

// intersectArches returns the order-preserving intersection of a
// and b. Used by `and`-joined same-LHS expressions: the matching
// arches are those present in every conjunct.
func intersectArches(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}

func mergeArches(a, b []string) []string {
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			a = append(a, x)
			seen[x] = true
		}
	}
	return a
}

// extractConditionalsFromVariables looks for a (?): key inside the
// top-level `variables:` map of doc, removes it from the tree, and
// returns the parsed branches. doc is expected to be the post-
// composer DocumentNode wrapping a top-level mapping (a .bst or
// project.conf).
//
// Branches whose expression we don't recognize get parsed with
// Arches=nil — they survive in the returned slice so a future
// expression-evaluator can evaluate them, but the v1 pipeline
// handler skips them (no select() branch emitted).
func extractConditionalsFromVariables(doc *yaml.Node) ([]conditionalBranch, error) {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	// Find variables: key.
	varsIdx := -1
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "variables" {
			varsIdx = i + 1
			break
		}
	}
	if varsIdx < 0 {
		return nil, nil
	}
	varsNode := root.Content[varsIdx]
	if varsNode.Kind != yaml.MappingNode {
		return nil, nil
	}
	// Find (?): inside variables.
	condIdx := -1
	for i := 0; i < len(varsNode.Content); i += 2 {
		if varsNode.Content[i].Value == "(?)" {
			condIdx = i
			break
		}
	}
	if condIdx < 0 {
		return nil, nil
	}
	condValue := varsNode.Content[condIdx+1]
	// Excise the (?): from variables: map.
	varsNode.Content = append(varsNode.Content[:condIdx], varsNode.Content[condIdx+2:]...)

	if condValue.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("(?): expected a sequence of branches, got node kind %d", condValue.Kind)
	}
	var branches []conditionalBranch
	for _, branchNode := range condValue.Content {
		// Each branch is a mapping with one key (the expression)
		// and the override map as its value.
		if branchNode.Kind != yaml.MappingNode || len(branchNode.Content) < 2 {
			return nil, fmt.Errorf("(?): branch must be a mapping with one expression key, got node kind %d", branchNode.Kind)
		}
		expr := branchNode.Content[0].Value
		overrides := branchNode.Content[1]
		varname, arches := parseArchExpression(expr)
		var constraints []conditionalConstraint
		if varname == "" {
			// Mixed-LHS and-chain fallback: produces multi-
			// constraint branches; Varname/Arches stay empty
			// so single-LHS callers cleanly skip this branch.
			constraints = parseMixedLHSAndChain(expr)
		}
		branches = append(branches, conditionalBranch{
			Varname:     varname,
			Arches:      arches,
			Constraints: constraints,
			Overrides:   overrides,
		})
	}
	return branches, nil
}

// parseMixedLHSAndChain handles the `and`-joined chain shape
// where conjuncts target different LHS variables, e.g.
// `target_arch == "x86_64" and bootstrap_build_arch == "aarch64"`.
// Same-LHS conjuncts intersect; different-LHS conjuncts each
// produce a constraint.
//
// Returns nil for any expression that isn't a pure
// and-chain-of-recognized-conjuncts (caller falls through to
// the silently-skipped behaviour for unrecognized syntax).
func parseMixedLHSAndChain(expr string) []conditionalConstraint {
	expr = strings.TrimSpace(expr)
	// Strip a single layer of outer parens (same logic as
	// parseArchExpression's prelude).
	if strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") {
		depth := 0
		ok := true
		for i, c := range expr {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 && i != len(expr)-1 {
					ok = false
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			expr = strings.TrimSpace(expr[1 : len(expr)-1])
		}
	}
	if !strings.Contains(expr, " and ") || strings.Contains(expr, " or ") {
		return nil
	}
	byVar := map[string][]string{}
	order := []string{}
	for _, part := range strings.Split(expr, " and ") {
		v, vs := parseArchExpression(strings.TrimSpace(part))
		if v == "" {
			return nil
		}
		if _, seen := byVar[v]; !seen {
			order = append(order, v)
			byVar[v] = vs
		} else {
			byVar[v] = intersectArches(byVar[v], vs)
		}
	}
	out := make([]conditionalConstraint, 0, len(order))
	for _, v := range order {
		out = append(out, conditionalConstraint{Varname: v, Values: byVar[v]})
	}
	return out
}

// branchMatchesTuple reports whether a conditional branch fires
// for a given dispatch tuple. tuple maps dispatch variable
// names to chosen values for the current select() arm. The
// branch is keyed off (Varname, Arches): match iff
// tuple[Varname] is in Arches.
//
// Empty Varname (unrecognized expression) never matches —
// callers skip those branches.
//
// Empty tuple (no dispatch dimensions): match iff the branch is
// keyed on a static-dispatch var whose folded value lies in
// Arches. v1 of the foldStaticConditionals pass already folded
// the static-keyed branches into element.Bst.Variables, so by
// the time we reach this function the surviving branches are
// dispatch-keyed; the no-tuple case shouldn't fire matches.
func branchMatchesTuple(b conditionalBranch, tuple map[string]string) bool {
	// Mixed-LHS path: every constraint must match.
	if len(b.Constraints) > 0 {
		for _, c := range b.Constraints {
			v, ok := tuple[c.Varname]
			if !ok {
				return false
			}
			matched := false
			for _, val := range c.Values {
				if val == v {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
		return true
	}
	// Single-LHS path.
	if b.Varname == "" {
		return false
	}
	v, ok := tuple[b.Varname]
	if !ok {
		return false
	}
	for _, a := range b.Arches {
		if a == v {
			return true
		}
	}
	return false
}

// extractConditionalsFromConfig pulls the `config: (?):` block
// out of a .bst doc tree analogously to
// extractConditionalsFromVariables. The returned branches'
// Overrides node is a partial pipelineCfg shape (e.g. just
// configure-commands) — pipelineHandler's resolveAt merges them
// per matching tuple.
//
// Element-level config: (?): is the FDSDK bootstrap pattern:
// per-arch configure-commands overrides on the same .bst.
// Without this extractor, the deep-strip pass drops them (the
// loader still succeeds, but per-arch overrides don't fire);
// with it, the pipeline handler resolves the right command set
// per dispatch tuple.
func extractConditionalsFromConfig(doc *yaml.Node) ([]conditionalBranch, error) {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}
	cfgIdx := -1
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "config" {
			cfgIdx = i + 1
			break
		}
	}
	if cfgIdx < 0 {
		return nil, nil
	}
	cfgNode := root.Content[cfgIdx]
	if cfgNode.Kind != yaml.MappingNode {
		return nil, nil
	}
	condIdx := -1
	for i := 0; i < len(cfgNode.Content); i += 2 {
		if cfgNode.Content[i].Value == "(?)" {
			condIdx = i
			break
		}
	}
	if condIdx < 0 {
		return nil, nil
	}
	condValue := cfgNode.Content[condIdx+1]
	cfgNode.Content = append(cfgNode.Content[:condIdx], cfgNode.Content[condIdx+2:]...)
	if condValue.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("config: (?): expected a sequence of branches, got node kind %d", condValue.Kind)
	}
	var branches []conditionalBranch
	for _, branchNode := range condValue.Content {
		if branchNode.Kind != yaml.MappingNode || len(branchNode.Content) < 2 {
			return nil, fmt.Errorf("config: (?): branch must be a mapping with one expression key, got node kind %d", branchNode.Kind)
		}
		expr := branchNode.Content[0].Value
		overrides := branchNode.Content[1]
		varname, arches := parseArchExpression(expr)
		var constraints []conditionalConstraint
		if varname == "" {
			// Mixed-LHS and-chain fallback: produces multi-
			// constraint branches; Varname/Arches stay empty
			// so single-LHS callers cleanly skip this branch.
			constraints = parseMixedLHSAndChain(expr)
		}
		branches = append(branches, conditionalBranch{
			Varname:     varname,
			Arches:      arches,
			Constraints: constraints,
			Overrides:   overrides,
		})
	}
	return branches, nil
}

// stripRemainingConditionals walks the post-extract tree and
// removes any `(?):` keys still present in deeper nested
// positions (under config:, environment:, public:, …) so the
// subsequent struct-decode pass doesn't choke on the
// list-of-mapping shape inside strict-typed slots.
//
// extractConditionalsFromVariables already pulled the
// `variables: (?):` block — the only conditional shape v1
// actually consumes. Branches deeper in the tree are silently
// dropped (the .bst loads but the per-arch overrides don't
// fire). FDSDK fixtures that hit per-arch `config:` blocks land
// the structured per-config extractor when they surface —
// tracked as a follow-up; today this strip just unblocks load.
func stripRemainingConditionals(doc *yaml.Node) {
	stripCondNode(doc)
}

func stripCondNode(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range node.Content {
			stripCondNode(c)
		}
	case yaml.MappingNode:
		out := node.Content[:0]
		for i := 0; i < len(node.Content); i += 2 {
			k := node.Content[i].Value
			if k == "(?)" {
				continue
			}
			out = append(out, node.Content[i], node.Content[i+1])
		}
		node.Content = out
		for i := 1; i < len(node.Content); i += 2 {
			stripCondNode(node.Content[i])
		}
	}
}

// staticDispatchVars carries the values of dispatch variables that
// (?): branches reference besides target_arch. write-a evaluates
// them statically — these aren't multi-arch-select() candidates
// (the dispatch values aren't constrained to @platforms//cpu:*),
// they're configuration knobs whose value is fixed at build-graph-
// load time.
//
// Auto-detected from runtime.GOARCH at init time so write-a
// produces correct branches on aarch64 / ppc64le / etc. dev
// hosts without manual configuration. CLI flags
// (--host-arch / --build-arch / --bootstrap-build-arch) override
// per-variable for cross-compile scenarios.
var staticDispatchVars = defaultStaticDispatchVars()

func defaultStaticDispatchVars() map[string]string {
	a := bstArchFromGOARCH(runtime.GOARCH)
	return map[string]string{
		"build_arch":           a,
		"host_arch":            a,
		"bootstrap_build_arch": a,
	}
}

// bstArchFromGOARCH maps Go's runtime.GOARCH names to BuildStream
// arch names (which in turn map onto Bazel's @platforms//cpu:*
// via archConstraintLabel). Unknown GOARCHes pass through
// unchanged — better than silently stamping the wrong arch onto
// every dispatch.
func bstArchFromGOARCH(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	case "ppc64le":
		return "ppc64le"
	case "riscv64":
		return "riscv64"
	case "loong64":
		return "loongarch64"
	default:
		return goarch
	}
}

// foldStaticConditionals partitions branches into:
//
//   - **target_arch-keyed** — survive in the returned slice for
//     the pipeline-handler's @platforms//cpu:* select() lowering.
//   - **option-typed** (varname is a key in optionTyped) — survive
//     in the returned slice for the pipeline-handler's
//     //options:<opt>_<val> select() lowering.
//   - **other** — statically evaluated against staticVars at
//     graph-load time; matching branches' overrides fold into
//     the variable map. Branches whose varname isn't in staticVars
//     also survive in the returned slice (the pipeline handler
//     skips them; effectively no-op until a value space is
//     declared).
//
// Branches with empty Varname (unrecognized expression) survive
// unchanged in the returned slice — the pipeline handler skips
// them, but they're available for a future expression-evaluator
// extension.
func foldStaticConditionals(vars map[string]string, branches []conditionalBranch, staticVars map[string]string, optionTyped map[string]bool) (map[string]string, []conditionalBranch) {
	out := map[string]string{}
	for k, v := range vars {
		out[k] = v
	}
	var remaining []conditionalBranch
	for _, b := range branches {
		// target_arch + option-typed survive for pipeline-handler
		// select() lowering — the static-fold can't represent
		// dynamic Bazel-side dispatch.
		if b.Varname == "target_arch" || b.Varname == "" || optionTyped[b.Varname] {
			remaining = append(remaining, b)
			continue
		}
		val, ok := staticVars[b.Varname]
		if !ok {
			// Unknown dispatch variable — preserve the branch
			// (caller skips); nothing to fold.
			remaining = append(remaining, b)
			continue
		}
		matches := false
		for _, a := range b.Arches {
			if a == val {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		// Fold the matching branch's overrides into vars.
		out = applyConditional(out, &b)
	}
	return out, remaining
}

// applyConditional layers the override variables for one
// conditional branch onto vars. Returns the merged map (does not
// mutate the input). Override values that reference other %{...}
// variables remain unresolved — callers run the result through the
// resolver to expand.
func applyConditional(vars map[string]string, branch *conditionalBranch) map[string]string {
	if branch == nil || branch.Overrides == nil {
		return vars
	}
	out := map[string]string{}
	for k, v := range vars {
		out[k] = v
	}
	if branch.Overrides.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i < len(branch.Overrides.Content); i += 2 {
		k := branch.Overrides.Content[i].Value
		out[k] = branch.Overrides.Content[i+1].Value
	}
	return out
}

// branchForArch returns the first branch whose Arches contains
// arch, or nil if no branch matches. Branches are evaluated in
// declaration order (BuildStream's contract: "first matching
// branch wins").
func branchForArch(branches []conditionalBranch, arch string) *conditionalBranch {
	for i := range branches {
		for _, a := range branches[i].Arches {
			if a == arch {
				return &branches[i]
			}
		}
	}
	return nil
}

// dispatchVar describes one variable that the pipeline handler
// dispatches over via Bazel select() — `target_arch` (lowered to
// @platforms//cpu:*) or an option-typed variable (lowered to
// //options:<name>_<value> config_setting). Values is the closed-
// set of values the dispatch enumerates.
type dispatchVar struct {
	Name   string
	Values []string
	Kind   string // "platform" for target_arch; "option" for project.conf options.
}

// dispatchSpaceForElement computes the ordered list of dispatch
// variables used by (?): branches in this element. Returns:
//
//   - empty: no conditional dispatch (single-string cmd, current path).
//   - one or more entries: the pipeline handler iterates the
//     cartesian product of values and emits one select() arm per
//     unique resolved phases group.
//
// The returned slice is sorted by Var.Name for deterministic
// rendering (config_setting names + select() arm order are stable
// across runs).
func dispatchSpaceForElement(elem *element, options map[string]bstOption) ([]dispatchVar, error) {
	seen := map[string]bool{}
	collect := func(b conditionalBranch) {
		// Mixed-LHS: each constraint contributes a dim.
		if len(b.Constraints) > 0 {
			for _, c := range b.Constraints {
				if dispatchable(c.Varname, options) {
					seen[c.Varname] = true
				}
			}
			return
		}
		if dispatchable(b.Varname, options) {
			seen[b.Varname] = true
		}
	}
	for _, b := range elem.ProjectConfConditionals {
		collect(b)
	}
	for _, b := range elem.Bst.Conditionals {
		collect(b)
	}
	// config: (?): branches contribute dispatch dimensions too —
	// per-arch configure-commands overrides need to fire under
	// the same per-tuple resolution path.
	for _, b := range elem.Bst.ConfigConditionals {
		collect(b)
	}
	if len(seen) == 0 {
		return nil, nil
	}
	names := sliceutil.SortedKeys(seen)
	out := make([]dispatchVar, 0, len(names))
	for _, name := range names {
		if name == "target_arch" {
			out = append(out, dispatchVar{Name: name, Values: supportedArches, Kind: "platform"})
			continue
		}
		opt := options[name]
		values := opt.Values
		if opt.Type == "bool" && len(values) == 0 {
			values = []string{"True", "False"}
		}
		out = append(out, dispatchVar{Name: name, Values: values, Kind: "option"})
	}
	return out, nil
}

// cartesianTuples generates every combination of dispatch values
// across the dispatch space. Returns one map per tuple (varname →
// value). Order is deterministic: outer loop iterates the first
// dispatch var's values; inner loops iterate subsequent vars'
// values.
func cartesianTuples(vars []dispatchVar) []map[string]string {
	if len(vars) == 0 {
		return nil
	}
	// Initialize with one empty tuple; expand one dimension at a time.
	tuples := []map[string]string{{}}
	for _, v := range vars {
		next := make([]map[string]string, 0, len(tuples)*len(v.Values))
		for _, t := range tuples {
			for _, val := range v.Values {
				clone := make(map[string]string, len(t)+1)
				for k, vv := range t {
					clone[k] = vv
				}
				clone[v.Name] = val
				next = append(next, clone)
			}
		}
		tuples = next
	}
	return tuples
}

// dispatchable reports whether a (?): branch keyed on varname
// produces a Bazel select() arm. target_arch (always) and
// project.conf-declared options participate; other variables are
// either static-folded (host_arch / build_arch) or skipped
// (unknown).
func dispatchable(varname string, options map[string]bstOption) bool {
	if varname == "" {
		return false
	}
	if varname == "target_arch" {
		return true
	}
	_, ok := options[varname]
	return ok
}

// resolveVarsForTuple is the multi-dispatch-variable resolver:
// each (varname, value) entry in tuple finds a matching (?):
// branch (Varname == varname && Arches contains value) and folds
// the branch's overrides into the variable scope before
// resolving. The tuple values themselves seed the variable scope
// (one layer above projectConf, below kindVars) so a reference
// like `%{target_arch}` resolves to the tuple's target_arch
// value.
//
// v1 only ever passes a one-entry tuple (per dispatchSpaceForElement's
// single-variable constraint). The signature accepts arbitrary
// tuples so the cross-product follow-up doesn't need to refactor.
func resolveVarsForTuple(elemBuiltins, projectConf, kindVars, elemVars map[string]string,
	tuple map[string]string,
	projectConditionals, elemConditionals []conditionalBranch) (map[string]string, error) {
	pc := projectConf
	for i := range projectConditionals {
		b := &projectConditionals[i]
		v, ok := tuple[b.Varname]
		if !ok {
			continue
		}
		for _, a := range b.Arches {
			if a == v {
				pc = applyConditional(pc, b)
				break
			}
		}
	}
	ev := elemVars
	for i := range elemConditionals {
		b := &elemConditionals[i]
		v, ok := tuple[b.Varname]
		if !ok {
			continue
		}
		for _, a := range b.Arches {
			if a == v {
				ev = applyConditional(ev, b)
				break
			}
		}
	}
	// Seed tuple values one layer above projectConf so
	// `%{target_arch}` etc. references resolve to the per-arm
	// dispatch value.
	pc2 := map[string]string{}
	for k, v := range pc {
		pc2[k] = v
	}
	for k, v := range tuple {
		pc2[k] = v
	}
	return resolveVars(elemBuiltins, pc2, kindVars, ev)
}
