// Package empfold partitions per-cell observations into a
// baseline (facts every cell agrees on) plus per-cell deltas
// (everything else). The algorithm is the cardinality fold the
// toolchain side has been using to compose ProbeResults into a
// ResolvedToolchain; extracted here so the per-element multi-
// platform fold can reuse the same primitive over a different
// fact shape.
//
// Two natural shapes consume Partition:
//
//   - Scalar facts (cmake cache vars): V = string. Each
//     facts[name][cell] is the value cell observed for that
//     name. Baseline keeps names whose value is identical
//     across every cell.
//
//   - Set-membership facts (per-attribute lists across
//     platforms): lift each item to facts[item][cell] = true.
//     Baseline keeps items present in every cell; deltas
//     identify items unique to specific cells.
//
// The package intentionally has no notion of cmake / Bazel /
// platforms. Callers wrap the primitive into their own
// schema-aware shape.
package empfold

// Partition splits per-cell observations into a baseline and
// per-cell deltas.
//
// cells names every cell participating in the fold. A fact is
// baseline only if every cell in this list observed it AND every
// cell observed the same value. A fact present in fewer cells
// than len(cells) — or observed with disagreeing values — goes
// to deltas: each cell that DID observe the fact records its
// own (name, value) under deltas[cell].
//
// facts is the per-cell fact table: facts[name][cell] = value.
// A nil or empty inner map is treated the same as "no cell
// observed this name" (filtered out trivially).
//
// V must be comparable so the equality check across cells works
// without a user-supplied predicate. For deeper structures (slice
// values), lift each member as a separate fact and use V = bool
// (or any unit type) — see the package doc.
//
// Returned maps are always non-nil. baseline contains one entry
// per name whose value is identical across every cell. deltas
// contains one entry per cell name from cells; the per-cell map
// holds the (name, value) pairs that landed outside baseline.
// A cell that observed no per-cell deltas still has an empty
// map under its name in deltas (so callers can iterate cells
// uniformly).
func Partition[V comparable](
	cells []string,
	facts map[string]map[string]V,
) (baseline map[string]V, deltas map[string]map[string]V) {
	baseline = map[string]V{}
	deltas = make(map[string]map[string]V, len(cells))
	for _, c := range cells {
		deltas[c] = map[string]V{}
	}

	allCellNames := make(map[string]bool, len(cells))
	for _, c := range cells {
		allCellNames[c] = true
	}

	for name, byCell := range facts {
		// Count only observations from declared cells. A fact
		// observed by a "ghost" cell not in the declared set is
		// dropped silently; baseline / delta cardinality is
		// computed against the declared cells only.
		declaredObservers := 0
		for cellName := range byCell {
			if allCellNames[cellName] {
				declaredObservers++
			}
		}
		// "Common" requires every declared cell observed the
		// fact. Anything missing from at least one cell is a
		// per-cell signal: each cell that DID observe the fact
		// records its own value as a delta.
		if declaredObservers != len(allCellNames) {
			for cellName, val := range byCell {
				if !allCellNames[cellName] {
					continue
				}
				deltas[cellName][name] = val
			}
			continue
		}
		// Every named cell observed the fact. Compare values:
		// identical → baseline; otherwise → per-cell deltas.
		var common V
		identical := true
		first := true
		for cellName, val := range byCell {
			if !allCellNames[cellName] {
				continue
			}
			if first {
				common = val
				first = false
				continue
			}
			if val != common {
				identical = false
				break
			}
		}
		if identical {
			baseline[name] = common
			continue
		}
		for cellName, val := range byCell {
			if !allCellNames[cellName] {
				continue
			}
			deltas[cellName][name] = val
		}
	}
	return baseline, deltas
}
