package lower

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// liftArgvFileProducing lifts the argv-declared codegen shape:
// `execute_process(COMMAND tool <input…> <output…>)` where the input and
// output FILES appear as argv elements (dd's if=/of=, m4 -o, custom
// generators taking explicit paths) rather than via the OUTPUT_FILE stdout
// redirect liftFileProducing hoists. add_custom_command gets this lift for
// free because cmake declares OUTPUT/DEPENDS into the ninja edge;
// execute_process declares nothing — but the contract is recoverable
// WITHOUT convert-time re-execution, because the configure already ran the
// call:
//
//   - an argv path anchoring under the SOURCE root and existing on disk is
//     an input (staged via srcs + $(location), the hoist's own policy);
//   - an argv path anchoring under the BUILD dir that another recovery
//     already produces (cc.OutToGenrule) is a GENERATED input — chains
//     resolve in trace order, the producing rule's out is referenced via
//     srcs exactly like a consumed generated source;
//   - a remaining build-dir argv FILE that exists on disk post-configure is
//     this call's OUTPUT — its presence is the configure's own evidence;
//   - anything build-dir-anchored that can't be classified (absent from
//     disk, or a directory) declines the lift, falling through to the
//     loud refusal (now mirrored into conversion-todos.json);
//   - a RELATIVE operand resolves against the process cwd — the BUILD
//     root, where cmake's configure runs — with on-disk existence (or a
//     known producer) as the path-vs-plain-flag discriminator: a relative
//     arg that names no build file is an ordinary string argument and
//     stays literal.
//
// Documented residue (sound-but-limited, all failing LOUDLY rather than
// silently wrong): an in-source WRITE classifies as an input (it exists
// post-configure; indistinguishable from a read without write tracing) and
// the re-run's attempt to write an input fails in the sandbox; undeclared
// sibling outputs (a tool writing out.h AND out.d) aren't in outs, so a
// consumer of the sibling fails at analysis; undeclared reads outside the
// argv fail in the sandbox. In-place shapes (an output that is also a
// staged input) decline — Bazel rejects input==output.
//
// One shape escapes the loud-failure net: a `key=` flag ASSIGNMENT whose
// value coincidentally resolves as an existing input (`-DNAME=/src/x` where
// /src/x exists) gets the input-side `$(location)` rewrite, so the re-run's
// argv silently differs from the configure's — if the tool bakes the string
// into its output rather than opening it, the bytes drift with no failure.
// Existence is the only discriminator available, and rewriting genuine
// `--config=<path>` inputs is desirable, so this stays a documented trade
// rather than a gate. (Output-side misclassification stays loud: the
// genrule fails on the never-written out.)
//
// Attempted only for BucketRefuse calls (cmake -E ops, POSIX copies,
// OUTPUT_FILE hoists, stamps and probes all classify earlier), with the
// same conservative keyword gates as liftFileProducing — the ROADMAP
// records the keyword-expansion order. Emits one multi-output genrule
// re-running the tool at Bazel build time (argv[0] PATH-portability per the
// hoist's contract), tagged with the -hoisted facet plus -argv-outs so
// audits can split the two hoist shapes.
func liftArgvFileProducing(call shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) ([]string, bool) {
	if !argvCodegenEligible(call) || anc.hostBuildDir == "" {
		return nil, false
	}
	argv := call.Commands[0]
	if !argvToolLiftable(argv[0], anc, cc) {
		return nil, false
	}
	outSet, ok := classifyArgvOutputs(argv, anc, cc)
	if !ok || len(outSet) == 0 {
		return nil, false
	}
	// Idempotency across duplicate trace calls: if every output is already
	// registered, reuse (the same contract as the other lifts). A PARTIAL
	// overlap (a distinct call sharing only some outs) declines — emitting
	// a second genrule re-claiming a registered out is an analysis error.
	rels := sortedArgvOuts(outSet)
	registered := 0
	for _, rel := range rels {
		if cc.outputClaimed(rel) {
			registered++
		}
	}
	if registered == len(rels) {
		return rels, true
	}
	if registered > 0 {
		return nil, false
	}

	srcs, rewritten, ok := rewriteArgvCodegen(argv, outSet, anc, cc)
	if !ok {
		return nil, false
	}
	// In-place decline: an output that is also a staged input would make
	// the genrule's src and out the same file — Bazel rejects it.
	srcSet := map[string]bool{}
	for _, sr := range srcs {
		srcSet[sr] = true
	}
	for _, rel := range rels {
		if srcSet[rel] {
			return nil, false
		}
	}
	var mkdirs []string
	seenDir := map[string]bool{}
	for _, rel := range rels {
		d := filepath.Dir(rel)
		if seenDir[d] {
			continue
		}
		seenDir[d] = true
		mkdirs = append(mkdirs, fmt.Sprintf(`mkdir -p "$$(dirname "$(location %s)")"`, rel))
	}
	cmd := strings.Join(append(mkdirs, strings.Join(rewritten, " ")), " && ")

	driver := executeProcessDriverBasename(argv[0])
	if driver == "" {
		driver = "unknown"
	}
	tags := append(fileProducingTags(driver), "cmake-codegen-execute-process-argv-outs")
	sort.Strings(tags)

	name := executeProcessGenruleName(rels[0])
	cc.Genrules = append(cc.Genrules, ir.Target{
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

// liftRecognizedExecuteProcessCodegen handles the execute_process codegen shape
// the argv-output lift can't: a tool that DERIVES its outputs from a flag
// directory (protoc `--cpp_out=DIR`) rather than naming them in argv, so
// classifyArgvOutputs finds nothing. The recognizer is the OUTPUT AUTHORITY —
// it SUPPLIES the derived output set from the tool convention; this corroborates
// that set against the files the configure's own run already produced on disk
// before emitting the native rule, so a mis-derivation declines to the existing
// genrule/refusal path (never a regression). Opt-in via cc.RecognizeCodegen.
// Consumer wiring (a target that lists a generated output as a source) is
// handled package-wide by rewriteNativeRuleConsumers via OutToNativeConsumerDep.
func liftRecognizedExecuteProcessCodegen(call shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) ([]string, bool) {
	if cc == nil || !cc.RecognizeCodegen || !argvCodegenEligibleRelaxed(call) || anc.hostBuildDir == "" {
		return nil, false
	}
	argv := call.Commands[0]
	// Key the recognizer on the SCRIPT when argv[0] is an interpreter (python
	// gen.py), and scan only the post-driver args for source inputs.
	driver, recArgs := codegenRecognitionDriver(argv)
	if driver == "" {
		return nil, false
	}
	var srcs []string
	for _, a := range recArgs {
		if rel, ok := executeProcessAnchorSource(stripArgvPathPrefix(a), anc); ok && rel != "" && rel != "." {
			srcs = append(srcs, rel)
		}
	}
	// outs aren't known until the recognizer supplies them (execute_process
	// records none), so the proto_path root can't be recovered here — resolve
	// imports under the source root (the standard layout). A rebased
	// --proto_path via execute_process declines earlier / falls back; the
	// custom-command paths carry the full proto_path handling.
	protoDeps := protoImportLabels(srcs, nil, anc.hostSrcDir, cc.BazelPackagePath)
	// The configure already ran, so the tool's outputs exist on disk under its
	// output dir. Discover them (outDir-relative) and feed the recognizer: a tool
	// that derives outputs from input CONTENTS can't predict them from a naming
	// convention, so it returns this set (or a subset/transform) as DerivedOutputs.
	outDir := argvCodegenOutDir(argv, anc)
	discovered := discoverCodegenOutDirFiles(outDir, anc, cc)
	res, matched, err := recognizeCodegenWith(cc.ExtraRecognizers, CodegenCommand{Driver: driver, Args: recArgs, Srcs: srcs, Pkg: cc.BazelPackagePath, ProtoDeps: protoDeps, DiscoveredOutputs: discovered})
	if !matched || err != nil || len(res.DerivedOutputs) == 0 {
		return nil, false
	}
	// Anchor each derived output under the tool's output directory, then
	// corroborate it against the configure's on-disk evidence.
	rels := make([]string, 0, len(res.DerivedOutputs))
	for _, d := range res.DerivedOutputs {
		rel := d
		if outDir != "" {
			rel = filepath.ToSlash(filepath.Join(outDir, d))
		}
		if st, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(rel))); err != nil || st.IsDir() {
			return nil, false
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	// Idempotency across duplicate trace calls (already wired by any producer).
	claimed := 0
	for _, rel := range rels {
		if cc.outputClaimed(rel) {
			claimed++
		}
	}
	if claimed == len(rels) {
		return rels, true
	}
	if claimed > 0 {
		return nil, false
	}
	cc.Genrules = append(cc.Genrules, res.Targets...)
	if len(res.ConsumerDeps) > 0 {
		consumer := strings.TrimPrefix(res.ConsumerDeps[0], ":")
		for _, rel := range rels {
			cc.OutToNativeConsumerDep[rel] = consumer
		}
	}
	// Sub-package placement (mirrors recognizeOrGenrule): land the native rules
	// in the package owning the generated outputs.
	if cc.NativeRuleSubPackage != nil && len(rels) > 0 {
		if dir := path.Dir(rels[0]); dir != "" && dir != "." {
			for _, t := range res.Targets {
				cc.NativeRuleSubPackage[t.Name] = dir
			}
		}
	}
	return rels, true
}

// discoverCodegenOutDirFiles enumerates the on-disk files under the tool's
// build-relative output directory (outDir), returned OUTDIR-RELATIVE (slash
// form) to match the recognizer's DerivedOutputs convention — directories,
// ninja-built outputs, and already-claimed outputs excluded. The execute_process
// counterpart to the genrule fallback's dir-operand walk: the configure already
// produced these, so they ARE the tool's discovered output set, fed to the
// recognizer for a content-derived-output tool. Empty outDir (the build root)
// returns nil — walking the whole build dir would sweep unrelated files.
func discoverCodegenOutDirFiles(outDir string, anc execAnchors, cc *codegenContext) []string {
	if outDir == "" || outDir == "." || anc.hostBuildDir == "" {
		return nil
	}
	root := filepath.Join(anc.hostBuildDir, filepath.FromSlash(outDir))
	var rels []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		buildRel, ok := relativeIfInsideRelaxed(anc.hostBuildDir, p)
		if !ok || cc.NinjaOuts[buildRel] || cc.outputClaimed(buildRel) {
			return nil
		}
		if rel := strings.TrimPrefix(buildRel, outDir+"/"); rel != buildRel && rel != "" {
			rels = append(rels, rel)
		}
		return nil
	})
	sort.Strings(rels)
	return rels
}

// argvCodegenOutDir returns the build-relative output directory a codegen tool
// writes to, from a `*_out=DIR` flag (protoc's --cpp_out / --grpc_out
// convention), anchored under the build root. "" means the build root itself
// (or no such flag). The bare two-token `--flag DIR` form isn't handled — the
// `=` form is what cmake's generators emit.
func argvCodegenOutDir(argv []string, anc execAnchors) string {
	for _, a := range argv[1:] {
		eq := strings.IndexByte(a, '=')
		if eq <= 0 || !strings.HasSuffix(a[:eq], "_out") {
			continue
		}
		if rel, ok := executeProcessAnchorOutput(a[eq+1:], anc); ok {
			if rel == "." {
				return ""
			}
			return rel
		}
		return ""
	}
	return ""
}

// argvToolLiftable reports whether argv[0] is a re-runnable tool. A
// configure-BUILT tool living in the build dir — absolute, or relative
// (resolving against the build-root cwd) naming an existing or produced
// build file — cannot be re-run by the genrule: it isn't on PATH, and
// srcs-staging a build artifact via $(location) without tools=/executable
// bits is wrong. The PATH-portability policy (basename) only makes sense
// for system tools, so the lift declines instead.
func argvToolLiftable(tool string, anc execAnchors, cc *codegenContext) bool {
	if _, anchored := executeProcessAnchorOutput(tool, anc); anchored {
		return false
	}
	if rel, ok := relativeArgvBuildRel(tool); ok {
		if cc.outputClaimed(rel) {
			return false
		}
		if st, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(rel))); err == nil && !st.IsDir() {
			return false
		}
	}
	return true
}

// argvCodegenEligible applies the conservative shape gates: a single
// COMMAND with argv, none of the keywords the file-producing lifters
// refuse, and no capture/redirect keywords (an OUTPUT_FILE call classifies
// as BucketFileProducing before ever reaching here; OUTPUT_VARIABLE shapes
// are probe-classified).
func argvCodegenEligible(call shadow.ExecuteProcessCall) bool {
	return len(call.Commands) == 1 && len(call.Commands[0]) > 1 &&
		call.WorkingDirectory == "" && len(call.Environment) == 0 &&
		call.Timeout == "" && call.InputFile == "" && call.ErrorFile == "" &&
		call.OutputFile == "" && call.OutputVariable == "" &&
		call.ErrorVariable == "" && call.ResultVariable == ""
}

// argvCodegenEligibleRelaxed is argvCodegenEligible but TOLERANT of a side
// OUTPUT_VARIABLE / RESULT_VARIABLE / RESULTS_VARIABLE capture — a log line, a
// version string, or an exit-status check that a generator emits ALONGSIDE its
// files. Used only by the recognizer and the File-API-corroborated unspecified-
// output lift, both of which key on robust evidence (a recognizer claim, a
// codemodel-demanded orphan) rather than the bare on-disk-existence heuristic —
// so a captured side value no longer blocks recovering the FILE outputs. The
// captured value still reaches a consuming configure_file independently: a
// probe's OUTPUT_VARIABLE is in dump-vars' cmakeVars, and the genrule's failure
// faithfully stands in for a RESULT_VARIABLE error check. ErrorVariable /
// ErrorFile stay blocking (a live stderr consumer / declared artifact), as do
// WorkingDirectory / Environment / Timeout / InputFile / OutputFile.
func argvCodegenEligibleRelaxed(call shadow.ExecuteProcessCall) bool {
	return len(call.Commands) == 1 && len(call.Commands[0]) > 1 &&
		call.WorkingDirectory == "" && len(call.Environment) == 0 &&
		call.Timeout == "" && call.InputFile == "" && call.ErrorFile == "" &&
		call.OutputFile == "" && call.ErrorVariable == ""
}

// classifyArgvOutputs walks argv (past the tool) and partitions the
// build-dir-anchored elements: produced-elsewhere paths are inputs (handled
// by the rewrite); a FILE existing on disk post-configure is an output;
// anything else build-dir-anchored is unclassifiable → decline. Returns the
// argv index → build-relative out path map.
func classifyArgvOutputs(argv []string, anc execAnchors, cc *codegenContext) (map[int]string, bool) {
	outs := map[int]string{}
	for i, a := range argv {
		if i == 0 {
			continue
		}
		p := stripArgvPathPrefix(a)
		rel, anchored := executeProcessAnchorOutput(p, anc)
		if !anchored {
			// Relative operand: resolves against the process cwd (the
			// build root). Existence / a known producer discriminates a
			// path from a plain string argument — a relative non-file is
			// an ordinary flag value and stays literal, NOT a decline.
			r, ok := relativeArgvBuildRel(p)
			if !ok {
				continue
			}
			if cc.outputClaimed(r) {
				continue
			}
			if st, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(r))); err == nil && !st.IsDir() {
				outs[i] = r
			}
			continue
		}
		if cc.outputClaimed(rel) {
			continue
		}
		st, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(rel)))
		if err != nil || st.IsDir() {
			return nil, false
		}
		outs[i] = rel
	}
	return outs, true
}

// relativeArgvBuildRel normalizes a relative argv operand into a build-root-
// relative slash path, rejecting flags ("-…"), the bare dot, and anything
// escaping the build root.
func relativeArgvBuildRel(p string) (string, bool) {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "-") {
		return "", false
	}
	rel := filepath.ToSlash(filepath.Clean(p))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// stripArgvPathPrefix peels a `key=` prefix off an argv element so dd-style
// `if=/abs/in` / `of=/abs/out` operands classify by their path part. A
// plain path returns unchanged.
func stripArgvPathPrefix(a string) string {
	if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/\\") {
		return a[eq+1:]
	}
	return a
}

// rewriteArgvCodegen renders the genrule cmd argv: outputs →
// `$(location <out>)`, source-tree FILE inputs → srcs + `$(location)`,
// produced build-dir inputs → srcs (the producing rule's out label) +
// `$(location)`, abs tool path → basename (the hoist's portability
// policy). A source-anchored FILE that does not exist on disk declines —
// it could be an in-source WRITE the classifier must not mistake for an
// input. A source-anchored DIRECTORY operand also declines: a literal
// path can't be staged, so under sandboxing a dir-scanning generator
// would see an absent/empty directory and could exit 0 over the empty
// view — a SILENT divergence, unlike the file-side guards.
func rewriteArgvCodegen(argv []string, outs map[int]string, anc execAnchors, cc *codegenContext) (srcs, rewritten []string, ok bool) {
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
		if rel, isOut := outs[i]; isOut {
			rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
			continue
		}
		path := stripArgvPathPrefix(a)
		if rel, anchored := executeProcessAnchorOutput(path, anc); anchored {
			// Build-dir input produced by an earlier recovery (the
			// classifier declined otherwise): reference the generated file.
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
			continue
		}
		if rel, ok := relativeArgvBuildRel(path); ok && !filepath.IsAbs(path) {
			if cc.outputClaimed(rel) {
				// Relative reference to another recovery's generated file.
				addSrc(rel)
				rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
				continue
			}
		}
		if rel, anchored := executeProcessAnchorSource(path, anc); anchored {
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

// sortedArgvOuts returns the deduped, sorted out rels of the classify map.
func sortedArgvOuts(outs map[int]string) []string {
	seen := map[string]bool{}
	var rels []string
	for _, rel := range outs {
		if !seen[rel] {
			seen[rel] = true
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	return rels
}
