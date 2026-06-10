package lower

import (
	"sort"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

// emitExecuteProcessRefusalTodos mirrors each refused execute_process call
// into a structured, agent-actionable todo — the per-call sibling of the
// coarse `rejection:unsupported-execute-process` mirror (which
// emitRejectionTodos now skips in favor of this producer; its single anchor
// was the entire formatted Tier-1 blob, useless for targeting work).
//
// Grouping: one todo per (source file, bucket) — the unit an author
// re-works together; the GroupKey is line-free per the todos id contract so
// the id is stable across unrelated edits. N refusals in one file/bucket
// fold into one todo with N anchors. Anchors dedupe identical
// (file, line, argv) triples — traces record duplicate calls on
// re-evaluation.
//
// Determinism: every reason/argv token passes the report-path
// normalization (the build dir is a per-run mktemp path; leaking it would
// break the byte-identical-report contract the todos-coverage gate pins).
func emitExecuteProcessRefusalTodos(c *todos.Collector, refusals []executeProcessRefusal, sourceRoot, buildDir string) {
	if c == nil || len(refusals) == 0 {
		return
	}
	type groupKey struct {
		file   string
		bucket Bucket
	}
	groups := map[groupKey][]executeProcessRefusal{}
	for _, r := range refusals {
		k := groupKey{file: normalizeReportPath(r.File, sourceRoot, buildDir), bucket: r.Bucket}
		groups[k] = append(groups[k], r)
	}
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].file != keys[j].file {
			return keys[i].file < keys[j].file
		}
		return keys[i].bucket < keys[j].bucket
	})
	for _, k := range keys {
		rs := groups[k]
		var anchors []todos.Anchor
		seenAnchor := map[string]bool{}
		reasonSet := map[string]bool{}
		var invocations []string
		seenArgv := map[string]bool{}
		for _, r := range rs {
			argv := strings.Join(normalizeReportPaths(strings.Fields(r.Argv), sourceRoot, buildDir), " ")
			reason := normalizeReportPath(sanitizeBuildDir(r.Reason, buildDir), sourceRoot, buildDir)
			reasonSet[reason] = true
			if !seenArgv[argv] {
				seenArgv[argv] = true
				invocations = append(invocations, argv)
			}
			ak := k.file + "\x00" + argv
			if seenAnchor[ak] {
				continue
			}
			seenAnchor[ak] = true
			anchors = append(anchors, todos.Anchor{
				File:      k.file,
				Line:      r.Line,
				Construct: "execute_process(" + argv + ")",
			})
		}
		reasons := make([]string, 0, len(reasonSet))
		for r := range reasonSet {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		sort.Strings(invocations)
		c.Add(todos.Todo{
			Kind:        "execute-process-refusal",
			Disposition: todos.Actionable,
			GroupKey:    k.file + "|" + string(k.bucket),
			Anchors:     anchors,
			Evidence: map[string]any{
				"bucket":      string(k.bucket),
				"reasons":     reasons,
				"invocations": invocations,
			},
			SuggestedShape: "a genrule/run_binary producing the artifact at build time; a bool_flag + config_setting for a feature probe; workspace-status stamping for VCS/date/identity values; or confirm the call is configure-only and needs no Bazel form",
			Prompt: "The converter refused " + plural(len(anchors), "execute_process call") +
				" in this file (bucket: " + string(k.bucket) + "). Reasons: " +
				strings.Join(reasons, "; ") +
				". Author the idiomatic Bazel form for each, or confirm none is needed.",
		})
	}
}

// plural renders "N thing"/"N things" for the refusal prompt.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
