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
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
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
func emitCMakePTestTodos(c *todos.Collector, reg *ctest.Registry, emittedTests []ir.Target, sourceRoot, buildDir string) {
	if c == nil || reg == nil {
		return
	}
	// Normalize the roots once: filepath.Clean strips any trailing
	// separator (common in operator-supplied paths) so the prefix match +
	// token slicing below can't miss or emit a malformed "<BUILD>x" token.
	// Guard empty — filepath.Clean("") is ".", which must not become a root.
	if sourceRoot != "" {
		sourceRoot = filepath.Clean(sourceRoot)
	}
	if buildDir != "" {
		buildDir = filepath.Clean(buildDir)
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
		key := cmakePTestGroupKey(tst.Target, script, sourceRoot)
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
			// Normalize recorded paths so the report doesn't leak (and churn
			// on) the converter's transient build-dir — cmake bakes the
			// $<TARGET_FILE:t> resolution as an absolute build-dir path into
			// add_test args. <BUILD>/<SRC> tokens keep the report
			// byte-identical across independent configures and portable.
			nargs := normalizeReportPaths(tst.Args, sourceRoot, buildDir)
			anchors = append(anchors, todos.Anchor{Construct: addTestConstruct(tst, nargs)})
			names = append(names, tst.Name)
			invocations = append(invocations, nargs)
			if tst.Target != "" {
				commands[tst.Target] = true
			}
		}
		evidence := map[string]any{
			"tests":       names,
			"invocations": invocations,
		}
		if g.script != "" {
			evidence["script"] = normalizeReportPath(g.script, sourceRoot, buildDir)
		}
		if len(commands) > 0 {
			cmds := sliceutil.SortedKeys(commands)
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
			Kind:        "cmake-p-test",
			Disposition: todos.Actionable,
			GroupKey:    key,
			Anchors:     anchors,
			Evidence:    evidence,
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

// cmakePTestGroupKey derives the shared-unit key for a cmake-p-test todo.
// For a `-P` script it is the script path made relative to the source
// root — checkout-stable (unlike the absolute path the codemodel records)
// AND directory-distinct, so two runners with the same basename in
// different dirs (tests/run.cmake vs tools/run.cmake) don't collapse to
// one id. Falls back to the basename when the script isn't under the
// source root, then to the COMMAND target, then a stable placeholder.
// Path-based, never line-based, so the id honors the line-free guarantee.
func cmakePTestGroupKey(target, script, sourceRoot string) string {
	if script != "" {
		if sourceRoot != "" {
			if rel, err := filepath.Rel(sourceRoot, script); err == nil && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
				return filepath.ToSlash(rel)
			}
		}
		return filepath.Base(script)
	}
	if target != "" {
		return target
	}
	return "(unknown)"
}

// addTestConstruct reconstructs a representative add_test(...) call for
// the todo anchor's construct text from the test's name/target and its
// already-path-normalized args. The ctest registry doesn't carry a
// backtrace, so anchors are synthesized (file/line left empty) — the
// construct text is the locating signal.
func addTestConstruct(t ctest.Test, normArgs []string) string {
	var b strings.Builder
	b.WriteString("add_test(NAME ")
	b.WriteString(t.Name)
	b.WriteString(" COMMAND ")
	if t.Target != "" {
		b.WriteString(t.Target)
	}
	for _, a := range normArgs {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	b.WriteString(")")
	return b.String()
}

// normalizeReportPaths applies normalizeReportPath to each arg.
func normalizeReportPaths(args []string, sourceRoot, buildDir string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = normalizeReportPath(a, sourceRoot, buildDir)
	}
	return out
}

// normalizeReportPath rewrites an absolute path that falls under the
// converter's build dir or source root to a stable "<BUILD>/…" / "<SRC>/…"
// token, so recorded args don't leak the transient build-dir (which would
// break byte-identical-across-configures determinism) or a machine-
// specific source path. Handles both a bare path arg and the value of a
// "-DKEY=<path>" arg. Build dir is checked first since it may itself sit
// under the source root. A non-path arg is returned unchanged.
func normalizeReportPath(arg, sourceRoot, buildDir string) string {
	key, val, hasEq := strings.Cut(arg, "=")
	target := arg
	if hasEq {
		target = val
	}
	norm := target
	if buildDir != "" && pathHasPrefix(target, buildDir) {
		norm = "<BUILD>" + filepath.ToSlash(target[len(buildDir):])
	} else if sourceRoot != "" && pathHasPrefix(target, sourceRoot) {
		norm = "<SRC>" + filepath.ToSlash(target[len(sourceRoot):])
	}
	if norm == target {
		return arg
	}
	if hasEq {
		return key + "=" + norm
	}
	return norm
}

// pathHasPrefix reports whether p is prefix itself or lies under it
// (prefix + path separator), avoiding false matches like "/a/bc" under
// "/a/b".
func pathHasPrefix(p, prefix string) bool {
	if p == prefix {
		return true
	}
	return strings.HasPrefix(p, prefix+string(filepath.Separator))
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
			Kind:        "cmake-internal-drop",
			Disposition: todos.Informational,
			GroupKey:    kind,
			Anchors:     anchors,
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
	// Traverse directories in sorted key order, not map-iteration order, so
	// anchor INSERTION order is deterministic. Report sorts anchors by
	// (file, line, construct), which already disambiguates anchors that
	// differ; this guarantees a stable order for anchors that fold under
	// one todo and compare EQUAL (e.g. several siteless install(CODE)),
	// without relying on the incidental "every Anchor field is in the sort
	// key" invariant.
	dirKeys := make([]string, 0, len(r.Directories))
	for k := range r.Directories {
		dirKeys = append(dirKeys, k)
	}
	sort.Strings(dirKeys)
	for _, dk := range dirKeys {
		dir := r.Directories[dk]
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
			Kind:        f.kind,
			Disposition: todos.Actionable,
			GroupKey:    f.groupKey,
			Anchors:     f.anchors,
			Evidence:    evidence,
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
// install(CODE) todo: the (file, scriptFile) pair when available, else
// the kind so siteless directives fold together deterministically rather
// than colliding on identical (kind, group_key) ids. The site's LINE is
// deliberately dropped — ids are hash(kind, group_key) and must stay
// line-free, so an edit that shifts a line can't churn the id (the line
// survives on the anchor, which is informational). Multiple directives in
// the same file then fold into one todo with multiple anchors.
func installScriptGroupKey(kind, scriptFile, site string) string {
	file, _ := splitSite(site)
	switch {
	case file != "" && scriptFile != "":
		return file + "|" + scriptFile
	case file != "":
		return file
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
