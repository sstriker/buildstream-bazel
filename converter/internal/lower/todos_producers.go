package lower

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// emitCMakePTestTodos records the add_test registrations that produced
// no cc_test as structured conversion-todos — one per COMMAND runner,
// the same grouping the stderr breadcrumb uses (brotli's 28 cmake -P
// tests collapse to one todo with 28 anchors). The canonical
// no-mechanical-form case: the runner is a cmake script whose Bazel
// form is an sh_test / diff_test driving the built CLI, reachable only
// by re-authoring the harness.
//
// No-op on a nil collector or absent test registry.
func emitCMakePTestTodos(c *todos.Collector, reg *ctest.Registry, emittedTests []ir.Target) {
	if c == nil || reg == nil {
		return
	}
	emitted := map[string]bool{}
	for _, t := range emittedTests {
		emitted[t.Name] = true
	}
	byCmd := map[string][]ctest.Test{}
	for _, tst := range reg.All() {
		if emitted[tst.Name] {
			continue
		}
		byCmd[tst.Target] = append(byCmd[tst.Target], tst)
	}
	for cmd, tests := range byCmd {
		sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
		anchors := make([]todos.Anchor, 0, len(tests))
		names := make([]string, 0, len(tests))
		invocations := make([][]string, 0, len(tests))
		for _, tst := range tests {
			anchors = append(anchors, todos.Anchor{Construct: addTestConstruct(tst)})
			names = append(names, tst.Name)
			invocations = append(invocations, append([]string{}, tst.Args...))
		}
		label := cmd
		if label == "" {
			label = "(unknown)"
		}
		evidence := map[string]any{
			"runner":      cmd,
			"tests":       names,
			"invocations": invocations,
		}
		c.Add(todos.Todo{
			Kind:     "cmake-p-test",
			GroupKey: label,
			Anchors:  anchors,
			Evidence: evidence,
			SuggestedShape: "one reusable macro wrapping a bazel_skylib diff_test / sh_test over " +
				"the built artifact, instantiated once per input",
			Prompt: fmt.Sprintf("Author the %d add_test(COMMAND %s …) registration(s) as plain "+
				"idiomatic Bazel: a native test rule driving the built artifact, not a wrapper "+
				"re-invoking the cmake runner. Group the shared contract into one macro and "+
				"instantiate it per invocation.", len(tests), label),
		})
	}
}

// addTestConstruct reconstructs a representative add_test(...) call for
// the todo anchor's construct text. The ctest registry doesn't carry a
// backtrace, so anchors are synthesized (file/line left empty) — the
// construct text is the locating signal.
func addTestConstruct(t ctest.Test) string {
	var b strings.Builder
	b.WriteString("add_test(NAME ")
	b.WriteString(t.Name)
	b.WriteString(" COMMAND ")
	if t.Target != "" {
		b.WriteString(t.Target)
	}
	for _, a := range t.Args {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	b.WriteString(")")
	return b.String()
}

// emitInternalDropTodos records the cmake command edges the
// standalone-genrule pass dropped (install / uninstall / cpack /
// create_symlink / …) as structured conversion-todos — one per drop
// kind, mirroring the stderr breadcrumb's grouping. These have no Bazel
// analogue mechanically; an author decides whether the project needs an
// equivalent (a pkg_tar, an alias rule, …) on the Bazel side.
//
// No-op on a nil collector or empty drop set.
func emitInternalDropTodos(c *todos.Collector, filtered map[string]string) {
	if c == nil || len(filtered) == 0 {
		return
	}
	byKind := map[string][]string{}
	for out, kind := range filtered {
		byKind[kind] = append(byKind[kind], out)
	}
	for kind, outs := range byKind {
		sort.Strings(outs)
		anchors := make([]todos.Anchor, 0, len(outs))
		for _, out := range outs {
			anchors = append(anchors, todos.Anchor{Construct: kind + ": " + out})
		}
		c.Add(todos.Todo{
			Kind:     "cmake-internal-drop",
			GroupKey: kind,
			Anchors:  anchors,
			Evidence: map[string]any{
				"drop_kind": kind,
				"outputs":   outs,
			},
			SuggestedShape: "no Bazel analogue — decide whether the project needs an equivalent " +
				"(e.g. pkg_tar / pkg_files for packaging, an alias for a symlink) or whether " +
				"the drop is correct",
			Prompt: fmt.Sprintf("The converter dropped %d cmake %q command edge(s) with no Bazel "+
				"analogue. Confirm the drop is acceptable, or author the idiomatic Bazel "+
				"equivalent for the ones the downstream build still needs.", len(outs), kind),
		})
	}
}

// emitInstallScriptTodos records install(SCRIPT) / install(CODE)
// directives as structured conversion-todos — one per (site,
// scriptFile), mirroring the stderr surface in install_script_surface.go.
// These run cmake script code at install time with no Bazel analogue;
// the Bazel form is operator-side (rules_pkg or hand-authored).
//
// No-op on a nil collector or absent reply.
func emitInstallScriptTodos(c *todos.Collector, r *fileapi.Reply) {
	if c == nil || r == nil {
		return
	}
	type unit struct {
		kind       string // "install-script" | "install-code"
		scriptFile string
		site       string
	}
	var units []unit
	for _, dir := range r.Directories {
		for _, inst := range dir.Installers {
			switch inst.Type {
			case "script":
				units = append(units, unit{kind: "install-script", scriptFile: inst.ScriptFile, site: installerSite(dir, inst.Backtrace)})
			case "code":
				units = append(units, unit{kind: "install-code", site: installerSite(dir, inst.Backtrace)})
			}
		}
	}
	for _, u := range units {
		groupKey := u.site
		if u.scriptFile != "" {
			groupKey += "|" + u.scriptFile
		}
		if groupKey == "" {
			// Inherently-ungrouped with no recoverable site: fall back to
			// the construct's own identity so the group key is non-empty
			// and stable.
			groupKey = u.kind
		}
		construct := "install(SCRIPT" + scriptSuffix(u.scriptFile) + ")"
		if u.kind == "install-code" {
			construct = "install(CODE)"
		}
		anchor := todos.Anchor{Construct: construct}
		if u.site != "" {
			if file, line := splitSite(u.site); file != "" {
				anchor.File = file
				anchor.Line = line
			}
		}
		evidence := map[string]any{}
		if u.scriptFile != "" {
			evidence["script_file"] = u.scriptFile
		}
		if u.site != "" {
			evidence["site"] = u.site
		}
		c.Add(todos.Todo{
			Kind:     u.kind,
			GroupKey: groupKey,
			Anchors:  []todos.Anchor{anchor},
			Evidence: evidence,
			SuggestedShape: "no Bazel analogue for install-time cmake script execution; " +
				"re-express the install-time effect operator-side (rules_pkg) or drop it",
			Prompt: "Re-express this install-time cmake " + strings.TrimPrefix(u.kind, "install-") +
				" as plain Bazel (e.g. rules_pkg) if the downstream packaging needs it, " +
				"or confirm the drop is acceptable.",
		})
	}
}

// splitSite parses an "file:line" install site back into its parts.
// Returns the file and 0 line when no ":line" suffix is present.
func splitSite(site string) (string, int) {
	idx := strings.LastIndex(site, ":")
	if idx < 0 {
		return site, 0
	}
	line, err := strconv.Atoi(site[idx+1:])
	if err != nil {
		return site, 0
	}
	return site[:idx], line
}
