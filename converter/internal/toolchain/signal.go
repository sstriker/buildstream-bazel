package toolchain

import "github.com/sstriker/buildstream-bazel/internal/sliceutil"

// FoldElementSignal additively merges the implicit include / link
// search directories observed from a real element's cmake configure
// into rt.Base.
//
// The dedicated toolchain probe (the probe/ project) compiles a
// minimal translation unit, so the implicit search roots it records
// are whatever the bare compiler reports. A real element's configure
// can drag in extra implicit roots — a sysroot leg a find_package
// added, a vendored-SDK include dir a project-side toolchain file
// injected — that the probe never sees. Folding the element signal
// recovers those facts without re-running the probe.
//
// The merge is strictly additive: a path the probe already recorded
// is left in place (and its position preserved), a path absent from
// the baseline is appended after the existing ones. Only languages
// already present in rt.Base are touched — a signal language the
// probe never exercised is skipped rather than synthesizing a
// half-populated Language entry (no compiler path / flags) the
// emitter would choke on.
//
// Returns the include / link paths actually added (sorted, deduped)
// for caller-side diagnostics; both are nil when the signal
// contributed nothing new.
func (rt *ResolvedToolchain) FoldElementSignal(signal *Model) (addedInclude, addedLink []string) {
	if rt == nil || rt.Base == nil || signal == nil {
		return nil, nil
	}
	incSet := map[string]bool{}
	linkSet := map[string]bool{}
	for lang, sl := range signal.Languages {
		bl, ok := rt.Base.Languages[lang]
		if !ok {
			continue
		}
		bl.BuiltinIncludeDirs = appendMissing(bl.BuiltinIncludeDirs, sl.BuiltinIncludeDirs, incSet)
		bl.BuiltinLinkDirs = appendMissing(bl.BuiltinLinkDirs, sl.BuiltinLinkDirs, linkSet)
		rt.Base.Languages[lang] = bl
	}
	return sortedSetKeys(incSet), sortedSetKeys(linkSet)
}

// appendMissing returns base with every element of extra not already
// present appended in order. Newly-added values are recorded in
// added so the caller can report them.
func appendMissing(base, extra []string, added map[string]bool) []string {
	have := make(map[string]bool, len(base))
	for _, p := range base {
		have[p] = true
	}
	for _, p := range extra {
		if have[p] {
			continue
		}
		have[p] = true
		base = append(base, p)
		added[p] = true
	}
	return base
}

func sortedSetKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := sliceutil.SortedKeys(m)
	return out
}
