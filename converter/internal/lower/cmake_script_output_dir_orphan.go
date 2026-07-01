package lower

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// recoverOutputDirOrphanEdges is the PRE-WALK pass for the OUTPUT_DIR
// consumed-orphan codegen shape. It runs BEFORE the target walk (alongside the
// other configure-time recoveries) so the orphan sources it attributes are
// registered in cc.OutToGenrule by the time a compile target's generated-source
// lowering checks cc.outputClaimed — closing the refusal before it fires.
//
// The shape (a hard Tier-1 refusal today): a `cmake -P <script>` custom command
// whose ninja-declared OUTPUT is only a `.cmake` stamp/manifest, while the REAL
// generated sources (`foo.c`/`foo.h`) are an UNDECLARED side effect — the script
// writes them (via file(WRITE) / configure_file COPYONLY) into a directory
// passed as a `-D<VAR>=<dir>` cache arg (e.g. `-DOUTPUT_DIR=<dir>`). A compile
// target consumes `<dir>/foo.c`, but cmake/ninja has no producer edge for it
// (only the stamp), so it is a consumed ORPHAN. Without recovery,
// recoverGenrule(foo.c) finds only the stamp's no-op phony, CommandFor returns
// "", and the converter REFUSES (EXIT 65).
//
// Why the other recoveries miss it: the script's file(WRITE) side effects are
// NOT execute_process outputs, so recoverExecuteProcess / recoverCmakeScriptCodegen
// (which key on execute_process leaf calls) don't attribute them; and the
// declared-output rungs re-derive their output set from genruleOuts(b) = the
// stamp only. So neither sees the orphan sources.
//
// Gated on CMakeScriptTrace + a usable cmake (the script re-trace's opt-in) — off
// → no-op, today's behavior holds. RecognizeCodegen is NOT required: it only
// upgrades a recovered tool to its native rule; without it the orphan producer is
// still extracted/baked, closing the refusal rather than gating it behind the
// genrule→native upgrade.
func (cc *codegenContext) recoverOutputDirOrphanEdges(g *ninja.Graph, cmakeSrc, buildDir string) {
	if cc == nil || g == nil || !cc.CMakeScriptTrace || cc.CMakeBinary == "" || buildDir == "" {
		return
	}
	if len(cc.ConsumedBuildRel) == 0 && len(cc.OuterConsumedBuildRel) == 0 {
		return // no demand side — nothing a consumer (local OR outer) needs a producer for
	}
	edges := ninja.CustomCommandEdges(g)
	for _, b := range edges {
		cmd, ok := ninja.CommandFor(g, b)
		if !ok || cmd == "" {
			continue
		}
		cmd = cc.realCmakeCommandForEdge(b, cmd, buildDir)
		cc.recoverOutputDirOrphans(b, cmd, cmakeSrc, buildDir, edges, g)
	}
}

// recoverOutputDirOrphans attributes the consumed orphans THIS `cmake -P` edge
// produces and emits ONE regenerating producer declaring them (+ the edge's
// stamp), registering them in cc.OutToGenrule so the consumer attaches instead
// of refusing. The mechanism is hermetic (consumed-orphans, NOT raw dir
// contents):
//
//   - AUTHORITY for what to declare = the parent's CONSUMED orphans
//     (cc.ConsumedBuildRel minus cc.NinjaOuts) — exactly the build-dir sources a
//     compile target consumes that no ninja edge produces.
//   - EVIDENCE that THIS edge produces them = the dir its TRACED WRITES target.
//     Re-trace the script and collect the build-relative directories its
//     file(WRITE/APPEND/COPY/COPY_FILE/TOUCH) + configure_file writes land in.
//     The OUTPUT_DIR is detected from where the writes ACTUALLY GO, not from a
//     hardcoded variable name (the `-D` var is project-specific).
//   - ATTRIBUTE the consumed orphans that live under a dir this edge's traced
//     writes target, corroborated ON DISK (the re-trace materialized them under
//     the convert build dir — os.Stat them, the same corroboration the
//     declared-output rungs use).
//
// Emission reuses bakeCmakeScriptGenrule (the existing cmake -P recovery
// emission): the script's writes are deterministic literals, so the bake
// reproduces them byte-exact as write_file / genrule targets, left on
// cc.Genrules (folded into pkg.Targets by the standard cc.Genrules pass) and
// registered in cc.OutToGenrule.
//
// DECLINE SAFELY (leave today's bake/refuse, never over-attribute) when: the
// edge isn't a `cmake -P` script, no consumed orphan lives under the edge's
// traced-write dirs, the orphans aren't on disk, or the shape is ambiguous
// (another `cmake -P` edge's traced writes could own the same orphan dir).
func (cc *codegenContext) recoverOutputDirOrphans(b *ninja.Build, cmd, cmakeSrc, buildDir string, edges []*ninja.Build, g *ninja.Graph) {
	if !usesCmakeScriptMode(cmd) {
		return
	}
	script := extractCmakeScriptPath(cmd)
	if script == "" || script == "<unknown-script>" {
		return
	}

	// The demand side: consumed build-dir sources no ninja edge produces and no
	// earlier recovery already claimed. Empty → nothing to attribute, decline.
	orphans := cc.unclaimedConsumedOrphans()
	if len(orphans) == 0 {
		return
	}

	// The evidence side: the build-relative directories THIS edge's traced writes
	// target. Re-tracing materializes the writes under the convert build dir (the
	// `-D` args carry its absolute path), which both reveals the write dirs AND
	// puts the orphan bytes on disk for corroboration.
	dArgs := extractCmakePDashArgs(cmd)
	// Pre-create the OUTPUT_DIR before the standalone re-trace. The real custom
	// command runs a sibling `cmake -E make_directory <OUTPUT_DIR>` COMMAND first,
	// which the standalone `cmake -P` re-trace does NOT — so a TOOL the script
	// runs into OUTPUT_DIR (`sh gen.sh <dir>`) would fail on the missing dir,
	// truncating the trace and leaving the orphans unmaterialized. Create the
	// stamp outputs' dirs and any `-D<VAR>=<build-subdir>` directory the args
	// carry (the OUTPUT_DIR is passed as such a cache arg). Mirrors the ladder's
	// recoverCmakeScriptCodegen pre-create; harmless for the file(WRITE) shape.
	cc.precreateOutputDirOrphanDirs(b, dArgs, cmakeSrc, buildDir)
	writeDirs, _ := cc.tracedScriptWriteDirs(script, dArgs, cmakeSrc, buildDir)
	if len(writeDirs) == 0 {
		return
	}

	// The edge's -D<VAR>=<dir> cache args name its exact OUTPUT_DIR(s) — the
	// hash-suffixed subdir the temp-dir-then-copy shape writes into. Use them to
	// CONSTRAIN attribution below: the traced write set adds a dir operand's
	// PARENT speculatively (addArgvDir, for the file-in-OUTPUT_DIR case), which
	// over-claims non-codegen files sitting directly under a SHARED component dir
	// when the OUTPUT_DIR is nested (parent = component dir, not the build root).
	// The -D-named dir is the exact target, so restricting to it drops the leak.
	outputDirs := cc.outputDirArgSet(dArgs, buildDir)

	// ATTRIBUTE: an orphan whose parent dir is one this edge writes into (and,
	// when the edge names explicit OUTPUT_DIRs, is UNDER one of those — not the
	// speculative parent), and which the re-trace materialized on disk under the
	// build dir (corroboration).
	var attributed []string
	attributedDirs := map[string]bool{}
	for _, o := range orphans {
		d := path.Dir(o)
		if !attributesOrphanDir(d, writeDirs, outputDirs) {
			continue
		}
		if !cc.orphanOnDisk(o, buildDir) {
			continue // the re-trace didn't actually produce it — don't claim it
		}
		attributed = append(attributed, o)
		attributedDirs[d] = true
	}
	if len(attributed) == 0 {
		return
	}

	// Over-attribution guard: if ANY OTHER `cmake -P` edge DEFINITELY writes into a
	// dir one of THIS edge's attributed orphans lives in, the orphan ownership is
	// ambiguous — decline rather than guess which edge produces the shared-dir
	// source. The basis is the attributed orphans' actual dirs (not the wide write
	// set), compared against each other edge's PRIMARY write dirs, so two edges
	// writing into DISTINCT subdirs of a shared parent don't phantom-contend on the
	// parent. Checked only once we have an attribution candidate (the re-trace of
	// every other script edge is the costly step; skip it when this edge owns nothing).
	if cc.otherScriptEdgeWritesTo(b, cmd, attributedDirs, edges, g, cmakeSrc, buildDir) {
		return
	}

	// Declare the attributed orphans PLUS the edge's own stamp output(s), so the
	// single recovered producer covers the whole edge (the stamp consumer — the
	// add_custom_target — wires to it too). bakeCmakeScriptGenrule reads each
	// declared output's bytes (the re-trace left them on disk under buildDir) and
	// emits a write_file / genrule per output, registered in cc.OutToGenrule;
	// SeenBuilds[b] is set so the standalone pass doesn't re-emit a raw `cmake -P`.
	declaredOuts := append(append([]string(nil), attributed...), genruleOuts(b, buildDir)...)
	sort.Strings(declaredOuts)
	declaredOuts = dedupSorted(declaredOuts)

	n := len(cc.Genrules)
	// EXTRACT over BAKE: if the script writes the orphans with a real TOOL (an
	// execute_process the script ran into OUTPUT_DIR), recover a REGENERATING
	// genrule that re-runs that tool — higher fidelity than freezing the bytes.
	// Declines for the file(WRITE) literal shape (no tool writes the orphans),
	// which then falls through to the byte bake below — correct for cmake-op
	// writes (no tool to extract).
	if cc.extractOutputDirOrphanTool(b, script, cmakeSrc, buildDir, dArgs, attributed, declaredOuts, g) {
		for i := n; i < len(cc.Genrules); i++ {
			cc.Genrules[i].Tags = append(cc.Genrules[i].Tags, "cmake-codegen-output-dir-orphan")
		}
		return
	}
	if _, _, ok := bakeCmakeScriptGenrule(cc, b, cmd, script, buildDir, g, declaredOuts); !ok {
		// A failed bake may have registered SOME outputs before hitting a missing
		// one; roll the appended genrules back so a partial leaves no half-emitted
		// producer. (The OutToGenrule registrations a partial bake made are inert —
		// a later recovery / the refusal still fires for the unattributed orphan.)
		cc.Genrules = cc.Genrules[:n]
		return
	}
	// Audit facet: distinguish this attribution from an ordinary cmake -P bake.
	for i := n; i < len(cc.Genrules); i++ {
		cc.Genrules[i].Tags = append(cc.Genrules[i].Tags, "cmake-codegen-output-dir-orphan")
	}
}

// extractOutputDirOrphanTool recovers the OUTPUT_DIR orphan codegen as a
// REGENERATING tool genrule (extract over bake): when a SINGLE liftable tool
// call in the re-traced script writes into the orphans' directory (a python3 /
// protoc / sh the script ran into OUTPUT_DIR), substitute that tool argv for
// `cmake -P <script>` and emit a genrule declaring the attributed orphans (+ the
// edge's stamp) via emitRecoveredGenrule's outsOverride — which anchors the
// tool's OUTPUT_DIR to $(RULEDIR) and registers each declared out in
// cc.OutToGenrule. Returns true on success.
//
// Declines (→ caller bakes, never worse than before) when: the orphans don't
// share one parent dir (the single-writer selection needs the OUTPUT_DIR), the
// re-trace yields no calls, NO liftable tool writes into the orphans' dir (the
// file(WRITE) literal shape — there is no tool to extract, so the bake is the
// right answer), more than one does (ambiguous), or the emit didn't end up
// claiming every attributed orphan (all-or-nothing — rolled back so the bake
// still covers the whole set).
func (cc *codegenContext) extractOutputDirOrphanTool(b *ninja.Build, script, cmakeSrc, buildDir string, dArgs, attributed, declaredOuts []string, g *ninja.Graph) bool {
	outsParent := path.Dir(attributed[0])
	for _, o := range attributed {
		if path.Dir(o) != outsParent {
			return false
		}
	}
	calls, relocs := cc.expandCommandSourcesAndRelocations(script, dArgs, cmakeSrc, buildDir)
	if len(calls) == 0 {
		return false
	}
	// outerBuildDirs lets argvWritesToDir anchor a CROSS-BOUNDARY OUTPUT_DIR: a
	// nested satellite's tool writes into `<OUTER_BUILD>/<outsParent>`, which only
	// resolves to outsParent ("gen") once the outer build dirs are known. Without
	// it the cross-boundary tool argv looks like it writes outside the build tree
	// and the extract declines to a bake (the satellite's regression).
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir, outerBuildDirs: cc.OuterBuildDirs}
	var chosen []string
	for _, raw := range calls {
		// producerCandidate bundles the relocation skip (a `cmake -E copy … OUTPUT_DIR`
		// is a relocation, not the generator) + argvCodegenEligibleRelaxed + liftable.
		// Site test: the tool writes DIRECTLY into the orphans' dir. The temp-dir-
		// relocate path below recovers a tool that instead writes to a tempdir.
		c, cand := cc.producerCandidate(raw, anc, argvCodegenEligibleRelaxed)
		if !cand || !argvWritesToDir(c.Commands[0], outsParent, anc) {
			continue
		}
		if chosen != nil {
			return false // ambiguous producer — bake rather than guess
		}
		chosen = c.Commands[0]
	}
	if chosen == nil {
		// No tool writes DIRECTLY into the orphans' dir. Before falling through to
		// the byte-bake, try the TEMP-DIR-THEN-COPY wrapper shape: a tool runs with
		// WORKING_DIRECTORY=<tmp> (its argv names the tempdir, not OUTPUT_DIR — so
		// the direct-write scan above can't see it) and a `cmake -E copy`/`rename` or
		// `file(COPY)`/`file(RENAME)` relocation moves its output into OUTPUT_DIR.
		// recoverTempDirToolRelocate already recovers this for standalone edges from
		// (calls + relocs); drive it with the ATTRIBUTED ORPHANS as the relocation
		// destinations (the orphan edge declares only its stamp). Checkpoint so a
		// partial claim rolls back to the bake.
		if cc.recoverOutputDirOrphanTempDirRelocate(b, calls, relocs, cmakeSrc, buildDir, attributed, g) {
			return true
		}
		return false // no tool writes the orphans (file(WRITE) literals) → bake
	}
	// All-or-nothing checkpoint (the same shape tryStandaloneCmakeScriptCodegen
	// uses): checkpointCodegen snapshots Genrules + OutToGenrule +
	// OutToNativeConsumerDep + the recognizer/stamp maps, so restoreCodegen unwinds
	// a recognizer match (OutToNativeConsumerDep) too, not just the genrule
	// fallback. SeenBuilds is keyed by *ninja.Build (not an output path) so the
	// checkpoint doesn't cover it — guard it separately: delete the key on rollback
	// only if it was ABSENT before (writing back "" would leave a phantom
	// present-with-empty-value entry a downstream `_, ok := SeenBuilds[b]` misreads).
	cp := cc.checkpointCodegen()
	_, hadSeen := cc.SeenBuilds[b]
	_, name, err := cc.emitRecoveredGenrule(b, strings.Join(chosen, " "), cmakeSrc, buildDir, attributed[0], g, declaredOuts)
	if err != nil {
		return false
	}
	// Every attributed orphan must be claimed, else the genrule covers only part of
	// the set — roll back so the bake still covers the whole edge.
	for _, o := range attributed {
		if !cc.outputClaimed(o) {
			cc.restoreCodegen(cp)
			if !hadSeen {
				delete(cc.SeenBuilds, b)
			}
			return false
		}
	}
	// The substituted tool replaced `cmake -P <script>`, so the wrapper `.cmake`
	// (a DEPENDS input genruleSrcs carried) is now a dead src — drop it (the same
	// cleanup the tool-shape recoveries apply, G10).
	cc.dropSubstitutedWrapperScriptSrc(name)
	return true
}

// recoverOutputDirOrphanTempDirRelocate drives recoverTempDirToolRelocate for the
// OUTPUT_DIR orphan caller: the attributed orphans are the relocation
// destinations (the orphan edge declares only its stamp, so genruleOuts(b) is the
// wrong destination set). Wraps the call in an all-or-nothing checkpoint — if the
// recovery doesn't end up claiming EVERY attributed orphan (a partial relocation
// map, an ambiguous tool, …), roll back so the caller's byte-bake still covers the
// whole edge. Returns true only when all attributed orphans are claimed.
func (cc *codegenContext) recoverOutputDirOrphanTempDirRelocate(b *ninja.Build, calls []shadow.ExecuteProcessCall, relocs []scriptRelocation, cmakeSrc, buildDir string, attributed []string, g *ninja.Graph) bool {
	cp := cc.checkpointCodegen()
	_, hadSeen := cc.SeenBuilds[b]
	if _, ok := cc.recoverTempDirToolRelocate(b, calls, relocs, cmakeSrc, buildDir, attributed[0], g, attributed); ok {
		allClaimed := true
		for _, o := range attributed {
			if !cc.outputClaimed(o) {
				allClaimed = false
				break
			}
		}
		if allClaimed {
			return true
		}
	}
	cc.restoreCodegen(cp)
	if !hadSeen {
		delete(cc.SeenBuilds, b)
	}
	return false
}

// unclaimedConsumedOrphans returns the consumed build-dir sources (codemodel
// demand) that no ninja edge produces (cc.NinjaOuts) and no earlier recovery
// already claimed (cc.outputClaimed) — the orphan set the OUTPUT_DIR attribution
// draws from. Sorted and deduped for deterministic emission.
//
// Demand has TWO sources, so a satellite sub-project whose `cmake -P` edges
// produce sources the OUTER project consumes can still attribute them:
//
//   - cc.ConsumedBuildRel — the LOCAL demand (a compile target in THIS cmake
//     build consumes the source). Keys are relative to THIS build dir.
//   - cc.OuterConsumedBuildRel — the CROSS-BOUNDARY demand threaded down from the
//     outer project (an outer compile target consumes a source THIS nested edge
//     produces into the outer build tree). Keys are relative to the OUTER build
//     dir; the on-disk corroboration in recoverOutputDirOrphans resolves them
//     against cc.OuterBuildDirs.
func (cc *codegenContext) unclaimedConsumedOrphans() []string {
	seen := map[string]bool{}
	add := func(o string) {
		if cc.NinjaOuts[o] || cc.outputClaimed(o) {
			return
		}
		seen[o] = true
	}
	for o := range cc.ConsumedBuildRel {
		add(o)
	}
	for _, o := range cc.OuterConsumedBuildRel {
		add(o)
	}
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// orphanOnDisk corroborates that a re-trace materialized the consumed orphan o
// as a regular file — the same on-disk check the declared-output rungs use,
// extended to be CROSS-BOUNDARY. A local orphan lands under buildDir; a
// cross-boundary orphan (one an outer target consumes) lands under an OUTER
// build dir, since the satellite's `cmake -P` writes into `<OUTER_BUILD>/...`.
// Try buildDir first, then each cc.OuterBuildDirs root.
func (cc *codegenContext) orphanOnDisk(o, buildDir string) bool {
	_, ok := fileUnderBuildRoots(o, buildDir, cc.OuterBuildDirs)
	return ok
}

// buildCorroborationRoots returns the ordered on-disk roots a build-relative
// generated path may live under: the LOCAL build dir first, then each ancestor
// (outer) build dir. A CROSS-BOUNDARY generated output — a nested/satellite
// sub-build writing UP into an outer build tree — lives under an outer root, so
// any on-disk corroboration (does the trace's declared output exist?) or byte
// read must walk all of them, not just buildDir. Empty roots are dropped.
func buildCorroborationRoots(buildDir string, outerDirs []string) []string {
	roots := make([]string, 0, 1+len(outerDirs))
	if buildDir != "" {
		roots = append(roots, buildDir)
	}
	for _, d := range outerDirs {
		if d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

// fileUnderBuildRoots reports the first corroboration root (buildDir, then each
// outer build dir) under which rel exists as a NON-DIRECTORY (a produced output
// file — the same "exists and isn't a dir" test the declared-output rungs and the
// original orphanOnDisk use; a symlink to a generated file counts), ok=false if
// none. The single home of the "does this generated output exist on disk, local
// OR cross-boundary" check — the corroboration every tool-shape recovery does
// before claiming an output, generalized to the outer build tree.
func fileUnderBuildRoots(rel, buildDir string, outerDirs []string) (root string, ok bool) {
	for _, r := range buildCorroborationRoots(buildDir, outerDirs) {
		if st, err := os.Stat(filepath.Join(r, filepath.FromSlash(rel))); err == nil && !st.IsDir() {
			return r, true
		}
	}
	return "", false
}

// dirUnderBuildRoots is fileUnderBuildRoots for a DIRECTORY: it returns the first
// corroboration root (buildDir, then each outer build dir) under which rel exists
// as a directory. Used by the codegen out-dir walk, whose OUTPUT_DIR may live in
// an outer build tree cross-boundary — the walk must then enumerate under the
// OWNING root and relativize against it, not the local build dir.
func dirUnderBuildRoots(rel, buildDir string, outerDirs []string) (root string, ok bool) {
	for _, r := range buildCorroborationRoots(buildDir, outerDirs) {
		if st, err := os.Stat(filepath.Join(r, filepath.FromSlash(rel))); err == nil && st.IsDir() {
			return r, true
		}
	}
	return "", false
}

// precreateOutputDirOrphanDirs creates the directories a standalone re-trace of
// the script needs to exist UP FRONT: the edge's stamp outputs' dirs and any
// `-D<VAR>=<build-subdir>` directory the cache args carry (the OUTPUT_DIR is
// handed to the script that way). The real custom command runs a sibling
// `cmake -E make_directory` COMMAND before the `cmake -P`, which the standalone
// re-trace doesn't — so a tool the script runs into OUTPUT_DIR would fail on the
// missing dir. Only paths that anchor under buildDir OR an ancestor (outer) build
// dir are created: the CROSS-BOUNDARY satellite shape hands the script an
// OUTPUT_DIR in the OUTER build tree (`-DOUTPUT_DIR=<OUTER_BUILD>/gen`), so the
// guard must accept the outer roots too or the re-trace's tool fails on a missing
// dir whenever the real build hasn't already materialized it.
func (cc *codegenContext) precreateOutputDirOrphanDirs(b *ninja.Build, dArgs []string, cmakeSrc, buildDir string) {
	// insideAnyBuildRoot reports whether abs anchors under buildDir or one of the
	// ancestor build dirs (relaxed, non-"." / non-"../"), the precondition for
	// pre-creating it. A path the script writes outside every build tree is not
	// an OUTPUT_DIR the orphan attribution owns, so it is left alone.
	insideAnyBuildRoot := func(abs string) bool {
		for _, root := range append([]string{buildDir}, cc.OuterBuildDirs...) {
			if root == "" {
				continue
			}
			if rel, ok := relativeIfInsideRelaxed(root, abs); ok && rel != "." && !strings.HasPrefix(rel, "../") {
				return true
			}
		}
		return false
	}
	mkUnderBuild := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(buildDir, filepath.FromSlash(p))
		}
		if insideAnyBuildRoot(p) {
			_ = os.MkdirAll(p, 0o755)
		}
	}
	for _, o := range genruleOuts(b, buildDir) {
		mkUnderBuild(path.Dir(o)) // the stamp's dir — typically IS the OUTPUT_DIR
	}
	for _, d := range dArgs {
		eq := strings.IndexByte(d, '=')
		if eq < 0 {
			continue
		}
		v := d[eq+1:]
		// Always create the value's PARENT (safe whether the value is a dir or a
		// file). Create the value ITSELF as a dir only when it has no file
		// extension — an OUTPUT_DIR (`gen`) vs a file arg (`manifest.cmake`): a
		// MkdirAll on a file path would create a DIRECTORY where the script then
		// can't write the file, truncating the trace.
		mkUnderBuild(filepath.Dir(v))
		if path.Ext(filepath.ToSlash(v)) == "" {
			mkUnderBuild(v)
		}
	}
}

// tracedScriptWriteDirs re-traces a `cmake -P <script>` and returns the set of
// build-relative DIRECTORIES the script's writes land in — the "OUTPUT_DIR"
// detected from where the writes ACTUALLY go (not a hardcoded variable name).
// Two write channels:
//
//   - cmake-level writes — file(WRITE/APPEND/…) + configure_file outputs. Each
//     target is an absolute path reduced to its PARENT dir (the file lands in
//     that dir).
//   - TOOL writes — the script's execute_process tool argvs. A tool the script
//     runs (python3 / protoc / sh) writes its outputs into a directory it's
//     handed as an operand (an OUTPUT_DIR), which the cmake trace records only as
//     the argv, not as a file write. So each build-subdir an execute_process
//     argv NAMES is a candidate write dir (the operand itself when it IS a dir,
//     and its parent when the operand is a file in that dir). The downstream
//     attribution gates this — an orphan is claimed only when it's under such a
//     dir AND the re-trace materialized it on disk — so naming a dir the tool
//     merely reads from can't over-attribute (a read input isn't an unclaimed
//     consumed orphan).
//
// A write that anchors outside the build dir (or to the build root itself) is
// ignored — the orphan attribution only owns sources a compile target consumes
// from a build SUBDIR. Empty when the trace fails or the script writes nothing
// build-dir-local.
//
// Returns TWO sets. `dirs` is the wide ATTRIBUTION set above (operand + its
// parent for tool argvs, since a file operand's writes land in the parent).
// `primary` is the narrower set of dirs the script DEFINITELY writes into —
// file(WRITE)/configure_file parents and tool operands themselves, but NOT the
// speculative parent-of-a-tool-operand. The over-attribution guard uses
// `primary`: when several `cmake -P` edges each target a DISTINCT subdir of a
// shared parent (`gen/a`, `gen/b`), the speculative `gen` parent the wide set
// adds to every edge would be a PHANTOM overlap firing the guard against all of
// them; `primary` keeps only the real targets, so disjoint subdirs don't
// contend.
func (cc *codegenContext) tracedScriptWriteDirs(scriptArg string, dArgs []string, cmakeSrc, buildDir string) (dirs, primary map[string]bool) {
	traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptArg, dArgs, "")
	if err != nil {
		return nil, nil
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir, outerBuildDirs: cc.OuterBuildDirs}
	dirs = map[string]bool{}
	primary = map[string]bool{}
	// addFileDir adds the PARENT dir of a written FILE (a file(WRITE) / configure_file
	// output lands IN its parent dir). A definite write → both sets.
	addFileDir := func(abs string) {
		rel, ok := executeProcessAnchorOutput(abs, anc)
		if !ok || rel == "" {
			return
		}
		if dir := path.Dir(rel); dir != "." && dir != "" {
			dirs[dir] = true
			primary[dir] = true
		}
	}
	// addArgvDir adds a build-subdir a tool argv NAMES: the operand itself when it
	// resolves to a build-subdir (the OUTPUT_DIR-is-a-dir case), and its parent
	// when the operand is a file within a build-subdir (the file-in-OUTPUT_DIR case).
	// The operand is a real target → both sets; its parent is SPECULATIVE (only the
	// real write dir if the operand is a file) → attribution set only, so it can't
	// phantom-fire the over-attribution guard for sibling subdirs.
	addArgvDir := func(abs string) {
		rel, ok := executeProcessAnchorOutput(abs, anc)
		if !ok || rel == "" {
			return
		}
		if rel != "." {
			dirs[rel] = true
			primary[rel] = true
		}
		if dir := path.Dir(rel); dir != "." && dir != "" {
			dirs[dir] = true
		}
	}
	dec := shadow.Decode(traceRaw, cmakeSrc, nil)
	// file(WRITE/APPEND/TOUCH/COPY/COPY_FILE) writers. cmakeSrc as sourceRoot,
	// buildDir as buildRoot so a build-tree-issued (generated+include()d) recipe
	// writer is kept (the [13]/[14] in-project + projectIO gate).
	for _, w := range shadow.ExtractFileWriterCalls(traceRaw, cmakeSrc, buildDir) {
		for _, o := range w.Outputs {
			addFileDir(o)
		}
	}
	// configure_file outputs (the COPYONLY / template-substituted file copy a
	// script can use to emit a generated source).
	for _, c := range dec.ConfigFiles {
		if c.Output != "" {
			addFileDir(c.Output)
		}
	}
	// execute_process tool argvs — the tool-driven OUTPUT_DIR channel. Both in-tree
	// and out-of-tree calls (the script may run a tool from anywhere); every
	// build-subdir an argv operand names is a candidate.
	for _, calls := range [][]shadow.ExecuteProcessCall{dec.ExecuteProcesses, dec.OutOfTreeExecuteProcesses} {
		for _, c := range calls {
			if len(c.Commands) == 0 {
				continue
			}
			for _, a := range c.Commands[0][1:] {
				addArgvDir(stripArgvPathPrefix(argvFlagValue(a)))
			}
		}
	}
	return dirs, primary
}

// outputDirArgSet returns the build-relative directories the edge's `-D` cache
// args NAME — a `-D<VAR>=<value>` whose value anchors to a build-subdir (local
// or outer build tree) AND exists ON DISK as a directory. The temp-dir-then-copy
// shape hands the script its exact hash-suffixed OUTPUT_DIR that way
// (`-DOUTPUT_DIR=<build>/gen/comp/<hash>`), so this is the authoritative,
// trace-independent statement of where the edge writes its codegen — used to
// CONSTRAIN attribution against the parent-expansion leak the traced write set
// carries. Var-name-agnostic: keyed on the value, not on a hardcoded `OUTPUT_DIR`
// spelling (the `-D` var is project-specific). The on-disk DIRECTORY test (the
// re-trace + precreateOutputDirOrphanDirs materialized the OUTPUT_DIR) is the
// robust, extension-agnostic filter — a file-valued arg (`-DMANIFEST=out.cmake`,
// on disk as a file), a source path (`-DTOOL=x.py`), and a scalar all fail it,
// while a dotted dir name (`foo.pb-<hash>`) passes. Empty when the edge names no
// such dir arg — attribution then falls back to the traced write set unchanged.
func (cc *codegenContext) outputDirArgSet(dArgs []string, buildDir string) map[string]bool {
	anc := execAnchors{recordedBuildDir: buildDir, outerBuildDirs: cc.OuterBuildDirs}
	out := map[string]bool{}
	for _, d := range dArgs {
		eq := strings.IndexByte(d, '=')
		if eq < 0 {
			continue
		}
		rel, ok := executeProcessAnchorOutput(stripArgvPathPrefix(d[eq+1:]), anc)
		if !ok || rel == "" || rel == "." {
			continue
		}
		if _, isDir := dirUnderBuildRoots(rel, buildDir, cc.OuterBuildDirs); !isDir {
			continue // a file-valued / source / scalar arg is not an output dir
		}
		out[rel] = true
	}
	return out
}

// attributesOrphanDir reports whether an orphan living in directory d is
// attributable to an edge whose traced writes cover writeDirs and whose `-D`
// cache args name outputDirs. writeDirs remains the precondition (the edge must
// actually write there), so this can only NARROW attribution, never widen it.
// When the edge names explicit OUTPUT_DIRs, an orphan must live UNDER one of them
// — dropping the speculative PARENT the traced write set adds for a dir operand
// (addArgvDir), which otherwise over-claims files sitting directly under a shared
// component dir. With no `-D`-named output dir (a shape that doesn't pass one),
// fall back to the traced write set unchanged (today's behavior).
func attributesOrphanDir(d string, writeDirs, outputDirs map[string]bool) bool {
	if !writeDirs[d] {
		return false
	}
	if len(outputDirs) == 0 {
		return true
	}
	return dirWithinAny(d, outputDirs)
}

// dirWithinAny reports whether build-relative dir d is one of set OR nested under
// one of set (an OUTPUT_DIR the edge names, or a subdir the codegen writes into
// beneath it). A proper ANCESTOR of a set member (the speculative parent) is NOT
// within — that's the leak dirWithinAny screens out.
func dirWithinAny(d string, set map[string]bool) bool {
	for od := range set {
		if d == od || strings.HasPrefix(d, od+"/") {
			return true
		}
	}
	return false
}

// otherScriptEdgeWritesTo reports whether any OTHER `cmake -P` custom-command
// edge (≠ self) DEFINITELY writes into one of `dirs` (the attributed orphans'
// dirs) — the over-attribution guard. When two script edges both write into the
// same dir an orphan lives in, attributing it to either is a guess, so the caller
// declines. The comparison uses each other edge's PRIMARY write dirs (real
// targets, not the speculative parent-of-a-tool-operand), so edges writing into
// DISTINCT subdirs of a shared parent do not phantom-contend on that parent.
// Re-traces each candidate script edge (bounded: only cmake -P edges, and only
// until the first overlap is found). A non-script edge, or a script whose
// re-trace fails / writes elsewhere, can't contend, so it doesn't trigger the
// guard.
func (cc *codegenContext) otherScriptEdgeWritesTo(self *ninja.Build, selfCmd string, dirs map[string]bool, edges []*ninja.Build, g *ninja.Graph, cmakeSrc, buildDir string) bool {
	for _, e := range edges {
		if e == self {
			continue
		}
		cmd, ok := ninja.CommandFor(g, e)
		if !ok || cmd == "" {
			continue
		}
		cmd = cc.realCmakeCommandForEdge(e, cmd, buildDir)
		if cmd == selfCmd || !usesCmakeScriptMode(cmd) {
			continue
		}
		script := extractCmakeScriptPath(cmd)
		if script == "" || script == "<unknown-script>" {
			continue
		}
		_, otherPrimary := cc.tracedScriptWriteDirs(script, extractCmakePDashArgs(cmd), cmakeSrc, buildDir)
		for d := range otherPrimary {
			if dirs[d] {
				return true
			}
		}
	}
	return false
}
