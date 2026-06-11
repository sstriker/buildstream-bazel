package commonflags

import "github.com/sstriker/buildstream-bazel/converter/ir"

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
func HoistCommonCopts(pkg *ir.Package, featureName string) []string {
	if pkg == nil {
		return nil
	}
	var cands []*ir.Target
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCBinary, ir.KindCCTest, ir.KindCCInterface:
		default:
			continue
		}
		if len(t.Copts) == 0 {
			continue
		}
		cands = append(cands, t)
	}
	if len(cands) < 2 {
		return nil
	}
	prefix := longestCommonCoptPrefix(cands)
	if len(prefix) == 0 {
		return nil
	}
	for _, t := range cands {
		t.Copts = append([]string(nil), t.Copts[len(prefix):]...)
		if len(t.Copts) == 0 {
			t.Copts = nil
		}
		if !contains(t.Features, featureName) {
			t.Features = append(t.Features, featureName)
		}
	}
	return prefix
}

// longestCommonCoptPrefix returns the longest leading run of copts that every
// candidate shares (by exact string equality, position by position).
func longestCommonCoptPrefix(cands []*ir.Target) []string {
	first := cands[0].Copts
	n := len(first)
	for _, t := range cands[1:] {
		if len(t.Copts) < n {
			n = len(t.Copts)
		}
	}
	i := 0
	for i < n {
		flag := first[i]
		match := true
		for _, t := range cands[1:] {
			if t.Copts[i] != flag {
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
