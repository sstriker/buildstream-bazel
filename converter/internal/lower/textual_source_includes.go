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
// quote-include marks a "textually include a .cc to intercept its internals"
// idiom (fmt's posix-mock-test does `#include "../src/os.cc"`). These are the
// extensions Bazel/rules_cc treat as COMPILED sources — a file with one of
// them can't simply be added to a consumer's srcs (it would be compiled
// standalone, duplicating its symbols), so the caller routes it to a
// textual_hdrs slot instead.
var ccSourceExts = map[string]bool{
	".cc": true, ".cpp": true, ".cxx": true, ".c++": true, ".c": true,
	".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
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
// hostSrcOnDisk. Each include resolves relative to its own including file's
// directory (quote-include semantics). A path that escapes the element root
// (".." after cleaning) or names a file the target already compiles, or one
// absent on disk, is skipped — only a real, in-tree, not-otherwise-compiled
// source qualifies.
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
			rel := filepath.ToSlash(filepath.Clean(filepath.Join(dir, inc)))
			// Escapes the element root (can't be expressed as a package input)
			// or is the includer itself — skip.
			if rel == "." || rel == s || strings.HasPrefix(rel, "../") || rel == ".." {
				continue
			}
			if compiled[rel] || seen[rel] {
				continue
			}
			if st, statErr := os.Stat(filepath.Join(hostSrc, filepath.FromSlash(rel))); statErr != nil || st.IsDir() {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// synthesizeTextualSourceIncludeLibs wires the textual-source-include idiom
// into the package for cc_binary / cc_test targets, which have NO
// textual_hdrs attribute. For each such target whose sources quote-include a
// .cc the target doesn't compile (findTextualSourceIncludes), it synthesizes a
// cc_library carrying those files in textual_hdrs and adds it to the target's
// deps: the file becomes a declared input (the quote-include resolves it
// relative to the including source) without being compiled standalone (which
// would duplicate its symbols). The synthesized lib lands in the root package
// (no SubPackages entry → root); under --split-packages its textual_hdrs are
// relabeled to cross-package file labels by the emitter, exactly like hdrs.
// Gated on hostSrcOnDisk (the scan reads source files); breadcrumbed so the
// synthesis is auditable. (cc_library targets, which DO have textual_hdrs, are
// left to add such files inline if a future case needs it — the synth-lib
// indirection exists only for the no-slot kinds.)
func synthesizeTextualSourceIncludeLibs(pkg *ir.Package, hostSrc string, hostSrcOnDisk bool, warn io.Writer) {
	if pkg == nil || !hostSrcOnDisk {
		return
	}
	names := map[string]bool{}
	for i := range pkg.Targets {
		names[pkg.Targets[i].Name] = true
	}
	uniqueName := func(base string) string {
		n := base
		for i := 1; names[n]; i++ {
			n = fmt.Sprintf("%s_%d", base, i)
		}
		names[n] = true
		return n
	}
	type rec struct {
		target, lib string
		srcs        []string
	}
	var recs []rec
	var synth []ir.Target
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindCCBinary && t.Kind != ir.KindCCTest {
			continue
		}
		incs := findTextualSourceIncludes(hostSrc, t.Srcs)
		if len(incs) == 0 {
			continue
		}
		lib := uniqueName(t.Name + "_textual_srcs")
		synth = append(synth, ir.Target{
			Name:        lib,
			Kind:        ir.KindCCLibrary,
			TextualHdrs: incs,
			Visibility:  []string{"//visibility:private"},
			Tags:        []string{"cmake-codegen-textual-source-include"},
		})
		t.Deps = appendUnique(t.Deps, ":"+lib)
		recs = append(recs, rec{target: t.Name, lib: lib, srcs: incs})
	}
	if len(synth) > 0 {
		pkg.Targets = append(pkg.Targets, synth...)
	}
	if len(recs) > 0 && warn != nil {
		fmt.Fprintf(warn,
			"lower: synthesized %d textual_hdrs cc_library(ies) for cc_binary/cc_test target(s) that textually #include a .cc they don't compile (those rules have no textual_hdrs slot):\n",
			len(recs))
		for _, r := range recs {
			fmt.Fprintf(warn, "  %s -> %s (textual_hdrs: %s)\n", r.target, r.lib, strings.Join(r.srcs, ", "))
		}
	}
}
