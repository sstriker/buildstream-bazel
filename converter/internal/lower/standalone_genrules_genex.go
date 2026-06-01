package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Generator-expression auditing for the standalone custom-command lift.
//
// cmake resolves generator expressions away at *generation* time, so the
// `build.ninja` command the genrule cmd is built from never contains `$<...>`
// — it carries the already-resolved (recording-machine-specific) value. The
// unresolved genex only survives in the `--trace-expand` record of the
// add_custom_command call (shadow.AddCustomCommandCall.Commands). The existing
// rewriteToolFromTarget pass already lifts a TARGET_FILE-family-derived
// build-dir-relative artifact path (e.g. `bin/t`) into a portable
// `$(location :t)` label + tools dep. What was missing is *visibility*: an
// operator couldn't tell a genex-bearing custom command from a plain one, nor
// whether a path-bearing genex was lifted to a label or left as a baked,
// non-portable literal.
//
// "Path-bearing" means the whole TARGET_FILE family extractTargetFileRefs
// scans — `$<TARGET_FILE[_DIR|_NAME]:t>`, `$<TARGET_LINKER_FILE[_DIR|_NAME]:t>`,
// `$<TARGET_SONAME_FILE:t>` — plus `$<TARGET_OBJECTS:t>`; all resolve to a path
// that must become a Bazel label to stay portable. Value-only genexes
// ($<CONFIG> and the like) carry no path and bake correctly for one configure.
//
// These helpers cross-reference each emitted genrule back to its trace call
// and emit an audit tag — cmake-codegen-cmd-genex-resolved when every
// path-bearing genex mapped to a label, cmake-codegen-cmd-genex-unresolved
// when at least one did not (its machine-specific path is baked verbatim).

const (
	cmdGenexResolvedTag   = "cmake-codegen-cmd-genex-resolved"
	cmdGenexUnresolvedTag = "cmake-codegen-cmd-genex-unresolved"
)

// customCommandGenex captures the generator-expression footprint of a
// trace-recorded add_custom_command's COMMAND argv.
type customCommandGenex struct {
	// hasGenex is true when the argv contained any `$<...>`.
	hasGenex bool
	// targetFileRefs is the cmake target names referenced by the TARGET_FILE
	// family (extractTargetFileRefs: TARGET_FILE[_DIR/_NAME],
	// TARGET_LINKER_FILE[_DIR/_NAME], TARGET_SONAME_FILE); targetObjects is
	// the names from $<TARGET_OBJECTS:t>. Both are the path-bearing genexes
	// whose resolution must become a Bazel label to stay portable.
	targetFileRefs []string
	targetObjects  []string
}

// buildOutputToCustomCommandGenex indexes each add_custom_command OUTPUT /
// BYPRODUCT to the generator-expression footprint of the call's COMMAND argv.
// Only calls whose argv actually carries a genex are recorded; returns nil
// when the trace carries no genex-bearing commands (the offline / genex-free
// path, where the audit tag is never added). First-write-wins on duplicate
// output paths, matching buildOutputToCustomTargetIndex.
func buildOutputToCustomCommandGenex(commands []shadow.AddCustomCommandCall) map[string]customCommandGenex {
	if len(commands) == 0 {
		return nil
	}
	idx := map[string]customCommandGenex{}
	for _, c := range commands {
		blob := joinCommandArgv(c.Commands)
		if !hasGenex(blob) {
			continue
		}
		info := customCommandGenex{
			hasGenex:       true,
			targetFileRefs: extractTargetFileRefs(blob),
			targetObjects:  extractTargetObjectsRefs(blob),
		}
		for _, o := range c.Outputs {
			if _, used := idx[o]; !used && o != "" {
				idx[o] = info
			}
		}
		for _, o := range c.ByProducts {
			if _, used := idx[o]; !used && o != "" {
				idx[o] = info
			}
		}
	}
	if len(idx) == 0 {
		return nil
	}
	return idx
}

// joinCommandArgv flattens an add_custom_command's [][]string COMMAND argv into
// a single space-joined blob for genex scanning. A trailing space per token is
// harmless — the genex scanners match on the `$<...>` substrings, not argv
// boundaries.
func joinCommandArgv(cmds [][]string) []byte {
	var b strings.Builder
	for _, argv := range cmds {
		for _, a := range argv {
			b.WriteString(a)
			b.WriteByte(' ')
		}
	}
	return []byte(b.String())
}

// customCommandGenexTag classifies the genex-resolution outcome for an emitted
// genrule against its trace call's recorded genex footprint, returning the
// audit tag (or "" when no genex-bearing call matched the edge's outputs).
//
// tools is the genrule's tools list (`:name` entries) produced by
// rewriteToolFromTarget: a path-bearing genex (any TARGET_FILE-family op or
// `$<TARGET_OBJECTS:t>`) counts as resolved iff its target name ended up there
// (i.e. its build-dir-relative path was lifted to `$(location :t)`). A call
// carrying only value genexes ($<CONFIG> and the like) has no path-portability
// hazard for a single configure, so it classifies as resolved.
func customCommandGenexTag(outs []string, idx map[string]customCommandGenex, tools []string) string {
	if len(idx) == 0 {
		return ""
	}
	info, found := customCommandGenex{}, false
	for _, o := range outs {
		if gi, ok := idx[o]; ok {
			info, found = gi, true
			break
		}
	}
	if !found || !info.hasGenex {
		return ""
	}
	mapped := make(map[string]bool, len(tools))
	for _, t := range tools {
		mapped[strings.TrimPrefix(t, ":")] = true
	}
	for _, name := range info.targetFileRefs {
		if !mapped[name] {
			return cmdGenexUnresolvedTag
		}
	}
	for _, name := range info.targetObjects {
		if !mapped[name] {
			return cmdGenexUnresolvedTag
		}
	}
	return cmdGenexResolvedTag
}
