package lower

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Unspecified-output execute_process codegen: `tool <input…>` whose outputs
// do NOT appear in the argv. The lift is DECLARATIVE — no convert-time
// re-execution, no per-tool tables, no timestamps — combining the three
// sources the converter already holds:
//
//   - File API (codemodel): consumed sources anchored under the BUILD dir
//     are the demand side — only files the build graph consumes need a
//     producer (cc.ConsumedBuildRel, populated by ToIR).
//   - ninja: anything with a build edge is build-time codegen, already
//     recovered (cc.NinjaOuts) — its absence is the orphan signal.
//   - trace argv: the producer linkage. Two signal classes:
//     (1) DIRECTORY-operand containment — the dominant real-world shapes
//     DO name an output directory (`protoc --cpp_out=<dir>`, `-d <dir>`):
//     on-disk files under that argv dir are the call's outputs, and the
//     re-run emission is clean because the tool is TOLD where to write
//     (the operand rewrites to the declared out location);
//     (2) derived-name correlation — a consumed orphan whose basename is
//     the argv input's name + suffix (`f → f.gz`) or stem + suffix
//     (`foo.proto → foo.pb.cc`); emission is the bake tier (placement at
//     re-run is writer-style-dependent), with the convert-time-baking
//     warning riding the tag.
//
// Ambiguity declines, never guesses: two eligible calls sharing (or
// nesting) a directory operand disqualify both; a stem-matched orphan
// claimed by more than one call is assigned to neither. Declined calls
// keep the loud refusal + conversion-todos mirror.

// unspecAssignments is planUnspecifiedOutputs' result: per call index, the
// non-overlapping build-relative directory operand and/or the uniquely
// stem-claimed consumed orphan rels.
type unspecAssignments struct {
	dirOperand map[int]string
	stemOuts   map[int][]string
}

// consumedBuildDirSources collects every codemodel target source that
// anchors under the build dir, as build-relative slash paths — the
// demand-side set for the derived-name linkage. IsGenerated is NOT
// required: a configure-time-written file exists when cmake stats it, so
// the codemodel records it as an ordinary source. Only ABSOLUTE paths
// qualify: the codemodel records a relative source path relative to the
// SOURCE dir, never the build dir.
func consumedBuildDirSources(r *fileapi.Reply, cmakeBuild string) map[string]bool {
	out := map[string]bool{}
	for _, t := range r.Targets {
		for _, src := range t.Sources {
			if !filepath.IsAbs(src.Path) {
				continue
			}
			if rel, ok := relativeIfInsideRelaxed(cmakeBuild, src.Path); ok && rel != "" {
				out[rel] = true
			}
		}
	}
	return out
}

// ninjaOutputSet collects every path any ninja build statement produces
// (build-relative as ninja records them) — the build-time-codegen
// exclusion for orphan identification.
func ninjaOutputSet(g *ninja.Graph) map[string]bool {
	if g == nil {
		return nil
	}
	out := map[string]bool{}
	for _, b := range g.Builds {
		for _, o := range b.Outputs {
			out[o] = true
		}
		for _, o := range b.ImplicitOuts {
			out[o] = true
		}
	}
	return out
}

// planUnspecifiedOutputs is the cross-call linkage pre-pass. Directory
// operands are collected per eligible refused call and disqualified on
// overlap (equal or nested dirs across DISTINCT call sites — first-writer
// attribution would be a guess); stem claims are collected per consumed
// orphan and kept only when exactly one call site claims it. Duplicate
// trace entries of one call (configure re-evaluation re-runs the same
// file:line) count as a single claim — the same call can't be ambiguous
// with itself — and every duplicate index gets the assignment (emission
// reuses via OutToGenrule).
func planUnspecifiedOutputs(calls []shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) unspecAssignments {
	plan := unspecAssignments{dirOperand: map[int]string{}, stemOuts: map[int][]string{}}
	if anc.hostBuildDir == "" {
		return plan
	}
	cl := collectUnspecClaims(calls, anc, cc)
	resolveUnspecDirClaims(cl.dirClaims, plan.dirOperand)
	// Stem claims: keep orphans claimed by exactly one SITE.
	for orphan, cis := range cl.stemClaims {
		if len(cl.stemClaimSites[orphan]) == 1 {
			for _, ci := range cis {
				plan.stemOuts[ci] = append(plan.stemOuts[ci], orphan)
			}
		}
	}
	for ci := range plan.stemOuts {
		sort.Strings(plan.stemOuts[ci])
	}
	return plan
}

// unspecDirClaim is one call site's claim on a build-relative directory
// operand; cis carries every duplicate trace index of that site.
type unspecDirClaim struct {
	cis  []int
	site string
	rel  string
}

// unspecClaims is collectUnspecClaims' result.
type unspecClaims struct {
	dirClaims      []unspecDirClaim
	stemClaims     map[string][]int           // orphan rel -> claiming call indexes
	stemClaimSites map[string]map[string]bool // orphan rel -> claiming sites
}

// collectUnspecClaims walks the eligible refused calls and gathers their
// directory-operand and derived-name claims, deduping same-site duplicate
// trace entries into one dir claim.
func collectUnspecClaims(calls []shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) unspecClaims {
	cl := unspecClaims{stemClaims: map[string][]int{}, stemClaimSites: map[string]map[string]bool{}}
	dirClaimIdx := map[string]int{} // site+"\x00"+rel -> dirClaims index
	for ci, call := range calls {
		if !argvCodegenEligibleRelaxed(call) || !isCodegenCandidateBucket(Classify(call).Bucket) {
			continue
		}
		site := fmt.Sprintf("%s:%d", call.File, call.Line)
		argv := call.Commands[0]
		for _, rel := range argvDirOperands(argv, anc) {
			key := site + "\x00" + rel
			if j, dup := dirClaimIdx[key]; dup {
				cl.dirClaims[j].cis = append(cl.dirClaims[j].cis, ci)
				continue
			}
			dirClaimIdx[key] = len(cl.dirClaims)
			cl.dirClaims = append(cl.dirClaims, unspecDirClaim{cis: []int{ci}, site: site, rel: rel})
		}
		inputs := argvInputBasenames(argv, anc)
		if len(inputs) == 0 {
			continue
		}
		for orphan := range cc.ConsumedBuildRel {
			if cc.NinjaOuts[orphan] || !derivedNameMatch(inputs, filepath.Base(orphan)) {
				continue
			}
			cl.stemClaims[orphan] = append(cl.stemClaims[orphan], ci)
			if cl.stemClaimSites[orphan] == nil {
				cl.stemClaimSites[orphan] = map[string]bool{}
			}
			cl.stemClaimSites[orphan][site] = true
		}
	}
	return cl
}

// argvDirOperands returns the build-relative existing-directory operands
// of one argv (past the tool), filtered through usableUnspecOutDir.
func argvDirOperands(argv []string, anc execAnchors) []string {
	var out []string
	for i, a := range argv {
		if i == 0 {
			continue
		}
		rel, ok := argvBuildRel(stripArgvPathPrefix(a), anc)
		if !ok || !usableUnspecOutDir(rel) {
			continue
		}
		// The dir operand may be in an OUTER build tree (cross-boundary): check
		// across the outer build dirs, not just the local one.
		if _, ok := dirUnderBuildRoots(rel, anc.hostBuildDir, anc.outerBuildDirs); ok {
			out = append(out, rel)
		}
	}
	return out
}

// resolveUnspecDirClaims drops every claim that overlaps a DIFFERENT
// site's claim (equal or nested dirs), then drops calls carrying more
// than one surviving operand (two candidate out dirs in one argv is
// ambiguous too), assigning the survivors into dirOperand.
func resolveUnspecDirClaims(dirClaims []unspecDirClaim, dirOperand map[int]string) {
	dirByCi := map[int][]string{}
	for i, c := range dirClaims {
		overlap := false
		for j, o := range dirClaims {
			if i == j || c.site == o.site {
				continue
			}
			if c.rel == o.rel || strings.HasPrefix(c.rel+"/", o.rel+"/") || strings.HasPrefix(o.rel+"/", c.rel+"/") {
				overlap = true
				break
			}
		}
		if !overlap {
			for _, ci := range c.cis {
				dirByCi[ci] = append(dirByCi[ci], c.rel)
			}
		}
	}
	for ci, rels := range dirByCi {
		if len(rels) == 1 {
			dirOperand[ci] = rels[0]
		}
	}
}

// argvBuildRel anchors an argv operand (absolute or process-cwd-relative)
// to a build-relative path.
func argvBuildRel(p string, anc execAnchors) (string, bool) {
	if rel, ok := executeProcessAnchorOutput(p, anc); ok {
		return rel, true
	}
	return relativeArgvBuildRel(p)
}

// usableUnspecOutDir rejects directory operands that can't soundly scope
// outputs: the build root itself and cmake's bookkeeping tree.
func usableUnspecOutDir(rel string) bool {
	return rel != "" && rel != "." && rel != "CMakeFiles" && !strings.HasPrefix(rel, "CMakeFiles/")
}

// argvInputBasenames returns the basenames of argv elements that anchor as
// existing source-tree files or already-produced build files — the input
// evidence the derived-name correlation links against.
func argvInputBasenames(argv []string, anc execAnchors) []string {
	var out []string
	for i, a := range argv {
		if i == 0 {
			continue
		}
		p := stripArgvPathPrefix(a)
		if rel, ok := executeProcessAnchorSource(p, anc); ok && rel != "" {
			if st, err := os.Stat(filepath.Join(anc.hostSrcDir, filepath.FromSlash(rel))); err == nil && !st.IsDir() {
				out = append(out, filepath.Base(rel))
			}
		}
	}
	return out
}

// derivedNameMatch reports whether orphanBase looks derived from one of the
// input basenames: the full input name + a suffix (`f → f.gz`), or the
// input stem (name minus its last extension, ≥3 chars against noise) + a
// suffix opening with '.' or '_' (`foo.proto → foo.pb.cc`, `foo → foo_gen.h`).
func derivedNameMatch(inputs []string, orphanBase string) bool {
	for _, in := range inputs {
		if in == "" || orphanBase == in {
			// An orphan NAMED like the input is a configure-time copy,
			// not a derivation — the copy lifts own that shape.
			continue
		}
		if strings.HasPrefix(orphanBase, in) {
			return true
		}
		stem := strings.TrimSuffix(in, filepath.Ext(in))
		if len(stem) >= 3 && strings.HasPrefix(orphanBase, stem) && len(orphanBase) > len(stem) {
			rest := orphanBase[len(stem):]
			if rest[0] == '.' || rest[0] == '_' {
				return true
			}
		}
	}
	return false
}

// liftUnspecifiedOutputs is the per-call emission half: the dir-operand
// class re-runs the tool at Bazel time with the operand rewritten to the
// declared out location; the stem class bakes the configure-written bytes.
func liftUnspecifiedOutputs(ci int, call shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext, plan unspecAssignments) ([]string, bool) {
	if dirRel, ok := plan.dirOperand[ci]; ok {
		if rels, lifted := liftDirOperandOutputs(call, dirRel, anc, cc); lifted {
			return rels, true
		}
	}
	if orphans := plan.stemOuts[ci]; len(orphans) > 0 {
		// Opt-in live upgrade: re-run the tool as a genrule instead of freezing
		// the configure-written bytes, when placement is sound. Falls back to
		// the bake when it can't (the safe default).
		if cc.LiftDerivedCodegen {
			if rels, lifted := liftDerivedOutputsRerun(call, orphans, anc, cc); lifted {
				return rels, true
			}
		}
		return bakeDerivedOutputs(call, orphans, anc, cc)
	}
	return nil, false
}

// liftDirOperandOutputs enumerates the on-disk files under the argv
// directory operand (minus build-time codegen and already-produced paths)
// as the call's outputs and emits a re-run genrule whose dir operand is
// rewritten to `$(RULEDIR)/<dir>` — the tool is told where to write, so
// placement is sound regardless of writer style.
func liftDirOperandOutputs(call shadow.ExecuteProcessCall, dirRel string, anc execAnchors, cc *codegenContext) ([]string, bool) {
	var rels []string
	registered := 0
	// The dir operand may live in an OUTER build tree (cross-boundary): walk under
	// its OWNING root and relativize against it. A dir under no root defaults to the
	// local build dir (walking a missing path finds nothing → declines below).
	owner, found := dirUnderBuildRoots(dirRel, anc.hostBuildDir, anc.outerBuildDirs)
	if !found {
		owner = anc.hostBuildDir
	}
	root := filepath.Join(owner, filepath.FromSlash(dirRel))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, ok := relativeIfInsideRelaxed(owner, p); ok {
			if !cc.NinjaOuts[rel] {
				if cc.outputClaimed(rel) {
					registered++
				}
				rels = append(rels, rel)
			}
		}
		return nil
	})
	if len(rels) == 0 {
		return nil, false
	}
	sort.Strings(rels)
	if registered == len(rels) {
		// Duplicate trace call: every out under the operand is already
		// registered — reuse, same contract as the other lifts.
		return rels, true
	}
	if registered > 0 {
		// Partial overlap with another recovery's outs — emitting a
		// second producer for a registered path is an analysis error.
		return nil, false
	}

	argv := call.Commands[0]
	if !argvToolLiftable(argv[0], anc, cc) {
		return nil, false
	}
	srcs, rewritten, ok := rewriteArgvUnspecDir(argv, dirRel, anc, cc)
	if !ok {
		return nil, false
	}
	cmd := fmt.Sprintf(`mkdir -p "$(RULEDIR)/%s" && %s`, dirRel, strings.Join(rewritten, " "))
	driver := executeProcessDriverBasename(argv[0])
	if driver == "" {
		driver = "unknown"
	}
	tags := append(fileProducingTags(driver), "cmake-codegen-execute-process-dir-outs")
	sort.Strings(tags)
	name := executeProcessGenruleName(rels[0])
	cc.appendExecProcGenrule(ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleCmd:  cmd,
		GenruleOuts: rels,
		Tags:        tags,
		Visibility:  []string{"//visibility:private"},
	})
	for _, rel := range rels {
		cc.OutToGenrule[rel] = name
	}
	return rels, true
}

// unspecBuildOperandRel classifies one argv element as a BUILD-path
// operand using the argv-codegen lift's discriminator: an absolute path
// anchoring under the build root always is; a clean RELATIVE token only
// when it names the dir operand, a produced file, or something existing
// on disk under the build root. A bare word naming nothing — old-style
// `tar xf` flags, subcommands, mode words — is an ordinary string
// argument, not a path, so it falls through to the literal branch
// instead of declining the lift.
func unspecBuildOperandRel(p, dirRel string, anc execAnchors, cc *codegenContext) (string, bool) {
	if rel, ok := executeProcessAnchorOutput(p, anc); ok {
		return rel, true
	}
	rel, ok := relativeArgvBuildRel(p)
	if !ok {
		return "", false
	}
	if rel == dirRel {
		return rel, true
	}
	if cc.outputClaimed(rel) {
		return rel, true
	}
	if _, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(rel))); err == nil {
		return rel, true
	}
	return "", false
}

// rewriteArgvUnspecDir renders the dir-operand re-run argv: the directory
// operand → `$(RULEDIR)/<dir>`, inputs per the argv-codegen policy
// (including its relative-operand existence discriminator — see
// unspecBuildOperandRel), other build-dir operands decline (an
// unclassified second build path means the shape isn't understood).
func rewriteArgvUnspecDir(argv []string, dirRel string, anc execAnchors, cc *codegenContext) (srcs, rewritten []string, ok bool) {
	srcSet := map[string]bool{}
	addSrc := func(rel string) {
		if !srcSet[rel] {
			srcSet[rel] = true
			srcs = append(srcs, rel)
		}
	}
	emitKeyed := func(a, repl string) string {
		if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/\\") {
			return a[:eq+1] + repl
		}
		return repl
	}
	for i, a := range argv {
		p := stripArgvPathPrefix(a)
		if rel, anchored := unspecBuildOperandRel(p, dirRel, anc, cc); anchored && i > 0 {
			if rel == dirRel {
				rewritten = append(rewritten, emitKeyed(a, "$(RULEDIR)/"+dirRel))
				continue
			}
			if cc.outputClaimed(rel) {
				addSrc(rel)
				rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
				continue
			}
			return nil, nil, false
		}
		if rel, anchored := executeProcessAnchorSource(p, anc); anchored && i > 0 {
			// Source-tree DIRECTORY operands decline (same policy as the
			// argv-codegen lift): an unstaged literal dir is absent/empty
			// under sandboxing — a silent-divergence shape.
			if rel == "" || isExistingDir(filepath.Join(anc.hostSrcDir, rel)) {
				return nil, nil, false
			}
			if _, err := os.Stat(filepath.Join(anc.hostSrcDir, filepath.FromSlash(rel))); err != nil {
				return nil, nil, false
			}
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
			continue
		}
		if i == 0 && filepath.IsAbs(a) {
			rewritten = append(rewritten, shellQuoteArg(filepath.Base(a)))
			continue
		}
		rewritten = append(rewritten, shellQuoteArg(a))
	}
	return srcs, rewritten, true
}

// liftDerivedOutputsRerun is the opt-in (--lift-derived-codegen) live upgrade of
// the derived-name stem-match bake: instead of freezing the configure-written
// bytes, emit a genrule that RE-RUNS the tool. The output isn't an argv operand
// (the tool derives it from the input), so the genrule runs `cd $(RULEDIR)` and
// the tool writes its derived name into the genrule's output dir. That placement
// is sound only when every orphan sits at the build ROOT (a cwd-relative write
// lands directly in $(RULEDIR)); a deeper path, an unliftable tool, or an
// unstageable input declines to the bake. $(location) inputs are made absolute
// (prefixed with the captured exec root $$ROOT) so they survive the cd.
func liftDerivedOutputsRerun(call shadow.ExecuteProcessCall, orphans []string, anc execAnchors, cc *codegenContext) ([]string, bool) {
	rels := append([]string(nil), orphans...)
	sort.Strings(rels)
	registered := 0
	for _, rel := range rels {
		if cc.outputClaimed(rel) {
			registered++
		}
	}
	if registered == len(rels) {
		return rels, true // duplicate trace call: outputs already produced
	}
	if registered > 0 {
		return nil, false // partial overlap with another recovery
	}
	argv := call.Commands[0]
	if !argvToolLiftable(argv[0], anc, cc) {
		return nil, false
	}
	srcs, rewritten, ok := rewriteArgvDerivedRerun(argv, anc, cc)
	if !ok {
		return nil, false
	}
	srcSet := map[string]bool{}
	for _, s := range srcs {
		srcSet[s] = true
	}
	for _, rel := range rels {
		if srcSet[rel] {
			return nil, false // in-place: an orphan that's also a staged (read-only) src
		}
	}
	// Outputs in a SUBDIR (gen/foo.pb.h) re-run fine: after cd $(RULEDIR) the
	// tool writes cwd-relative just as the configure did under the build dir, so
	// the same relative path reproduces — we only have to pre-create the dir the
	// tool won't (mkdir -p, mirroring liftDirOperandOutputs). A build-root output
	// has no parent to create. The mkdirs run RELATIVE to the post-cd cwd.
	var mkdirs []string
	seenDir := map[string]bool{}
	for _, rel := range rels {
		d := path.Dir(rel)
		if d == "." || d == "" || seenDir[d] {
			continue
		}
		seenDir[d] = true
		mkdirs = append(mkdirs, fmt.Sprintf(`mkdir -p %s`, shellQuoteArg(d)))
	}
	prologue := `ROOT="$$(pwd)" && cd "$(RULEDIR)"`
	if len(mkdirs) > 0 {
		prologue += " && " + strings.Join(mkdirs, " && ")
	}
	cmd := prologue + " && " + strings.Join(rewritten, " ")
	driver := executeProcessDriverBasename(argv[0])
	if driver == "" {
		driver = "unknown"
	}
	tags := append(fileProducingTags(driver), "cmake-codegen-execute-process-derived-rerun")
	sort.Strings(tags)
	name := executeProcessGenruleName(rels[0])
	cc.appendExecProcGenrule(ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleCmd:  cmd,
		GenruleOuts: rels,
		Tags:        tags,
		Visibility:  []string{"//visibility:private"},
	})
	for _, rel := range rels {
		cc.OutToGenrule[rel] = name
	}
	return rels, true
}

// rewriteArgvDerivedRerun rewrites a stem-match codegen argv for a `cd $(RULEDIR)`
// re-run: argv[0] stays a PATH basename (re-runnable per the hoist contract);
// each source-tree input becomes "$$ROOT/$(location <rel>)" (absolute, so it
// survives the cd) and is staged as a src; a build-dir input already produced by
// another recovery is referenced the same way; a non-path arg stays literal. A
// build-dir path with no producer can't be staged → decline.
func rewriteArgvDerivedRerun(argv []string, anc execAnchors, cc *codegenContext) (srcs, rewritten []string, ok bool) {
	srcSet := map[string]bool{}
	addSrc := func(rel string) {
		if !srcSet[rel] {
			srcSet[rel] = true
			srcs = append(srcs, rel)
		}
	}
	loc := func(rel string) string { return `"$$ROOT/$(location ` + rel + `)"` }
	emitKeyed := func(a, repl string) string {
		if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/\\") {
			return a[:eq+1] + repl
		}
		return repl
	}
	for i, a := range argv {
		if i == 0 {
			rewritten = append(rewritten, shellQuoteArg(filepath.Base(a)))
			continue
		}
		p := stripArgvPathPrefix(a)
		if rel, anchored := executeProcessAnchorSource(p, anc); anchored {
			if rel == "" || isExistingDir(filepath.Join(anc.hostSrcDir, rel)) {
				return nil, nil, false
			}
			if _, err := os.Stat(filepath.Join(anc.hostSrcDir, filepath.FromSlash(rel))); err != nil {
				return nil, nil, false
			}
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, loc(rel)))
			continue
		}
		if rel, anchored := executeProcessAnchorOutput(p, anc); anchored {
			if !cc.outputClaimed(rel) {
				return nil, nil, false // unproduced build-dir path: can't stage
			}
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, loc(rel)))
			continue
		}
		rewritten = append(rewritten, shellQuoteArg(a))
	}
	return srcs, rewritten, true
}

// bakeDerivedOutputs captures the configure-written bytes of the uniquely
// stem-claimed orphans (they exist on disk — the configure produced them)
// via the shared bakeFileTarget chooser. The bake trade-off (no liveness on
// input edits) is the same contract as --cmake-script-bake and rides the
// -derived-bake facet for audit; the re-run upgrade is writer-style-
// dependent and deliberately deferred.
func bakeDerivedOutputs(call shadow.ExecuteProcessCall, orphans []string, anc execAnchors, cc *codegenContext) ([]string, bool) {
	driver := executeProcessDriverBasename(call.Commands[0][0])
	if driver == "" {
		driver = "unknown"
	}
	tags := append(fileProducingTags(driver), "cmake-codegen-execute-process-derived-bake")
	sort.Strings(tags)
	var rels []string
	for _, rel := range orphans {
		if cc.outputClaimed(rel) {
			rels = append(rels, rel)
			continue
		}
		body, err := os.ReadFile(filepath.Join(anc.hostBuildDir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		name := executeProcessGenruleName(rel)
		cc.appendExecProcGenrule(bakeFileTarget(name, rel, body, tags))
		cc.OutToGenrule[rel] = name
		rels = append(rels, rel)
	}
	return rels, len(rels) > 0
}
