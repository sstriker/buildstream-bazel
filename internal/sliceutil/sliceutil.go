// Package sliceutil holds small generic slice/map helpers shared across the
// converter and internal trees. It is a leaf package (depends only on the
// stdlib), so both converter/* and internal/* can import it without cycles.
//
// These consolidate idioms the codebase previously hand-rolled in ~a dozen
// places each: "sorted keys of a map", order-preserving dedup, and sorted dedup.
package sliceutil

import (
	"cmp"
	"maps"
	"slices"
)

// SortedKeys returns the keys of m sorted in ascending order. Replaces the
// make([]K,0,len(m)) + range-append + sort.Strings idiom.
func SortedKeys[M ~map[K]V, K cmp.Ordered, V any](m M) []K {
	return slices.Sorted(maps.Keys(m))
}

// Unique returns the elements of in with duplicates removed, preserving
// first-occurrence order. Returns nil for empty/nil input. The input is not
// mutated.
func Unique[E comparable](in []E) []E {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[E]struct{}, len(in))
	out := make([]E, 0, len(in))
	for _, e := range in {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// SortedUnique returns the elements of in sorted ascending with duplicates
// removed. Returns nil for empty/nil input. The input is not mutated.
func SortedUnique[E cmp.Ordered](in []E) []E {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
