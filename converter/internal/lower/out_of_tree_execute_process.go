package lower

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
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
//     loud per-call execute-process-refusal — rather than a vague note). The
//     codemodel signal is the STRONGEST one and takes precedence regardless of
//     where the call was issued from: a call that OPERATES on a build-dir
//     location (its WORKING_DIRECTORY, not only its issuing CMakeLists) with
//     codemodel sources beneath it is a subproject even when issued from the
//     prefix tree (a find_package config file driving a real sub-build);
//   - everything else uncertain (build-dir without codemodel sources, a
//     find_package prefix-tree probe) → NOTED as a conversion-todo.
//
// Two location-INDEPENDENT signals refine the last bucket — the codemodel-source
// check isn't the only way to know a call is real codegen:
//
//   - The RECOGNIZER signal: a registered codegen recognizer claiming the call's
//     tool (e.g. protoc --cpp_out). The codemodel often doesn't attribute an
//     out-of-tree tool's outputs as sources, dropping good signal.
//   - The PROJECT-I/O signal: the call reads an in-tree source (an argv path
//     under the source root) or touches a build-dir path (an argv path under
//     the build dir — an OUTPUT it writes, or an INPUT it reads from an upstream
//     step, e.g. a chained genrule consuming an earlier tool's build-dir
//     artifact). Whose DATA a call processes is project intent even when the
//     helper that ISSUED it lives out of tree — a find_package prefix-tree
//     config file driving a tool on the project's OWN .proto, or feeding off the
//     project's OWN build dir, is the consumer's codegen, not the dependency's.
//
// A build-dir-without-codemodel-sources call, OR any out-of-tree call carrying
// one of these signals, is the project's OWN under-attributed codegen and is
// LIFTED like a subproject (the lift corroborates the derived outputs on disk
// before emitting, so a mis-match still declines safely). The --fidelity dial
// gates it: under BEST-EFFORT such a call is lifted even WITHOUT a recognizer
// match (recoverExecuteProcess gives a genrule/probe fallback — recover
// something runnable); under STRICT it lifts only on a recognizer match, else
// stays a note (faithful-or-fail — don't silently genrule an unattributed
// call). A prefix-tree call with NO project-I/O and no recognizer hit is the
// DEPENDENCY's codegen, so it's never genrule-lifted (that would re-emit the
// dependency's rules) — it stays a NOTE, sharpened to name the tool +
// native-rule shape when recognized.

// outOfTreeExecSignal records WHY an out-of-tree execute_process call was
// surfaced — the signal feeds the todo's grouping and prompt so an author
// can tell a codemodel-backed sub-build (the strong case) from a weaker
// build-dir / prefix-tree probe.
type outOfTreeExecSignal string

const (
	// signalBuildSubproject: the call OPERATES on a build-dir location (its
	// issuing CMakeLists or its WORKING_DIRECTORY) the codemodel lists sources
	// under — a configure-time cmake subproject the lift didn't reproduce. The
	// strongest signal, taking precedence over the issuing-site signals below.
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
	// Recognized is true when a codegen recognizer claims the call's tool (e.g.
	// protoc --cpp_out). The classification is location-based, but the recognizer
	// signal is location-independent — so a prefix-tree call that's recognized
	// gets a sharper todo (name the tool + native-rule shape) even though it
	// stays a note (it's the dependency's codegen, not the consumer's to emit).
	Recognized bool
}

// partitionOutOfTreeExec splits shadow's out-of-tree execute_process calls
// into the ones to LIFT (codemodel-source-backed build-dir subprojects —
// routed through recoverExecuteProcess exactly like in-source-tree calls) and
// the ones to NOTE (uncertain: a build-dir location with no codemodel sources
// beneath it, or a find_package prefix-tree probe). The confident noise
// (cmake's try_compile scratch under <build>/CMakeFiles and bundled-module
// probes that issue from neither the build dir nor the prefix tree) is dropped
// silently — neither returned. recordedSrcDir / recordedBuildDir / prefixDir
// are in the SAME path space as call.File (the trace's recorded paths);
// consumedBuildRel is the codemodel's build-relative source set
// (cc.ConsumedBuildRel).
func partitionOutOfTreeExec(calls []shadow.ExecuteProcessCall, recordedSrcDir, recordedBuildDir, prefixDir string, consumedBuildRel map[string]bool, cc *codegenContext) (lift []shadow.ExecuteProcessCall, note []outOfTreeExecNote) {
	for _, c := range calls {
		if len(c.Commands) == 0 {
			continue
		}
		sig, ok := classifyOneOutOfTreeExec(c.File, c.WorkingDirectory, recordedBuildDir, prefixDir, consumedBuildRel)
		if !ok {
			continue // confident noise — the skip is correct, stay silent
		}
		if sig == signalBuildSubproject {
			// Strong signal: the codemodel lists this subproject's sources, so
			// the call backs real targets. Lift it like an in-tree call.
			lift = append(lift, c)
			continue
		}
		// Two location-independent signals mark a call as the project's OWN
		// codegen, whatever out-of-tree helper issued it: a registered recognizer
		// claiming the tool (protoc --cpp_out, …), or the call reading an in-tree
		// source / writing a build-dir output (projectIO).
		recognized := outOfTreeExecRecognized(c, cc)
		projectIO := outOfTreeExecTouchesProjectIO(c, recordedSrcDir, recordedBuildDir)
		if (sig == signalBuildDirOther || projectIO) && (recognized || outOfTreeBestEffort(cc)) {
			// A build-dir codegen call the codemodel didn't attribute sources to,
			// or any out-of-tree call operating on the project's own I/O.
			// RECOGNIZED → recoverExecuteProcess lifts it via the recognizer (the
			// native rule), in BOTH fidelity modes. UNRECOGNIZED under BEST-EFFORT
			// → lift it anyway so recoverExecuteProcess gives a genrule/probe
			// fallback (best-effort = recover something runnable). UNRECOGNIZED
			// under STRICT falls through to a note: faithful-or-fail, don't
			// silently genrule an unattributed out-of-tree call. The lift
			// corroborates outputs on disk before emitting, so it declines safely.
			lift = append(lift, c)
			continue
		}
		// What reaches here is the dependency's own codegen (a prefix-tree call
		// touching only the dependency's files) or a strict unrecognized call with
		// no project I/O — a note, sharpened to name the tool when recognized.
		note = append(note, outOfTreeExecNote{File: c.File, Line: c.Line, Argv: c.Commands[0], Signal: sig, Recognized: recognized})
	}
	return lift, note
}

// outOfTreeBestEffort reports whether the conversion is in best-effort fidelity
// — where an out-of-tree codegen call worth recovering is lifted (genrule
// fallback) even without a recognizer match; strict only lifts on a match.
func outOfTreeBestEffort(cc *codegenContext) bool {
	return cc != nil && cc.Fidelity == convmode.FidelityBestEffort
}

// outOfTreeExecRecognized reports whether a codegen recognizer claims an
// out-of-tree call's tool. Gated on cc.RecognizeCodegen (off → unchanged
// behavior). Keys on the same driver basename + argv the lift path uses, so the
// partition and the lift agree on what's recognizable.
func outOfTreeExecRecognized(c shadow.ExecuteProcessCall, cc *codegenContext) bool {
	if cc == nil || !cc.RecognizeCodegen || len(c.Commands) == 0 {
		return false
	}
	argv := c.Commands[0]
	if len(argv) == 0 {
		return false
	}
	// Interpreter-led generators (python gen.py, perl xxd.pl) key the recognizer
	// on the SCRIPT, not the interpreter — see codegenRecognitionDriver.
	driver, recArgs := codegenRecognitionDriver(argv)
	if driver == "" {
		return false
	}
	return codegenRecognizerMatches(cc.ExtraRecognizers, CodegenCommand{Driver: driver, Args: recArgs})
}

// outOfTreeExecTouchesProjectIO reports whether an out-of-tree call reads an
// in-tree source (an argv path under the recorded source root) or touches a
// build-dir path (an argv path under the recorded build dir — an output it
// writes, or an input it reads from an upstream step, e.g. a chained genrule
// consuming an earlier tool's build-dir artifact). Position isn't distinguished:
// any src/build path in the argv is the signal. Either is location-independent
// evidence that the call is the PROJECT's own codegen — a tool the project
// drives on its OWN files — even when the issuing helper (a find_package
// prefix-tree config file, say) lives out of tree. The issuing site says where
// the call was written; the I/O says whose data it processes.
func outOfTreeExecTouchesProjectIO(c shadow.ExecuteProcessCall, recordedSrcDir, recordedBuildDir string) bool {
	for _, argv := range c.Commands {
		for _, tok := range argv {
			p := outOfTreeExecPathToken(tok)
			if p == "" {
				continue
			}
			if recordedSrcDir != "" {
				if _, ok := buildRelAbs(recordedSrcDir, p); ok {
					return true
				}
			}
			if recordedBuildDir != "" {
				if _, ok := buildRelAbs(recordedBuildDir, p); ok {
					return true
				}
			}
		}
	}
	return false
}

// outOfTreeExecPathToken extracts the path-like value of one argv token for the
// project-I/O check: the part after the first '=' for a KEY=VALUE / -DKEY=path /
// --out=path token, else the token itself — but only when ABSOLUTE. A relative
// path can't be tied to the source/build tree without resolving the call's
// working directory, and buildRelAbs (which the check uses) guards on absolute
// paths anyway; returning "" here keeps the intent explicit.
func outOfTreeExecPathToken(tok string) string {
	cand := tok
	if _, val, hasEq := strings.Cut(tok, "="); hasEq {
		cand = val
	}
	if !filepath.IsAbs(cand) {
		return ""
	}
	return cand
}

// classifyOneOutOfTreeExec maps one out-of-tree call to a surfacing signal, or
// (_, false) when the call is confident noise. See the rule table in the
// package doc comment above. workingDir is the call's WORKING_DIRECTORY (the
// dir the subprocess runs in) — where the call OPERATES, which can differ from
// `file`, the CMakeLists it was ISSUED from.
func classifyOneOutOfTreeExec(file, workingDir, recordedBuildDir, prefixDir string, consumedBuildRel map[string]bool) (outOfTreeExecSignal, bool) {
	// (1) Confident noise keyed on the ISSUING file: cmake's own try_compile /
	// compiler-id scratch under <build>/CMakeFiles. The issuing-file location
	// is the reliable noise signal — a scratch CMakeLists is never project
	// intent, whatever it does.
	fileRel, fileUnderBuild := buildRelAbs(recordedBuildDir, file)
	if fileUnderBuild && hasScratchSegment(fileRel) {
		return "", false
	}

	// (2) Strongest signal, regardless of where the call was ISSUED from: does
	// it OPERATE on a build-dir DIRECTORY the codemodel lists sources under? A
	// find_package prefix-tree config file can drive a real sub-build whose
	// sources land in the build dir (codemodel-backed), which is a subproject
	// to LIFT, not a prefix probe to note. The two locations anchor
	// asymmetrically: the issuing FILE is a path, so its containing directory
	// (slashDir) is the operative dir; the WORKING_DIRECTORY already IS a
	// directory, so it's used as-is — applying slashDir to it would strip a
	// real component and check the PARENT subtree (missing a one-level
	// sub-build, and over-matching a sibling's sources). Scratch dirs don't
	// count (cmake's own).
	opDirs := make([]string, 0, 2)
	if fileUnderBuild && !hasScratchSegment(fileRel) {
		opDirs = append(opDirs, slashDir(fileRel))
	}
	if wdRel, ok := buildRelAbs(recordedBuildDir, workingDir); ok && !hasScratchSegment(wdRel) {
		opDirs = append(opDirs, wdRel)
	}
	for _, dir := range opDirs {
		if buildSubtreeHasConsumedSources(dir, consumedBuildRel) {
			return signalBuildSubproject, true
		}
	}

	// (3) Issued from a build-dir location but no codemodel sources beneath it.
	if fileUnderBuild {
		return signalBuildDirOther, true
	}
	// (4) Issued from the synthesized find_package prefix tree.
	if _, ok := buildRelAbs(prefixDir, file); ok {
		return signalPrefixTree, true
	}
	// (5) Neither the build dir nor the prefix tree — a bundled cmake module
	// (/usr/share/cmake-*) or other system path. Confident noise: silent.
	return "", false
}

// hasScratchSegment reports whether a build-relative path lies under cmake's
// own try_compile / compiler-id scratch (a CMakeFiles or CMakeScratch path
// component) — never project intent.
func hasScratchSegment(rel string) bool {
	return pathHasSegment(rel, "CMakeFiles") || pathHasSegment(rel, "CMakeScratch")
}

// buildRelAbs returns the slash-form path of abs relative to root when abs is
// an ABSOLUTE path inside root, else (_, false). The absolute guard matters
// for the WORKING_DIRECTORY signal: the trace records it absolute when set,
// and a relative WD must NOT be mistaken for a build-relative path (which
// relativeIfInsideRelaxed would do for any non-absolute input).
func buildRelAbs(root, abs string) (string, bool) {
	if root == "" || abs == "" || !filepath.IsAbs(abs) {
		return "", false
	}
	return relativeIfInsideRelaxed(root, abs)
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
// call's operative location — its issuing CMakeLists or its WORKING_DIRECTORY).
// An empty or "." dir (the location at the build root) never matches — the
// codemodel sources of a real subproject live under its own build-dir subtree,
// not at the root (and a WORKING_DIRECTORY equal to the build dir would
// otherwise match every project that has any source).
func buildSubtreeHasConsumedSources(dir string, consumedBuildRel map[string]bool) bool {
	if dir == "" || dir == "." {
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
		// Recognizer signal: surface the known codegen tool(s) in this group so
		// the author gets the native-rule shape, not the generic "author a Bazel
		// equivalent" prose. (Recognized build-dir codegen was already lifted, so
		// what reaches a note recognized is the dependency's prefix-tree codegen.)
		var recognizedTools []string
		seenTool := map[string]bool{}
		for _, n := range ns {
			if !n.Recognized || len(n.Argv) == 0 {
				continue
			}
			if t := executeProcessDriverBasename(n.Argv[0]); t != "" && !seenTool[t] {
				seenTool[t] = true
				recognizedTools = append(recognizedTools, t)
			}
		}
		sort.Strings(recognizedTools)
		ev := map[string]any{"signal": string(sig), "invocations": invocations}
		shape := outOfTreeExecShape(sig)
		prompt := "The converter found " + plural(len(anchors), "execute_process call") +
			" issued from outside the source tree (" + outOfTreeExecReason(sig) +
			"). These aren't lifted. Confirm each is configure-only and needs no Bazel form, " +
			"or author the idiomatic Bazel equivalent."
		if len(recognizedTools) > 0 {
			ev["recognized_tools"] = recognizedTools
			tools := strings.Join(recognizedTools, ", ")
			shape = "recognized codegen tool(s) " + tools + " — the idiomatic Bazel form is the native " +
				"rule (e.g. protoc → proto_library + cc_proto_library). If this is a DEPENDENCY's codegen " +
				"(a find_package prefix tree), the dependency emits it — no action; convert that dependency " +
				"as its own element. If it's your project's, run it in-tree so the recognizer lifts it."
			prompt = "The converter recognized the codegen tool(s) " + tools + " in " +
				plural(len(anchors), "execute_process call") + " issued from outside the source tree (" +
				outOfTreeExecReason(sig) + "). A recognizer could lower these to native rules, but they're " +
				"out-of-tree (a dependency's, or unattributed) so they're surfaced not emitted — see suggested_shape."
		}
		c.Add(todos.Todo{
			Kind:           "out-of-tree-execute-process",
			Disposition:    todos.Actionable,
			GroupKey:       string(sig),
			Anchors:        anchors,
			Evidence:       ev,
			SuggestedShape: shape,
			Prompt:         prompt,
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
