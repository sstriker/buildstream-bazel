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

// parseNestedCMakeArgv recognizes the three nested-cmake argv shapes:
// `cmake -S <src> -B <build> …` (configure; -S/-B in either order,
// separated or joined form), `cmake --build <build> …`, and
// `cmake --install <build> …`. Returns ok=false for every other cmake
// invocation (-E is handled earlier; -P stays on the refuse path).
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
	if src == "" || build == "" {
		return nestedCMakeShape{}, false
	}
	return nestedCMakeShape{kind: "configure", srcDir: src, buildDir: build}, true
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
	// A RELATIVE -B resolves against the cmake process cwd — the outer
	// build root under the runner's cmd.Dir contract — UNLESS the call
	// sets WORKING_DIRECTORY, which moves the resolution base somewhere
	// the anchor below would silently misname; refuse that combination
	// explicitly rather than warning about a phantom directory.
	if !filepath.IsAbs(shape.buildDir) && call.WorkingDirectory != "" {
		return &executeProcessRefusal{
			File:   call.File,
			Line:   call.Line,
			Bucket: BucketNestedCMake,
			Reason: "nested cmake " + shape.kind + " uses a relative build dir " + shape.buildDir + " with WORKING_DIRECTORY " + call.WorkingDirectory + "; the lift anchors relative dirs against the outer build root and can't honor the moved cwd",
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
	for _, nb := range opts.NestedBuilds {
		nestedPkg, err := lowerOneNestedBuild(nb, opts, hostSrc)
		if err != nil {
			fmt.Fprintf(warningsOrDiscard(opts.Warnings),
				"lower: nested cmake build %s: lowering failed (%v); falling back to the not-lifted warning\n", nb.BuildRel, err)
			continue
		}
		cc.NestedLifted[nb.BuildRel] = true
		mergeNestedPackage(pkg, nestedPkg, nb, cc, opts, hostSrc)
		outs = append(outs, bakeNestedGeneratedHeaders(nb, cc, opts)...)
	}
	return outs, nil
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
func lowerOneNestedBuild(nb NestedBuildInput, opts Options, hostSrc string) (*ir.Package, error) {
	// resolveElementSourceRoot requires an absolute root; a relative
	// --source-root (CI scripts, ad-hoc runs) reaches here verbatim, so
	// absolutize against the converter's cwd first — without this the
	// nested lowering fails and the whole lift degrades to the
	// not-lifted warning on otherwise-fine invocations.
	rootAbs, absErr := filepath.Abs(hostSrc)
	if absErr != nil {
		return nil, absErr
	}
	nestedOpts := Options{
		// The nested source dir is where the nested cmake configured;
		// ElementSourceRoot forces label anchoring at the OUTER root
		// (the cuda-samples overlay shape), so merged targets' srcs
		// carry the `<nested-src-rel>/` prefix the outer BUILD needs.
		HostSourceRoot:    nb.SrcDir,
		ElementSourceRoot: rootAbs,
		BuildDir:          nb.HostBuildDir,
		TraceRaw:          nb.TraceRaw,
		// The superbuild-chain recursion: this build's own nested
		// builds (driver-harvested from its trace) lower inside the
		// recursive ToIR exactly as this one lowers inside its
		// parent. Lifted children mark the local sink's NestedLifted,
		// so only genuinely-unlifted grandchildren warn.
		NestedBuilds:     nb.Children,
		Imports:          opts.Imports,
		BazelPackagePath: opts.BazelPackagePath,
		CMakeVars:        opts.CMakeVars,
		Coverage:         opts.Coverage,
		Todos:            opts.Todos,
		Warnings:         opts.Warnings,
		BakeIn:           opts.BakeIn,
	}
	return ToIR(nb.Reply, nb.Graph, nestedOpts)
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
// non-producer kinds.
func producerOuts(t *ir.Target) []string {
	switch t.Kind {
	case ir.KindWriteFile:
		return []string{t.WriteFileOut}
	case ir.KindGenrule:
		return t.GenruleOuts
	}
	// KindCMakeConfigureFile (the configure_file LIFT tier, whose
	// CMakeConfigureFile.Out is equally nested-build-relative) is
	// DELIBERATELY absent, not an oversight: it is unreachable in a nested
	// lowering today because lowerOneNestedBuild's nestedOpts does not
	// thread LiftConfigureFile, so every nested configure_file recovery
	// takes the BAKE tier (KindWriteFile, covered above) — which is what
	// the gate's "configure_file channel facet, not the build-dir-bake
	// fallback" assertion exercises. If LiftConfigureFile is ever threaded
	// into the nested options (a natural next step now that the nested
	// trace makes the lift recoverable there), re-homing must learn this
	// kind in BOTH sites or the lift-tier outs silently materialize at the
	// outer package root with collidable rule names: add the
	// `return []string{t.CMakeConfigureFile.Out}` case here (guarded
	// non-nil) AND a CMakeConfigureFile.Out re-anchor branch in
	// applyNestedProducerReHome alongside the WriteFile/Genrule rename — a
	// one-liner here alone would re-home-map the out but never apply it —
	// then pin it with a nested-configure_file-lift fixture.
	return nil
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
	if t.Kind == ir.KindWriteFile || t.Kind == ir.KindGenrule {
		renamed := false
		if newRel, ok := rehome[t.WriteFileOut]; ok {
			t.WriteFileOut = newRel
			renamed = true
		}
		for i, out := range t.GenruleOuts {
			if newRel, ok := rehome[out]; ok {
				t.GenruleOuts[i] = newRel
				renamed = true
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
		if _, produced := cc.OutToGenrule[outRel]; produced {
			// Another channel already owns the bytes (typically the
			// nested lowering's own re-homed build-dir bake); don't
			// duplicate the rule, but DO surface the out so the outer
			// consumer attribution still attaches it.
			outs = append(outs, executeProcessOut{RelOutput: outRel})
			return nil
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
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
		if !cc.NestedLifted[rel] {
			rels = append(rels, rel)
		}
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
