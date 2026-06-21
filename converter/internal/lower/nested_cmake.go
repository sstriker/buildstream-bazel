package lower

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Nested cmake lift (the superbuild-at-configure idiom): the outer
// configure runs `execute_process(${CMAKE_COMMAND} -S <src> -B <build>)`
// + `cmake --build <build>`, then the outer targets consume the
// sub-build's artifacts (a bootstrap tool, a static archive, generated
// headers). Refusing makes the whole element unconvertible while EVERY
// signal needed to lift it exists:
//
//   - pass 1's trace records the nested (src, build) pair;
//   - the nested build dir lives under the outer build dir, so the
//     driver can stage File API query files into it and re-run the WARM
//     outer configure — execute_process re-runs every configure, and the
//     nested cmake then writes a codemodel reply;
//   - the nested reply lowers through the same ToIR (recursively, with
//     labels anchored at the OUTER root via the ElementSourceRoot
//     machinery), and the merged targets register their artifacts so the
//     outer link fragments and include dirs wire to real labels.
//
// Loud degradation: when the warm pass can't run (offline --reply-dir,
// --two-pass-genex=false, nested reply missing), the detected nested
// build surfaces as a stderr warning + structured conversion-todo
// instead of a refusal — strictly more convertible than the historical
// Tier-1 abort, with the gap still visible.

// nestedCMakeShape is the parsed form of a recognized nested cmake argv.
type nestedCMakeShape struct {
	kind     string // "configure", "build", or "install"
	srcDir   string // configure only (as recorded in the trace)
	buildDir string
}

// parseNestedCMakeArgv recognizes the nested-cmake argv shapes:
// `cmake -S <src> -B <build> …` (configure; -S/-B in either order,
// separated or joined form), the positional-source configure
// `cmake [-G <gen>] <src>` (the build dir comes from WORKING_DIRECTORY —
// the dominant "download/build at configure" idiom, where the call runs
// IN the build dir; resolved by the caller via resolveNestedCMakeDirs),
// `cmake --build <build> …`, and `cmake --install <build> …`. Returns
// ok=false for every other cmake invocation (-E is handled earlier; -P
// stays on the refuse path). The returned dirs are raw (as the argv spells
// them — possibly relative, ".", or, for the positional form, an empty
// buildDir); resolveNestedCMakeDirs anchors them against WORKING_DIRECTORY.
func parseNestedCMakeArgv(driver string, argv []string) (nestedCMakeShape, bool) {
	if driver != "cmake" || len(argv) < 2 {
		return nestedCMakeShape{}, false
	}
	switch argv[1] {
	case "--build", "--install":
		if len(argv) < 3 || strings.HasPrefix(argv[2], "-") {
			return nestedCMakeShape{}, false
		}
		kind := "build"
		if argv[1] == "--install" {
			kind = "install"
		}
		return nestedCMakeShape{kind: kind, buildDir: argv[2]}, true
	}
	// -E (command mode) and -P (script mode) are not configures — they're
	// handled on other paths; never mistake their operands for a positional
	// source dir in the fallback below.
	for _, a := range argv[1:] {
		if a == "-E" || a == "-P" {
			return nestedCMakeShape{}, false
		}
	}
	var src, build string
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-S" && i+1 < len(argv):
			src = argv[i+1]
			i++
		case strings.HasPrefix(a, "-S") && len(a) > 2:
			src = a[2:]
		case a == "-B" && i+1 < len(argv):
			build = argv[i+1]
			i++
		case strings.HasPrefix(a, "-B") && len(a) > 2:
			build = a[2:]
		}
	}
	if src != "" && build != "" {
		return nestedCMakeShape{kind: "configure", srcDir: src, buildDir: build}, true
	}
	// No explicit -S/-B pair. Fall back to a positional source
	// (`cmake [-G <gen>] <src>`). A SOURCE is the thing the recursive
	// lowering needs; the build dir may be left empty here for the caller
	// to fill from WORKING_DIRECTORY (resolveNestedCMakeDirs declines when
	// there's none). So an explicit `-S <src>` WITHOUT `-B` is accepted too
	// — it configures into the process cwd, which a WORKING_DIRECTORY moves,
	// exactly like the positional form. A lone `-B` with NO source stays
	// unrecognized: a source-less reconfigure of an existing build dir gives
	// the recursive lowering no source tree to work from.
	if src == "" {
		src = nestedPositionalSourceArg(argv)
	}
	if src == "" {
		return nestedCMakeShape{}, false
	}
	return nestedCMakeShape{kind: "configure", srcDir: src, buildDir: build}, true
}

// nestedPositionalSourceArg returns the trailing positional source-dir
// operand of a cmake configure argv — the last element that isn't a flag
// and isn't the separate value of a preceding value-taking flag (-G/-D/…),
// so `cmake -G Ninja .` reads "." (not "Ninja") and `cmake -G Ninja` reads
// nothing. Empty when there's no positional operand.
func nestedPositionalSourceArg(argv []string) string {
	for i := len(argv) - 1; i >= 1; i-- {
		a := argv[i]
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		if i-1 >= 1 && isCMakeSeparateValueFlag(argv[i-1]) {
			continue
		}
		return a
	}
	return ""
}

// isCMakeSeparateValueFlag reports whether a cmake flag consumes the next
// argv element as its value (so that element is not a positional operand).
func isCMakeSeparateValueFlag(a string) bool {
	switch a {
	case "-G", "-S", "-B", "-D", "-U", "-C", "-T", "-A":
		return true
	}
	return false
}

// resolveNestedCMakeDirs fills and resolves a parsed shape's dirs against
// the call's WORKING_DIRECTORY. cmake resolves a relative -B/-S/positional
// dir — and an in-source build with no -B — against the process cwd, which
// a WORKING_DIRECTORY moves; so when it's set, an empty/"."/relative dir
// anchors under it (an empty build dir is an in-source build AT the working
// directory, the download-idiom shape). With no WORKING_DIRECTORY the dirs
// are left as parsed: an absolute path anchors via executeProcessAnchorOutput,
// a given relative one via relativeArgvBuildRel (the outer-build-root cwd),
// and an empty build dir (the positional-source form) is unresolvable, so
// ok=false (the caller declines rather than reconfiguring the outer cwd).
func resolveNestedCMakeDirs(shape nestedCMakeShape, workingDir string) (nestedCMakeShape, bool) {
	if workingDir == "" {
		if shape.buildDir == "" {
			return shape, false
		}
		return shape, true
	}
	resolve := func(d string) string {
		if d == "" || d == "." {
			return filepath.Clean(workingDir)
		}
		if filepath.IsAbs(d) {
			return d
		}
		return filepath.Join(workingDir, d)
	}
	out := shape
	out.buildDir = resolve(shape.buildDir)
	if shape.kind == "configure" {
		out.srcDir = resolve(shape.srcDir)
	}
	return out, true
}

// classifyNestedCMake recognizes the nested-cmake shapes for Classify.
// RESULT_VARIABLE is allowed (exit-status-as-answer, the idiom's
// standard error check); a call that CAPTURES bytes (OUTPUT_VARIABLE /
// OUTPUT_FILE / ERROR_VARIABLE feeding configure logic) stays on the
// refuse path — the lift re-runs nothing at convert time and can't
// reproduce captured output.
func classifyNestedCMake(driver string, call shadow.ExecuteProcessCall) (ClassifyResult, bool) {
	if len(call.Commands) != 1 {
		return ClassifyResult{}, false
	}
	shape, ok := parseNestedCMakeArgv(driver, call.Commands[0])
	if !ok {
		return ClassifyResult{}, false
	}
	// Anchor relative/positional/in-source dirs against WORKING_DIRECTORY;
	// an unresolvable shape (positional source with no WORKING_DIRECTORY)
	// isn't a liftable nested build — decline so it falls through to the
	// generic execute_process handling rather than reconfiguring the cwd.
	shape, ok = resolveNestedCMakeDirs(shape, call.WorkingDirectory)
	if !ok {
		return ClassifyResult{}, false
	}
	if call.OutputVariable != "" || call.OutputFile != "" || call.ErrorVariable != "" {
		return ClassifyResult{
			Bucket: BucketRefuse,
			Reason: "nested cmake " + shape.kind + " captures output (" + outputContext(call) + " ) — the nested-build lift can't reproduce captured bytes",
		}, true
	}
	return ClassifyResult{
		Bucket: BucketNestedCMake,
		Reason: "nested cmake " + shape.kind + " of " + shape.buildDir,
	}, true
}

// DetectNestedConfigures scans a nested build's trace for that build's
// OWN nested cmake configures (the superbuild-chain shape) — the
// driver-side worklist's detection step, run on each harvested trace to
// decide which grandchild dirs to stage and traced-re-configure next.
// srcDir/buildDir are the nested build the trace belongs to;
// the returned map is grandchildBuildRel (relative to buildDir) →
// grandchild source dir, exactly the sink shape runNestedCMakePass
// consumes.
//
// Only shapes the lowering will actually LIFT are returned — the guards
// mirror classifyNestedCMake + recoverNestedCMakeCall, so detection and
// lift can't drift: captured-output calls, relative -B under a moved
// WORKING_DIRECTORY, and build dirs not under buildDir are all skipped
// here and left to the nested lowering's own refusal/warning paths.
func DetectNestedConfigures(traceRaw []byte, srcDir, buildDir string) map[string]string {
	if len(traceRaw) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, call := range shadow.ExtractExecuteProcess(traceRaw, srcDir) {
		if len(call.Commands) != 1 || len(call.Commands[0]) == 0 {
			continue
		}
		shape, ok := parseNestedCMakeArgv(executeProcessDriverBasename(call.Commands[0][0]), call.Commands[0])
		if !ok || shape.kind != "configure" {
			continue
		}
		if call.OutputVariable != "" || call.OutputFile != "" || call.ErrorVariable != "" {
			continue // captured output refuses at lowering; don't stage it
		}
		var rel string
		if filepath.IsAbs(shape.buildDir) {
			r, inside := relativeIfInside(buildDir, shape.buildDir)
			if !inside || r == "" {
				continue // outside (or equal to) this build dir — not liftable
			}
			rel = r
		} else {
			if call.WorkingDirectory != "" {
				continue // moved cwd: the relative anchor would misname the dir
			}
			r, ok := relativeArgvBuildRel(shape.buildDir)
			if !ok {
				continue
			}
			rel = r
		}
		out[rel] = shape.srcDir
	}
	return out
}

// NestedBuildInput is one detected-and-replied nested build the driver
// hands back into the second ToIR pass (Options.NestedBuilds): the
// outer-build-relative nested build dir, the nested source dir as the
// trace recorded it, the nested File API reply, the nested ninja graph
// (nil when unparsable), and the live nested build dir on the convert
// host (where baked bytes are read from).
type NestedBuildInput struct {
	BuildRel     string
	SrcDir       string
	Reply        *fileapi.Reply
	Graph        *ninja.Graph
	HostBuildDir string
	// TraceRaw is the nested configure's own --trace-expand capture
	// (the driver's traced re-configure of the nested build dir;
	// see runNestedTraceReconfigure). Nil when the traced re-run
	// failed or was skipped — the nested lowering then runs
	// trace-less, recovering consumable outputs via the generic
	// on-disk bakes only.
	TraceRaw []byte
	// Children are this nested build's OWN nested builds (the
	// superbuild-chain shape: the sub-project's configure runs a
	// grandchild cmake), harvested by the driver's worklist from
	// this build's trace. Each child's BuildRel is relative to THIS
	// build's dir. lowerOneNestedBuild threads them into the
	// recursive ToIR's Options.NestedBuilds, so the whole
	// merge/re-home/bake machinery composes level by level: the
	// grandchild merges into the child package (child-relative
	// re-homes), then the child package merges into the outer one
	// (child-prefix re-homes apply on top). Empty when the trace
	// surfaced no liftable grandchild configures, the driver's
	// depth cap stopped the descent, or the cycle guard skipped a
	// repeated dir — capped/skipped grandchildren then land in the
	// child lowering's local sink and warn not-lifted, the same
	// loud degradation every other nested failure takes.
	Children []NestedBuildInput
}

// recoverNestedCMakeCall dispatches one BucketNestedCMake call inside
// recoverExecuteProcess. A configure records its (src, buildRel) pair
// into the sink (the driver stages File API queries there for the warm
// pass); --build/--install companions are benign — the configure's lift
// covers the whole nested pipeline, and an orphan companion (no
// configure seen for its dir) records the dir too so the warning names
// it. Returns a refusal only when the nested build dir doesn't anchor
// under the outer build dir (a nested build OUTSIDE our tree can't be
// staged or re-run).
func recoverNestedCMakeCall(call shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) *executeProcessRefusal {
	shape, _ := parseNestedCMakeArgv(executeProcessDriverBasename(call.Commands[0][0]), call.Commands[0])
	// Anchor relative/positional/in-source dirs against WORKING_DIRECTORY
	// (a relative -B/`--build .`/positional source resolves against the
	// cmake process cwd, which WORKING_DIRECTORY moves). With no
	// WORKING_DIRECTORY a given relative dir still anchors against the outer
	// build root below; only an unresolvable positional-source configure
	// (no -B and no WORKING_DIRECTORY) declines here.
	shape, ok := resolveNestedCMakeDirs(shape, call.WorkingDirectory)
	if !ok {
		return &executeProcessRefusal{
			File:   call.File,
			Line:   call.Line,
			Bucket: BucketNestedCMake,
			Reason: "nested cmake " + shape.kind + " has no resolvable build dir (no -B and no WORKING_DIRECTORY to anchor against)",
			Argv:   formatExecuteProcessArgv(call),
		}
	}
	rel, anchored := executeProcessAnchorOutput(shape.buildDir, anc)
	if !anchored {
		rel, anchored = relativeArgvBuildRel(shape.buildDir)
	}
	if !anchored || rel == "" {
		return &executeProcessRefusal{
			File:   call.File,
			Line:   call.Line,
			Bucket: BucketNestedCMake,
			Reason: "nested cmake " + shape.kind + " build dir " + shape.buildDir + " is not under the outer build dir; the nested-build lift can't stage or re-run it",
			Argv:   formatExecuteProcessArgv(call),
		}
	}
	if cc.NestedConfigureSink != nil {
		if shape.kind == "configure" {
			cc.NestedConfigureSink[rel] = shape.srcDir
		} else if _, seen := cc.NestedConfigureSink[rel]; !seen {
			// Orphan --build/--install: still record the dir (empty
			// src) so the not-lifted warning names it; a configure
			// seen later overwrites with the real src.
			cc.NestedConfigureSink[rel] = ""
		}
	}
	return nil
}

// accumulateRecipeIncludes unions the inherited ancestor recipe `.cmake` paths
// with THIS build's own include()d / target_sources recipes (cc.IncludeCalls +
// cc.TargetSourcesCalls), deduped, for threading into the nested lowerings. Paths
// are kept absolute (as the trace records them) — the nested
// recoverUtilityRecipeCommands relativizes them against its own build dir and
// ignores any that don't land inside it. Filtered to `.cmake` so the threaded
// slice stays small (only recipe-shaped paths can drive the recipe tie).
func accumulateRecipeIncludes(inherited []string, cc *codegenContext) []string {
	seen := make(map[string]bool, len(inherited))
	out := make([]string, 0, len(inherited))
	add := func(p string) {
		if p == "" || seen[p] || !strings.HasSuffix(strings.ToLower(p), ".cmake") {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range inherited {
		add(p)
	}
	if cc != nil {
		for _, inc := range cc.IncludeCalls {
			add(inc.Path)
		}
		for _, ts := range cc.TargetSourcesCalls {
			add(ts.Recipe)
		}
	}
	return out
}

// accumulateTargetSources unions the inherited ancestor target_sources() calls
// with THIS build's own (cc.TargetSourcesCalls), deduped by (Target, Recipe,
// joined Sources), for threading into the nested lowerings — so a nested build's
// recovery can learn which generated sources an OUTER-included recipe pulls in.
func accumulateTargetSources(inherited []shadow.TargetSourcesCall, cc *codegenContext) []shadow.TargetSourcesCall {
	seen := make(map[string]bool, len(inherited))
	out := make([]shadow.TargetSourcesCall, 0, len(inherited))
	add := func(ts shadow.TargetSourcesCall) {
		key := ts.Target + "\x00" + ts.Recipe + "\x00" + strings.Join(ts.Sources, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ts)
	}
	for _, ts := range inherited {
		add(ts)
	}
	if cc != nil {
		for _, ts := range cc.TargetSourcesCalls {
			add(ts)
		}
	}
	return out
}

// lowerNestedBuilds is the ToIR pre-pass over Options.NestedBuilds (the
// warm second pass's harvest): recursively lower each nested reply with
// labels anchored at the OUTER root, merge the nested targets into the
// outer package, register nested artifacts for the link-fragment wiring
// (cc.NestedArtifactDeps), and bake the nested build dir's
// configure-generated headers (on disk, not produced by the nested ninja
// graph) so outer `-I <build>/<nested>` consumers resolve them. Returns
// the baked rels for the same consumer attribution executeProcess lifts
// get (targetBuildIncs prefix-matching in lowerTarget).
func lowerNestedBuilds(pkg *ir.Package, opts Options, cc *codegenContext, hostSrc string) ([]executeProcessOut, error) {
	var outs []executeProcessOut
	// Thread THIS build's recipe `.cmake` include()s / target_sources recipes —
	// accumulated onto any inherited from ancestors — into the nested lowerings, so
	// a nested build's UTILITY-produced recipe that an OUTER configure include()s
	// (the superbuild-at-configure shape) is still recovered there (its own gate
	// only sees the nested trace's includes). See Options.OuterRecipeIncludes.
	opts.OuterRecipeIncludes = accumulateRecipeIncludes(opts.OuterRecipeIncludes, cc)
	opts.OuterTargetSources = accumulateTargetSources(opts.OuterTargetSources, cc)
	for _, nb := range opts.NestedBuilds {
		nestedPkg, nestedStatus, err := lowerOneNestedBuild(nb, opts, hostSrc)
		if err != nil {
			fmt.Fprintf(warningsOrDiscard(opts.Warnings),
				"lower: nested cmake build %s: lowering failed (%v); falling back to the not-lifted warning\n", nb.BuildRel, err)
			continue
		}
		// No-silent-dangle guard. A build-dir-sourced nested project
		// (anchored at its OWN root because its source isn't under the outer
		// tree — generated into the build dir by a higher-level configure)
		// whose targets COMPILE build-dir-resident sources would merge
		// dangling srcs: the source bytes live in the build dir, not the
		// outer package, and aren't baked/staged yet. Emitting them would be
		// a SILENT broken conversion, so leave the build not-lifted (the loud
		// nested-cmake-not-lifted todo) until build-dir source staging lands.
		// A TARGET-LESS build-dir nested (a downloader / superbuild bootstrap
		// — cryptoauthlib's mbedtls project(NONE)+ExternalProject) merges
		// nothing, so it stays a clean no-op lift.
		if rootAbs, aerr := filepath.Abs(hostSrc); aerr == nil &&
			nestedElementRoot(rootAbs, nb.SrcDir) != rootAbs &&
			nestedHasCompiledSources(nestedPkg) {
			fmt.Fprintf(warningsOrDiscard(opts.Warnings),
				"lower: nested cmake build %s: its sources live in the build dir (not the outer source tree) and aren't staged yet; leaving it not-lifted rather than emitting dangling srcs\n", nb.BuildRel)
			continue
		}
		cc.NestedLifted[nb.BuildRel] = true
		mergeNestedPackage(pkg, nestedPkg, nb, cc, opts, hostSrc)
		cc.mergeNestedStampCommands(nestedStatus)
		outs = append(outs, bakeNestedGeneratedHeaders(nb, cc, opts)...)
	}
	return outs, nil
}

// mergeNestedStampCommands folds a nested build's workspace-status commands
// (status key -> producing shell command, from the nested lowering's own
// WorkspaceStatusSink) into the outer cc.StampCommands, first-write-wins so an
// outer key keeps its own command. This lets the outer populateWorkspaceStatusSink
// emit a nested configure_file's stamp key: the merged nested target carries
// the stamp_values key, but its producing command lived only in the nested pass.
func (cc *codegenContext) mergeNestedStampCommands(nested map[string]string) {
	for key, cmd := range nested {
		if _, ok := cc.StampCommands[key]; !ok {
			cc.StampCommands[key] = cmd
		}
	}
}

// nestedHasCompiledSources reports whether a nested package has any target
// carrying a file-label-bearing attribute — srcs / hdrs / textual_hdrs /
// data — i.e. paths that would merge into the outer package as labels. A
// build-dir-sourced nested target carries build-dir-resident paths in ALL
// of them, so any non-empty one would merge a dangling label; covering them
// all keeps the guard's invariant (no silent dangling file labels) matching
// its name. A target-less nested package (only UTILITY targets, which the
// converter skips — a downloader / superbuild bootstrap) has none, so it
// merges nothing and is safe to lift as a no-op even when build-dir-sourced.
func nestedHasCompiledSources(pkg *ir.Package) bool {
	if pkg == nil {
		return false
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if len(t.Srcs) > 0 || len(t.Hdrs) > 0 || len(t.TextualHdrs) > 0 || len(t.Data) > 0 {
			return true
		}
	}
	return false
}

// nestedElementRoot picks the label-anchor root for a nested lowering.
// Promote to the OUTER root (outerRootAbs) when the nested SOURCES live
// under the outer source tree (the in-tree subproject / cuda-samples
// overlay shape) — merged srcs then carry the <nested-src-rel>/ prefix the
// outer BUILD needs. But when the nested source dir is NOT under the outer
// tree — generated into the build dir by a higher-level configure (e.g.
// cryptoauthlib's mbedtls downloader: configure_file writes its CMakeLists
// into the build dir, then project(NONE)+ExternalProject yields no
// buildable targets) — there is no outer-root-relative home, and promoting
// it would make resolveElementSourceRoot reject the whole lowering (the
// outer root isn't an ancestor of a build-dir source), turning an empty,
// nothing-to-merge lift into a MISLEADING "targets missing"
// nested-cmake-not-lifted todo. So anchor a build-dir-sourced nested build
// at its OWN root instead: a target-less downloader lowers to an empty
// package (clean no-op), and any build-dir sources of a hypothetical real
// target surface as honest unsupported-source-path findings rather than
// hard-failing. The Rel/`..` test is exactly the ancestor check
// resolveElementSourceRoot would apply — reused here as a
// promote-vs-own-root SELECTOR rather than a gate. Empty or
// un-absolutizable nestedSrc keeps the outer root (the historical default).
func nestedElementRoot(outerRootAbs, nestedSrc string) string {
	if nestedSrc == "" {
		return outerRootAbs
	}
	nestedAbs, err := filepath.Abs(nestedSrc)
	if err != nil {
		return outerRootAbs
	}
	rel, err := filepath.Rel(outerRootAbs, nestedAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nestedAbs
	}
	return outerRootAbs
}

// lowerOneNestedBuild recursively runs ToIR over one nested reply. The
// nested package's labels anchor at the OUTER label root via
// ElementSourceRoot (the cuda-samples overlay machinery), so merged
// targets' srcs/hdrs resolve in the outer BUILD without re-anchoring.
// We can't inject trace argv into the project's own cmake call, but
// the driver's traced re-configure of the nested dir captures an
// equivalent trace (nb.TraceRaw) — when present, the FULL trace-driven
// recovery ladder runs inside the nested lowering (configure_file
// lifts, execute_process classification, stamp vars) exactly as it
// does for the outer project. Nil TraceRaw degrades to the trace-less
// lowering: the codemodel still carries targets, sources, flags, and
// artifacts, and the generic on-disk bakes cover consumable outputs.
func lowerOneNestedBuild(nb NestedBuildInput, opts Options, hostSrc string) (*ir.Package, map[string]string, error) {
	// resolveElementSourceRoot requires an absolute root; a relative
	// --source-root (CI scripts, ad-hoc runs) reaches here verbatim, so
	// absolutize against the converter's cwd first — without this the
	// nested lowering fails and the whole lift degrades to the
	// not-lifted warning on otherwise-fine invocations.
	rootAbs, absErr := filepath.Abs(hostSrc)
	if absErr != nil {
		return nil, nil, absErr
	}
	// Promote to the OUTER root for an in-tree nested source, or fall back
	// to the nested's OWN root for a build-dir-generated one (see
	// nestedElementRoot).
	elementRoot := nestedElementRoot(rootAbs, nb.SrcDir)
	nestedOpts := nestedOptionsFor(nb, opts, elementRoot)
	// Give the nested lowering its OWN fresh workspace-status sink (a
	// status-key -> producing-command map) — distinct from the outer's, which
	// nestedOptionsFor clears to avoid cross-contamination. lowerNestedBuilds
	// folds the result into the outer cc.StampCommands so a nested
	// configure_file's stamp key reaches the --out-workspace-status helper
	// (the merged nested target carries the stamp_values key, but its producing
	// command lived only in the nested cc).
	nestedStatus := map[string]string{}
	nestedOpts.WorkspaceStatusSink = nestedStatus
	pkg, err := ToIR(nb.Reply, nb.Graph, nestedOpts)
	return pkg, nestedStatus, err
}

// nestedOptionsFor builds the Options the recursive ToIR runs the nested
// build under. It is the single chokepoint deciding which of the outer
// Options propagate into a nested lowering — split out (and unit-tested in
// TestNestedOptionsFor) so the forward/drop policy is explicit and a newly
// added Option field can't silently fail to flow. See the inline buckets
// for which fields forward and why the rest are deliberately dropped.
func nestedOptionsFor(nb NestedBuildInput, opts Options, elementRoot string) Options {
	// Default-FORWARD: start from a copy of the outer options so every
	// operator dial (the cmake -P lift family, codegen recognizers, download
	// lift, standalone genrules, BackedFeatures, Fidelity, Rejections, …)
	// reaches the nested lowering automatically. This is deliberately the
	// inverse of hand-listing each forwarded field: a NEWLY added Option flows
	// into nested builds with no change here, so a nested cmake -P script or
	// protoc codegen can't silently get a lower-fidelity conversion than the
	// same construct at the top level. The only maintenance burden is the
	// explicit clears below — and forgetting one cross-contaminates loudly
	// (a nested run writing into an outer sink) rather than failing invisibly.
	n := opts

	// (1) Per-nested CONTEXT — overrides the outer values.
	// HostSourceRoot is where the nested cmake configured; ElementSourceRoot
	// anchors the merged labels (OUTER root for an in-tree nested source so
	// srcs carry the <nested-src-rel>/ prefix, or the nested's OWN root for a
	// build-dir-generated project; see nestedElementRoot). NestedBuilds is the
	// superbuild-chain recursion: this build's own driver-harvested children
	// lower inside the recursive ToIR exactly as this one lowers in its parent.
	n.HostSourceRoot = nb.SrcDir
	n.ElementSourceRoot = elementRoot
	n.BuildDir = nb.HostBuildDir
	n.TraceRaw = nb.TraceRaw
	n.NestedBuilds = nb.Children

	// (2) Outer-SCOPED state — cleared so it can't cross-contaminate the
	// nested run. Two kinds: pass-1 orchestration SINKS (the driver reads
	// these after the outer pass 1 to drive its warm second pass) and
	// OUTER-configure-derived DATA (captured from the outer project's own
	// configure / trace). CTest is cleared too: its registry is parsed from
	// the OUTER build dir's CTestTestfile.cmake, so forwarding it would
	// mis-classify nested executables (a nested registry needs its own
	// harvest). HostPrefixDir is the outer synth-prefix anchor. Keep this list
	// in sync with the "drop" entries in nestedOptionsClass (the test guard).
	n.HostPrefixDir = ""
	n.CTest = nil
	n.SetAssignments = nil
	n.ParentScopeForwards = nil
	n.StampVarSink = nil
	n.WorkspaceStatusSink = nil
	n.NestedConfigureSink = nil
	n.CaptureRefusalSink = nil
	n.DeadCaptureVars = nil
	n.NonExpandedFileWriters = nil
	n.GenexProbes = nil
	n.ConfigureLog = nil
	n.LiteralProbeSink = nil
	n.LiteralResolutions = nil

	return n
}

// mergeNestedPackage folds the nested package's targets into the outer
// one. Name collisions with already-present targets skip the nested copy
// with a warning (rare; the outer target wins). Each merged target with
// a codemodel artifact registers `<nestedBuildRel>/<artifact>` →
// `:<name>` in cc.NestedArtifactDeps so outer link fragments naming the
// nested archive wire to the real label.
func mergeNestedPackage(pkg *ir.Package, nestedPkg *ir.Package, nb NestedBuildInput, cc *codegenContext, opts Options, hostSrc string) {
	rehome := nestedProducerReHomes(nestedPkg, nb, hostSrc)
	namePrefix := sanitizeOutputName(nb.BuildRel)
	present := map[string]bool{}
	for _, t := range pkg.Targets {
		present[t.Name] = true
	}
	for _, t := range nestedPkg.Targets {
		applyNestedProducerReHome(&t, rehome, namePrefix)
		if present[t.Name] {
			fmt.Fprintf(warningsOrDiscard(opts.Warnings),
				"lower: nested cmake build %s: target %q collides with an outer target; keeping the outer one\n", nb.BuildRel, t.Name)
			continue
		}
		t.Tags = append(t.Tags, "cmake-codegen-nested-cmake")
		sort.Strings(t.Tags)
		// Re-home the nested target's BUILD-dir includes: inside the
		// nested lowering, an include of the nested build root
		// relativizes to "." and a subdir to its bare nested-build-
		// relative form ("gen") — both of which mis-anchor in the OUTER
		// package ("." is the workspace root, which Bazel rejects
		// outright; "gen" points at the outer root). A nested SOURCE
		// include can never produce "." (the nested source root is a
		// strict subdir under ElementSourceRoot anchoring), but a bare
		// subdir form is ambiguous — the nested target may legitimately
		// include a sibling source dir under the outer root. On-disk
		// existence is the discriminator: a dir present under the outer
		// source root is a source include and stays; one present under
		// the nested build dir re-homes to its <buildRel>/ form. A name
		// present under BOTH resolves as the source include (the check
		// order's tie-break — the conservative default).
		for i, inc := range t.Includes {
			if inc == "." {
				t.Includes[i] = nb.BuildRel
				continue
			}
			if filepath.IsAbs(inc) {
				continue
			}
			if isExistingDir(filepath.Join(hostSrc, filepath.FromSlash(inc))) {
				continue
			}
			if isExistingDir(filepath.Join(nb.HostBuildDir, filepath.FromSlash(inc))) {
				t.Includes[i] = nb.BuildRel + "/" + inc
			}
		}
		pkg.Targets = append(pkg.Targets, t)
		present[t.Name] = true
		// Register an APPENDED producer's outs in the OUTER producer
		// map (collision-skipped rules must not register a dangling
		// name): bakeNestedGeneratedHeaders defers to these instead
		// of duplicating, and later outer recoveries see the edge.
		for _, out := range producerOuts(&t) {
			cc.OutToGenrule[out] = t.Name
		}
		if t.ArtifactName != "" {
			cc.NestedArtifactDeps[nb.BuildRel+"/"+filepath.ToSlash(t.ArtifactName)] = ":" + t.Name
		}
	}
	if len(nestedPkg.SubPackages) > 0 {
		if pkg.SubPackages == nil {
			pkg.SubPackages = map[string]string{}
		}
		for name, dir := range nestedPkg.SubPackages {
			if _, exists := pkg.SubPackages[name]; !exists {
				pkg.SubPackages[name] = dir
			}
		}
	}
	// NOTE: we deliberately do NOT carry the nested build's include() events into
	// the outer cc.IncludeCalls. The OUTPUT->include tie (adoptIncludedRecipeOutput)
	// runs only in the OUTER lowerTarget walk, over the OUTER package's codemodel
	// targets — nested targets are merged here as already-lowered ir.Targets (their
	// sources were resolved during the recursive nested lowering, which had the
	// nested trace). An OUTER target's source can only be added by a target_sources()
	// that executes in the OUTER cmake process (a nested cmake build is a separate
	// process and cannot target_sources() an outer target), so the include() that
	// adds it is already in the outer trace (opts.TraceRaw -> cc.IncludeCalls). The
	// earlier nested-include carry (#721) was therefore dead weight.
}

// nestedProducerReHomes maps the nested lowering's producer-rule outs
// (write_file / genrule — build-dir bakes, configure_file recoveries,
// execute_process lifts, custom-command genrules) to their outer-
// package homes. Inside the nested lowering a BUILD-dir out anchors at
// the NESTED build root ("sub_config.h"), which in the outer BUILD
// would materialize at the package root instead of under <buildRel>/.
// A genrule out can also be SOURCE-relative (the in-place-rewrite
// shape rewrites a committed source) — those already anchor correctly
// under ElementSourceRoot, so on-disk existence is the discriminator,
// the same tie-break the include re-homing uses: present under the
// outer source root → source out, stays; present under the nested
// build dir → re-homes. Present under neither (an unbuilt nested
// custom-command out) conservatively stays.
func nestedProducerReHomes(nestedPkg *ir.Package, nb NestedBuildInput, hostSrc string) map[string]string {
	rehome := map[string]string{}
	for i := range nestedPkg.Targets {
		t := &nestedPkg.Targets[i]
		for _, out := range producerOuts(t) {
			if isExistingFile(filepath.Join(hostSrc, filepath.FromSlash(out))) {
				continue
			}
			if isExistingFile(filepath.Join(nb.HostBuildDir, filepath.FromSlash(out))) {
				rehome[out] = nb.BuildRel + "/" + out
			}
		}
	}
	return rehome
}

// producerOuts lists a producer rule's output paths; empty for
// non-producer kinds. The three kinds here and the rename branch in
// applyNestedProducerReHome are a matched pair: a kind listed here
// without a re-home application there would map its out but never
// apply it, silently materializing the file at the outer package root
// with a collidable rule name.
func producerOuts(t *ir.Target) []string {
	switch t.Kind {
	case ir.KindWriteFile:
		return []string{t.WriteFileOut}
	case ir.KindGenrule:
		return t.GenruleOuts
	case ir.KindCMakeConfigureFile:
		// The configure_file LIFT tier (nestedOpts threads the
		// operator's LiftConfigureFile opt-in): Out is equally
		// nested-build-relative.
		if t.CMakeConfigureFile != nil {
			return []string{t.CMakeConfigureFile.Out}
		}
	case ir.KindNativeRule:
		// The codegen-recognizer registry substrate (pkg_tar, future
		// http_file / proto rules): its produced outputs live in the
		// `out`/`outs` attrs. Reading them generically keeps every
		// native-rule lift participating in the nested-merge re-home
		// (and any future producer-outputs consumer) without a
		// per-rule special case — the whole point of the substrate.
		return nativeRuleOuts(t.NativeRule)
	}
	return nil
}

// nativeRuleOuts returns the build-relative output paths a NativeRuleSpec
// declares via its `out` (scalar) and `outs` (list) attributes — the
// kind-agnostic producer-output accessor for the registry substrate.
func nativeRuleOuts(spec *ir.NativeRuleSpec) []string {
	if spec == nil {
		return nil
	}
	var outs []string
	for _, a := range spec.Attrs {
		switch a.Name {
		case "out":
			if a.Str != "" {
				outs = append(outs, a.Str)
			}
		case "outs":
			outs = append(outs, a.List...)
		}
	}
	return outs
}

// rewriteRuledirDir rewrites `$(RULEDIR)/<oldDir>` to `$(RULEDIR)/<newDir>` at a
// path-token boundary (the dir followed by whitespace, `/`, or end) — so a
// genrule whose cmd anchored an output directory survives the nested re-home that
// moved its declared outs under <buildRel>/.
func rewriteRuledirDir(cmd, oldDir, newDir string) string {
	if oldDir == "" || oldDir == "." {
		return cmd
	}
	old := "$(RULEDIR)/" + oldDir
	repl := "$(RULEDIR)/" + newDir
	var b strings.Builder
	for i := 0; i < len(cmd); {
		if strings.HasPrefix(cmd[i:], old) {
			if j := i + len(old); j == len(cmd) || cmd[j] == ' ' || cmd[j] == '\t' || cmd[j] == '/' {
				b.WriteString(repl)
				i = j
				continue
			}
		}
		b.WriteByte(cmd[i])
		i++
	}
	return b.String()
}

// applyNestedProducerReHome re-anchors one merged target against the
// re-homes: a producer rule's outs gain the <buildRel>/ prefix and the
// rule renames (two nested builds recovering the same-named rule must
// not collide in the outer package); any other target's srcs/hdrs
// entries pointing at a re-homed rel re-point. namePrefix is the
// sanitized buildRel.
func applyNestedProducerReHome(t *ir.Target, rehome map[string]string, namePrefix string) {
	if len(rehome) == 0 {
		return
	}
	if t.Kind == ir.KindNativeRule && t.NativeRule != nil {
		applyNestedNativeRuleReHome(t, rehome, namePrefix)
		return
	}
	if t.Kind == ir.KindWriteFile || t.Kind == ir.KindGenrule ||
		(t.Kind == ir.KindCMakeConfigureFile && t.CMakeConfigureFile != nil) {
		renamed := false
		if newRel, ok := rehome[t.WriteFileOut]; ok {
			t.WriteFileOut = newRel
			renamed = true
		}
		for i, out := range t.GenruleOuts {
			if newRel, ok := rehome[out]; ok {
				// A recovered genrule whose cmd anchored its output DIRECTORY to
				// $(RULEDIR)/<dir> (a tool that takes an outdir arg, e.g. the recipe
				// gen_src recovery) must follow the re-home: the declared out moved
				// gen/… -> <buildRel>/gen/…, so the cmd's $(RULEDIR)/gen must become
				// $(RULEDIR)/<buildRel>/gen or it writes to the wrong place.
				if od, nd := slashDir(out), slashDir(newRel); od != nd {
					t.GenruleCmd = rewriteRuledirDir(t.GenruleCmd, od, nd)
				}
				t.GenruleOuts[i] = newRel
				renamed = true
			}
		}
		if t.Kind == ir.KindCMakeConfigureFile {
			if newRel, ok := rehome[t.CMakeConfigureFile.Out]; ok {
				// Copy-on-write: the spec pointer may be shared; the
				// re-homed rule must not mutate the nested package's
				// original.
				spec := *t.CMakeConfigureFile
				spec.Out = newRel
				t.CMakeConfigureFile = &spec
				renamed = true
			}
		}
		// A producer can CONSUME another producer's re-homed out (the
		// writer-index cp lift declares a produced build-dir source in
		// Srcs and bakes the same rel into its $(location …) token) —
		// re-point both, or the merged rule references a label the
		// outer package doesn't have.
		for i, src := range t.Srcs {
			if newRel, ok := rehome[src]; ok {
				t.Srcs[i] = newRel
				t.GenruleCmd = strings.ReplaceAll(t.GenruleCmd, "$(location "+src+")", "$(location "+newRel+")")
			}
		}
		if renamed {
			t.Name = nestedProducerName(t, namePrefix)
		}
		return
	}
	for i, s := range t.Srcs {
		if newRel, ok := rehome[s]; ok {
			t.Srcs[i] = newRel
		}
	}
	for i, h := range t.Hdrs {
		if newRel, ok := rehome[h]; ok {
			t.Hdrs[i] = newRel
		}
	}
}

// applyNestedNativeRuleReHome re-anchors a KindNativeRule producer's
// outputs (and any srcs label-refs into other re-homed producers)
// against the nested merge re-homes — the registry substrate's share of
// the producer re-home, copy-on-write so the nested package's original
// spec isn't mutated.
func applyNestedNativeRuleReHome(t *ir.Target, rehome map[string]string, namePrefix string) {
	spec := *t.NativeRule
	attrs := append([]ir.NativeAttr(nil), spec.Attrs...)
	renamed := false
	for i := range attrs {
		switch attrs[i].Name {
		case "out":
			if newRel, ok := rehome[attrs[i].Str]; ok {
				attrs[i].Str = newRel
				renamed = true
			}
		case "outs", "srcs":
			list := append([]string(nil), attrs[i].List...)
			for j, v := range list {
				if newRel, ok := rehome[v]; ok {
					list[j] = newRel
					if attrs[i].Name == "outs" {
						renamed = true
					}
				}
			}
			attrs[i].List = list
		}
	}
	spec.Attrs = attrs
	t.NativeRule = &spec
	if renamed {
		t.Name = namePrefix + "_" + t.Name
	}
}

// nestedProducerName names a re-homed producer: build-dir bakes keep
// their canonical shape (bakedBuildDirName over the re-homed rel,
// which already encodes the buildRel); every other producer prefixes
// the sanitized buildRel onto its nested name.
func nestedProducerName(t *ir.Target, namePrefix string) string {
	if stringSliceContains(t.Tags, "cmake-codegen-build-dir-bake") {
		outs := producerOuts(t)
		if len(outs) > 0 {
			return bakedBuildDirName(outs[0])
		}
	}
	return namePrefix + "_" + t.Name
}

// isExistingFile reports whether p exists and is a regular file.
func isExistingFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// bakeNestedGeneratedHeaders bakes the nested build dir's
// configure-generated headers: on-disk header-shaped files that the
// NESTED ninja graph does not produce (so they were written by the
// nested configure — configure_file outputs and kin; the nested build
// has no trace, so the configure_file ladder can't recover them
// natively). Build-time artifacts and cmake bookkeeping are excluded.
// Registered at `<nestedBuildRel>/<rel>` so the outer targetBuildIncs
// prefix-match attaches them to consumers including the nested build
// dir. Same convert-time-bake trade as the script bake (re-run convert
// to refresh), riding its own audit facet.
func bakeNestedGeneratedHeaders(nb NestedBuildInput, cc *codegenContext, opts Options) []executeProcessOut {
	ninjaOuts := ninjaOutputSet(nb.Graph)
	var outs []executeProcessOut
	root := nb.HostBuildDir
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "CMakeFiles" || name == ".cmake" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !nestedBakeableHeader(rel) || ninjaOuts[rel] {
			return nil
		}
		outRel := nb.BuildRel + "/" + rel
		if cc.outputClaimed(outRel) {
			// Another channel already owns the bytes (typically the
			// nested lowering's own re-homed build-dir bake); don't
			// duplicate the rule, but DO surface the out so the outer
			// consumer attribution still attaches it.
			outs = append(outs, executeProcessOut{RelOutput: outRel})
			return nil
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			// A header-shaped file we'd otherwise bake for outer consumers
			// exists on disk but can't be read: note the uncertain drop
			// rather than skipping silently. outRel is build-relative —
			// leak-safe for the report.
			cc.noteUnresolvedRecoveryInput(unresolvedNestedHeaderUnreadable, outRel)
			return nil
		}
		name := executeProcessGenruleName(outRel)
		tags := []string{"cmake-codegen", "cmake-codegen-nested-cmake", "cmake-codegen-nested-cmake-bake"}
		cc.Genrules = append(cc.Genrules, bakeFileTarget(name, outRel, body, tags))
		cc.OutToGenrule[outRel] = name
		outs = append(outs, executeProcessOut{RelOutput: outRel})
		return nil
	})
	return outs
}

// nestedBakeableHeader reports whether a nested-build-relative file is
// part of the consumable configure-generated header surface: header-ish
// extensions only — archives/objects/binaries ride the artifact wiring,
// and cmake bookkeeping (CMakeCache.txt, build.ninja, …) is never
// consumed by outer compiles.
func nestedBakeableHeader(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".h", ".hh", ".hpp", ".hxx", ".inc", ".inl", ".ipp", ".def":
		return true
	}
	return false
}

// nestedCompiledArtifact reports whether a nested-build-relative file is a
// COMPILED artifact (library / archive / object) — evidence the nested build
// produces real link targets a lift would emit, which a header bake can't stand
// in for. Used to keep the not-lifted todo when those targets are genuinely
// missing.
func nestedCompiledArtifact(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".a", ".so", ".dylib", ".lib", ".o", ".obj", ".dll", ".pyd":
		return true
	}
	// Versioned shared libs (libfoo.so.1.2).
	return strings.Contains(filepath.Base(rel), ".so.")
}

// nestedBuildFullyRecovered reports whether a NOT-lifted nested build is
// nonetheless fully accounted for: it's HEADER-ONLY (no compiled
// library/archive/object on disk — nothing a lift would have emitted as a link
// target) and EVERY consumable header under it was already recovered (claimed by
// the outer build-dir bake / another channel). In that case the
// nested-cmake-not-lifted todo is redundant — the configure-generated headers
// the outer compiles include are present, and there are no missing targets — so
// it's suppressed. Requires a live build dir (offline runs can't bake the bytes,
// so the headers wouldn't be recovered and the todo correctly stands).
func nestedBuildFullyRecovered(buildRel, hostBuildDir string, cc *codegenContext) bool {
	if hostBuildDir == "" {
		return false
	}
	root := filepath.Join(hostBuildDir, filepath.FromSlash(buildRel))
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return false
	}
	sawHeader, recovered := false, true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == "CMakeFiles" || n == ".cmake" {
				return filepath.SkipDir
			}
			return nil
		}
		r, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		r = filepath.ToSlash(r)
		switch {
		case nestedCompiledArtifact(r):
			recovered = false // a real link target the bake can't replace
			return filepath.SkipAll
		case nestedBakeableHeader(r):
			sawHeader = true
			if !cc.outputClaimed(buildRel + "/" + r) {
				recovered = false // a consumable header that wasn't recovered
				return filepath.SkipAll
			}
		}
		return nil
	})
	return recovered && sawHeader
}

// warningsOrDiscard guards the nil-Warnings case for the nested lift's
// stderr breadcrumbs.
func warningsOrDiscard(w interface{ Write([]byte) (int, error) }) interface{ Write([]byte) (int, error) } {
	if w == nil {
		return discardWriter{}
	}
	return w
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// warnUnliftedNestedBuilds surfaces nested builds pass 1 detected but no
// pass lifted (offline runs, --two-pass-genex=false, a failed nested
// lowering): one stderr warning + one structured conversion-todo per
// nested build dir, replacing the historical Tier-1 refusal with loud
// degradation.
func warnUnliftedNestedBuilds(opts Options, cc *codegenContext) {
	if len(cc.NestedConfigureSink) == 0 {
		return
	}
	var rels []string
	for rel := range cc.NestedConfigureSink {
		if cc.NestedLifted[rel] {
			continue
		}
		// Not lifted, but if the build is header-only and all its
		// configure-generated headers were recovered (outer build-dir bake),
		// there are no missing targets — the not-lifted todo would be redundant.
		if nestedBuildFullyRecovered(rel, opts.BuildDir, cc) {
			continue
		}
		rels = append(rels, rel)
	}
	if len(rels) == 0 {
		return
	}
	sort.Strings(rels)
	if opts.Warnings != nil {
		fmt.Fprintf(opts.Warnings,
			"lower: %d nested cmake build(s) detected but not lifted (no warm second pass — offline run, --two-pass-genex=false, or the nested reply failed to load); the sub-build's targets are missing from the conversion: %s\n",
			len(rels), strings.Join(rels, ", "))
	}
	emitNestedCMakeTodos(opts.Todos, rels, cc.NestedConfigureSink, opts.HostSourceRoot, opts.BuildDir)
}

// emitNestedCMakeTodos mirrors each detected-but-not-lifted nested cmake
// build into a structured conversion-todo (one per nested build dir; the
// build dir is the unit an author re-works). Paths pass the report
// normalization so the byte-identical-report contract holds.
func emitNestedCMakeTodos(c *todos.Collector, rels []string, sink map[string]string, sourceRoot, buildDir string) {
	if c == nil || len(rels) == 0 {
		return
	}
	for _, rel := range rels {
		src := normalizeReportPath(sink[rel], sourceRoot, buildDir)
		c.Add(todos.Todo{
			Kind:        "nested-cmake-not-lifted",
			Disposition: todos.Actionable,
			GroupKey:    rel,
			Evidence: map[string]any{
				"nested_build_dir": rel,
				"nested_source":    src,
			},
			SuggestedShape: "re-run the converter with a live build dir and --two-pass-genex (the default) so the nested reply can be staged and merged; or convert the nested source dir as its own element and wire its artifacts via the imports manifest",
			Prompt: "A configure-time nested cmake build (" + rel + ", source " + src +
				") was detected but not lifted — its targets are missing from the conversion. Re-run with the warm second pass available, or convert the nested project separately.",
		})
	}
}
