// Package elementfold composes N per-platform IR Packages into
// one merged Package whose Targets carry per-platform attribute
// deltas in Target.PerPlatform. The fold is the per-element
// equivalent of toolchain.Observe: cells contribute facts (per-
// attribute item lists), and the empfold cardinality primitive
// partitions them into a flat baseline (items present in every
// cell) plus per-cell deltas (items unique to specific cells).
//
// With a single cell, the fold is identity: the one cell's
// Package round-trips with PerPlatform left empty, so emit
// produces the existing single-platform BUILD.bazel byte-
// identical to today's. With more cells, divergent items land
// under PerPlatform[attr][selectKey]; emit renders them as
// `<flat> + select({selectKey: delta, ..., "//conditions:default":
// []})`.
package elementfold

import (
	"fmt"
	"sort"

	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
	"github.com/sstriker/cmake-to-bazel/internal/empfold"
)

// Platform names the matrix entry the corresponding cell came
// from. Constraints are the Bazel constraint_value labels (e.g.
// "@platforms//os:linux") declared on the platform() rule the
// operator's //platforms package emits — same shape the
// platforms-json manifest the rest of the project consumes
// uses.
//
// SelectKey is the constraint label the fold attaches this
// platform's deltas under in the rendered select() block. It
// should be a label that UNIQUELY identifies this platform
// within the matrix — see PickSelectKeys for the auto-detection
// callers should typically use to derive it.
type Platform struct {
	Name        string
	Constraints []string
	SelectKey   string
}

// Cell is one (platform, IR) pair: the IR Package that one
// platform's convert-element call produced, paired with the
// platform identity the fold attaches its deltas under.
type Cell struct {
	Platform Platform
	Pkg      *ir.Package
}

// Fold composes N cells into one merged Package with per-
// attribute select() deltas baked into Target.PerPlatform.
//
// All cells must agree on the Package's Name + SourceRoot, and
// every cell must declare the same Targets keyed by (Name,
// Kind). Targets present in some cells but missing from others
// are rejected with an error: lifting a whole target into
// select() at the package level is materially more complex
// than per-attribute deltas (Bazel's select() applies to
// attribute values, not to target existence) and is deferred
// per the plan.
//
// Boolean attributes (Linkstatic, Alwayslink) and identity-like
// fields (InstallDest, ArtifactName, LinkLanguage, the Genrule*
// fields, the Test* fields, StaticLibrary, SharedLibrary) must
// match exactly across cells. A disagreement signals a
// fundamentally divergent target shape that select() can't
// express; the fold errors out so the operator either declares
// the difference explicitly or reduces the matrix.
func Fold(cells []Cell) (*ir.Package, error) {
	if len(cells) == 0 {
		return nil, fmt.Errorf("elementfold: no cells")
	}
	for i, c := range cells {
		if c.Pkg == nil {
			return nil, fmt.Errorf("elementfold: cells[%d] (%q) has nil Pkg", i, c.Platform.Name)
		}
		if c.Platform.Name == "" {
			return nil, fmt.Errorf("elementfold: cells[%d] has empty Platform.Name", i)
		}
		if c.Platform.SelectKey == "" {
			return nil, fmt.Errorf("elementfold: cells[%d] (%q) has empty Platform.SelectKey; call PickSelectKeys to derive it", i, c.Platform.Name)
		}
	}

	// Validate package-level consistency across cells.
	primary := cells[0].Pkg
	for i, c := range cells[1:] {
		if c.Pkg.Name != primary.Name {
			return nil, fmt.Errorf("elementfold: cells[0] and cells[%d] disagree on Package.Name (%q vs %q)", i+1, primary.Name, c.Pkg.Name)
		}
	}

	// Build the per-target map keyed by (Name, Kind). All cells
	// must agree on the key set; any cell missing a target the
	// others have is an error.
	type targetKey struct {
		name string
		kind ir.Kind
	}
	targetsByCell := make(map[string]map[targetKey]ir.Target, len(cells))
	for _, c := range cells {
		m := make(map[targetKey]ir.Target, len(c.Pkg.Targets))
		for _, t := range c.Pkg.Targets {
			k := targetKey{t.Name, t.Kind}
			if _, dup := m[k]; dup {
				return nil, fmt.Errorf("elementfold: cell %q has duplicate target (%s, %s)", c.Platform.Name, t.Name, t.Kind)
			}
			m[k] = t
		}
		targetsByCell[c.Platform.Name] = m
	}
	// keyOrder follows cells[0]'s declared order. Every other
	// cell must declare exactly the same target set (Name, Kind);
	// missing or extra targets in another cell are rejected
	// because select() can't conditionally instantiate a target
	// at the package level.
	keyOrder := make([]targetKey, 0, len(cells[0].Pkg.Targets))
	for _, t := range cells[0].Pkg.Targets {
		keyOrder = append(keyOrder, targetKey{t.Name, t.Kind})
	}
	for _, c := range cells[1:] {
		if len(targetsByCell[c.Platform.Name]) != len(targetsByCell[cells[0].Platform.Name]) {
			return nil, fmt.Errorf("elementfold: cell %q has %d targets but cell %q has %d; whole-target select() is not supported — declare every target in every platform's IR or reduce the matrix",
				c.Platform.Name, len(targetsByCell[c.Platform.Name]),
				cells[0].Platform.Name, len(targetsByCell[cells[0].Platform.Name]))
		}
		for _, k := range keyOrder {
			if _, ok := targetsByCell[c.Platform.Name][k]; !ok {
				return nil, fmt.Errorf("elementfold: target (%s, %s) missing from cell %q (present in cell %q); whole-target select() is not supported — declare the target unconditionally or reduce the matrix",
					k.name, k.kind, c.Platform.Name, cells[0].Platform.Name)
			}
		}
	}

	out := &ir.Package{
		Name:       primary.Name,
		SourceRoot: primary.SourceRoot,
		Targets:    make([]ir.Target, 0, len(keyOrder)),
	}
	for _, k := range keyOrder {
		// Pull this target's variants from each cell.
		variants := make(map[string]ir.Target, len(cells))
		for _, c := range cells {
			variants[c.Platform.Name] = targetsByCell[c.Platform.Name][k]
		}
		merged, err := foldTarget(variants, cells)
		if err != nil {
			return nil, fmt.Errorf("elementfold: target %s: %w", k.name, err)
		}
		out.Targets = append(out.Targets, *merged)
	}
	return out, nil
}

// foldTarget applies the per-attribute fold to one target's
// per-cell variants. The first cell's target is the seed for
// non-list scalar fields (booleans, identity strings); the
// other cells must agree on those fields.
func foldTarget(variants map[string]ir.Target, cells []Cell) (*ir.Target, error) {
	first := variants[cells[0].Platform.Name]
	// Scalar / boolean attrs must agree across cells.
	for _, c := range cells[1:] {
		v := variants[c.Platform.Name]
		if v.Linkstatic != first.Linkstatic {
			return nil, fmt.Errorf("Linkstatic disagrees: cell %q has %v, cell %q has %v",
				cells[0].Platform.Name, first.Linkstatic, c.Platform.Name, v.Linkstatic)
		}
		if v.Alwayslink != first.Alwayslink {
			return nil, fmt.Errorf("Alwayslink disagrees: cell %q has %v, cell %q has %v",
				cells[0].Platform.Name, first.Alwayslink, c.Platform.Name, v.Alwayslink)
		}
		if v.InstallDest != first.InstallDest {
			return nil, fmt.Errorf("InstallDest disagrees: %q vs %q", first.InstallDest, v.InstallDest)
		}
		if v.ArtifactName != first.ArtifactName {
			return nil, fmt.Errorf("ArtifactName disagrees: %q vs %q", first.ArtifactName, v.ArtifactName)
		}
		if v.LinkLanguage != first.LinkLanguage {
			return nil, fmt.Errorf("LinkLanguage disagrees: %q vs %q", first.LinkLanguage, v.LinkLanguage)
		}
		if v.StaticLibrary != first.StaticLibrary {
			return nil, fmt.Errorf("StaticLibrary disagrees: %q vs %q", first.StaticLibrary, v.StaticLibrary)
		}
		if v.SharedLibrary != first.SharedLibrary {
			return nil, fmt.Errorf("SharedLibrary disagrees: %q vs %q", first.SharedLibrary, v.SharedLibrary)
		}
		if v.GenruleCmd != first.GenruleCmd {
			return nil, fmt.Errorf("GenruleCmd disagrees")
		}
		// Genrule outs / tools, test args / data / env / timeout
		// must also match exactly. Differences here would mean
		// fundamentally different rule shapes per platform, which
		// select() can't express at the attribute level.
		if !sliceEqual(v.GenruleOuts, first.GenruleOuts) {
			return nil, fmt.Errorf("GenruleOuts disagrees")
		}
		if !sliceEqual(v.GenruleTools, first.GenruleTools) {
			return nil, fmt.Errorf("GenruleTools disagrees")
		}
		if !sliceEqual(v.TestArgs, first.TestArgs) {
			return nil, fmt.Errorf("TestArgs disagrees")
		}
		if !sliceEqual(v.TestData, first.TestData) {
			return nil, fmt.Errorf("TestData disagrees")
		}
		if !sliceEqual(v.TestEnv, first.TestEnv) {
			return nil, fmt.Errorf("TestEnv disagrees")
		}
		if v.TestTimeout != first.TestTimeout {
			return nil, fmt.Errorf("TestTimeout disagrees")
		}
	}

	merged := first
	merged.PerPlatform = nil
	// Visibility and Tags fold the union across cells; they're
	// metadata that should be at least as permissive as any
	// single cell's view.
	merged.Visibility = sortedUnion(extractStringSlice(variants, cells, func(t ir.Target) []string { return t.Visibility }))
	merged.Tags = sortedUnion(extractStringSlice(variants, cells, func(t ir.Target) []string { return t.Tags }))

	// Per-attribute item-set partition. For each named attr, lift
	// each item to facts[item][cellName]=true; empfold.Partition
	// returns baseline (items every cell observed) and deltas
	// (items unique per cell). Baseline lands in the flat
	// attribute slice; deltas land in PerPlatform under the
	// cell's SelectKey.
	cellNames := make([]string, len(cells))
	for i, c := range cells {
		cellNames[i] = c.Platform.Name
	}

	for _, def := range targetAttrs {
		facts := map[string]map[string]bool{}
		for _, c := range cells {
			items := def.get(variants[c.Platform.Name])
			for _, item := range items {
				if facts[item] == nil {
					facts[item] = map[string]bool{}
				}
				facts[item][c.Platform.Name] = true
			}
		}
		baseline, deltas := empfold.Partition(cellNames, facts)
		flat := make([]string, 0, len(baseline))
		for k := range baseline {
			flat = append(flat, k)
		}
		sort.Strings(flat)
		def.set(&merged, flat)

		// Per-cell deltas → PerPlatform[attr][selectKey] = delta items.
		for _, c := range cells {
			d := deltas[c.Platform.Name]
			if len(d) == 0 {
				continue
			}
			items := make([]string, 0, len(d))
			for k := range d {
				items = append(items, k)
			}
			sort.Strings(items)
			if merged.PerPlatform == nil {
				merged.PerPlatform = map[string]map[string][]string{}
			}
			if merged.PerPlatform[def.name] == nil {
				merged.PerPlatform[def.name] = map[string][]string{}
			}
			merged.PerPlatform[def.name][c.Platform.SelectKey] = items
		}
	}
	return &merged, nil
}

// targetAttrs declares the IR attributes the fold partitions
// per platform. Order is irrelevant (each is folded
// independently); the spelling of `name` matches the Bazel
// attribute name emit/bazel uses to look up
// Target.PerPlatform[name].
var targetAttrs = []struct {
	name string
	get  func(ir.Target) []string
	set  func(*ir.Target, []string)
}{
	{"srcs", func(t ir.Target) []string { return t.Srcs }, func(t *ir.Target, v []string) { t.Srcs = v }},
	{"hdrs", func(t ir.Target) []string { return t.Hdrs }, func(t *ir.Target, v []string) { t.Hdrs = v }},
	{"includes", func(t ir.Target) []string { return t.Includes }, func(t *ir.Target, v []string) { t.Includes = v }},
	{"copts", func(t ir.Target) []string { return t.Copts }, func(t *ir.Target, v []string) { t.Copts = v }},
	{"defines", func(t ir.Target) []string { return t.Defines }, func(t *ir.Target, v []string) { t.Defines = v }},
	{"linkopts", func(t ir.Target) []string { return t.LinkOpts }, func(t *ir.Target, v []string) { t.LinkOpts = v }},
	{"deps", func(t ir.Target) []string { return t.Deps }, func(t *ir.Target, v []string) { t.Deps = v }},
}

// PickSelectKeys auto-detects the constraint label that
// uniquely identifies each platform within the matrix.
//
// Algorithm: count each constraint label's occurrences across
// the matrix. A label that appears exactly once uniquely
// identifies its platform — assign it as that platform's
// SelectKey. When a platform has multiple unique constraint
// labels, the lexicographically-smallest one is chosen for
// determinism (e.g. between "@platforms//cpu:x86_64" and
// "@platforms//os:linux", `@platforms//cpu:...` sorts first).
//
// Returns an error naming the offending platform if any
// platform has no constraint label that uniquely identifies it
// in the matrix. The operator's escalation path then is to
// declare a config_setting per platform in their //platforms
// package and pass those labels in explicitly (a follow-up to
// this MVP — for now, this error tells them to reduce the
// matrix or split the conversion).
func PickSelectKeys(platforms []Platform) (map[string]string, error) {
	counts := map[string]int{}
	for _, p := range platforms {
		for _, c := range p.Constraints {
			counts[c]++
		}
	}
	out := make(map[string]string, len(platforms))
	for _, p := range platforms {
		var unique string
		for _, c := range p.Constraints {
			if counts[c] != 1 {
				continue
			}
			if unique == "" || c < unique {
				unique = c
			}
		}
		if unique == "" {
			return nil, fmt.Errorf("elementfold: platform %q has no constraint that uniquely identifies it within the matrix; declare a config_setting in your //platforms package", p.Name)
		}
		out[p.Name] = unique
	}
	return out, nil
}

func extractStringSlice(variants map[string]ir.Target, cells []Cell, get func(ir.Target) []string) [][]string {
	out := make([][]string, len(cells))
	for i, c := range cells {
		out[i] = get(variants[c.Platform.Name])
	}
	return out
}

func sortedUnion(slices [][]string) []string {
	seen := map[string]bool{}
	for _, s := range slices {
		for _, v := range s {
			seen[v] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
