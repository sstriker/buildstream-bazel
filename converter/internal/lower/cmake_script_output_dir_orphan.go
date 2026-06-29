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
// Gated on RecognizeCodegen + CMakeScriptTrace + a usable cmake (the script
// re-trace's opt-in) — off → no-op, today's behavior holds.
func (cc *codegenContext) recoverOutputDirOrphanEdges(g *ninja.Graph, cmakeSrc, buildDir string) {
	if cc == nil || g == nil || !cc.RecognizeCodegen || !cc.CMakeScriptTrace || cc.CMakeBinary == "" || buildDir == "" {
		return
	}
	if len(cc.ConsumedBuildRel) == 0 {
		return // no demand side — nothing a consumer needs a producer for
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
	writeDirs := cc.tracedScriptWriteDirs(script, dArgs, cmakeSrc, buildDir)
	if len(writeDirs) == 0 {
		return
	}

	// ATTRIBUTE: an orphan whose parent dir is one this edge writes into, and
	// which the re-trace materialized on disk under the build dir (corroboration).
	var attributed []string
	for _, o := range orphans {
		if !writeDirs[path.Dir(o)] {
			continue
		}
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			continue // the re-trace didn't actually produce it — don't claim it
		}
		attributed = append(attributed, o)
	}
	if len(attributed) == 0 {
		return
	}

	// Over-attribution guard: if ANY OTHER `cmake -P` edge's traced writes target
	// one of this edge's write dirs, the orphan ownership is ambiguous — decline
	// rather than guess which edge produces the shared-dir source. Checked only
	// once we have an attribution candidate (the re-trace of every other script
	// edge is the costly step; skip it when this edge owns nothing).
	if cc.otherScriptEdgeWritesTo(b, cmd, writeDirs, edges, g, cmakeSrc, buildDir) {
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
	calls, _ := cc.expandCommandSourcesAndRelocations(script, dArgs, cmakeSrc, buildDir)
	if len(calls) == 0 {
		return false
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}
	var chosen []string
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if !argvCodegenEligibleRelaxed(c) || !argvToolLiftable(c.Commands[0][0], anc, cc) {
			continue
		}
		if !argvWritesToDir(c.Commands[0], outsParent, anc) {
			continue
		}
		if chosen != nil {
			return false // ambiguous producer — bake rather than guess
		}
		chosen = c.Commands[0]
	}
	if chosen == nil {
		return false // no tool writes the orphans (file(WRITE) literals) → bake
	}
	n := len(cc.Genrules)
	seenBefore := cc.SeenBuilds[b]
	_, name, err := cc.emitRecoveredGenrule(b, strings.Join(chosen, " "), cmakeSrc, buildDir, attributed[0], g, declaredOuts)
	if err != nil {
		return false
	}
	// All-or-nothing: every attributed orphan must be claimed, else the genrule
	// covers only part of the set — roll back (genrules + the OutToGenrule entries
	// this emit added + SeenBuilds) so the bake still covers the whole edge.
	for _, o := range attributed {
		if !cc.outputClaimed(o) {
			cc.Genrules = cc.Genrules[:n]
			for _, d := range declaredOuts {
				if cc.OutToGenrule[d] == name {
					delete(cc.OutToGenrule, d)
				}
			}
			cc.SeenBuilds[b] = seenBefore
			return false
		}
	}
	// The substituted tool replaced `cmake -P <script>`, so the wrapper `.cmake`
	// (a DEPENDS input genruleSrcs carried) is now a dead src — drop it (the same
	// cleanup the tool-shape recoveries apply, G10).
	cc.dropSubstitutedWrapperScriptSrc(name)
	return true
}

// unclaimedConsumedOrphans returns the consumed build-dir sources (codemodel
// demand) that no ninja edge produces (cc.NinjaOuts) and no earlier recovery
// already claimed (cc.outputClaimed) — the orphan set the OUTPUT_DIR attribution
// draws from. Sorted for deterministic emission.
func (cc *codegenContext) unclaimedConsumedOrphans() []string {
	var out []string
	for o := range cc.ConsumedBuildRel {
		if cc.NinjaOuts[o] || cc.outputClaimed(o) {
			continue
		}
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// precreateOutputDirOrphanDirs creates, under buildDir, the directories a
// standalone re-trace of the script needs to exist UP FRONT: the edge's stamp
// outputs' dirs and any `-D<VAR>=<build-subdir>` directory the cache args carry
// (the OUTPUT_DIR is handed to the script that way). The real custom command runs
// a sibling `cmake -E make_directory` COMMAND before the `cmake -P`, which the
// standalone re-trace doesn't — so a tool the script runs into OUTPUT_DIR would
// fail on the missing dir. Only paths that anchor under buildDir are created.
func (cc *codegenContext) precreateOutputDirOrphanDirs(b *ninja.Build, dArgs []string, cmakeSrc, buildDir string) {
	mkUnderBuild := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(buildDir, filepath.FromSlash(p))
		}
		if rel, ok := relativeIfInsideRelaxed(buildDir, p); ok && rel != "." && !strings.HasPrefix(rel, "../") {
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
func (cc *codegenContext) tracedScriptWriteDirs(scriptArg string, dArgs []string, cmakeSrc, buildDir string) map[string]bool {
	traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptArg, dArgs, "")
	if err != nil {
		return nil
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}
	dirs := map[string]bool{}
	// addFileDir adds the PARENT dir of a written FILE (a file(WRITE) / configure_file
	// output lands IN its parent dir).
	addFileDir := func(abs string) {
		rel, ok := executeProcessAnchorOutput(abs, anc)
		if !ok || rel == "" {
			return
		}
		if dir := path.Dir(rel); dir != "." && dir != "" {
			dirs[dir] = true
		}
	}
	// addArgvDir adds a build-subdir a tool argv NAMES: the operand itself when it
	// resolves to a build-subdir (the OUTPUT_DIR-is-a-dir case), and its parent
	// when the operand is a file within a build-subdir (the file-in-OUTPUT_DIR case).
	addArgvDir := func(abs string) {
		rel, ok := executeProcessAnchorOutput(abs, anc)
		if !ok || rel == "" {
			return
		}
		if rel != "." {
			dirs[rel] = true
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
	return dirs
}

// otherScriptEdgeWritesTo reports whether any OTHER `cmake -P` custom-command
// edge (≠ self) writes into one of dirs — the over-attribution guard. When two
// script edges both target the same OUTPUT_DIR, attributing a shared-dir orphan
// to either is a guess, so the caller declines. Re-traces each candidate script
// edge (bounded: only cmake -P edges, and only until the first overlap is
// found). A non-script edge, or a script whose re-trace fails / writes
// elsewhere, can't contend, so it doesn't trigger the guard.
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
		other := cc.tracedScriptWriteDirs(script, extractCmakePDashArgs(cmd), cmakeSrc, buildDir)
		for d := range other {
			if dirs[d] {
				return true
			}
		}
	}
	return false
}
