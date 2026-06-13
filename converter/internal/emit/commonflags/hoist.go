package commonflags

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// HoistCommonCopts finds the longest copt prefix shared by every eligible cc
// target, strips it from each, and tags those targets
// `features = [featureName]` so the prefix is reapplied by the cc_toolchain
// feature (before per-target copts — matching cmake's global-flags-first
// order). Returns the hoisted prefix (the flags Emit's feature .bzl should
// carry), or nil when there's nothing to hoist. Mutates pkg in place.
//
// Eligibility: cc_library / cc_binary / cc_test / cc_interface targets with a
// non-empty flat Copts. Targets with empty copts are skipped (nothing to
// strip, and they shouldn't shrink the shared prefix). A target's PerPlatform
// copts select-arms are untouched — only the always-applied flat prefix is
// hoisted, which the always-enabled-per-target feature faithfully restores.
//
// Conservative by construction: the prefix is what ALL candidates share from
// position 0, so every candidate is stripped uniformly and order is preserved
// exactly (feature flags emit before the target's remaining copts). A single
// candidate whose flags diverge at position k caps the prefix at k; no
// reordering risk.
//
// This is the cc_toolchain-FEATURE mode (--out-common-compile-flags-feature),
// which only hoists copts: a feature is a compile-flag construct, with no
// faithful home for link flags or the PRIVATE local_defines a feature would
// over-apply. The self-contained `.bzl` mode (HoistCommonFlagsToConstants)
// hoists all three axes.
func HoistCommonCopts(pkg *ir.Package, featureName string) []string {
	if pkg == nil {
		return nil
	}
	prefix := hoistAxis(pkg, axisCopts, func(t *ir.Target) {
		if !contains(t.Features, featureName) {
			t.Features = append(t.Features, featureName)
		}
	})
	return prefix
}

// axisSpec describes one hoistable flag axis: how to read/write the target's
// flat slice for it, and whether the axis is a SET (order-insensitive, so it's
// sorted before the common-prefix is taken — local_defines) or order-SENSITIVE
// (copts/linkopts, where flag order is load-bearing and must be preserved).
type axisSpec struct {
	get      func(*ir.Target) []string
	set      func(*ir.Target, []string)
	sortList bool
}

var (
	axisCopts = axisSpec{
		get: func(t *ir.Target) []string { return t.Copts },
		set: func(t *ir.Target, v []string) { t.Copts = v },
		// copts order matters (cmake global-flags-first); never sort.
	}
	axisLocalDefines = axisSpec{
		get:      func(t *ir.Target) []string { return t.LocalDefines },
		set:      func(t *ir.Target, v []string) { t.LocalDefines = v },
		sortList: true, // a -D set; the emitter sorts local_defines anyway.
	}
	axisLinkopts = axisSpec{
		get: func(t *ir.Target) []string { return t.LinkOpts },
		set: func(t *ir.Target, v []string) { t.LinkOpts = v },
		// link order matters; never sort.
	}
)

// hoistAxis computes the longest prefix every eligible cc target shares on one
// axis, strips it from each candidate (storing the remainder back), runs mark
// on each stripped target, and returns the prefix (or nil when nothing hoists).
// For a SET axis (sortList) each candidate's slice is sorted before the prefix
// is taken, so the prefix is the common LEADING sorted elements and the stored
// remainder stays sorted — `<CONST> + [delta]` recombines to the same sorted
// set the inline emission would render. Conservative for sets: it hoists only
// the common leading elements, not the maximal common subset.
func hoistAxis(pkg *ir.Package, ax axisSpec, mark func(*ir.Target)) []string {
	var cands []*ir.Target
	var lists [][]string
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCBinary, ir.KindCCTest, ir.KindCCInterface:
		default:
			continue
		}
		vals := ax.get(t)
		if len(vals) == 0 {
			continue
		}
		list := append([]string(nil), vals...)
		if ax.sortList {
			sort.Strings(list)
		}
		cands = append(cands, t)
		lists = append(lists, list)
	}
	if len(cands) < 2 {
		return nil
	}
	prefix := longestCommonPrefix(lists)
	if len(prefix) == 0 {
		return nil
	}
	for i, t := range cands {
		rest := lists[i][len(prefix):]
		if len(rest) == 0 {
			ax.set(t, nil)
		} else {
			ax.set(t, append([]string(nil), rest...))
		}
		mark(t)
	}
	return prefix
}

// longestCommonPrefix returns the longest leading run of strings every list
// shares (by exact equality, position by position).
func longestCommonPrefix(lists [][]string) []string {
	first := lists[0]
	n := len(first)
	for _, l := range lists[1:] {
		if len(l) < n {
			n = len(l)
		}
	}
	i := 0
	for i < n {
		v := first[i]
		match := true
		for _, l := range lists[1:] {
			if l[i] != v {
				match = false
				break
			}
		}
		if !match {
			break
		}
		i++
	}
	return append([]string(nil), first[:i]...)
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// HoistCommonFlagsToConstants is the self-contained `defs.bzl` mode: it strips
// the longest shared PREFIX on each of copts, local_defines, and linkopts from
// every eligible cc target, sets the matching Prepend* flag on each stripped
// target, and records loadLabel on the package — so the emitter renders
// `<CONST> + [delta]` per axis and a single `load(loadLabel, …)` of the
// constants actually referenced. The flags ride one generated
// `common_compile_flags.bzl` (EmitConstants) the emitted BUILDs load directly
// — no cc_toolchain wiring. Returns the three hoisted prefixes (for
// EmitConstants). Mutates pkg in place.
func HoistCommonFlagsToConstants(pkg *ir.Package, loadLabel string) (copts, localDefines, linkopts []string) {
	if pkg == nil {
		return nil, nil, nil
	}
	copts = hoistAxis(pkg, axisCopts, func(t *ir.Target) { t.PrependCommonCopts = true })
	localDefines = hoistAxis(pkg, axisLocalDefines, func(t *ir.Target) { t.PrependCommonLocalDefines = true })
	linkopts = hoistAxis(pkg, axisLinkopts, func(t *ir.Target) { t.PrependCommonLinkopts = true })
	if len(copts) > 0 || len(localDefines) > 0 || len(linkopts) > 0 {
		pkg.CommonCoptsLabel = loadLabel
	}
	return copts, localDefines, linkopts
}

// HoistCommonCoptsToConstant is the copts-only entry point retained for
// callers that only hoist compile flags; it delegates to the generalized
// HoistCommonFlagsToConstants and returns just the copts prefix.
func HoistCommonCoptsToConstant(pkg *ir.Package, loadLabel string) []string {
	copts, _, _ := HoistCommonFlagsToConstants(pkg, loadLabel)
	return copts
}

// EmitConstants renders the self-contained `common_compile_flags.bzl`. It
// always defines COMMON_COPTS (an empty list when nothing hoisted, so a stray
// load never breaks); COMMON_LOCAL_DEFINES and COMMON_LINKOPTS are emitted only
// when non-empty, so a copts-only project's `.bzl` stays byte-identical to the
// copts-only era.
func EmitConstants(copts, localDefines, linkopts []string) []byte {
	var buf bytes.Buffer
	buf.WriteString(constHeader)
	writeConst(&buf, "COMMON_COPTS", copts, true)
	writeConst(&buf, "COMMON_LOCAL_DEFINES", localDefines, false)
	writeConst(&buf, "COMMON_LINKOPTS", linkopts, false)
	return buf.Bytes()
}

// EmitConstant is the copts-only shim (COMMON_COPTS only), kept for callers /
// tests predating the multi-axis hoist.
func EmitConstant(copts []string) []byte {
	return EmitConstants(copts, nil, nil)
}

// writeConst appends `NAME = [...]` for a flag list. When empty: emits
// `NAME = []` if always is set (COMMON_COPTS's stable contract), else nothing.
func writeConst(buf *bytes.Buffer, name string, flags []string, always bool) {
	if len(flags) == 0 {
		if always {
			fmt.Fprintf(buf, "%s = []\n", name)
		}
		return
	}
	fmt.Fprintf(buf, "%s = [\n", name)
	for _, f := range flags {
		fmt.Fprintf(buf, "    %q,\n", f)
	}
	buf.WriteString("]\n")
}

const constHeader = `# Generated by convert-element-cmake. DO NOT EDIT.
#
# The flags shared by EVERY converted cc target — the longest common PREFIX of
# each axis, typically cmake's project-wide CMAKE_<LANG>_FLAGS / link flags /
# compile definitions:
#   COMMON_COPTS          shared copts (-O2, arch flags, ...)
#   COMMON_LOCAL_DEFINES  shared PRIVATE -D defines (only when present)
#   COMMON_LINKOPTS       shared link flags (only when present)
# The converter STRIPPED these from each target and rewrote that axis as
# CONST + [per-target flags], so the flags live in one place instead of
# repeating per target. Self-contained: no cc_toolchain wiring needed (cf. the
# --out-common-compile-flags-feature mode, which hoists copts only).
#
# Byte-stable across runs; re-generated by
# convert-element-cmake --emit-common-compile-flags-bzl.

`
