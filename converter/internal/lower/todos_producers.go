package lower

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// emitCMakePTestTodos records the add_test registrations that produced
// no cc_test as structured conversion-todos, grouped by the SHARED UNIT
// an author re-authors once. For the canonical `cmake -P <script>`
// harness that unit is the SCRIPT (brotli's N roundtrip tests share one
// run_test.cmake → one todo, N anchors): two different `-P` scripts have
// two different contracts, so they must be separate todos — keying on the
// COMMAND basename alone (always "cmake" for the -P shape) would collapse
// them and make the `id` too coarse for the post-pass. For any other
// unconverted COMMAND the unit is the executable target.
//
// The Bazel form is an sh_test / diff_test driving the built CLI,
// reachable only by re-authoring the harness.
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
	type group struct {
		tests  []ctest.Test
		script string // the -P script (as recorded), "" when not a cmake -P runner
	}
	byUnit := map[string]*group{}
	var order []string
	for _, tst := range reg.All() {
		if emitted[tst.Name] {
			continue
		}
		script := cmakeScriptArg(tst.Args)
		key := cmakePTestGroupKey(tst.Target, script)
		g := byUnit[key]
		if g == nil {
			g = &group{script: script}
			byUnit[key] = g
			order = append(order, key)
		}
		g.tests = append(g.tests, tst)
	}
	sort.Strings(order)
	for _, key := range order {
		g := byUnit[key]
		tests := g.tests
		sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
		anchors := make([]todos.Anchor, 0, len(tests))
		names := make([]string, 0, len(tests))
		invocations := make([][]string, 0, len(tests))
		commands := map[string]bool{}
		for _, tst := range tests {
			anchors = append(anchors, todos.Anchor{Construct: addTestConstruct(tst)})
			names = append(names, tst.Name)
			invocations = append(invocations, append([]string{}, tst.Args...))
			if tst.Target != "" {
				commands[tst.Target] = true
			}
		}
		evidence := map[string]any{
			"tests":       names,
			"invocations": invocations,
		}
		if g.script != "" {
			evidence["script"] = g.script
		}
		if len(commands) > 0 {
			cmds := make([]string, 0, len(commands))
			for cmd := range commands {
				cmds = append(cmds, cmd)
			}
			sort.Strings(cmds)
			evidence["command"] = cmds
		}
		var prompt string
		if g.script != "" {
			prompt = fmt.Sprintf("Author the %d add_test(COMMAND cmake -P %s …) registration(s) as "+
				"plain idiomatic Bazel: a native test rule driving the built artifact, not a "+
				"wrapper re-invoking the cmake script %s. Group the shared contract into one macro "+
				"and instantiate it per invocation.", len(tests), key, key)
		} else {
			prompt = fmt.Sprintf("Author the %d add_test(COMMAND %s …) registration(s) as plain "+
				"idiomatic Bazel: a native test rule driving the built artifact. The COMMAND "+
				"executable wasn't converted, so re-express the test (and the missing target if "+
				"needed). Group the shared contract into one macro and instantiate it per "+
				"invocation.", len(tests), key)
		}
		c.Add(todos.Todo{
			Kind:     "cmake-p-test",
			GroupKey: key,
			Anchors:  anchors,
			Evidence: evidence,
			SuggestedShape: "one reusable macro wrapping a bazel_skylib diff_test / sh_test over " +
				"the built artifact, instantiated once per input",
			Prompt: prompt,
		})
	}
}

// cmakeScriptArg returns the `<script>` of a `-P <script>` cmake
// invocation in args, or "" when no `-P` is present (not a cmake -P
// runner). cmake's script-mode flag takes the next token as the file.
func cmakeScriptArg(args []string) string {
	for i, a := range args {
		if a == "-P" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// cmakePTestGroupKey derives the shared-unit key for a cmake-p-test todo:
// the basename of the `-P` script when present (stable across machines,
// unlike the absolute path the codemodel records, and distinct per
// script), else the COMMAND target, else a stable placeholder.
func cmakePTestGroupKey(target, script string) string {
	if script != "" {
		return filepath.Base(script)
	}
	if target != "" {
		return target
	}
	return "(unknown)"
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
// directives as structured conversion-todos, folded by (kind,
// group_key) so the `id` (= hash(kind, group_key)) stays UNIQUE per todo
// and the report stays deterministic. The grouping key is the
// (site, scriptFile) pair — normally unique per directive — but degrades
// to the kind when neither a backtrace site nor a scriptFile is
// recoverable (older cmake replies' siteless install(CODE)); multiple
// such directives must therefore fold into ONE todo with N anchors
// rather than emit N todos that collide on (kind, group_key).
//
// These run cmake script code at install time with no Bazel analogue;
// the Bazel form is operator-side (rules_pkg or hand-authored). The
// File API exposes the scriptFile path but not the install(CODE) body,
// so the post-pass reads the script from the source tree to judge
// drop-vs-re-express.
//
// No-op on a nil collector or absent reply.
func emitInstallScriptTodos(c *todos.Collector, r *fileapi.Reply) {
	if c == nil || r == nil {
		return
	}
	type folded struct {
		kind        string
		groupKey    string
		anchors     []todos.Anchor
		scriptFiles map[string]bool
	}
	byKey := map[string]*folded{}
	var order []string
	add := func(kind, scriptFile, site string) {
		groupKey := installScriptGroupKey(kind, scriptFile, site)
		mapKey := kind + "\x00" + groupKey
		f := byKey[mapKey]
		if f == nil {
			f = &folded{kind: kind, groupKey: groupKey, scriptFiles: map[string]bool{}}
			byKey[mapKey] = f
			order = append(order, mapKey)
		}
		construct := "install(SCRIPT" + scriptSuffix(scriptFile) + ")"
		if kind == "install-code" {
			construct = "install(CODE)"
		}
		anchor := todos.Anchor{Construct: construct}
		if file, line := splitSite(site); file != "" {
			anchor.File = file
			anchor.Line = line
		}
		f.anchors = append(f.anchors, anchor)
		if scriptFile != "" {
			f.scriptFiles[scriptFile] = true
		}
	}
	for _, dir := range r.Directories {
		for _, inst := range dir.Installers {
			switch inst.Type {
			case "script":
				add("install-script", inst.ScriptFile, installerSite(dir, inst.Backtrace))
			case "code":
				add("install-code", "", installerSite(dir, inst.Backtrace))
			}
		}
	}
	sort.Strings(order)
	for _, mapKey := range order {
		f := byKey[mapKey]
		evidence := map[string]any{}
		if len(f.scriptFiles) > 0 {
			scripts := make([]string, 0, len(f.scriptFiles))
			for s := range f.scriptFiles {
				scripts = append(scripts, s)
			}
			sort.Strings(scripts)
			if len(scripts) == 1 {
				evidence["script_file"] = scripts[0]
			} else {
				evidence["script_files"] = scripts
			}
		}
		c.Add(todos.Todo{
			Kind:     f.kind,
			GroupKey: f.groupKey,
			Anchors:  f.anchors,
			Evidence: evidence,
			SuggestedShape: "no Bazel analogue for install-time cmake script execution; " +
				"re-express the install-time effect operator-side (rules_pkg) or drop it",
			Prompt: fmt.Sprintf("Re-express %d install-time cmake %s directive(s) as plain Bazel "+
				"(e.g. rules_pkg) if the downstream packaging needs it, or confirm the drop is "+
				"acceptable. The cmake body isn't in the codemodel — read the source at the "+
				"anchor site(s) to judge.", len(f.anchors), strings.TrimPrefix(f.kind, "install-")),
		})
	}
}

// installScriptGroupKey derives the fold key for an install(SCRIPT) /
// install(CODE) todo: the (site, scriptFile) pair when available, else
// the kind so siteless directives fold together deterministically rather
// than colliding on identical (kind, group_key) ids.
func installScriptGroupKey(kind, scriptFile, site string) string {
	switch {
	case site != "" && scriptFile != "":
		return site + "|" + scriptFile
	case site != "":
		return site
	case scriptFile != "":
		return scriptFile
	default:
		return kind
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
