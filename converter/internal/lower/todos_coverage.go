package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// sanitizeBuildDir replaces the (run-specific, often mktemp) cmake build
// directory with a stable placeholder so a refusal message that embeds an
// absolute build-dir path doesn't make the deterministic report vary across
// runs/checkouts. No-op when buildDir is empty.
func sanitizeBuildDir(s, buildDir string) string {
	if buildDir == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, buildDir, "<builddir>")
}

// Generic conversion-todos producers that give the report FULL coverage of the
// converter's refusal + bake surfaces (beyond the three no-mechanical-form
// breadcrumb producers in todos_producers.go). Each entry carries a best-guess
// `disposition` (see todos.Disposition) — a fallible hint the agent may
// override.

// rejectionDisposition is the default disposition per Tier-1 failure code.
// Unmapped codes default to Actionable (an unknown refusal most likely needs an
// author). Data/structural errors the agent cannot author are Informational;
// "no obvious mechanical form but plausibly authorable" lean Improvement so the
// agent is invited to look rather than told to skip.
var rejectionDisposition = map[failure.Code]todos.Disposition{
	// No-mechanical-form: author the Bazel equivalent.
	failure.UnsupportedExecuteProcess:      todos.Actionable,
	failure.UnsupportedCustomCommand:       todos.Actionable,
	failure.UnsupportedCustomCommandScript: todos.Actionable,
	// Plausibly authorable, converter just couldn't — invite the agent.
	failure.UnsupportedTargetType: todos.Improvement,
	failure.UnsupportedSourcePath: todos.Improvement,
	// Data / structural / converter-capability errors the agent can't author.
	failure.UnresolvedInclude:       todos.Informational,
	failure.UnresolvedLinkDep:       todos.Informational,
	failure.AllSourcesElided:        todos.Informational,
	failure.FileAPIMalformed:        todos.Informational,
	failure.MissingTraceData:        todos.Informational,
	failure.BazelCanonicalizeFailed: todos.Informational,
}

func dispositionForCode(c failure.Code) todos.Disposition {
	if d, ok := rejectionDisposition[c]; ok {
		return d
	}
	return todos.Actionable
}

// emitRejectionTodos mirrors every collected Tier-1 refusal into a todo, one
// grouped unit per failure code (anchors = the refusal sites). The rejection
// collector is only populated in diagnostic mode
// (--ignore-rejections-for-diagnostics); in normal mode a refusal aborts before
// the producers run, so this is a no-op there.
func emitRejectionTodos(c *todos.Collector, rej *rejection.Collector, buildDir string) {
	if c == nil || rej == nil {
		return
	}
	items := rej.Items()
	if len(items) == 0 {
		return
	}
	byCode := map[failure.Code][]rejection.Rejection{}
	for _, r := range items {
		byCode[r.Code] = append(byCode[r.Code], r)
	}
	for code, rs := range byCode {
		anchors := make([]todos.Anchor, 0, len(rs))
		for _, r := range rs {
			construct := sanitizeBuildDir(r.Message, buildDir)
			if r.Target != "" {
				construct = r.Target + ": " + construct
			}
			anchors = append(anchors, todos.Anchor{File: sanitizeBuildDir(r.Source, buildDir), Construct: construct})
		}
		c.Add(todos.Todo{
			Kind:           "rejection:" + string(code),
			Disposition:    dispositionForCode(code),
			GroupKey:       string(code),
			Anchors:        anchors,
			Evidence:       map[string]any{"failure_code": string(code), "count": len(rs)},
			SuggestedShape: "author the idiomatic Bazel form for this refused construct",
			Prompt: "The converter refused this construct (Tier-1 `" + string(code) +
				"`) and emitted nothing for it. Supply the Bazel form, or confirm it needs none.",
		})
	}
}

// emitBakeTodos mirrors every convert-time bake (the rules carrying a
// convertTimeBakedShapes tag) into a todo — one per baked target, anchors =
// its bake reasons. Default disposition Improvement (the build works but the
// value is frozen); a lift site that knows better can override per target via
// cc.bakeTodoDisposition (e.g. a baked VCS stamp → Actionable, vs a baked
// check-probe → Improvement, both under the same tag).
func emitBakeTodos(c *todos.Collector, pkg *ir.Package, overrides map[string]todos.Disposition) {
	if c == nil || pkg == nil {
		return
	}
	entries := collectBakedEntries(pkg) // deduped, sorted (name, reason)
	if len(entries) == 0 {
		return
	}
	byName := map[string][]string{}
	var order []string
	for _, e := range entries {
		if _, seen := byName[e.name]; !seen {
			order = append(order, e.name)
		}
		byName[e.name] = append(byName[e.name], e.reason)
	}
	for _, name := range order {
		reasons := byName[name]
		anchors := make([]todos.Anchor, 0, len(reasons))
		for _, reason := range reasons {
			anchors = append(anchors, todos.Anchor{Construct: reason})
		}
		disp := todos.Improvement
		if o, ok := overrides[name]; ok {
			disp = o
		}
		c.Add(todos.Todo{
			Kind:           "bake",
			Disposition:    disp,
			GroupKey:       name,
			Anchors:        anchors,
			Evidence:       map[string]any{"target": name},
			SuggestedShape: "replace the frozen convert-time value with a dynamic Bazel form (e.g. a select()/config_setting, a live cmake_configure_file, or a genrule)",
			Prompt: "This target baked a convert-time value instead of a faithful, dynamic Bazel form. " +
				"Check whether the value correlates with a platform/sysroot/toolchain and could lift to a select/flag.",
		})
	}
}

// unresolvedGenexTags are audit tags marking a genex the converter couldn't
// resolve and baked the rendered bytes (or left a non-portable literal) for.
var unresolvedGenexTags = map[string]bool{
	"cmake-codegen-genex-unresolved":     true,
	"cmake-codegen-cmd-genex-unresolved": true,
}

// emitUnresolvedGenexTodos mirrors targets carrying an unresolved-genex audit
// tag into a todo, grouped by tag (anchors = the targets). Disposition
// Improvement: the build works (bytes baked / literal frozen) but a faithful
// genex resolution may exist — some of these are accepted residue
// ($<TARGET_OBJECTS> in argv, cross-element $<TARGET_FILE>), which the agent can
// recognize and leave alone.
func emitUnresolvedGenexTodos(c *todos.Collector, pkg *ir.Package) {
	if c == nil || pkg == nil {
		return
	}
	byTag := map[string][]string{}
	for _, t := range pkg.Targets {
		for _, tag := range t.Tags {
			if unresolvedGenexTags[tag] {
				byTag[tag] = append(byTag[tag], t.Name)
			}
		}
	}
	tags := sliceutil.SortedKeys(byTag)
	for _, tag := range tags {
		names := byTag[tag]
		anchors := make([]todos.Anchor, 0, len(names))
		for _, n := range names {
			anchors = append(anchors, todos.Anchor{Construct: n})
		}
		c.Add(todos.Todo{
			Kind:           "genex-unresolved",
			Disposition:    todos.Improvement,
			GroupKey:       tag,
			Anchors:        anchors,
			Evidence:       map[string]any{"audit_tag": tag},
			SuggestedShape: "resolve the generator expression to its faithful Bazel form (label/select), or confirm it's accepted residue",
			Prompt: "The converter could not resolve a generator expression here and baked/froze it (`" + tag +
				"`). Resolve it to a portable Bazel form if one exists, or leave it if it's accepted residue.",
		})
	}
}
