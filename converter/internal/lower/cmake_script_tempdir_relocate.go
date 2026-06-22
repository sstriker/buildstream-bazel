package lower

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// argvStructurallyLiftableInWrapper is argvCodegenEligibleRelaxed with the
// WorkingDirectory / Environment gates DROPPED — used only inside a re-traced
// `cmake -P` wrapper, where those fields are artifacts of the wrapper (a
// `WORKING_DIRECTORY <tmp>` the script set, or `cmake -E env` populating
// Environment), not signals of a non-liftable call. The other structural gates
// (single command, real argv, no live stderr/timeout/output-file consumer)
// stay, as does the OUTPUT_VARIABLE / RESULT_VARIABLE tolerance.
func argvStructurallyLiftableInWrapper(call shadow.ExecuteProcessCall) bool {
	return len(call.Commands) == 1 && len(call.Commands[0]) > 1 &&
		call.Timeout == "" && call.InputFile == "" && call.ErrorFile == "" &&
		call.OutputFile == "" && call.ErrorVariable == ""
}

// cmakeECopySingle reports a `cmake -E copy|copy_if_different <src> <dst>`
// single-file relocation (the 2-operand form), returning the raw src + dst.
// ok=false for any other call (a different driver/op, or the multi-source
// `copy a b destdir/` form, which the temp-dir pattern doesn't use).
func cmakeECopySingle(call shadow.ExecuteProcessCall) (src, dst string, ok bool) {
	if len(call.Commands) != 1 {
		return "", "", false
	}
	argv := call.Commands[0]
	if len(argv) != 5 {
		return "", "", false
	}
	if executeProcessDriverBasename(argv[0]) != "cmake" && argv[0] != "${CMAKE_COMMAND}" {
		return "", "", false
	}
	if argv[1] != "-E" || (argv[2] != "copy" && argv[2] != "copy_if_different") {
		return "", "", false
	}
	return argv[3], argv[4], true
}

// recoverTempDirToolRelocate recovers the temp-dir-then-copy codegen shape that
// hides inside a re-traced `cmake -P` wrapper: a tool runs with
// WORKING_DIRECTORY=<tmp> (writing its outputs there and naming no final path in
// argv), then one or more `cmake -E copy[_if_different] <tmp>/<x> <declared>`
// calls relocate them to the custom command's DECLARED outputs.
//
// Without this, the tool call is gated out of recoverTracedToolCommand (its
// WORKING_DIRECTORY trips the eligibility gate, and its argv doesn't name the
// declared output dir), so the only recovery is recoverExecuteProcess FREEZING
// each copy's dst bytes (bakeBuildDirCopyOutput) — a static snapshot that won't
// refresh when the tool's inputs change. Recovering the TOOL instead makes the
// output regenerate: a recognizer's native rule when the tool is claimed (the
// relocation is irrelevant — the native rule re-derives outputs from inputs),
// else a genrule that runs the tool and relocates its output to $(RULEDIR).
//
// Runs BEFORE recoverExecuteProcess so its claim wins: bakeBuildDirCopyOutput
// then sees the output already claimed and defers (returns the existing
// producer) instead of frozen-baking. Declines (→ recoverExecuteProcess handles
// the calls as before, frozen-bake included) unless every gate below holds, so
// it never regresses a shape it doesn't fully own.
func (cc *codegenContext) recoverTempDirToolRelocate(b *ninja.Build, calls []shadow.ExecuteProcessCall, fileCopies []shadow.FileWriterCall, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, bool) {
	declared := genruleOuts(b, buildDir)
	if len(declared) == 0 {
		return "", false
	}
	declaredSet := map[string]bool{}
	inDeclared := false
	outsParent := path.Dir(declared[0])
	for _, o := range declared {
		declaredSet[o] = true
		if o == relOut {
			inDeclared = true
		}
		if path.Dir(o) != outsParent {
			return "", false // v1: the declared outputs share one directory
		}
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			return "", false // declared output the trace didn't actually produce
		}
	}
	if !inDeclared {
		return "", false
	}

	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}
	// Harvest the relocations whose dst anchors to a declared output, mapping
	// declaredOut -> raw copy source. Two forms, identical (src, dst) semantics:
	//   - `cmake -E copy[_if_different] <src> <dst>` (an execute_process), and
	//   - `file(COPY <src…> DESTINATION <dir>)` (a cmake command harvested as a
	//     file-writer, dst = <dir>/<basename(src)> per source).
	relocate := map[string]string{}
	addReloc := func(src, dst string) {
		dstRel, ok := executeProcessAnchorOutput(dst, anc)
		if ok && declaredSet[dstRel] {
			relocate[dstRel] = src
		}
	}
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if src, dst, ok := cmakeECopySingle(c); ok {
			addReloc(src, dst)
		}
	}
	for _, w := range fileCopies {
		for i := range w.Outputs {
			if i < len(w.Sources) {
				addReloc(w.Sources[i], w.Outputs[i])
			}
		}
	}
	// Every declared output must be a relocation destination — otherwise the tool
	// alone doesn't account for the declaration and we'd under-produce.
	for _, o := range declared {
		if _, ok := relocate[o]; !ok {
			return "", false
		}
	}

	// Find the single liftable tool whose WORKING_DIRECTORY holds the copy
	// sources (the tempdir it wrote into). >1 such tool is ambiguous.
	var toolArgv []string
	var toolWorkdir string
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if c.WorkingDirectory == "" || !argvStructurallyLiftableInWrapper(c) {
			continue
		}
		if !argvToolLiftable(c.Commands[0][0], anc, cc) {
			continue
		}
		// The tool's working dir must contain every relocation source.
		owns := true
		for _, src := range relocate {
			if _, inside := relativeIfInsideRelaxed(c.WorkingDirectory, src); !inside {
				owns = false
				break
			}
		}
		if !owns {
			continue
		}
		if toolArgv != nil {
			return "", false // ambiguous producer
		}
		toolArgv = c.Commands[0]
		toolWorkdir = c.WorkingDirectory
	}
	if toolArgv == nil {
		return "", false
	}

	// Emit the recovery. Pass the TOOL argv alone so the recognizer chokepoint
	// can claim it (a recognized tool lowers to its native rule, the relocation
	// irrelevant). emitRecoveredGenrule declares the edge's outs and registers
	// relOut (OutToGenrule on the genrule fallback, OutToNativeConsumerDep on a
	// recognizer match).
	if _, _, err := cc.emitRecoveredGenrule(b, strings.Join(toolArgv, " "), cmakeSrc, buildDir, relOut, g, nil); err != nil {
		return "", false
	}
	if !cc.outputClaimed(relOut) {
		return "", false
	}
	// Genrule fallback (not recognized): the tool ran with WORKING_DIRECTORY
	// stripped, so it writes its outputs into the genrule's cwd under the SAME
	// names they had in the tempdir; append a relocation of each to $(RULEDIR)/
	// <declared> so the genrule actually produces the declared outputs. A
	// recognized tool's native rule already produces them — leave it untouched.
	if _, isGenrule := cc.OutToGenrule[relOut]; isGenrule {
		appendTempDirRelocations(cc, b, buildDir, declared, relocate, toolWorkdir)
	}
	return cc.SeenBuilds[b], true
}

// appendTempDirRelocations rewrites the just-emitted genrule's cmd to copy each
// tempdir-written output (now in the genrule cwd, since the tool ran with its
// WORKING_DIRECTORY stripped) to its declared $(RULEDIR) destination.
func appendTempDirRelocations(cc *codegenContext, b *ninja.Build, buildDir string, declared []string, relocate map[string]string, toolWorkdir string) {
	name := cc.SeenBuilds[b]
	for i := range cc.Genrules {
		gen := &cc.Genrules[i]
		if gen.Name != name || gen.Kind != ir.KindGenrule {
			continue
		}
		var sb strings.Builder
		sb.WriteString(gen.GenruleCmd)
		for _, o := range declared {
			src := relocate[o]
			cwdRel, inside := relativeIfInsideRelaxed(toolWorkdir, src)
			if !inside {
				cwdRel = path.Base(src)
			}
			dst := "$(RULEDIR)/" + o
			if d := path.Dir(o); d != "." && d != "" {
				sb.WriteString(" && mkdir -p $(RULEDIR)/" + d)
			}
			sb.WriteString(" && cp " + cwdRel + " " + dst)
		}
		gen.GenruleCmd = sb.String()
		return
	}
}
