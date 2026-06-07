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
)

// quoteIncludeRe matches a C/C++ quote-form include directive — `#include
// "path"` — capturing the quoted path. Angle includes (`#include <...>`) are
// deliberately excluded: a textually-included source is always pulled by a
// relative quote path. Whitespace between `#`, `include`, and the quote is
// tolerated (`#  include "x"`). A pragmatic line scan, like the rest of the
// converter's include handling — not a full preprocessor.
var quoteIncludeRe = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*include[ \t]*"([^"]+)"`)

// ccSourceExts are the compiled-source extensions whose presence in a
// quote-include marks a "textually include a compiled source to intercept its
// internals" idiom (fmt's posix-mock-test does `#include "../src/os.cc"`;
// OpenBLAS's GenerateNamedObjects wrappers do `#include "kernel/x86_64/amax_sse.S"`).
// These are the extensions Bazel/rules_cc treat as COMPILED sources — a file
// with one of them can't simply be added to a consumer's srcs (it would be
// compiled standalone, duplicating its symbols), so the caller routes it to a
// textual_hdrs slot instead. Keys are lowercase; callers lowercase the ext
// before lookup (so preprocessed-assembly `.S` matches `.s`).
var ccSourceExts = map[string]bool{
	".cc": true, ".cpp": true, ".cxx": true, ".c++": true, ".c": true,
	".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
	".s": true, ".sx": true, ".asm": true,
}

// findTextualSourceIncludes scans a target's compiled source files for
// quote-includes of OTHER source files — `#include "x.cc"` — that the target
// does not itself compile. This is the "textually include a .cc to intercept
// its internals" idiom (fmt's posix-mock-test defines POSIX mocks, then
// `#include "../src/os.cc"` so os.cc's syscalls bind to them). Such a file
// must be a declared INPUT but must NOT be compiled standalone (that would
// duplicate its symbols), so it cannot sit in srcs; it's surfaced here so the
// caller can route it to a textual_hdrs slot (directly on a cc_library, or
// via a synthesized textual-hdrs cc_library dep for a cc_binary / cc_test,
// which have no textual_hdrs attribute).
//
// Returns element-root-relative paths (the shape of irt.Srcs), sorted and
// deduped. hostSrc must be the on-disk source root — the caller gates on
// hostSrcOnDisk. Each include resolves against the including file's own
// directory first (the fmt posix-mock `#include "../src/os.cc"` shape) and then
// against its ancestor directories (the gtest "fused source" shape, where
// gtest-all.cc does `#include "src/gtest.cc"` resolved against the target's
// include root — the package dir above src/); see resolveTextualInclude. A path
// that escapes the element root (".." after cleaning) or names a file the
// target already compiles, or one absent on disk, is skipped — only a real,
// in-tree, not-otherwise-compiled source qualifies.
func findTextualSourceIncludes(hostSrc string, srcs []string) []string {
	if hostSrc == "" || len(srcs) == 0 {
		return nil
	}
	compiled := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		compiled[filepath.ToSlash(filepath.Clean(s))] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range srcs {
		data, err := os.ReadFile(filepath.Join(hostSrc, filepath.FromSlash(s)))
		if err != nil {
			// Missing/unreadable (e.g. a generated source absent at convert
			// time) — nothing to scan; skip rather than fail.
			continue
		}
		dir := filepath.Dir(s)
		for _, m := range quoteIncludeRe.FindAllSubmatch(data, -1) {
			inc := string(m[1])
			if !ccSourceExts[strings.ToLower(filepath.Ext(inc))] {
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
			rel := resolveTextualInclude(hostSrc, dir, inc, s, compiled)
			if rel == "" || seen[rel] {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// resolveTextualInclude resolves a quote-include `inc` (a compiled-source path)
// to an element-root-relative path. It tries the including file's own directory
// `dir` first — the fmt posix-mock `#include "../src/os.cc"` shape — then walks
// up the ancestor directories, the gtest "fused source" shape where
// `googletest/src/gtest-all.cc` does `#include "src/gtest.cc"` that cmake
// resolves against the target's include root (the package dir `googletest/`
// above `src/`), not the includer's `src/` dir. Returns the first candidate
// that exists on disk under hostSrc, isn't the includer `self`, isn't already
// compiled, and doesn't escape the element root; "" when none qualifies. The
// deepest (most specific) ancestor wins, since the walk starts at `dir`.
func resolveTextualInclude(hostSrc, dir, inc, self string, compiled map[string]bool) string {
	for base := dir; ; base = filepath.Dir(base) {
		rel := filepath.ToSlash(filepath.Clean(filepath.Join(base, inc)))
		if rel != "." && rel != ".." && rel != self &&
			!strings.HasPrefix(rel, "../") && !compiled[rel] {
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
// into the package: a target whose sources quote-include a .cc the target
// doesn't compile (findTextualSourceIncludes) needs that file as a declared
// INPUT but must NOT compile it standalone (that would duplicate its symbols),
// so it belongs in a textual_hdrs slot. Two shapes:
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
func synthesizeTextualSourceIncludeLibs(pkg *ir.Package, hostSrc string, hostSrcOnDisk bool, warn io.Writer) {
	if pkg == nil || !hostSrcOnDisk {
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
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCInterface, ir.KindCCBinary, ir.KindCCTest:
		default:
			continue
		}
		incs := findTextualSourceIncludes(hostSrc, t.Srcs)
		if len(incs) == 0 {
			continue
		}
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
				"lower: added textual_hdrs to %d cc_library/cc_interface target(s) that textually #include a .cc they don't compile (the fused-source idiom):\n",
				len(inlineRecs))
			for _, r := range inlineRecs {
				fmt.Fprintf(warn, "  %s (textual_hdrs: %s)\n", r.target, strings.Join(r.srcs, ", "))
			}
		}
		if len(recs) > 0 {
			fmt.Fprintf(warn,
				"lower: synthesized %d textual_hdrs cc_library(ies) for cc_binary/cc_test target(s) that textually #include a .cc they don't compile (those rules have no textual_hdrs slot):\n",
				len(recs))
			for _, r := range recs {
				fmt.Fprintf(warn, "  %s -> %s (textual_hdrs: %s)\n", r.target, r.lib, strings.Join(r.srcs, ", "))
			}
		}
	}
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
func textualIncludeClosure(hostSrc string, seeds []string, compiled map[string]bool) []string {
	result := map[string]bool{}
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
			if !ccSourceExts[strings.ToLower(filepath.Ext(inc))] {
				continue
			}
			if strings.HasPrefix(inc, "/") || filepath.IsAbs(inc) {
				continue
			}
			push(resolveTextualInclude(hostSrc, dir, inc, cur, compiled))
		}
	}
	out := make([]string, 0, len(result))
	for r := range result {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
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
