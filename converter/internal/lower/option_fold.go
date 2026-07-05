package lower

import (
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// The option fold (--lift-options; stages a+b of the option-lift
// ROADMAP.md item) is the multi-config fold on the option axis:
// instead of diffing codemodels across build types, it diffs the
// primary configure's codemodel against a flip configure's (same
// project, one option() cache entry inverted) and projects the
// attribute deltas onto select() arms keyed by
// //options:<name>_{on,off} config_settings. The orchestration
// half (running the flip configures, the target-set guard, the
// //options package emit) lives in cmd/convert-element-cmake;
// this file is the pure fold on an already-loaded pair of views.

// OptionCellLabel maps a lifted cmake option name + boolean value
// onto the select() arm label the fold keys deltas under:
// //options:<name-lowercased>_{on,off}. The backing bool_flag +
// config_settings are emitted by convert-element-cmake
// --out-option-settings (the emit/optionsettings package); the
// TestOptionCellLabel_MatchesOptionSettingsEmit parity test pins
// that the two sides agree on naming.
func OptionCellLabel(option string, on bool) string {
	suffix := "_off"
	if on {
		suffix = "_on"
	}
	return "//options:" + toLower(option) + suffix
}

// ApplyOptionFold projects one lifted option's cross-value deltas
// into PerPlatform-shaped select() arms on pkg.Targets. byCell maps
// target NAME → arm label → that cell's codemodel view, with
// exactly two cells per target: the primary configure's view under
// OptionCellLabel(name, baseValue) and the flip configure's under
// the inverted label. Keying on target name (not codemodel id)
// keeps the fold robust to any id drift between the two replies —
// the caller's target-set guard has already established the name
// sets match.
//
// idToName translates codemodel dependency ids (from EITHER reply)
// to target names for the deps arms; cmakeSrc / cmakeBuild anchor
// the same path-reanchoring passes the multi-config fold applies
// (the caller canonicalizes the flip reply's scratch build-dir
// paths onto cmakeBuild before handing views here).
//
// Returns the names of targets that gained at least one select()
// arm — empty means the option toggles nothing attribute-shaped
// (or nothing the fold routes), so the caller can skip emitting a
// flag nobody reads. Baseline facts (identical in both cells) are
// left untouched: they're already in the flat attributes from the
// primary lower. Mirrors lowerMultiConfigDeltas' shape; no PCH
// pass (config-varying PCH is a $<CONFIG> idiom, not an option
// one) and no sanitizer filtering (option names aren't config
// names).
func ApplyOptionFold(pkg *ir.Package, byCell map[string]map[string]fileapi.Target, cells []string, cmakeSrc, cmakeBuild string, idToName map[string]string) []string {
	if pkg == nil || len(byCell) == 0 || len(cells) != 2 {
		return nil
	}
	folds := configfold.Project(byCell, cells)
	if len(folds) == 0 {
		return nil
	}

	byName := map[string]*ir.Target{}
	for i := range pkg.Targets {
		byName[pkg.Targets[i].Name] = &pkg.Targets[i]
	}

	identity := func(cell string) string { return cell }
	var lifted []string
	for _, fold := range folds {
		tgt, ok := byName[fold.Name]
		if !ok {
			continue
		}
		before := perPlatformArmCount(tgt)
		applyPartition(tgt, "defines", fold.Defines, cmakeSrc, cmakeBuild, identity)
		applyPartition(tgt, "copts", fold.CompileFragments, cmakeSrc, cmakeBuild, identity)
		applyPartition(tgt, "linkopts", fold.LinkFragments, cmakeSrc, cmakeBuild, identity)
		applyPartition(tgt, "includes", fold.Includes, cmakeSrc, cmakeBuild, identity)
		applyPartition(tgt, "srcs", fold.Sources, cmakeSrc, cmakeBuild, identity)
		applyPartition(tgt, "deps", relabelDependencyPartition(fold.Dependencies, idToName), cmakeSrc, cmakeBuild, identity)
		// Same baseline hygiene as the multi-config fold: a value the
		// primary lower put in the flat attribute that turns out to be
		// option-conditional moves to its arm (the select is the
		// canonical source for varying values), and arms the inner
		// filters emptied are pruned rather than rendered as noise.
		dedupBaselineAgainstDeltas(tgt)
		pruneEmptyPerPlatform(tgt)
		if perPlatformArmCount(tgt) > before {
			lifted = append(lifted, fold.Name)
		}
	}
	sort.Strings(lifted)
	return lifted
}

// perPlatformArmCount counts the non-empty select() arm cells on a
// target — the "did the fold land anything" signal ApplyOptionFold
// uses, robust to applyPartition+prune round-trips that net to
// nothing.
func perPlatformArmCount(tgt *ir.Target) int {
	n := 0
	for _, arms := range tgt.PerPlatform {
		for _, vs := range arms {
			if len(vs) > 0 {
				n++
			}
		}
	}
	return n
}

// optionsBakedHeader is the first line of the options inventory
// block optionsHeaderComments writes (see lower.go); the annotate
// pass below relocates lifted entries out of it.
const optionsBakedHeader = "cmake options resolved at convert time (values baked in; re-convert to change):"

// optionsLiftedHeader introduces the lifted-options block
// AnnotateLiftedOptions appends: these are real build-time toggles
// now, so "re-convert to change" would be wrong for them.
const optionsLiftedHeader = "cmake options lifted to build-time flags (toggle with --//options:<name>=true|false):"

// AnnotateLiftedOptions rewrites pkg.HeaderComments after the
// option fold's outcome is known: entries of the baked-options
// inventory block whose option was successfully lifted move to a
// separate "lifted to build-time flags" block, so the baked block's
// "re-convert to change" claim stays true for exactly the options
// it still applies to. lifted maps the cmake option name to the
// bool_flag label backing it (e.g. "BUILD_TESTS" →
// "//options:build_tests"). No-op when lifted is empty or the
// baked block isn't present (e.g. a reply with no options).
func AnnotateLiftedOptions(pkg *ir.Package, lifted map[string]string) {
	if pkg == nil || len(lifted) == 0 {
		return
	}
	headerAt := -1
	for i, line := range pkg.HeaderComments {
		if line == optionsBakedHeader {
			headerAt = i
			break
		}
	}
	if headerAt == -1 {
		return
	}
	var kept, moved []string
	i := headerAt + 1
	for ; i < len(pkg.HeaderComments); i++ {
		line := pkg.HeaderComments[i]
		name, ok := bakedOptionLineName(line)
		if !ok {
			break // end of the inventory block
		}
		if label, isLifted := lifted[name]; isLifted {
			moved = append(moved, "  - "+name+" ("+label+", default from this convert)")
		} else {
			kept = append(kept, line)
		}
	}
	tail := pkg.HeaderComments[i:]
	out := append([]string{}, pkg.HeaderComments[:headerAt]...)
	if len(kept) > 0 {
		out = append(out, optionsBakedHeader)
		out = append(out, kept...)
	} else if headerAt > 0 && out[headerAt-1] == "" {
		// The baked block emptied; drop its leading blank separator too.
		out = out[:headerAt-1]
	}
	if len(moved) > 0 {
		out = append(out, "", optionsLiftedHeader)
		out = append(out, moved...)
	}
	pkg.HeaderComments = append(out, tail...)
}

// bakedOptionLineName parses one baked-options inventory line
// ("  - NAME = VALUE (doc)") back to the option name; ok=false for
// anything not of that shape (the block terminator).
func bakedOptionLineName(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "  - ")
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, " = ")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}
