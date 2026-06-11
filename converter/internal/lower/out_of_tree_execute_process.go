package lower

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Out-of-tree execute_process surfacing.
//
// The lift/refusal machinery (recoverExecuteProcess) only sees
// execute_process calls whose issuing CMakeLists lives INSIDE the source
// tree — shadow's inSourceTree filter drops everything else at extraction so
// cmake's own try_compile / compiler-id probes (which run dozens of
// execute_process calls from /usr/share/cmake-* modules and build-dir scratch)
// don't flood the converter with refusals for calls the project never wrote.
//
// That filter is correct for the lift, but it was ALSO silent for the calls
// that genuinely are project intent yet happen to issue from outside the
// source tree:
//
//   - A configure-time cmake SUBPROJECT whose CMakeLists was generated into
//     the build dir (a superbuild / ExternalProject-at-configure shape). The
//     codemodel lists that subproject's sources anchored under the build dir,
//     so the codemodel itself confirms the call backs a real target — a
//     strong signal the drop is lossy.
//   - A find_package config file in the synthesized prefix tree that runs a
//     probe when the package is resolved.
//
// Per the no-silent-drops contract: a silent skip is fine when we are 100%
// confident of the outcome (cmake's internal probes), but a skip we take
// because we don't know what to do must be accounted for. partitionOutOfTreeExec
// splits the calls three ways:
//
//   - confident noise → dropped silently;
//   - codemodel-source-backed build-dir subproject → LIFTED through the same
//     recoverExecuteProcess machinery as an in-source-tree call (the codemodel
//     confirms a real target backs it, so it gets a genrule/probe lift — or a
//     loud per-call execute-process-refusal — rather than a vague note);
//   - everything else uncertain (build-dir without codemodel sources, a
//     find_package prefix-tree probe) → NOTED as a conversion-todo.

// outOfTreeExecSignal records WHY an out-of-tree execute_process call was
// surfaced — the signal feeds the todo's grouping and prompt so an author
// can tell a codemodel-backed sub-build (the strong case) from a weaker
// build-dir / prefix-tree probe.
type outOfTreeExecSignal string

const (
	// signalBuildSubproject: the call issues from a build-dir location and
	// the codemodel lists sources under that same location — a configure-time
	// cmake subproject the lift didn't reproduce. The strongest signal.
	signalBuildSubproject outOfTreeExecSignal = "codemodel-build-subproject"
	// signalBuildDirOther: the call issues from a build-dir location with NO
	// codemodel sources beneath it — a configure-time call we can't tie to a
	// target, but not cmake's own try_compile scratch either.
	signalBuildDirOther outOfTreeExecSignal = "build-dir-no-codemodel"
	// signalPrefixTree: the call issues from the synthesized find_package
	// prefix tree (a package config file's probe).
	signalPrefixTree outOfTreeExecSignal = "find-package-prefix-tree"
)

// outOfTreeExecNote is one out-of-tree execute_process call the converter
// decided to surface rather than silently drop.
type outOfTreeExecNote struct {
	File   string // recorded-path issuing CMakeLists
	Line   int
	Argv   []string // the first COMMAND clause's argv (for the anchor)
	Signal outOfTreeExecSignal
}

// partitionOutOfTreeExec splits shadow's out-of-tree execute_process calls
// into the ones to LIFT (codemodel-source-backed build-dir subprojects —
// routed through recoverExecuteProcess exactly like in-source-tree calls) and
// the ones to NOTE (uncertain: a build-dir location with no codemodel sources
// beneath it, or a find_package prefix-tree probe). The confident noise
// (cmake's try_compile scratch under <build>/CMakeFiles and bundled-module
// probes that issue from neither the build dir nor the prefix tree) is dropped
// silently — neither returned. recordedBuildDir / prefixDir are in the SAME
// path space as call.File (the trace's recorded paths); consumedBuildRel is
// the codemodel's build-relative source set (cc.ConsumedBuildRel).
func partitionOutOfTreeExec(calls []shadow.ExecuteProcessCall, recordedBuildDir, prefixDir string, consumedBuildRel map[string]bool) (lift []shadow.ExecuteProcessCall, note []outOfTreeExecNote) {
	for _, c := range calls {
		if len(c.Commands) == 0 {
			continue
		}
		sig, ok := classifyOneOutOfTreeExec(c.File, recordedBuildDir, prefixDir, consumedBuildRel)
		if !ok {
			continue // confident noise — the skip is correct, stay silent
		}
		if sig == signalBuildSubproject {
			// Strong signal: the codemodel lists this subproject's sources, so
			// the call backs real targets. Lift it like an in-tree call.
			lift = append(lift, c)
			continue
		}
		note = append(note, outOfTreeExecNote{File: c.File, Line: c.Line, Argv: c.Commands[0], Signal: sig})
	}
	return lift, note
}

// classifyOneOutOfTreeExec maps one out-of-tree issuing file to a surfacing
// signal, or (_, false) when the call is confident noise. See the rule table
// in the package doc comment above.
func classifyOneOutOfTreeExec(file, recordedBuildDir, prefixDir string, consumedBuildRel map[string]bool) (outOfTreeExecSignal, bool) {
	if recordedBuildDir != "" {
		if rel, ok := relativeIfInsideRelaxed(recordedBuildDir, file); ok {
			if pathHasSegment(rel, "CMakeFiles") || pathHasSegment(rel, "CMakeScratch") {
				// try_compile / compiler-id scratch — cmake's own probes,
				// never project intent. 100% confident: silent.
				return "", false
			}
			if buildSubtreeHasConsumedSources(slashDir(rel), consumedBuildRel) {
				return signalBuildSubproject, true
			}
			return signalBuildDirOther, true
		}
	}
	if prefixDir != "" {
		if _, ok := relativeIfInsideRelaxed(prefixDir, file); ok {
			return signalPrefixTree, true
		}
	}
	// Neither the build dir nor the prefix tree — a bundled cmake module
	// (/usr/share/cmake-*) or other system path. Confident noise: silent.
	return "", false
}

// pathHasSegment reports whether the slash-form relative path rel contains
// seg as a whole path component (so "CMakeFiles" matches "CMakeFiles/x" and
// "a/CMakeFiles/b" but not "MyCMakeFilesDir").
func pathHasSegment(rel, seg string) bool {
	for _, c := range strings.Split(rel, "/") {
		if c == seg {
			return true
		}
	}
	return false
}

// slashDir returns the directory portion of a slash-form path, or "" when the
// path has no separator (a bare filename, e.g. a CMakeLists at the build root).
func slashDir(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

// buildSubtreeHasConsumedSources reports whether any codemodel-consumed
// build-relative source lives under dir (the build-relative directory of the
// issuing CMakeLists). An empty dir (issuing file at the build root) never
// matches — the codemodel sources of a real subproject live under its own
// build-dir subtree, not at the root.
func buildSubtreeHasConsumedSources(dir string, consumedBuildRel map[string]bool) bool {
	if dir == "" {
		return false
	}
	prefix := dir + "/"
	for rel := range consumedBuildRel {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// warnOutOfTreeExecuteProcess surfaces the uncertain out-of-tree
// execute_process calls partitionOutOfTreeExec stashed on cc (the ones NOT
// routed into the lift) — a loud stderr breadcrumb plus structured
// conversion-todos. No-op when there's nothing uncertain to note.
func warnOutOfTreeExecuteProcess(opts Options, cc *codegenContext) {
	notes := cc.OutOfTreeExecNotes
	if len(notes) == 0 {
		return
	}
	if opts.Warnings != nil {
		fmt.Fprintf(opts.Warnings,
			"lower: %d execute_process call(s) issued from outside the source tree with no codemodel sources to anchor a lift (a build-dir location or find_package prefix-tree probe) — surfaced as conversion-todos rather than dropped silently\n",
			len(notes))
	}
	emitOutOfTreeExecuteProcessTodos(opts.Todos, notes, opts.HostSourceRoot, opts.BuildDir, opts.HostPrefixDir)
}

// emitOutOfTreeExecuteProcessTodos mirrors the uncertain out-of-tree
// execute_process calls into structured conversion-todos, one per signal
// (the unit an author re-works together — all the codemodel-subproject calls,
// all the prefix-tree probes). Anchors dedupe identical (file, line, argv)
// triples; every path passes the report-path normalization so the build dir
// (a per-run mktemp path) never leaks and the byte-identical-report contract
// holds.
func emitOutOfTreeExecuteProcessTodos(c *todos.Collector, notes []outOfTreeExecNote, sourceRoot, buildDir, prefixDir string) {
	if c == nil || len(notes) == 0 {
		return
	}
	bySignal := map[outOfTreeExecSignal][]outOfTreeExecNote{}
	for _, n := range notes {
		bySignal[n.Signal] = append(bySignal[n.Signal], n)
	}
	signals := make([]outOfTreeExecSignal, 0, len(bySignal))
	for s := range bySignal {
		signals = append(signals, s)
	}
	sort.Slice(signals, func(i, j int) bool { return signals[i] < signals[j] })
	for _, sig := range signals {
		ns := bySignal[sig]
		var anchors []todos.Anchor
		seenAnchor := map[string]bool{}
		var invocations []string
		seenArgv := map[string]bool{}
		for _, n := range ns {
			file := normalizeOOTReportPath(n.File, sourceRoot, buildDir, prefixDir)
			argv := strings.Join(normalizeOOTReportPaths(n.Argv, sourceRoot, buildDir, prefixDir), " ")
			ak := file + "\x00" + strconv.Itoa(n.Line) + "\x00" + argv
			if seenAnchor[ak] {
				continue
			}
			seenAnchor[ak] = true
			if !seenArgv[argv] {
				seenArgv[argv] = true
				invocations = append(invocations, argv)
			}
			anchors = append(anchors, todos.Anchor{
				File:      file,
				Line:      n.Line,
				Construct: "execute_process(" + argv + ")",
			})
		}
		sort.Strings(invocations)
		c.Add(todos.Todo{
			Kind:        "out-of-tree-execute-process",
			Disposition: todos.Actionable,
			GroupKey:    string(sig),
			Anchors:     anchors,
			Evidence: map[string]any{
				"signal":      string(sig),
				"invocations": invocations,
			},
			SuggestedShape: outOfTreeExecShape(sig),
			Prompt: "The converter found " + plural(len(anchors), "execute_process call") +
				" issued from outside the source tree (" + outOfTreeExecReason(sig) +
				"). These aren't lifted. Confirm each is configure-only and needs no Bazel form, " +
				"or author the idiomatic Bazel equivalent.",
		})
	}
}

// normalizeOOTReportPaths applies normalizeOOTReportPath to each arg.
func normalizeOOTReportPaths(args []string, sourceRoot, buildDir, prefixDir string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = normalizeOOTReportPath(a, sourceRoot, buildDir, prefixDir)
	}
	return out
}

// normalizeOOTReportPath is normalizeReportPath plus a <PREFIX> arm for the
// synthesized find_package prefix tree. The prefix-tree signal's anchors and
// argv live UNDER prefixDir, which normalizeReportPath (sourceRoot/buildDir
// only) doesn't rewrite — so a prefix-anchored path would land raw in the
// todo and, since the prefix is per-run staging, byte-vary the report (the
// same byte-identity break class as #580's template-error finding, and
// unpinnable by the todos-coverage gate because no fixture emits out-of-tree
// calls). The prefix is checked first; non-prefix args fall through to
// normalizeReportPath unchanged. Mirrors normalizeReportPath's "-DKEY=<path>"
// value handling.
func normalizeOOTReportPath(arg, sourceRoot, buildDir, prefixDir string) string {
	if prefixDir != "" {
		key, val, hasEq := strings.Cut(arg, "=")
		target := arg
		if hasEq {
			target = val
		}
		if pathHasPrefix(target, prefixDir) {
			norm := "<PREFIX>" + filepath.ToSlash(target[len(prefixDir):])
			if hasEq {
				return key + "=" + norm
			}
			return norm
		}
	}
	return normalizeReportPath(arg, sourceRoot, buildDir)
}

// outOfTreeExecReason renders the human-readable WHY for a signal.
func outOfTreeExecReason(sig outOfTreeExecSignal) string {
	switch sig {
	case signalBuildSubproject:
		return "a configure-time cmake subproject whose sources the codemodel lists under the build dir"
	case signalBuildDirOther:
		return "a build-dir location with no codemodel sources beneath it"
	case signalPrefixTree:
		return "a find_package config file in the prefix tree"
	default:
		return string(sig)
	}
}

// outOfTreeExecShape renders the SuggestedShape hint for a signal.
func outOfTreeExecShape(sig outOfTreeExecSignal) string {
	switch sig {
	case signalBuildSubproject:
		return "the codemodel lists this subproject's sources — convert the nested project as its own " +
			"element and wire its artifacts via the imports manifest, or reproduce the call's outputs " +
			"with a genrule/run_binary"
	case signalPrefixTree:
		return "a find_package config-file probe — usually configure-only; confirm it needs no Bazel form, " +
			"or model the value it computes (a config_setting / a stamped value) on the Bazel side"
	default:
		return "confirm the call is configure-only and needs no Bazel form, or author the idiomatic Bazel " +
			"equivalent (a genrule/run_binary for a produced artifact, a config_setting for a probe)"
	}
}
