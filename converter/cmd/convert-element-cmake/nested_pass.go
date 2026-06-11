package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// stageNestedFileAPIQueries stages File API query files into each nested build
// dir pass 1 detected, so the WARM outer reconfigure (the coalesced warm second
// pass in runLowerPasses) makes the nested cmake write a codemodel reply.
// Returns every nested rel (sorted) plus how many were actually staged; the
// caller skips the warm pass's nested half when staged == 0. The reconfigure
// itself is NOT run here — it's shared with the genex/stamp demands.
func stageNestedFileAPIQueries(hostBuildDir string, sink map[string]string) (rels []string, staged int) {
	rels = make([]string, 0, len(sink))
	for rel := range sink {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		nb := filepath.Join(hostBuildDir, filepath.FromSlash(rel))
		if st, err := os.Stat(nb); err != nil || !st.IsDir() {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested cmake build dir %s not on disk; skipping its lift.\n", rel)
			continue
		}
		if err := cmakerun.StageFileAPIQueries(nb); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: staging File API queries into nested build %s failed (%v); skipping its lift.\n", rel, err)
			continue
		}
		staged++
	}
	return rels, staged
}

// harvestNestedBuilds reads each nested build's File API reply + ninja graph
// (after the shared warm reconfigure ran the nested cmake) and assembles the
// NestedBuildInput merge inputs, recursing into superbuild grandchildren. The
// shared outer reconfigure has already happened; this is the post-configure
// half of the old runNestedCMakePass. Failures degrade to nil/skip — the
// not-lifted warning + structured todo stay loud in the final pass.
func harvestNestedBuilds(ctx context.Context, a cli.Args, hostBuildDir string, rels []string, sink map[string]string) []lower.NestedBuildInput {
	// Cycle guard for the superbuild-chain worklist below: canonical
	// build dirs already claimed by a lift (the outer dir + every
	// harvested nested dir). A chain that re-configures an
	// already-claimed dir (A configures B, B configures A) stops at
	// the guard instead of looping.
	visited := map[string]bool{canonicalBuildDir(hostBuildDir): true}
	var out []lower.NestedBuildInput
	for _, rel := range rels {
		nb := filepath.Join(hostBuildDir, filepath.FromSlash(rel))
		visited[canonicalBuildDir(nb)] = true
	}
	for _, rel := range rels {
		nb := filepath.Join(hostBuildDir, filepath.FromSlash(rel))
		// Traced re-configure (third nested run, warm): the outer pass
		// above re-ran the nested cmake but couldn't instrument it —
		// the argv belongs to the project's execute_process, and cmake
		// has no env knob to force tracing on a child. Re-running the
		// nested dir DIRECTLY with the trace flags (no -G/-D: the warm
		// cache pins the project's own decisions) captures the trace
		// that switches on the full recovery ladder inside the nested
		// lowering — configure_file lifts, execute_process
		// classification, stamp vars — instead of the trace-less
		// fallback (generic on-disk bakes). It also refreshes the File
		// API reply, so the reply loaded below describes the SAME run
		// as the trace. Failure degrades to the trace-less lowering,
		// not to losing the lift.
		traceRaw := runNestedTraceReconfigure(ctx, a, rel, nb)
		replyDir := filepath.Join(nb, ".cmake", "api", "v1", "reply")
		r, err := fileapi.Load(replyDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested build %s produced no loadable File API reply (%v); staying unlifted.\n", rel, err)
			continue
		}
		// The nested ninja graph is the exclusion set for the
		// generated-header bake; nil (non-ninja nested generator,
		// parse failure) degrades to baking more than necessary
		// rather than failing the lift.
		g, _ := ninja.ParseFile(filepath.Join(nb, "build.ninja"))
		input := lower.NestedBuildInput{
			BuildRel:     rel,
			SrcDir:       sink[rel],
			Reply:        r,
			Graph:        g,
			HostBuildDir: nb,
			TraceRaw:     traceRaw,
		}
		harvestNestedDescendants(ctx, a, &input, rel, 1, visited)
		out = append(out, input)
	}
	return out
}

// maxNestedCMakeDepth caps the superbuild-chain descent (levels of
// nested cmake below the outer project). Real chains are 2-3 deep;
// anything past the cap lands in its parent lowering's local sink and
// warns not-lifted — loud degradation, never a loop.
const maxNestedCMakeDepth = 4

// harvestNestedDescendants is the superbuild-chain worklist: scan a
// harvested nested build's trace for ITS OWN nested configures, and
// stage + traced-re-configure each grandchild dir DIRECTLY (its cache
// is warm — the parent's traced re-configure just re-ran its
// configure — and TraceReconfigure both refreshes the staged File API
// reply and captures the grandchild's trace in one run, so no parent
// re-run is needed). Each harvested grandchild recurses, building the
// Children tree lowerOneNestedBuild threads into the recursive ToIR.
// outerRel is the dir's outer-build-relative path (display/labels in
// warnings); BuildRel on the produced input is PARENT-relative, the
// frame the child's own lowering anchors in. Every failure degrades to
// skipping that grandchild — it then warns not-lifted from the parent
// lowering's local sink.
func harvestNestedDescendants(ctx context.Context, a cli.Args, parent *lower.NestedBuildInput, outerRel string, depth int, visited map[string]bool) {
	kids := lower.DetectNestedConfigures(parent.TraceRaw, parent.SrcDir, parent.HostBuildDir)
	if len(kids) == 0 {
		return
	}
	if depth >= maxNestedCMakeDepth {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested build %s configures further nested build(s) beyond the depth cap (%d); they stay unlifted.\n", outerRel, maxNestedCMakeDepth)
		return
	}
	rels := make([]string, 0, len(kids))
	for rel := range kids {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		childRel := outerRel + "/" + rel
		dir := filepath.Join(parent.HostBuildDir, filepath.FromSlash(rel))
		canon := canonicalBuildDir(dir)
		if visited[canon] {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested build %s re-configures an already-lifted build dir (%s); skipping the repeat (cycle guard).\n", outerRel, childRel)
			continue
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested cmake build dir %s not on disk; skipping its lift.\n", childRel)
			continue
		}
		visited[canon] = true
		if err := cmakerun.StageFileAPIQueries(dir); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: staging File API queries into nested build %s failed (%v); skipping its lift.\n", childRel, err)
			continue
		}
		traceRaw := runNestedTraceReconfigure(ctx, a, childRel, dir)
		r, err := fileapi.Load(filepath.Join(dir, ".cmake", "api", "v1", "reply"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested build %s produced no loadable File API reply (%v); staying unlifted.\n", childRel, err)
			continue
		}
		g, _ := ninja.ParseFile(filepath.Join(dir, "build.ninja"))
		child := lower.NestedBuildInput{
			BuildRel:     rel,
			SrcDir:       kids[rel],
			Reply:        r,
			Graph:        g,
			HostBuildDir: dir,
			TraceRaw:     traceRaw,
		}
		harvestNestedDescendants(ctx, a, &child, childRel, depth+1, visited)
		parent.Children = append(parent.Children, child)
	}
}

// canonicalBuildDir resolves a build dir to its symlink-free absolute
// form for the cycle guard; unresolvable paths fall back to the
// cleaned absolute form (best effort — a dir that can't resolve also
// can't be re-configured).
func canonicalBuildDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(dir)
}

// runNestedTraceReconfigure runs the instrumented re-configure of one
// nested build dir and reads the trace back. Soft on every failure
// (nil trace → the nested lowering runs trace-less, exactly as before
// this pass existed) — a nested project that re-configures
// non-idempotently or rejects the re-run must not cost the lift.
func runNestedTraceReconfigure(ctx context.Context, a cli.Args, rel, nb string) []byte {
	tracePath := filepath.Join(nb, "trace.jsonl")
	if err := cmakerun.TraceReconfigure(ctx, nb, tracePath, a.PrefixDir, os.Stderr, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: traced re-configure of nested build %s failed (%v); lowering it without a trace.\n", rel, err)
		return nil
	}
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: reading nested build %s trace failed (%v); lowering it without a trace.\n", rel, err)
		return nil
	}
	return raw
}
