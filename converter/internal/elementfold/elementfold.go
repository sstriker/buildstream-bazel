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
// All cells must agree on the Package's Name, and every cell
// must declare the same Targets keyed by (Name, Kind). The
// merged Package's SourceRoot is taken from cells[0] without
// cross-cell validation: per-platform conversions can run on
// distinct workers whose worker-local SourceRoot paths legitimately
// differ, and the rendered BUILD.bazel doesn't reference
// SourceRoot directly (FUSE source-key rewriting in emit/bazel
// uses an out-of-band Options.SourceKey, not the IR field).
// Targets present in some cells but missing from others are
// rejected with an error: lifting a whole target into select()
// at the package level is materially more complex than
// per-attribute deltas (Bazel's select() applies to attribute
// values, not to target existence) and is deferred per the plan.
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
	seenName := map[string]int{}
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
		if prev, dup := seenName[c.Platform.Name]; dup {
			return nil, fmt.Errorf("elementfold: cells[%d] reuses Platform.Name %q first declared by cells[%d]; each cell needs a unique platform name because the fold keys per-platform maps by it",
				i, c.Platform.Name, prev)
		}
		seenName[c.Platform.Name] = i
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
		firstName, vName := cells[0].Platform.Name, c.Platform.Name
		if v.InstallDest != first.InstallDest {
			return nil, fmt.Errorf("InstallDest disagrees: cell %q has %q, cell %q has %q", firstName, first.InstallDest, vName, v.InstallDest)
		}
		if v.ArtifactName != first.ArtifactName {
			return nil, fmt.Errorf("ArtifactName disagrees: cell %q has %q, cell %q has %q", firstName, first.ArtifactName, vName, v.ArtifactName)
		}
		if v.LinkLanguage != first.LinkLanguage {
			return nil, fmt.Errorf("LinkLanguage disagrees: cell %q has %q, cell %q has %q", firstName, first.LinkLanguage, vName, v.LinkLanguage)
		}
		if v.StaticLibrary != first.StaticLibrary {
			return nil, fmt.Errorf("StaticLibrary disagrees: cell %q has %q, cell %q has %q", firstName, first.StaticLibrary, vName, v.StaticLibrary)
		}
		if v.SharedLibrary != first.SharedLibrary {
			return nil, fmt.Errorf("SharedLibrary disagrees: cell %q has %q, cell %q has %q", firstName, first.SharedLibrary, vName, v.SharedLibrary)
		}
		if v.GenruleCmd != first.GenruleCmd {
			return nil, fmt.Errorf("GenruleCmd disagrees: cell %q has %q, cell %q has %q", firstName, first.GenruleCmd, vName, v.GenruleCmd)
		}
		// Genrule outs / tools, test args / data / env / timeout
		// must also match exactly. Differences here would mean
		// fundamentally different rule shapes per platform, which
		// select() can't express at the attribute level.
		if !sliceEqual(v.GenruleOuts, first.GenruleOuts) {
			return nil, fmt.Errorf("GenruleOuts disagrees: cell %q has %v, cell %q has %v", firstName, first.GenruleOuts, vName, v.GenruleOuts)
		}
		if !sliceEqual(v.GenruleTools, first.GenruleTools) {
			return nil, fmt.Errorf("GenruleTools disagrees: cell %q has %v, cell %q has %v", firstName, first.GenruleTools, vName, v.GenruleTools)
		}
		if !sliceEqual(v.TestArgs, first.TestArgs) {
			return nil, fmt.Errorf("TestArgs disagrees: cell %q has %v, cell %q has %v", firstName, first.TestArgs, vName, v.TestArgs)
		}
		if !sliceEqual(v.TestData, first.TestData) {
			return nil, fmt.Errorf("TestData disagrees: cell %q has %v, cell %q has %v", firstName, first.TestData, vName, v.TestData)
		}
		if !sliceEqual(v.TestEnv, first.TestEnv) {
			return nil, fmt.Errorf("TestEnv disagrees: cell %q has %v, cell %q has %v", firstName, first.TestEnv, vName, v.TestEnv)
		}
		if v.TestTimeout != first.TestTimeout {
			return nil, fmt.Errorf("TestTimeout disagrees: cell %q has %q, cell %q has %q", firstName, first.TestTimeout, vName, v.TestTimeout)
		}
	}

	merged := first
	merged.PerPlatform = nil
	// Visibility and Tags fold the union across cells; they're
	// metadata that should be at least as permissive as any
	// single cell's view.
	merged.Visibility = sortedUnion(extractStringSlice(variants, cells, func(t ir.Target) []string { return t.Visibility }))
	merged.Tags = sortedUnion(extractStringSlice(variants, cells, func(t ir.Target) []string { return t.Tags }))

	// Per-attribute fold. Order-insensitive attrs (srcs, hdrs,
	// includes, defines, deps) decompose into a set-membership
	// partition: items every cell carries land in the flat
	// baseline, items unique to a subset of cells land in
	// PerPlatform[attr][SelectKey], all sorted lexicographically.
	// Order-sensitive attrs (copts, linkopts) refuse the
	// per-item partition because re-sequencing flags can flip
	// compiler/linker semantics (last-flag-wins, include
	// precedence, etc.). They fold conservatively: when every
	// cell has byte-identical sequences, the baseline takes that
	// shared sequence with no deltas; otherwise the baseline is
	// empty and each cell's FULL sequence routes through its
	// SelectKey arm verbatim — emit then renders the attribute as
	// a bare select() so each platform sees its own original
	// sequence at build time.
	cellNames := make([]string, len(cells))
	for i, c := range cells {
		cellNames[i] = c.Platform.Name
	}

	for _, def := range targetAttrs {
		if def.orderSensitive {
			foldOrderSensitiveAttr(def, &merged, variants, cells)
			continue
		}
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

// foldOrderSensitiveAttr handles copts / linkopts. When every
// cell carries the same sequence, the merged target gets that
// sequence as a flat baseline (and no deltas — emit produces
// today's pre-PerPlatform shape). When sequences diverge in any
// way (extra items, reorder), each cell's full sequence is
// stored verbatim under PerPlatform[attr][SelectKey] with the
// flat baseline cleared, so emit renders a bare select() that
// preserves per-platform ordering exactly.
func foldOrderSensitiveAttr(def attrDef, merged *ir.Target, variants map[string]ir.Target, cells []Cell) {
	first := def.get(variants[cells[0].Platform.Name])
	allEqual := true
	for _, c := range cells[1:] {
		if !sliceEqual(first, def.get(variants[c.Platform.Name])) {
			allEqual = false
			break
		}
	}
	if allEqual {
		def.set(merged, append([]string(nil), first...))
		return
	}
	def.set(merged, nil)
	if merged.PerPlatform == nil {
		merged.PerPlatform = map[string]map[string][]string{}
	}
	if merged.PerPlatform[def.name] == nil {
		merged.PerPlatform[def.name] = map[string][]string{}
	}
	for _, c := range cells {
		seq := def.get(variants[c.Platform.Name])
		merged.PerPlatform[def.name][c.Platform.SelectKey] = append([]string(nil), seq...)
	}
}

// targetAttrs declares the IR attributes the fold partitions
// per platform. Each entry names the Bazel attribute (matches
// the keys emit/bazel looks up in Target.PerPlatform[name]) and
// flags whether its sequence is order-sensitive. copts and
// linkopts are passed unchanged to the compiler / linker so
// flag order is semantic; the rest (srcs, hdrs, includes,
// defines, deps) are unordered and sort lexicographically for
// stable diffs (matching emit/bazel's sortedCopy convention on
// the baseline side).
type attrDef struct {
	name           string
	orderSensitive bool
	get            func(ir.Target) []string
	set            func(*ir.Target, []string)
}

var targetAttrs = []attrDef{
	{"srcs", false, func(t ir.Target) []string { return t.Srcs }, func(t *ir.Target, v []string) { t.Srcs = v }},
	{"hdrs", false, func(t ir.Target) []string { return t.Hdrs }, func(t *ir.Target, v []string) { t.Hdrs = v }},
	{"includes", false, func(t ir.Target) []string { return t.Includes }, func(t *ir.Target, v []string) { t.Includes = v }},
	{"copts", true, func(t ir.Target) []string { return t.Copts }, func(t *ir.Target, v []string) { t.Copts = v }},
	{"defines", false, func(t ir.Target) []string { return t.Defines }, func(t *ir.Target, v []string) { t.Defines = v }},
	{"linkopts", true, func(t ir.Target) []string { return t.LinkOpts }, func(t *ir.Target, v []string) { t.LinkOpts = v }},
	{"deps", false, func(t ir.Target) []string { return t.Deps }, func(t *ir.Target, v []string) { t.Deps = v }},
}

// PickSelectKeys derives the select-arm label for each
// platform in the matrix, honouring operator overrides where
// present and auto-detecting from constraints otherwise.
//
// Override path: any platform whose SelectKey is already
// populated by the caller is passed through verbatim. This is
// the escalation path for matrices where no single constraint
// axis uniquely identifies each platform (the classic
// {linux_x86_64, linux_aarch64, darwin_arm64} shape): the
// operator declares a config_setting per platform in their
// //platforms package and supplies its label as SelectKey.
//
// Auto-detection (for platforms with empty SelectKey): count
// each constraint label's occurrences across THE FULL MATRIX
// (override platforms included), after de-duplicating each
// platform's own constraint list. A label that appears
// exactly once uniquely identifies its platform — assign it
// as that platform's SelectKey. When a platform has multiple
// unique constraint labels, the lexicographically-smallest
// one is chosen for determinism.
//
// Counting across the FULL matrix matters: at Bazel analysis
// time the rendered select() sees every arm, so an
// auto-detected key that collides with an override platform's
// constraint set (e.g. picking @platforms//cpu:arm64 for
// darwin_arm64 when linux_aarch64 also carries that
// constraint) would produce "multiple conditions match"
// failures. Restricting the count to the auto-detect subset
// would mask that.
//
// Post-pick validation: collect every final SelectKey and
// reject any duplicate. Catches operator typos
// (two platforms share an override label) and
// override-vs-auto collisions (an operator-supplied label
// that happens to equal an auto-detect platform's
// constraint).
//
// Returns an error naming the offending platform if a
// platform without an operator-supplied SelectKey has no
// constraint label that uniquely identifies it across the
// full matrix. The actionable response is to declare a
// config_setting per platform in //platforms and rerun with
// select_label set.
func PickSelectKeys(platforms []Platform) (map[string]string, error) {
	dedupedConstraints := make([][]string, len(platforms))
	for i, p := range platforms {
		seen := map[string]bool{}
		uniq := make([]string, 0, len(p.Constraints))
		for _, c := range p.Constraints {
			if seen[c] {
				continue
			}
			seen[c] = true
			uniq = append(uniq, c)
		}
		dedupedConstraints[i] = uniq
	}
	// Count constraint labels across the full matrix —
	// override-platform constraints included — so the
	// uniqueness check matches the analysis-time view of the
	// rendered select() block (every arm contributes; Bazel
	// errors on multiple-condition matches).
	counts := map[string]int{}
	for _, cs := range dedupedConstraints {
		for _, c := range cs {
			counts[c]++
		}
	}
	out := make(map[string]string, len(platforms))
	for i, p := range platforms {
		if p.SelectKey != "" {
			if dup, exists := out[p.Name]; exists {
				return nil, fmt.Errorf("elementfold: platform %q appears twice in PickSelectKeys input (existing key %q)", p.Name, dup)
			}
			out[p.Name] = p.SelectKey
			continue
		}
		var unique string
		for _, c := range dedupedConstraints[i] {
			if counts[c] != 1 {
				continue
			}
			if unique == "" || c < unique {
				unique = c
			}
		}
		if unique == "" {
			return nil, fmt.Errorf("elementfold: platform %q has no constraint that uniquely identifies it within the matrix; declare a config_setting in your //platforms package and pass its label via select_label", p.Name)
		}
		out[p.Name] = unique
	}
	// Final-key uniqueness check: catches two operators
	// supplying the same override label and the rare
	// override-vs-auto collision (an operator-supplied label
	// that happens to equal an auto-detect platform's
	// uniquely-counted constraint). Either case would land
	// duplicate select() arms whose deltas would silently
	// overwrite each other in PerPlatform.
	keyToName := make(map[string]string, len(out))
	for _, p := range platforms {
		k := out[p.Name]
		if prev, dup := keyToName[k]; dup {
			return nil, fmt.Errorf("elementfold: platforms %q and %q both resolve to SelectKey %q; supply distinct select_label values per platform in your //platforms package", prev, p.Name, k)
		}
		keyToName[k] = p.Name
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
