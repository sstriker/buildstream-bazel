package lower

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// recoverWriteInPlaceTool recovers the WRITE-IN-PLACE codegen shape hidden in a
// re-traced `cmake -P` wrapper: a tool runs with WORKING_DIRECTORY == the custom
// command's DECLARED-output directory and writes its outputs there BY BASENAME,
// naming no path in argv and with NO subsequent relocation (`cmake -E copy`/
// `rename`) step. Because the tool's cwd already IS the output dir, the outputs
// land directly at the declared paths.
//
// The earlier rungs can't see it: recoverExecuteProcess / recoverTracedToolCommand
// both need the argv to NAME the output (an argv operand or a `--out=<dir>` flag),
// and recoverTempDirToolRelocate needs a relocation call to harvest. So this shape
// otherwise bottoms out at the `cmake -P` refusal. Recover the regenerating TOOL:
// emit a genrule that runs the tool in the genrule cwd (WORKING_DIRECTORY stripped,
// so it writes the same basenames there) and copies each to $(RULEDIR)/<declared>
// — the same emission the temp-dir-relocate genrule fallback uses, with the
// relocation map SYNTHESIZED from the WORKING_DIRECTORY anchor rather than
// harvested from copy calls.
//
// Runs as the LAST rung (after recoverTracedToolCommand), so it only owns the
// shape every more-explicit recovery declined. Gated tightly: exactly one liftable
// tool whose WORKING_DIRECTORY anchors to the declared outputs' shared parent dir
// (>1 is ambiguous), the declared outputs share that one dir, and every declared
// output exists on disk under the build dir (the trace produced it in place).
func (cc *codegenContext) recoverWriteInPlaceTool(b *ninja.Build, calls []shadow.ExecuteProcessCall, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, bool) {
	declared, outsParent, ok := writeInPlaceDeclared(b, buildDir, relOut)
	if !ok {
		return "", false
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}

	toolArgv, toolWorkdir, ok := cc.writeInPlaceProducer(calls, anc, outsParent)
	if !ok {
		return "", false
	}

	// Synthesize the relocation map: each declared output is written by the tool
	// as its basename in the WORKING_DIRECTORY (which IS the output dir).
	relocate := map[string]string{}
	for _, o := range declared {
		relocate[o] = path.Join(toolWorkdir, path.Base(o))
	}

	// Substitute the traced tool argv for `cmake -P <script>` and reuse the shared
	// emission (a recognized tool lowers to its native rule — relocation irrelevant;
	// an unrecognized one to a genrule whose cwd output we relocate below).
	if _, _, err := cc.emitRecoveredGenrule(b, strings.Join(toolArgv, " "), cmakeSrc, buildDir, relOut, g, nil); err != nil {
		return "", false
	}
	if !cc.outputClaimed(relOut) {
		return "", false
	}
	if _, isGenrule := cc.OutToGenrule[relOut]; isGenrule {
		appendTempDirRelocations(cc, b, buildDir, declared, relocate, toolWorkdir)
	}
	return cc.SeenBuilds[b], true
}

// writeInPlaceDeclared returns the edge's declared outputs + their shared parent
// dir when they hold the write-in-place invariants: relOut among them, all
// sharing one directory, and all on disk under the build dir (the trace produced
// them in place). ok=false otherwise.
func writeInPlaceDeclared(b *ninja.Build, buildDir, relOut string) (declared []string, outsParent string, ok bool) {
	declared = genruleOuts(b, buildDir)
	if len(declared) == 0 {
		return nil, "", false
	}
	outsParent = path.Dir(declared[0])
	inDeclared := false
	for _, o := range declared {
		if o == relOut {
			inDeclared = true
		}
		if path.Dir(o) != outsParent {
			return nil, "", false // v1: the declared outputs share one directory
		}
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			return nil, "", false // declared output the trace didn't actually produce
		}
	}
	if !inDeclared {
		return nil, "", false
	}
	return declared, outsParent, true
}

// writeInPlaceProducer finds the single liftable tool call whose WORKING_DIRECTORY
// anchors to the declared outputs' directory (outsParent) — the tool writes the
// outputs there by basename. ok=false when none match or more than one does (an
// ambiguous producer; don't guess). Returns the tool argv + its working dir.
func (cc *codegenContext) writeInPlaceProducer(calls []shadow.ExecuteProcessCall, anc execAnchors, outsParent string) (argv []string, workdir string, ok bool) {
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if c.WorkingDirectory == "" || !argvStructurallyLiftableInWrapper(c) {
			continue
		}
		if !argvToolLiftable(c.Commands[0][0], anc, cc) {
			continue
		}
		wdRel, anchored := executeProcessAnchorOutput(c.WorkingDirectory, anc)
		if !anchored {
			continue
		}
		if wdRel == "" {
			wdRel = "."
		}
		if wdRel != outsParent {
			continue
		}
		if argv != nil {
			return nil, "", false // ambiguous producer
		}
		argv = c.Commands[0]
		workdir = c.WorkingDirectory
	}
	return argv, workdir, argv != nil
}
