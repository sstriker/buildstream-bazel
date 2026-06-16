package lower

import (
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// minStampDefineValueLen is the shortest stamp value worth matching against a
// compile define. 7 is the default `git rev-parse --short HEAD` length — the
// single most common VCS stamp this feature targets — so the floor must not
// exceed it. Its only job is to keep a TRIVIAL stamp value ("", "0", "1.0",
// all length <= 4) from coincidentally matching an unrelated -D define and
// producing a noisy todo; since detection is EXACT value-equality against a
// recovered stamp value (not a pattern), a 7-char match must be byte-identical
// to the real commit sha, so 7 is safe. The full sha (40), `git describe`, and
// epoch/date timestamps all clear it comfortably.
const minStampDefineValueLen = 7

// emitStampInDefineTodos surfaces a VCS/identity/date stamp value that was
// baked into a compile -D define (target_compile_definitions / add_definitions
// with `GIT_SHA="${GIT_SHA}"`-style) as an ACTIONABLE conversion-todo. Unlike a
// configure_file / file(WRITE) template — which the lift re-reads from the
// workspace status under --stamp (stamp_values) — a value baked into Bazel
// `defines`/`local_defines` is frozen at convert time: Bazel's defines can't
// read the workspace status, so the macro pins the convert-time revision and
// goes stale on every later commit. The converter can't safely auto-rewrite a
// define into a generated header (it would change compile semantics — the
// source gets the macro from the command line, not an include), so this names
// the frozen stamp loudly instead of letting it bake silently, pointing the
// author at the header-based stamping path.
//
// Detection is value-equality against the recovered stamp set: a define whose
// VALUE equals a known stamp variable's configure-time value (cmakeVars[var]
// for var in stampVars), guarded by minStampDefineValueLen so a trivial value
// can't false-positive. No-op on a nil collector or when the project has no
// stamps / defines.
func emitStampInDefineTodos(c *todos.Collector, pkg *ir.Package, stampVars, cmakeVars map[string]string) {
	if c == nil || pkg == nil || len(stampVars) == 0 || len(cmakeVars) == 0 {
		return
	}
	// Distinctive stamp value -> its workspace-status key.
	byValue := map[string]string{}
	for v, key := range stampVars {
		val := cmakeVars[v]
		if len(val) < minStampDefineValueLen {
			continue
		}
		if _, seen := byValue[val]; !seen {
			byValue[val] = key
		}
	}
	if len(byValue) == 0 {
		return
	}

	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		anchors, evidence := stampDefineFindings(t, byValue)
		if len(anchors) == 0 {
			continue
		}
		c.Add(todos.Todo{
			Kind:        "stamp-baked-define",
			Disposition: todos.Actionable,
			GroupKey:    t.Name,
			Anchors:     anchors,
			Evidence:    evidence,
			SuggestedShape: "route the stamped value through a generated header instead of a -D define: a " +
				"configure_file / cmake_configure_file emitting `#define <NAME> \"@<VAR>@\"` picks up stamp_values " +
				"(re-read from the workspace status under --stamp), then #include it and drop the define. " +
				"Or accept the frozen convert-time value if a live stamp isn't wanted.",
			Prompt: "This target bakes a VCS/identity/date stamp into a compile -D define, so the macro is FROZEN " +
				"at the convert-time value and won't refresh under --stamp (Bazel defines can't read the workspace " +
				"status). See suggested_shape to make it dynamic via a generated header.",
		})
	}
}

// stampDefineFindings returns one anchor per (define -> stamp) match on a
// target's Defines/LocalDefines, plus the evidence map. The anchor Construct
// names the define and the matched stamp key so the report is self-explaining.
func stampDefineFindings(t *ir.Target, byValue map[string]string) ([]todos.Anchor, map[string]any) {
	var matches []string // "NAME=STATUS_KEY", sorted
	for _, def := range append(append([]string{}, t.Defines...), t.LocalDefines...) {
		name, val, ok := splitDefine(def)
		if !ok {
			continue
		}
		key, isStamp := byValue[val]
		if !isStamp {
			continue
		}
		matches = append(matches, name+"="+key)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	sort.Strings(matches)
	matches = dedupeSorted(matches)
	anchors := make([]todos.Anchor, 0, len(matches))
	for _, m := range matches {
		anchors = append(anchors, todos.Anchor{Construct: "compile define " + m + " bakes a frozen stamp"})
	}
	return anchors, map[string]any{"target": t.Name, "defines": matches}
}

// splitDefine parses a Bazel define entry `NAME=VALUE` (or `NAME` with no
// value) into its name and unquoted value. Returns ok=false for a value-less
// define (nothing to match a stamp against).
func splitDefine(def string) (name, value string, ok bool) {
	eq := strings.IndexByte(def, '=')
	if eq < 0 {
		return "", "", false
	}
	name = def[:eq]
	value = strings.TrimSpace(def[eq+1:])
	// Strip one layer of matching surrounding quotes (`NAME="v"` / `NAME='v'`).
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return name, value, value != ""
}

// dedupeSorted drops adjacent duplicates from a sorted slice.
func dedupeSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
