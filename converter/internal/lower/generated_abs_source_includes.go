package lower

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/cclang"
)

// stageGeneratedSourceRootIncludes handles the codegen idiom where a
// convert-time-generated wrapper source — a write_file bake recovered from a
// configure_file / file(WRITE) call — textually `#include`s a real in-tree
// source by its SOURCE-ROOT-ABSOLUTE path. OpenBLAS's GenerateNamedObjects
// (cmake/utils.cmake) is the motivating case: it `file(WRITE)`s ~1951 per-
// routine wrappers, each `#define`-ing a routine's name macros then
//
//	#include "<source-root>/lapack/getf2/zgetf2_k.c"
//
// cmake bakes the ABSOLUTE configure-time path because the wrapper lives in
// the build dir and the kernel isn't on any -I path. That absolute path
// doesn't exist in the Bazel sandbox, so the compile fails "No such file".
//
// This pass applies the two coordinated fixes that idiom needs, both required:
//
//  1. Rewrite the absolute `#include` baked into the write_file CONTENT to a
//     WORKSPACE-relative path (`<bazelPackagePath>/<source-root-rel>`), which
//     resolves through Bazel's default `-iquote <exec-root>` regardless of the
//     source-vs-generated tree split (the wrapper compiles out of bazel-out/,
//     the kernel lives in the source tree).
//  2. Stage the included in-tree source as a textual_hdr on the target(s) that
//     compile the wrapper, so it's a declared INPUT (present in the sandbox +
//     passes include validation) without being compiled standalone — same slot
//     mechanics as synthesizeTextualSourceIncludeLibs (directly on a
//     cc_library/cc_interface; via a synth carrier lib for cc_binary/cc_test).
//
// Only absolute quote-includes that (a) resolve INSIDE the source tree and
// (b) name an on-disk compiled source (.c/.cc/…) are touched — a `#include`
// of `/usr/include/...` or of a missing path is left verbatim (it either
// resolves via the toolchain or is a real error to surface, not ours to
// rewrite). Gated on hostSrcOnDisk (it stats candidate sources).
func stageGeneratedSourceRootIncludes(pkg *ir.Package, hostSrc, bazelPackagePath string, hostSrcOnDisk bool, warn io.Writer) {
	if pkg == nil || !hostSrcOnDisk || hostSrc == "" {
		return
	}
	included, rewritten, wrappers := rewriteGeneratedWrapperIncludes(pkg, hostSrc, bazelPackagePath)
	if len(included) == 0 {
		return
	}

	// Wire the staged sources onto every target that compiles a rewritten
	// wrapper: union the includes across all of the target's wrapper srcs,
	// drop any the target itself compiles, and route the rest to a textual_hdrs
	// slot. Collect synth libs first, append after the scan.
	uniqueName := targetNamer(pkg)
	type rec struct {
		target string
		srcs   []string
	}
	var inlineRecs, synthRecs []rec
	var synth []ir.Target
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCInterface, ir.KindCCBinary, ir.KindCCTest:
		default:
			continue
		}
		compiled := make(map[string]bool, len(t.Srcs))
		for _, s := range t.Srcs {
			compiled[filepath.ToSlash(filepath.Clean(s))] = true
		}
		var seeds []string
		for _, s := range t.Srcs {
			if staged, ok := included[filepath.ToSlash(filepath.Clean(s))]; ok {
				seeds = append(seeds, staged...)
			}
		}
		if len(seeds) == 0 {
			continue
		}
		// Stage the TRANSITIVE textual-include closure of the directly-included
		// kernels: an OpenBLAS kernel #includes sibling micro-kernel sources by
		// relative path, which must also be declared inputs (they resolve
		// against the kernel's own dir at compile time — no rewrite needed).
		incs, readers := textualIncludeClosure(hostSrc, seeds, compiled)
		if len(incs) == 0 {
			continue
		}
		// Publish the closure files whose bytes drove the expansion as declared
		// source-byte reads (the narrowing-lens exception). See
		// ir.Package.SourceByteReads.
		pkg.SourceByteReads = append(pkg.SourceByteReads, readers...)
		lib := attachTextualSourceIncludes(pkg, t, incs, "cmake-codegen-generated-source-include", &synth, uniqueName)
		if lib == "" {
			inlineRecs = append(inlineRecs, rec{target: t.Name, srcs: incs})
		} else {
			synthRecs = append(synthRecs, rec{target: lib, srcs: incs})
		}
	}
	if len(synth) > 0 {
		pkg.Targets = append(pkg.Targets, synth...)
	}
	if warn != nil {
		fmt.Fprintf(warn,
			"lower: rewrote %d source-root-absolute #include(s) across %d generated wrapper source(s) to workspace-relative paths and staged their kernels as textual_hdrs (the GenerateNamedObjects-style codegen idiom):\n",
			rewritten, wrappers)
		for _, r := range inlineRecs {
			fmt.Fprintf(warn, "  %s (textual_hdrs: %s)\n", r.target, strings.Join(r.srcs, ", "))
		}
		for _, r := range synthRecs {
			fmt.Fprintf(warn, "  %s [synth carrier] (textual_hdrs: %s)\n", r.target, strings.Join(r.srcs, ", "))
		}
	}
}

// rewriteGeneratedWrapperIncludes scans WriteFile targets for the
// "generated wrapper textually #includes a source-root-ABSOLUTE compiled
// source" idiom (the GenerateNamedObjects-style codegen shape), rewrites each
// such include in place to its workspace-relative path, and returns the
// per-wrapper-output map of source-root-relative sources to stage as
// textual_hdrs on whatever compiles the wrapper (plus the rewritten-line and
// wrapper counts for the warning). Precondition: the caller has already
// verified hostSrc is non-empty and on disk (stageGeneratedSourceRootIncludes's
// hostSrcOnDisk guard).
func rewriteGeneratedWrapperIncludes(pkg *ir.Package, hostSrc, bazelPackagePath string) (map[string][]string, int, int) {
	// included[generated-output-path] = the source-root-relative sources that
	// output's wrapper textually includes (sorted, deduped).
	included := map[string][]string{}
	var rewritten, wrappers int
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindWriteFile || len(t.WriteFileContent) == 0 {
			continue
		}
		var incs []string
		for li, line := range t.WriteFileContent {
			m := quoteIncludeRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			inc := m[1]
			var rel string
			if filepath.IsAbs(inc) {
				r, inside := relativeIfInside(hostSrc, filepath.Clean(inc))
				if !inside {
					// Absolute but outside the source tree (e.g. /usr/include) —
					// leave it for the toolchain.
					continue
				}
				rel = filepath.ToSlash(r)
			} else {
				// Source-root-RELATIVE include. cmake 4.x bakes OpenBLAS's
				// GenerateNamedObjects wrappers with a source-root-relative path
				// (`lapack/getf2/zgetf2_k.c`) where older cmake baked the absolute
				// one; the includer (a generated CMakeFiles/*.c) compiles with the
				// source root on its -I, so the path resolves from THERE, not the
				// includer's own dir. The IsCompiledSource + on-disk Stat gates
				// below distinguish this from a genuinely includer-relative include
				// (which won't name a real compiled source at the source root), so
				// only a real source-root source is rewritten + staged. Without
				// this the kernel source is never declared an input and the bare
				// include misses under Bazel's sandbox (the cmake-4.x OpenBLAS
				// build regression).
				rel = filepath.ToSlash(filepath.Clean(inc))
				if strings.HasPrefix(rel, "../") || rel == ".." {
					// Escapes the source root — not a source-root-relative path.
					continue
				}
			}
			if !cclang.IsCompiledSource(rel) {
				// Only the "textually include a compiled source" idiom; an
				// absolute header include is a different (rarer) shape.
				continue
			}
			if st, err := os.Stat(filepath.Join(hostSrc, filepath.FromSlash(rel))); err != nil || st.IsDir() {
				// Not a real in-tree file — can't stage it; leave the line so
				// the failure is honest rather than silently mis-rewritten.
				continue
			}
			ws := rel
			if !pkgPathIsRoot(bazelPackagePath) {
				ws = bazelPackagePath + "/" + rel
			}
			// Replace the exact original quoted path (robust to `..` and to
			// whitespace variants the regex tolerated).
			t.WriteFileContent[li] = strings.Replace(line, "\""+inc+"\"", "\""+ws+"\"", 1)
			rewritten++
			incs = appendUnique(incs, rel)
		}
		if len(incs) > 0 {
			sort.Strings(incs)
			included[filepath.ToSlash(filepath.Clean(t.WriteFileOut))] = incs
			wrappers++
		}
	}
	return included, rewritten, wrappers
}
