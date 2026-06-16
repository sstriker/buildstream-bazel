package lower

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/cclang"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// quoteIncludeRe matches a C/C++ quote-form include directive — `#include
// "path"` — capturing the quoted path. Angle includes (`#include <...>`) are
// deliberately excluded: a textually-included source is always pulled by a
// relative quote path. Whitespace between `#`, `include`, and the quote is
// tolerated (`#  include "x"`). A pragmatic line scan, like the rest of the
// converter's include handling — not a full preprocessor.
var quoteIncludeRe = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*include[ \t]*"([^"]+)"`)

// textualFileScan is the cached result of scanning ONE includer file for
// quote-includes of other sources: the resolved element-root-relative includes
// it pulls (may repeat across files; the caller dedups) and whether the file's
// bytes yielded ≥1 kept include (a "reader"). Keyed by includer path so a file
// shared across many targets (a widely-#included public header, the dominant
// cost) is read + regex-scanned at most ONCE per pass.
type textualFileScan struct {
	includes []string
	reader   bool
}

// findTextualSourceIncludes scans a target's compiled source files for
// quote-includes of OTHER source files — `#include "x.cc"`. Two project
// shapes produce this: the "textually include a .cc to intercept its
// internals" idiom (fmt's posix-mock-test defines POSIX mocks, then
// `#include "../src/os.cc"` so os.cc's syscalls bind to them — the included
// file is NOT compiled standalone), and the fused shape where the included
// file IS also compiled standalone (VTK's bundled lz4: lz4.c compiles AND
// lz4hc.c textually includes it for its internal statics). Either way the
// included file must be a declared INPUT of the includer's compile action —
// sibling srcs are not staged for each other's actions — so it's surfaced
// here for the caller to route to a textual_hdrs slot (directly on a
// cc_library, or via a synthesized textual-hdrs cc_library dep for a
// cc_binary / cc_test, which have no textual_hdrs attribute). A file already
// in srcs STAYS there; textual_hdrs is additive staging.
//
// Returns element-root-relative paths (the shape of irt.Srcs), sorted and
// deduped. hostSrc must be the on-disk source root — the caller gates on
// hostSrcOnDisk. Each include resolves against the including file's own
// directory first (the fmt posix-mock `#include "../src/os.cc"` shape) and then
// against its ancestor directories (the gtest "fused source" shape, where
// gtest-all.cc does `#include "src/gtest.cc"` resolved against the target's
// include root — the package dir above src/); see resolveTextualInclude. A path
// that escapes the element root (".." after cleaning) or is absent on disk is
// skipped — only a real, in-tree source qualifies (compiled or not; see the
// fused shape above).
// Also returns `readers`: the element-root-relative INCLUDER sources whose
// bytes yielded ≥1 kept textual include — i.e. the source files whose CONTENT
// determined the result (the includer's `#include "x.cc"` line). The included
// file itself is only os.Stat'd (existence) by resolveTextualInclude, so its
// bytes don't affect the output and it is NOT a reader. readers is the declared
// source-byte-read set this scan contributes (see ir.Package.SourceByteReads):
// the source-narrowing lens keeps these real so the detection stays stable.
func findTextualSourceIncludes(hostSrc string, srcs []string) (includes, readers []string) {
	return findTextualSourceIncludesCached(hostSrc, srcs, map[string]textualFileScan{})
}

// findTextualSourceIncludesCached is findTextualSourceIncludes backed by a
// caller-owned per-file scan cache, so a file shared across targets is read
// once. synthesizeTextualSourceIncludeLibs threads ONE cache across every
// target in the pass — the fix for re-reading a widely-#included header per
// target (a header in N targets' Hdrs was previously os.ReadFile'd N times).
func findTextualSourceIncludesCached(hostSrc string, srcs []string, cache map[string]textualFileScan) (includes, readers []string) {
	if hostSrc == "" || len(srcs) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	var read []string
	for _, s := range srcs {
		fs := scanTextualFile(hostSrc, s, cache)
		if fs.reader {
			read = append(read, filepath.ToSlash(filepath.Clean(s)))
		}
		for _, rel := range fs.includes {
			if seen[rel] {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	sort.Strings(read)
	return out, read
}

// scanTextualFile reads + scans one includer file for textual source includes,
// memoizing the result in cache so the same file is read at most once per pass
// (the per-target re-read of shared headers was the performance hit). The
// result is target-INDEPENDENT — it depends only on the file's path, bytes, and
// on-disk siblings — so caching by includer path is sound.
func scanTextualFile(hostSrc, s string, cache map[string]textualFileScan) textualFileScan {
	if r, ok := cache[s]; ok {
		return r
	}
	r := computeTextualFileScan(hostSrc, s)
	cache[s] = r
	return r
}

// computeTextualFileScan is the actual read + regex + resolve of one includer.
func computeTextualFileScan(hostSrc, s string) textualFileScan {
	data, err := os.ReadFile(filepath.Join(hostSrc, filepath.FromSlash(s)))
	if err != nil {
		// Missing/unreadable (e.g. a generated source absent at convert
		// time) — nothing to scan; skip rather than fail.
		return textualFileScan{}
	}
	dir := filepath.Dir(s)
	var includes []string
	for _, m := range quoteIncludeRe.FindAllSubmatch(data, -1) {
		inc := string(m[1])
		// Follow a quote-include of an implementation file: a compiled
		// source the includer textually pastes (fmt/gtest/lz4 fused-source),
		// OR a non-self-contained impl header (.inl/.tcc/.ipp/.txx/.def/.inc)
		// the genclass idiom pulls in (glm's `#include "func_common.inl"`,
		// VTK's `.txx`). A plain self-contained header (.h/.hpp) is NOT an
		// implementation include and is left alone.
		if !cclang.IsCompiledSource(inc) && !cclang.IsTextualImplHeader(inc) {
			continue
		}
		// An absolute include ("/usr/...", "C:\\...") is non-portable and
		// never a stageable in-tree textual source — reject it before
		// resolving. (filepath.Join folds an absolute element under dir
		// rather than honoring it, so it could otherwise coincidentally
		// match a same-named in-tree file and stage the wrong thing.)
		if strings.HasPrefix(inc, "/") || filepath.IsAbs(inc) {
			continue
		}
		rel := resolveTextualInclude(hostSrc, dir, inc, s)
		if rel == "" {
			continue
		}
		includes = append(includes, rel)
	}
	return textualFileScan{includes: includes, reader: len(includes) > 0}
}

// resolveTextualInclude resolves a quote-include `inc` (a compiled-source path)
// to an element-root-relative path. It tries the including file's own directory
// `dir` first — the fmt posix-mock `#include "../src/os.cc"` shape — then walks
// up the ancestor directories, the gtest "fused source" shape where
// `googletest/src/gtest-all.cc` does `#include "src/gtest.cc"` that cmake
// resolves against the target's include root (the package dir `googletest/`
// above `src/`), not the includer's `src/` dir. Returns the first candidate
// that exists on disk under hostSrc, isn't the includer `self`, and doesn't
// escape the element root; "" when none qualifies. The deepest (most specific)
// ancestor wins, since the walk starts at `dir`.
//
// A candidate the target ALSO compiles is returned too: cmake builds both
// shapes — VTK's bundled lz4 compiles lz4.c standalone AND lz4hc.c textually
// `#include "lz4.c"` for its internal statics. Under Bazel a sibling src is
// NOT an input of the includer's compile action, so the file must appear in
// textual_hdrs as well (it stays in srcs; the routing never removes it) or
// the includer fails "lz4.c: No such file or directory" in the sandbox.
func resolveTextualInclude(hostSrc, dir, inc, self string) string {
	for base := dir; ; base = filepath.Dir(base) {
		rel := filepath.ToSlash(filepath.Clean(filepath.Join(base, inc)))
		if rel != "." && rel != ".." && rel != self &&
			!strings.HasPrefix(rel, "../") {
			if st, err := os.Stat(filepath.Join(hostSrc, filepath.FromSlash(rel))); err == nil && !st.IsDir() {
				return rel
			}
		}
		// filepath.Dir("a")=="." and Dir(".")=="."; "/" is the abs-root fixpoint.
		if base == "." || base == "/" || base == "" {
			return ""
		}
	}
}

// synthesizeTextualSourceIncludeLibs wires the textual-source-include idiom
// into the package: a target whose sources quote-include a .cc
// (findTextualSourceIncludes) needs that file as a declared INPUT of the
// includer's compile action, so it belongs in a textual_hdrs slot — whether
// or not the target also compiles the file standalone (the fmt/gtest idiom
// doesn't; VTK's fused lz4 does, and keeps it in srcs). Two routings:
//
//   - cc_library / cc_interface HAVE a textual_hdrs attribute, so the included
//     sources are added directly to the target's own textual_hdrs. This is the
//     gtest/gmock "fused source" idiom — `gtest` compiles only
//     `src/gtest-all.cc`, which `#include`s `src/gtest.cc` et al.; those land
//     in `gtest`'s textual_hdrs.
//   - cc_binary / cc_test have NO textual_hdrs attribute, so a carrier
//     cc_library is synthesized (textual_hdrs = the included files) and added
//     to the target's deps (fmt's posix-mock-test). The synth lib is
//     co-located in the consumer's package (pkg.SubPackages[lib] =
//     pkg.SubPackages[consumer]) so the dep stays same-package under
//     --split-packages.
//
// Under --split-packages the emitter relabels textual_hdrs to cross-package
// file labels, exactly like hdrs. Gated on hostSrcOnDisk (the scan reads source
// files); breadcrumbed so the wiring is auditable.
func synthesizeTextualSourceIncludeLibs(pkg *ir.Package, hostSrc string, hostSrcOnDisk, detectFused bool, extraExts map[string]bool, warn io.Writer) {
	if pkg == nil {
		return
	}
	uniqueName := targetNamer(pkg)
	type rec struct {
		target, lib string
		srcs        []string
	}
	var recs []rec       // synth-lib wirings (cc_binary / cc_test)
	var inlineRecs []rec // direct textual_hdrs wirings (cc_library / cc_interface)
	var synth []ir.Target
	// One per-file scan cache for the whole pass; ONLY the opt-in fused scan
	// reads files, so this stays empty in the default (zero-read) path.
	scanCache := map[string]textualFileScan{}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCInterface, ir.KindCCBinary, ir.KindCCTest:
		default:
			continue
		}
		// (1) ZERO-READ extension routing. A Hdrs entry whose extension marks a
		// non-self-contained textual include — a compiled source (a
		// HEADER_FILE_ONLY .cc reclassifyHeaderOnlySources moved into hdrs), a
		// built-in impl header (.inl/.txx/.tcc/.ipp/.def/.inc), or an operator
		// --textual-include-exts extension — can only be pasted into an includer,
		// never compiled/parsed standalone (a parse_headers/layering_check build
		// would fail it alone). Route it to textual_hdrs from the IR alone, with
		// NO file read. This covers the LISTED impl-header / genclass idiom (the
		// common case) without touching the disk.
		extIncs := textualImplExtEntries(t.Hdrs, extraExts)
		// (2) OPT-IN read-based fused scan. Catches a textual include the target
		// does NOT list in hdrs — a compiled source #included from another file
		// (fmt's posix-mock, gtest's gtest-all.cc, VTK's lz4hc.c `#include
		// "lz4.c"`) or a header pulling an on-disk .cc (glm). It reads source
		// bytes, so it's gated on --detect-fused-sources (+ an on-disk tree); off
		// by default so the common convert reads zero files.
		var fusedIncs, readers []string
		if detectFused && hostSrcOnDisk {
			includers := append(append([]string(nil), t.Srcs...), t.Hdrs...)
			fusedIncs, readers = findTextualSourceIncludesCached(hostSrc, includers, scanCache)
		}
		incs := sliceutil.SortedUnique(append(append([]string(nil), extIncs...), fusedIncs...))
		if len(incs) == 0 {
			continue
		}
		// Only the fused scan read bytes; publish those includer reads so the
		// source-narrowing lens keeps them real (see ir.Package.SourceByteReads).
		// Extension routing reads nothing and contributes no reads.
		pkg.SourceByteReads = append(pkg.SourceByteReads, readers...)
		// Drop the routed entries from hdrs so they aren't double-listed; a
		// compiled-source fused include (lz4.c) isn't in hdrs, so it stays in srcs.
		removeFromHdrs(t, incs)
		lib := attachTextualSourceIncludes(pkg, t, incs, "cmake-codegen-textual-source-include", &synth, uniqueName)
		if lib == "" {
			inlineRecs = append(inlineRecs, rec{target: t.Name, srcs: incs})
		} else {
			recs = append(recs, rec{target: t.Name, lib: lib, srcs: incs})
		}
	}
	if len(synth) > 0 {
		pkg.Targets = append(pkg.Targets, synth...)
	}
	if warn != nil {
		if len(inlineRecs) > 0 {
			fmt.Fprintf(warn,
				"lower: routed textual_hdrs onto %d cc_library/cc_interface target(s) carrying a non-self-contained textual include (impl-header / fused-source idiom):\n",
				len(inlineRecs))
			for _, r := range inlineRecs {
				fmt.Fprintf(warn, "  %s (textual_hdrs: %s)\n", r.target, strings.Join(r.srcs, ", "))
			}
		}
		if len(recs) > 0 {
			fmt.Fprintf(warn,
				"lower: synthesized %d textual_hdrs cc_library(ies) for cc_binary/cc_test target(s) carrying a non-self-contained textual include (those rules have no textual_hdrs slot):\n",
				len(recs))
			for _, r := range recs {
				fmt.Fprintf(warn, "  %s -> %s (textual_hdrs: %s)\n", r.target, r.lib, strings.Join(r.srcs, ", "))
			}
		}
		// Note: the read-based fused-source scan is opt-in. When it's OFF and we
		// routed something by extension (the project uses textual includes), a
		// textual include the target does NOT list (a compiled source #included
		// from elsewhere) won't be detected — point the operator at the flag.
		if !detectFused && (len(inlineRecs) > 0 || len(recs) > 0) {
			fmt.Fprint(warn,
				"  note: the read-based fused-source scan is off (default, reads no files); pass --detect-fused-sources to also detect a source textually #included but not listed on the target (fmt/gtest/lz4-style) — SLOW: it reads every source + header.\n")
		}
	}
}

// textualImplExtEntries returns the Hdrs entries whose extension marks a
// non-self-contained textual include — a compiled source (a HEADER_FILE_ONLY
// .cc reclassified into hdrs), a built-in impl header
// (.inl/.txx/.tcc/.ipp/.def/.inc), or an operator-supplied extra extension.
// These move to textual_hdrs with no file read (the zero-read default path).
func textualImplExtEntries(hdrs []string, extra map[string]bool) []string {
	var out []string
	for _, h := range hdrs {
		if cclang.IsCompiledSource(h) || cclang.IsTextualImplHeader(h) || extra[strings.ToLower(filepath.Ext(h))] {
			out = append(out, h)
		}
	}
	return out
}

// normalizeTextualIncludeExts lowercases the operator's --textual-include-exts
// values and ensures a leading dot, into a set for textualImplExtEntries.
func normalizeTextualIncludeExts(exts []string) map[string]bool {
	if len(exts) == 0 {
		return nil
	}
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		m[e] = true
	}
	return m
}

// textualIncludeClosure expands a seed set of element-root-relative compiled
// sources to its transitive textual-include closure: each seed (and each newly
// reached file) is scanned ON DISK for quote-includes of OTHER compiled sources
// (resolved against the includer's dir + ancestors, like resolveTextualInclude),
// following the chain to fixpoint. OpenBLAS kernels #include sibling micro-kernel
// sources (caxpy.c -> caxpy_microk_haswell-2.c, often #ifdef-guarded per arch)
// that are themselves only ever textually included, so the whole chain — not
// just the first hop — must be staged. Returns the closure (seeds included),
// sorted, excluding any path in `compiled` (a source the target builds
// standalone) and any absent/escaping path. hostSrc must be on disk.
// Also returns `readers`: the closure files whose bytes yielded ≥1 further
// textual include — i.e. the files whose CONTENT shaped the closure (a leaf,
// read but with no resolving include, is part of the result by virtue of its
// parent's include line + its own existence, not its bytes, so it is NOT a
// reader). readers is the declared source-byte-read set this closure
// contributes (see ir.Package.SourceByteReads).
//
// ASYMMETRY with findTextualSourceIncludes, deliberate for now: that pass
// (post-lz4) surfaces a textually-included file the target ALSO compiles, on
// the sandbox argument that sibling srcs aren't inputs of each other's
// compile actions; this closure still excludes compiled files. The seeds
// here are generated-wrapper kernel chains whose members are include-only in
// the corpus (OpenBLAS micro-kernels), so no member trips the gap — if a
// closure chain ever reaches a compiled sibling, drop the `compiled`
// exclusion from push (and re-validate OpenBLAS) rather than rediscovering
// this note the hard way.
func textualIncludeClosure(hostSrc string, seeds []string, compiled map[string]bool) (closure, readers []string) {
	result := map[string]bool{}
	read := map[string]bool{}
	var work []string
	push := func(rel string) {
		if rel == "" || result[rel] || compiled[rel] {
			return
		}
		result[rel] = true
		work = append(work, rel)
	}
	for _, s := range seeds {
		push(filepath.ToSlash(filepath.Clean(s)))
	}
	for len(work) > 0 {
		cur := work[len(work)-1]
		work = work[:len(work)-1]
		data, err := os.ReadFile(filepath.Join(hostSrc, filepath.FromSlash(cur)))
		if err != nil {
			continue
		}
		dir := filepath.Dir(cur)
		for _, m := range quoteIncludeRe.FindAllSubmatch(data, -1) {
			inc := string(m[1])
			if !cclang.IsCompiledSource(inc) {
				continue
			}
			if strings.HasPrefix(inc, "/") || filepath.IsAbs(inc) {
				continue
			}
			rel := resolveTextualInclude(hostSrc, dir, inc, cur)
			if rel == "" {
				continue
			}
			read[cur] = true // cur's bytes resolved ≥1 child → output-affecting
			push(rel)
		}
	}
	return sliceutil.SortedKeys(result), sliceutil.SortedKeys(read)
}

// targetNamer returns a closure that yields collision-free target names within
// pkg: it seeds a set from the existing target names and appends a numeric
// suffix (`_1`, `_2`, …) until the candidate is unused, registering each name
// it hands out. Shared by the tail passes that synthesize carrier libraries.
func targetNamer(pkg *ir.Package) func(string) string {
	names := map[string]bool{}
	for i := range pkg.Targets {
		names[pkg.Targets[i].Name] = true
	}
	return func(base string) string {
		n := base
		for i := 1; names[n]; i++ {
			n = fmt.Sprintf("%s_%d", base, i)
		}
		names[n] = true
		return n
	}
}

// removeFromHdrs drops every path in `incs` from t.Hdrs (set membership). Used
// when a textually-#included impl header is moved into textual_hdrs, so the same
// file isn't declared in both attributes. Compiled-source incs are never in
// hdrs, so this only affects the impl-header (genclass) case.
func removeFromHdrs(t *ir.Target, incs []string) {
	if len(t.Hdrs) == 0 || len(incs) == 0 {
		return
	}
	drop := make(map[string]bool, len(incs))
	for _, p := range incs {
		drop[p] = true
	}
	kept := t.Hdrs[:0:0]
	for _, h := range t.Hdrs {
		if !drop[h] {
			kept = append(kept, h)
		}
	}
	t.Hdrs = kept
}

// attachTextualSourceIncludes routes the textually-#included source files
// `incs` (element-root-relative paths) onto consumer target t so they're
// declared INPUTS without being compiled standalone (which would duplicate
// their symbols). Two shapes, mirroring the two slots Bazel offers:
//
//   - cc_library / cc_interface HAVE a textual_hdrs attribute, so the sources
//     are added directly to t's own textual_hdrs; returns "".
//   - cc_binary / cc_test have NO textual_hdrs slot, so a carrier cc_library
//     (textual_hdrs = incs, tagged `tag`) is appended to *synth, added to t's
//     deps, and co-located in t's package (pkg.SubPackages) so the dep stays
//     same-package under --split-packages; returns the synth lib's name.
//
// uniqueName must be a collision-free namer over pkg (see targetNamer).
func attachTextualSourceIncludes(pkg *ir.Package, t *ir.Target, incs []string, tag string, synth *[]ir.Target, uniqueName func(string) string) string {
	if t.Kind == ir.KindCCLibrary || t.Kind == ir.KindCCInterface {
		for _, inc := range incs {
			t.TextualHdrs = appendUnique(t.TextualHdrs, inc)
		}
		sort.Strings(t.TextualHdrs)
		return ""
	}
	lib := uniqueName(t.Name + "_textual_srcs")
	*synth = append(*synth, ir.Target{
		Name:        lib,
		Kind:        ir.KindCCLibrary,
		TextualHdrs: incs,
		Visibility:  []string{"//visibility:private"},
		Tags:        []string{tag},
	})
	t.Deps = appendUnique(t.Deps, ":"+lib)
	// Co-locate the synth lib in the consumer's package so the dep stays
	// SAME-package under --split-packages. A private lib left in the root
	// package would be a cross-package dep — and Bazel-rejected — for a
	// consumer in a subpackage. pkg.SubPackages carries every target's dir
	// (root → ""), populated during lowering before this tail pass.
	if pkg.SubPackages != nil {
		pkg.SubPackages[lib] = pkg.SubPackages[t.Name]
	}
	return lib
}
