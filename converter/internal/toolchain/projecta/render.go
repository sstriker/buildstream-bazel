// Package projecta renders the "project A" BUILD.bazel that drives
// per-cell cmake probes via Bazel's action graph. Each cell is one
// (variant, platform) pair: the BUILD.bazel declares one genrule
// per cell, the genrule invokes probe-cell with the variant's
// CacheVars, and exec_compatible_with carries the platform's
// constraint set so Bazel routes the action to a worker that
// matches.
//
// One filegroup ("all_probes") aggregates every cell's output so
// downstream tooling can `bazel build //probe:all_probes` to
// materialize the entire matrix in one command.
//
// The renderer is pure (input → bytes); CLI plumbing lives in
// cmd/render-project-a. The skeleton is reusable: the future
// per-element multi-platform fold plan will introduce a second
// cell type (per-element conversion) that emits the same BUILD.bazel
// shape with a different worker binary and different per-cell args.
package projecta

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
)

// Platform pairs a stable name with the constraint_value labels
// that identify the platform in Bazel's toolchain resolver. The
// name appears in cell output filenames; the constraints feed
// genrule.exec_compatible_with.
type Platform struct {
	// Name is a short, filesystem-safe identifier
	// ("linux_x86_64", "linux_aarch64"). Combined with the
	// variant's name to produce the cell label.
	Name string

	// Constraints are Bazel labels of constraint_value targets,
	// e.g. ["@platforms//os:linux", "@platforms//cpu:x86_64"].
	// Match the constraint_values list of the platform() rule
	// the operator's //platforms package declares.
	Constraints []string
}

// ToolchainProbeArgs is the input to the toolchain-probe cell type.
type ToolchainProbeArgs struct {
	// CmakeSourceLabel is the Bazel label of the probe project
	// filegroup the genrules consume as srcs. Typical:
	// "//probe:source" (a filegroup that globs CMakeLists.txt +
	// probe.c + probe.cpp under the probe-project package).
	CmakeSourceLabel string

	// CmakeListsLabel is the Bazel label of the CMakeLists.txt
	// file specifically. The genrule uses $(execpath ...) on this
	// label to derive the cmake source root via dirname.
	CmakeListsLabel string

	// ProbeCellLabel is the Bazel label of the probe-cell binary
	// the genrules invoke. Typical: "//tools:probe-cell".
	ProbeCellLabel string

	// Variants is the list of variants to probe (one per cell per
	// platform). Typically loaded from the project's CMakePresets.json
	// via toolchain/presets.LoadFile.
	Variants []toolchain.Variant

	// Platforms is the set of platforms to probe each variant on.
	// Operator-supplied; the unifier (Stage 5) requires the same
	// platform set when folding probe.json artifacts back into
	// per-platform ResolvedToolchains.
	Platforms []Platform
}

// RenderToolchainProbe renders one BUILD.bazel for the toolchain-probe
// cell type. The output bytes can be written directly to
// <project-A-package>/BUILD.bazel.
//
// Returns an error if any required label or platform field is
// empty (defensive — Bazel parser errors are far less actionable
// than a Go-side check naming the missing field).
func RenderToolchainProbe(args ToolchainProbeArgs) ([]byte, error) {
	if args.CmakeSourceLabel == "" {
		return nil, fmt.Errorf("projecta: CmakeSourceLabel required")
	}
	if args.CmakeListsLabel == "" {
		return nil, fmt.Errorf("projecta: CmakeListsLabel required")
	}
	if args.ProbeCellLabel == "" {
		return nil, fmt.Errorf("projecta: ProbeCellLabel required")
	}
	if len(args.Variants) == 0 {
		return nil, fmt.Errorf("projecta: Variants must be non-empty")
	}
	if len(args.Platforms) == 0 {
		return nil, fmt.Errorf("projecta: Platforms must be non-empty")
	}
	seenPlat := map[string]bool{}
	for i, p := range args.Platforms {
		if p.Name == "" {
			return nil, fmt.Errorf("projecta: Platforms[%d] has empty Name", i)
		}
		// '.' splits filenames in unify-toolchains
		// (<platform>.<variant>.probe.json — strings.IndexByte('.')
		// recovers the platform); a dotted platform name would land
		// the cell under the wrong group. Reject up front rather
		// than producing ambiguous artifacts.
		if strings.ContainsRune(p.Name, '.') {
			return nil, fmt.Errorf("projecta: Platforms[%d].Name %q contains '.', reserved as the <platform>.<variant> separator in probe artifact filenames", i, p.Name)
		}
		// Duplicate platform names → duplicate Bazel target names
		// in the rendered BUILD.bazel (genrule(name="<plat>.<var>")
		// repeats per platform per variant). Reject before Bazel
		// has to.
		if seenPlat[p.Name] {
			return nil, fmt.Errorf("projecta: duplicate Platforms[].Name %q", p.Name)
		}
		seenPlat[p.Name] = true
		if len(p.Constraints) == 0 {
			return nil, fmt.Errorf("projecta: Platforms[%d] (%s) has no Constraints", i, p.Name)
		}
	}
	seenVar := map[string]bool{}
	for i, v := range args.Variants {
		if v.Name == "" {
			return nil, fmt.Errorf("projecta: Variants[%d] has empty Name", i)
		}
		// Variant names become Bazel target name suffixes ("<plat>.<var>")
		// and probe artifact filenames; reject characters Bazel doesn't
		// accept in target names. The allowed set is the conservative
		// subset used elsewhere in the project (alphanumerics + '_' + '-').
		for _, r := range v.Name {
			ok := (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				r == '_' || r == '-'
			if !ok {
				return nil, fmt.Errorf("projecta: Variants[%d].Name %q contains %q; allowed: [a-zA-Z0-9_-]", i, v.Name, r)
			}
		}
		if seenVar[v.Name] {
			return nil, fmt.Errorf("projecta: duplicate Variants[].Name %q", v.Name)
		}
		seenVar[v.Name] = true
	}

	var b bytes.Buffer
	b.WriteString("# Generated by render-project-a. DO NOT EDIT.\n")
	b.WriteString("# Drives per-cell cmake probes; one genrule per (variant, platform).\n")
	b.WriteString("\n")

	var allOuts []string
	// Outer loop: platforms; inner: variants. Stable ordering for
	// byte-deterministic output across runs.
	for _, p := range args.Platforms {
		for _, v := range args.Variants {
			cell := cellName(v.Name, p.Name)
			out := cell + ".probe.json"
			allOuts = append(allOuts, out)
			renderCellGenrule(&b, args, p, v, cell, out)
		}
	}

	fmt.Fprintf(&b, "filegroup(\n")
	fmt.Fprintf(&b, "    name = %q,\n", "all_probes")
	fmt.Fprintf(&b, "    srcs = [\n")
	for _, o := range allOuts {
		fmt.Fprintf(&b, "        %q,\n", o)
	}
	fmt.Fprintf(&b, "    ],\n")
	fmt.Fprintf(&b, "    visibility = [\"//visibility:public\"],\n")
	fmt.Fprintf(&b, ")\n")

	return b.Bytes(), nil
}

// renderCellGenrule emits one (variant, platform) cell.
func renderCellGenrule(b *bytes.Buffer, args ToolchainProbeArgs, p Platform, v toolchain.Variant, cell, out string) {
	fmt.Fprintf(b, "genrule(\n")
	fmt.Fprintf(b, "    name = %q,\n", cell)
	// CmakeListsLabel must be a direct prerequisite for the
	// $(execpath ...) reference in cmd to resolve at analysis
	// time. Bazel deduplicates inputs in the action's merkle
	// tree, so listing it alongside the source filegroup that
	// already includes it is safe.
	if args.CmakeSourceLabel == args.CmakeListsLabel {
		fmt.Fprintf(b, "    srcs = [%q],\n", args.CmakeSourceLabel)
	} else {
		fmt.Fprintf(b, "    srcs = [\n")
		fmt.Fprintf(b, "        %q,\n", args.CmakeSourceLabel)
		fmt.Fprintf(b, "        %q,\n", args.CmakeListsLabel)
		fmt.Fprintf(b, "    ],\n")
	}
	fmt.Fprintf(b, "    outs = [%q],\n", out)
	fmt.Fprintf(b, "    tools = [%q],\n", args.ProbeCellLabel)

	// exec_compatible_with: deterministic order via sort. The
	// constraint set itself is what matters to Bazel; the order
	// is for golden-test stability.
	cs := append([]string(nil), p.Constraints...)
	sort.Strings(cs)
	fmt.Fprintf(b, "    exec_compatible_with = [\n")
	for _, c := range cs {
		fmt.Fprintf(b, "        %q,\n", c)
	}
	fmt.Fprintf(b, "    ],\n")

	fmt.Fprintf(b, "    cmd = \"\"\"\n")
	fmt.Fprintf(b, "        $(location %s) \\\n", args.ProbeCellLabel)
	fmt.Fprintf(b, "            --cmake-source $$(dirname $(execpath %s)) \\\n", args.CmakeListsLabel)
	fmt.Fprintf(b, "            --variant %s \\\n", shellQuote(v.Name))
	for _, k := range sortedCacheVarKeys(v) {
		fmt.Fprintf(b, "            --cache-var %s \\\n", shellQuote(k+"="+v.CacheVars[k]))
	}
	// Each cell has a single declared output, so $@ resolves to
	// that output's exec-root path. The $(location ...) form would
	// also work but $@ is shorter and matches the rest of this
	// repo's genrules.
	fmt.Fprintf(b, "            --out $@\n")
	fmt.Fprintf(b, "    \"\"\",\n")
	fmt.Fprintf(b, ")\n\n")
}

// cellName joins variant + platform into a stable,
// filesystem-safe identifier. Order: <platform>.<variant> so that
// `ls` groups cells by platform first — usually what a human
// debugging the matrix wants.
func cellName(variantName, platformName string) string {
	return platformName + "." + variantName
}

// sortedCacheVarKeys returns the Variant's cache var keys in
// lexicographic order. Mirrors toolchain.SortedCacheVarKeys but
// inlined here to avoid a circular import once projecta becomes
// a reusable building block.
func sortedCacheVarKeys(v toolchain.Variant) []string {
	keys := make([]string, 0, len(v.CacheVars))
	for k := range v.CacheVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shellQuote wraps a string in single quotes for shell-safe
// embedding inside the genrule's cmd. Single quotes preserve
// every byte literally except a single quote itself, which we
// escape via the standard close-quote / backslash-escaped quote
// / open-quote sequence (single quote, backslash, single quote,
// single quote — written literally as the four-byte run
// `'\”`). This handles cmake's flag values that include spaces,
// equals signs, etc.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\$`!#&|;<>(){}[]*?") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
