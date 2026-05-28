package lower

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// convertTimeBakedShapes identifies the cmake-codegen-* tags that
// signal "this rule's output bytes were materialized at convert
// time" — meaning Bazel won't re-run the upstream cmake-side
// computation when its inputs change. Operators have to re-run
// convert-element-cmake to refresh.
//
// The shapes covered:
//
//   - `cmake-codegen-lifted` — file(GENERATE) (b) base64-of-
//     rendered-bytes shape AND configure_file lift's legacy
//     bytes-embedded form. Both bake cmake's rendered output
//     into the genrule cmd.
//   - `cmake-codegen-execute-process` — captured stamp / probe
//     values from execute_process calls (hoisted to convert
//     time and embedded as a static genrule output).
//   - `cmake-codegen-execute-process-hoisted` — explicit subset
//     of the above that surfaces the hoist shape.
//   - `cmake-codegen-file-generate` (without `-evaluated` or
//     `-lifted`) — file(GENERATE) calls whose rendered bytes
//     are baked into the cmd because the genex evaluator
//     declined.
//   - `cmake-codegen-cmake-script-lift` — cmake -P lift via
//     operator-staged runner. Inputs reach Bazel correctly via
//     the genrule srcs, but the script-internal hardcoded paths
//     issue means the rendered output may not match a fresh
//     re-run; warn so operators verify.
//
// The tag-driven shape keeps the warning loose: any rule
// carrying one of these tags counts as a baked output. Adding
// a new bake-shape only requires adding the tag.
var convertTimeBakedShapes = map[string]string{
	"cmake-codegen-lifted":                            "rendered bytes baked into genrule cmd at convert time",
	"cmake-codegen-execute-process":                   "execute_process value captured at convert time",
	"cmake-codegen-execute-process-hoisted":           "execute_process result hoisted to a static convert-time output",
	"cmake-codegen-file-generate":                     "file(GENERATE) rendered bytes baked at convert time (genex evaluator declined)",
	"cmake-codegen-execute-process-op=configure_file": "configure_file shape lifted at convert time",
	"cmake-codegen-cmake-script-lift":                 "cmake -P script lifted via operator-staged runner (script-internal paths must survive the sandbox)",
	"cmake-codegen-autoinit-bake":                     "VTK-shape AUTOINIT_INCLUDE header bytes baked at convert time",
}

// warnConvertTimeBaking walks the package's targets and emits
// one aggregated warning per kind of convert-time-baked output
// to opts.Warnings (typically os.Stderr). Operators see at
// convert time which rules carry bytes that won't auto-refresh
// when upstream inputs change.
//
// The warning is informational, not blocking. Operators who
// understand the trade-off can ignore it; first-time conversions
// of large projects benefit from seeing the inventory.
//
// Nil sink suppresses the message (the lower-as-pure-function
// shape every existing test depends on); non-nil emits one
// "convert-time-baked outputs: N rules" header line plus a
// sorted "name (reason)" entry per rule.
func warnConvertTimeBaking(pkg *ir.Package, sink io.Writer) {
	if pkg == nil || sink == nil {
		return
	}
	type entry struct {
		name, reason string
	}
	var entries []entry
	seen := map[string]bool{}
	for _, t := range pkg.Targets {
		for _, tag := range t.Tags {
			reason, ok := convertTimeBakedShapes[tag]
			if !ok {
				continue
			}
			key := t.Name + "\x00" + reason
			if seen[key] {
				continue
			}
			seen[key] = true
			entries = append(entries, entry{t.Name, reason})
		}
	}
	if len(entries) == 0 {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].reason < entries[j].reason
	})
	var b strings.Builder
	fmt.Fprintf(&b, "lower: %d convert-time-baked output(s) — these don't auto-refresh when upstream inputs change; re-run convert-element-cmake to update:\n",
		len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "  - %s: %s\n", e.name, e.reason)
	}
	_, _ = io.WriteString(sink, b.String())
}
