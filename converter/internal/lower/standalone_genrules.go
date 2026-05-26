package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// lowerStandaloneCustomCommands walks every CUSTOM_COMMAND edge in
// build.ninja and emits an ir.KindGenrule for each edge whose outputs
// aren't already covered by an existing genrule in cc.Genrules.
//
// Phase 4 of the generator-parity uplift (ROADMAP.md). The existing
// recoverGenrule path covers custom-command edges whose outputs
// other cmake targets reference as sources; this consumer picks up
// the standalone shape (add_custom_target without a downstream
// consumer, add_custom_command that drives an out-of-graph side
// effect like a code generator that produces files no cmake target
// claims).
//
// Dedup: an edge is skipped when ANY of its outputs is already
// present as an `outs` entry in cc.Genrules. First-output-wins isn't
// safe because a single CUSTOM_COMMAND can produce multiple
// outputs and only one might be referenced by another target.
//
// Naming: standalone genrules name themselves
// `custom_command_<sanitized first output>` so the name is stable
// across rebuilds. Conflicting names (multiple edges that would
// collide after sanitization) get a `_<index>` suffix.
//
// buildDir is the cmake build directory — used to convert build-
// relative output paths to package-relative paths the emitted
// genrule's outs reference.
func lowerStandaloneCustomCommands(g *ninja.Graph, existing []ir.Target, buildDir string) []ir.Target {
	if g == nil {
		return nil
	}
	covered := coveredOuts(existing)
	edges := ninja.CustomCommandEdges(g)
	if len(edges) == 0 {
		return nil
	}

	var out []ir.Target
	seenNames := map[string]int{}
	for _, b := range edges {
		if edgeCovered(b, covered) {
			continue
		}
		cmd, ok := ninja.CommandFor(g, b)
		if !ok || cmd == "" {
			// Rule without a command binding — cmake's no-op
			// stamp shape (just declares a phony output). Skip.
			continue
		}
		// All outputs reference relative to the build dir's
		// per-target convention; emit them as-is. Stripping
		// buildDir isn't safe because the outputs are already
		// build-dir-relative in cmake's Ninja generator.
		outs := append([]string(nil), b.Outputs...)
		outs = append(outs, b.ImplicitOuts...)
		// Drop outputs that contain unexpanded ninja variable
		// references (e.g. `${cmake_ninja_workdir}foo.txt`).
		// cmake's Ninja generator pairs every real custom-command
		// output with a `${cmake_ninja_workdir}<basename>`
		// implicit-output shadow so the restat=1 semantics see
		// both relative and absolute paths; a Bazel genrule can't
		// declare an outs entry whose path is a ninja-time
		// variable reference. The real (variable-free) output
		// stays in the list and drives the genrule's outs / name.
		outs = filterOutVarRefs(outs)
		// Sort for byte-stability and dedup.
		sort.Strings(outs)
		outs = dedupSorted(outs)
		if len(outs) == 0 {
			continue
		}
		// Skip cmake-internal bookkeeping edges. cmake's Ninja
		// generator adds standalone CUSTOM_COMMAND edges for its
		// own IDE / regen workflows: `CMakeFiles/edit_cache.util`,
		// `CMakeFiles/rebuild_cache.util`, and a handful more
		// (install / package / package_source / test / list_install
		// _components) under multi-target generators. These run
		// cmake itself or echo a message; lifting them to genrules
		// in the rendered BUILD.bazel adds noise without
		// operator-visible value (the cmake re-invoke shape
		// doesn't work outside cmake's own build dir, and the IDE
		// hooks are no-ops under bazel anyway). Filter every edge
		// whose first output is `CMakeFiles/<name>.util` —
		// cmake's bookkeeping outputs uniformly land under that
		// path with the .util suffix.
		if isCMakeBookkeepingOutput(outs[0]) {
			continue
		}
		srcs := append([]string(nil), b.Inputs...)
		srcs = append(srcs, b.ImplicitInputs...)
		sort.Strings(srcs)
		srcs = dedupSorted(srcs)

		baseName := "custom_command_" + sanitizeOutputName(outs[0])
		name := baseName
		if n, used := seenNames[baseName]; used {
			seenNames[baseName] = n + 1
			name = baseName + "_" + intToStr(n+1)
		} else {
			seenNames[baseName] = 1
		}

		out = append(out, ir.Target{
			Name:        name,
			Kind:        ir.KindGenrule,
			Srcs:        srcs,
			GenruleOuts: outs,
			GenruleCmd:  cmd,
			Visibility:  []string{"//visibility:private"},
			Tags:        []string{"cmake-codegen-standalone-custom-command"},
		})
	}
	return out
}

// coveredOuts collects every output path that an existing IR
// genrule already declares. Used to dedup standalone-edge emission
// against the recoverGenrule path.
func coveredOuts(existing []ir.Target) map[string]bool {
	covered := map[string]bool{}
	for _, t := range existing {
		if t.Kind != ir.KindGenrule {
			continue
		}
		for _, o := range t.GenruleOuts {
			covered[o] = true
		}
	}
	return covered
}

// edgeCovered reports whether ANY of the build edge's outputs are
// already covered by an existing genrule. Conservative dedup:
// even one overlap is enough to skip, on the theory that the
// existing genrule was emitted for a good reason and emitting a
// second one for the same producer would double-build.
func edgeCovered(b *ninja.Build, covered map[string]bool) bool {
	for _, o := range b.Outputs {
		if covered[o] {
			return true
		}
	}
	for _, o := range b.ImplicitOuts {
		if covered[o] {
			return true
		}
	}
	return false
}

// filterOutVarRefs returns xs with any entry containing an
// unexpanded ninja variable reference (substring `${`) removed.
// cmake's Ninja generator pairs every real CUSTOM_COMMAND output
// with a `${cmake_ninja_workdir}<basename>` implicit-output
// shadow used for restat tracking — those shadow paths are
// ninja-internal and can't be emitted as Bazel genrule outs.
//
// Note: also drops outputs that use any other unresolved
// ninja-var reference — none of those have a defined meaning at
// Bazel emission time either.
func filterOutVarRefs(xs []string) []string {
	out := xs[:0]
	for _, x := range xs {
		if strings.Contains(x, "${") {
			continue
		}
		out = append(out, x)
	}
	return out
}

// isCMakeBookkeepingOutput reports whether a build-edge output
// path is one of cmake's internal IDE / regen utility outputs.
// cmake's Ninja generator emits a standalone CUSTOM_COMMAND for
// each of edit_cache / rebuild_cache (always present), plus a
// handful more under multi-target generators (install / package /
// package_source / test / list_install_components). The
// canonical shape is `CMakeFiles/<name>.util` — checking that
// prefix + suffix pair is both necessary and sufficient: no
// user-declared add_custom_command lands an output under
// `CMakeFiles/<n>.util` (cmake reserves the `.util` extension
// for these bookkeeping edges).
func isCMakeBookkeepingOutput(p string) bool {
	const prefix = "CMakeFiles/"
	const suffix = ".util"
	return strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix)
}

// sanitizeOutputName converts a path like `gen/version.h` into a
// Bazel target-name-safe stem like `gen_version_h`. Mirrors the
// directory-installer sanitizer but tuned for path-with-extension
// shapes (preserves the `.h` / `.cc` suffix as `_h` / `_cc`).
func sanitizeOutputName(p string) string {
	clean := filepath.ToSlash(filepath.Clean(p))
	var sb strings.Builder
	sb.Grow(len(clean))
	lastWasUnderscore := false
	for _, r := range clean {
		isAlnum := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
		if isAlnum {
			sb.WriteRune(r)
			lastWasUnderscore = false
			continue
		}
		if !lastWasUnderscore {
			sb.WriteRune('_')
			lastWasUnderscore = true
		}
	}
	return strings.Trim(sb.String(), "_")
}

// dedupSorted removes consecutive duplicates from a sorted slice.
// More efficient than a map-based dedup for small slices (typical
// build edge has 1-2 outputs).
func dedupSorted(xs []string) []string {
	if len(xs) <= 1 {
		return xs
	}
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// intToStr is a tiny non-fmt itoa for the name-collision suffix.
// Avoids pulling fmt into the per-edge loop's hot path.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
