package lower

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
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
	"cmake-codegen-nested-cmake-bake":                 "nested cmake build's configure-generated header baked at convert time",
}

// bakedEntry is one (target, reason) row in the inventory.
type bakedEntry struct {
	name, reason string
}

// collectBakedEntries walks pkg.Targets and returns the deduped,
// sorted list of (target, reason) entries for every rule carrying a
// convertTimeBakedShapes tag. Both the warn-path and the reject-path
// consume the same list — warn writes it to a sink, reject embeds it
// in an error.
func collectBakedEntries(pkg *ir.Package) []bakedEntry {
	if pkg == nil {
		return nil
	}
	seen := map[string]bool{}
	var entries []bakedEntry
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
			entries = append(entries, bakedEntry{t.Name, reason})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].reason < entries[j].reason
	})
	return entries
}

// formatBakedInventory renders the entries as the human-readable
// listing both the warning text and the rejection error reuse.
// Leading-line phrasing is tuned for the warn case; reject prepends
// its own framing.
func formatBakedInventory(entries []bakedEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d convert-time-baked output(s) — these don't auto-refresh when upstream inputs change; re-run convert-element-cmake to update:\n",
		len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "  - %s: %s\n", e.name, e.reason)
	}
	return b.String()
}

// applyBakeInPolicy is the single entry point called from ToIR after
// every emit-time tagging is done. It enforces the policy:
//
//   - BakeInAllow:  no-op (operator opted into silent baking).
//   - BakeInWarn:   writes the inventory to sink (today's behaviour).
//   - BakeInReject: returns an error embedding the inventory so the
//     converter exits non-zero. The same inventory is also written
//     to sink for visibility, since CLI consumers typically dump
//     stderr alongside the exit code.
//
// Empty policy (the zero value) resolves to warn via
// convmode.ParseBakeIn so callers leaving the field zero-valued get
// today's behavior. Nil sink suppresses the warn-path emission
// (preserves the lower-as-pure-function shape every existing test
// depends on); reject still returns the error.
func applyBakeInPolicy(pkg *ir.Package, sink io.Writer, policy convmode.BakeIn) error {
	resolved, err := convmode.ParseBakeIn(string(policy))
	if err != nil {
		return err
	}
	if resolved == convmode.BakeInAllow {
		return nil
	}
	entries := collectBakedEntries(pkg)
	if len(entries) == 0 {
		return nil
	}
	if sink != nil {
		_, _ = io.WriteString(sink, "lower: "+formatBakedInventory(entries))
	}
	if resolved == convmode.BakeInReject {
		return fmt.Errorf("--bake-in=reject refusing: %s", formatBakedInventory(entries))
	}
	return nil
}
