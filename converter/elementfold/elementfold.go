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
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/empfold"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
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
// platform's convert-element-cmake call produced, paired with the
// platform identity the fold attaches its deltas under.
type Cell struct {
	Platform Platform
	Pkg      *ir.Package
}

// Fold composes N cells into one merged Package with per-
// attribute select() deltas baked into Target.PerPlatform.
//
// All cells must agree on the Package's Name. The merged
// Package's SourceRoot is taken from cells[0] without
// cross-cell validation: per-platform conversions can run on
// distinct workers whose worker-local SourceRoot paths legitimately
// differ, and the rendered BUILD.bazel doesn't reference
// SourceRoot directly (FUSE source-key rewriting in emit/bazel
// uses an out-of-band Options.SourceKey, not the IR field).
//
// Target enumeration is the union of all cells' (Name, Kind)
// keys, preserving cells[0]'s order first then any not-yet-seen
// keys from cells[1..] in declaration order. When every cell
// declares the same target set (the common case), this matches
// cells[0]'s order exactly — single-platform goldens stay byte-
// stable. When cells diverge, the fold takes the "phantom-target
// select" shape: the absent platforms simply don't contribute to
// the target's per-attr delta arms; emit/bazel renders the target
// unconditionally with select() arms keyed only on the platforms
// that have it. Bazel consumers depending on the target on an
// absent platform see the rule's attrs resolve to the default
// arm — `[]` for list attrs (the select's `//conditions:default`)
// or `None` for scalar attrs (Bazel treats that as "attribute
// unset" per cc_import's optional-path-attr semantics). Empty
// inputs or unset path attrs fail at the dep site with a legible
// diagnostic — the right outcome for a target that genuinely
// doesn't exist on that platform.
//
// Boolean attributes (Linkstatic, Alwayslink) and identity-like
// fields (InstallDest, ArtifactName, LinkLanguage, the Genrule*
// fields, the Test* fields) must match exactly across the cells
// that DECLARE the target. A disagreement among present cells
// signals a fundamentally divergent target shape that select()
// can't express; the fold errors out. Cells where the target
// is absent contribute nothing to the comparison.
//
// StaticLibrary and SharedLibrary (the cc_import path attrs) fold
// per platform: when every present cell agrees they land in the
// flat scalar field, and when they diverge the baseline clears and
// each present cell's path moves into PerPlatformScalar so emit
// renders a select(). This is the round-2 fallback's main
// divergence axis — `lib/x86_64-linux-gnu/libfoo.a` on linux vs
// `lib/libfoo.dylib` on darwin lives at distinct paths inside
// the install root.
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

	// Union the cells' select-arm family maps: per-platform converts
	// run with --lift-options carry //options:* (and //config:*) arms
	// whose labels must keep their family registration through the
	// fold, or the merged emit would collapse every arm into one
	// select() — exactly the "Illegal ambiguous match" shape the
	// per-family split exists to prevent. A label registered under
	// DIFFERENT families by two cells is incoherent (same //options
	// label backed by different flags) and errors out.
	families := map[string]string{}
	for _, c := range cells {
		for label, fam := range c.Pkg.SelectArmFamilies {
			if prev, ok := families[label]; ok && prev != fam {
				return nil, fmt.Errorf("elementfold: cells disagree on select-arm family for %q (%q vs %q)", label, prev, fam)
			}
			families[label] = fam
		}
	}
	sink := &groupSink{families: families, defs: map[string]ir.Target{}}

	// Build the per-target map keyed by (Name, Kind). Each cell
	// can declare its own target set; absent cells contribute
	// nothing to a given target's fold ("phantom-target select"
	// shape).
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
	// keyOrder is the union of all cells' target keys, taking
	// cells[0]'s declared order first then any not-yet-seen
	// keys from cells[1..]. When every cell happens to declare
	// the same set (the common case), this matches cells[0]'s
	// order exactly — preserving single-platform goldens byte-
	// for-byte. When cells diverge, the union shape lets the
	// fold emit a target that "phantoms" through the absent
	// cells: present-cell deltas land in the rendered select()
	// arms, absent cells contribute nothing.
	//
	// Reject Name reuse across distinct Kinds: the merged target
	// list is a Bazel package, where rule names must be unique
	// regardless of kind. Two cells declaring `foo` as cc_library
	// and cc_binary respectively would each fold to a separate
	// merged target with the same Bazel `name`, producing an
	// invalid BUILD file. Same goes for a single cell that
	// declares two targets with the same Name and different Kind
	// — the per-cell duplicate check above keyed by (Name, Kind)
	// would miss that, so we catch both shapes here in one pass.
	keyOrder := make([]targetKey, 0)
	seenKey := map[targetKey]bool{}
	nameToKind := map[string]ir.Kind{}
	nameToCell := map[string]string{}
	for _, c := range cells {
		for _, t := range c.Pkg.Targets {
			if prevKind, dup := nameToKind[t.Name]; dup && prevKind != t.Kind {
				return nil, fmt.Errorf("elementfold: target Name %q appears with kind %s in cell %q AND kind %s in cell %q; Bazel rule names must be unique per package regardless of kind, so the merged target list can't carry both",
					t.Name, prevKind, nameToCell[t.Name], t.Kind, c.Platform.Name)
			}
			nameToKind[t.Name] = t.Kind
			nameToCell[t.Name] = c.Platform.Name
			k := targetKey{t.Name, t.Kind}
			if seenKey[k] {
				continue
			}
			seenKey[k] = true
			keyOrder = append(keyOrder, k)
		}
	}

	// allCellNames is the full matrix's platform-name list,
	// passed verbatim to empfold.Partition so phantom targets
	// (those declared by only a subset of cells) get their
	// per-attr deltas keyed by present-cell SelectKeys with NO
	// flat baseline — Partition's "baseline = fact in every
	// declared cell" rule naturally collapses to empty when
	// absent cells contribute no facts.
	allCellNames := make([]string, len(cells))
	for i, c := range cells {
		allCellNames[i] = c.Platform.Name
	}

	out := &ir.Package{
		Name:       primary.Name,
		SourceRoot: primary.SourceRoot,
		Targets:    make([]ir.Target, 0, len(keyOrder)),
	}
	if len(families) > 0 {
		out.SelectArmFamilies = families
	}
	for _, k := range keyOrder {
		// Per-target presence subset: only the cells that
		// declared this target participate in scalar agreement
		// and contribute facts to the list partition. Absent
		// cells contribute nothing (no arm, no flat-baseline
		// pressure), so the merged target's attrs route through
		// per-present-platform select() arms while the rendered
		// shape on absent platforms resolves to the rule's
		// default (a list attr falls through "//conditions:default":
		// [], a scalar attr falls through "//conditions:default":
		// None — Bazel treats that as "attribute unset" per the
		// optional-path-attr semantics on cc_import).
		presentCells := make([]Cell, 0, len(cells))
		variants := make(map[string]ir.Target, len(cells))
		for _, c := range cells {
			t, ok := targetsByCell[c.Platform.Name][k]
			if !ok {
				continue
			}
			presentCells = append(presentCells, c)
			variants[c.Platform.Name] = t
		}
		merged, err := foldTarget(variants, presentCells, allCellNames, sink)
		if err != nil {
			return nil, fmt.Errorf("elementfold: target %s: %w", k.name, err)
		}
		out.Targets = append(out.Targets, *merged)
	}
	// The AND-groups the platform-conditional pre-existing arms
	// minted render as in-package selects.config_setting_group
	// targets, after the folded rules.
	out.Targets = append(out.Targets, sink.sorted()...)
	return out, nil
}

// foldTarget applies the per-attribute fold to one target's
// per-cell variants. `cells` is the subset of the full matrix
// that DECLARED this target (the "present cells"); scalar /
// boolean agreement runs across this subset only. `allCellNames`
// names every cell in the full matrix, including those absent
// for this target — used to seed empfold.Partition so phantom
// targets (those declared by only some cells) produce
// present-platform deltas with NO flat baseline.
//
// The first present cell is the seed for non-list scalar
// agreement (booleans, identity strings); the rest must agree
// across present cells. Absent cells contribute nothing — no
// agreement pressure, no per-attr arm.
// groupSink accumulates the config_setting_group targets the fold's
// platform-conditional pre-existing arms need (an //options or
// //config arm present in SOME cells only must AND with the
// platform: `selects.config_setting_group(match_all=[<platform
// select key>, <arm label>])`). Groups dedupe by name and register
// their labels under a per-base-family group family (groups of one
// option flag are pairwise exclusive across platforms; groups of
// different flags can match simultaneously and must split selects).
type groupSink struct {
	families map[string]string    // the merged package's unified family map (mutated)
	defs     map[string]ir.Target // group label (":<name>") → target
}

// group mints (or reuses) the AND-group for (cell, armLabel) and
// returns its select-arm label. Two DISTINCT (SelectKey, armLabel)
// pairs can sanitize to one name (platform names "linux" vs "Linux",
// punctuation-only differences); reusing the label would attach
// items to the wrong AND-condition, so a colliding name
// disambiguates with a deterministic numeric suffix.
func (g *groupSink) group(c Cell, armLabel string) string {
	base := sanitizeGroupPart(c.Platform.Name) + "_and_" + sanitizeGroupPart(armLabel)
	matchAll := []string{c.Platform.SelectKey, armLabel}
	name := base
	for i := 2; ; i++ {
		label := ":" + name
		existing, ok := g.defs[label]
		if !ok {
			g.defs[label] = ir.Target{
				Name:          name,
				Kind:          ir.KindConfigSettingGroup,
				GroupMatchAll: matchAll,
				Visibility:    []string{"//visibility:private"},
			}
			fam := g.families[armLabel]
			if fam == "" {
				fam = armLabel
			}
			g.families[label] = fam + "+platform"
			return label
		}
		if sliceEqual(existing.GroupMatchAll, matchAll) {
			return label
		}
		name = fmt.Sprintf("%s_%d", base, i)
	}
}

// sorted returns the group targets in deterministic label order.
func (g *groupSink) sorted() []ir.Target {
	labels := sliceutil.SortedKeys(g.defs)
	out := make([]ir.Target, 0, len(labels))
	for _, l := range labels {
		out = append(out, g.defs[l])
	}
	return out
}

// sanitizeGroupPart maps a platform name or arm label onto a
// target-name-safe fragment: ASCII-lowercased, every byte outside
// [a-z0-9_.-] replaced by '_', label punctuation collapsed
// ("//options:foo_on" → "options_foo_on").
func sanitizeGroupPart(s string) string {
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimLeft(s, "/")
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+'a'-'A')
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// foldPreexistingArms consumes the cells' pre-existing
// PerPlatform[def.name] arms and splits them by cross-cell
// agreement: an arm ITEM present in that arm in EVERY present cell
// passes through under the arm's own label (the #217 shape — cmake
// derives the guard identically on every configure platform, and
// option arms that agree across platforms stay platform-independent);
// an item present in SOME cells' arm only is option×platform-
// conditional and routes to the AND-group of (cell, armLabel). The
// returned claimed set lists every item any arm carried (excluded
// from the flat-baseline membership partition).
func foldPreexistingArms(def attrDef, variants map[string]ir.Target, cells []Cell, sink *groupSink) (arms map[string][]string, claimed map[string]bool) {
	membership := map[string]map[string]map[string]bool{} // armLabel → item → cellName
	for _, c := range cells {
		for armLabel, items := range variants[c.Platform.Name].PerPlatform[def.name] {
			if membership[armLabel] == nil {
				membership[armLabel] = map[string]map[string]bool{}
			}
			for _, item := range items {
				if membership[armLabel][item] == nil {
					membership[armLabel][item] = map[string]bool{}
				}
				membership[armLabel][item][c.Platform.Name] = true
			}
		}
	}
	arms = map[string][]string{}
	claimed = map[string]bool{}
	for armLabel, items := range membership {
		for item, present := range items {
			claimed[item] = true
			if len(present) == len(cells) {
				arms[armLabel] = append(arms[armLabel], item)
				continue
			}
			for _, c := range cells {
				if present[c.Platform.Name] {
					label := sink.group(c, armLabel)
					arms[label] = append(arms[label], item)
				}
			}
		}
	}
	return arms, claimed
}

func foldTarget(variants map[string]ir.Target, cells []Cell, allCellNames []string, sink *groupSink) (*ir.Target, error) {
	first := variants[cells[0].Platform.Name]
	artifactNameDiverged := false
	// Scalar / boolean attrs must agree across cells.
	for _, c := range cells[1:] {
		v := variants[c.Platform.Name]
		if v.Linkstatic != first.Linkstatic {
			return nil, fmt.Errorf("attribute Linkstatic disagrees: cell %q has %v, cell %q has %v",
				cells[0].Platform.Name, first.Linkstatic, c.Platform.Name, v.Linkstatic)
		}
		if v.Alwayslink != first.Alwayslink {
			return nil, fmt.Errorf("attribute Alwayslink disagrees: cell %q has %v, cell %q has %v",
				cells[0].Platform.Name, first.Alwayslink, c.Platform.Name, v.Alwayslink)
		}
		firstName, vName := cells[0].Platform.Name, c.Platform.Name
		if v.InstallDest != first.InstallDest {
			return nil, fmt.Errorf("InstallDest disagrees: cell %q has %q, cell %q has %q", firstName, first.InstallDest, vName, v.InstallDest)
		}
		// ArtifactName (the on-disk file name, e.g. libSDL3.so /
		// SDL3.dll / libSDL3.dylib) LEGITIMATELY diverges across
		// platforms — the OS dictates the prefix/suffix. Unlike the
		// other scalars, a disagreement here is expected for any shared
		// library in a real multi-platform fold, so it must NOT be a
		// hard error. ArtifactName feeds only the synthesized
		// <Pkg>Config.cmake bundle's install path
		// (cmakecfg: InstallDest/ArtifactName); the emitted BUILD rules
		// don't reference it. We keep the FIRST cell's value (the merged
		// target's flat ArtifactName, deterministic by cell order) and
		// tag the divergence so it's auditable. A fully per-platform
		// config bundle is a separate follow-on; for now the bundle
		// carries the primary platform's name and the tag records that
		// the other platforms differ.
		if v.ArtifactName != first.ArtifactName {
			artifactNameDiverged = true
		}
		if v.LinkLanguage != first.LinkLanguage {
			return nil, fmt.Errorf("LinkLanguage disagrees: cell %q has %q, cell %q has %q", firstName, first.LinkLanguage, vName, v.LinkLanguage)
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
	// Record per-platform ArtifactName divergence (kept the first cell's
	// value above). Tag so operators can grep for targets whose
	// synthesized config-bundle install path is correct only for the
	// primary platform — the full per-platform bundle is a follow-on.
	if artifactNameDiverged {
		merged.Tags = sortedUnion([][]string{merged.Tags, {"cmake-codegen-artifact-name-per-platform"}})
	}

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
	//
	// Phantom targets (present in only a subset of the full
	// matrix) seed Partition with `allCellNames`, which always
	// includes the absent cells. Absent cells contribute no
	// facts, so no item is "in every declared cell" — Partition's
	// baseline collapses to empty and every present-cell
	// observation routes to a per-present-platform delta arm.
	// Order-sensitive and scalar attrs detect phantom shape via
	// `phantom := len(cells) != len(allCellNames)` and force the
	// per-platform-arm shape even when every present cell agrees,
	// so absent platforms don't inherit a flat baseline that
	// describes a target they don't have.
	phantom := len(cells) != len(allCellNames)

	for _, def := range scalarTargetAttrs {
		foldScalarAttr(def, &merged, variants, cells, phantom)
	}

	for _, def := range targetAttrs {
		if def.orderSensitive {
			foldOrderSensitiveAttr(def, &merged, variants, cells, phantom, sink)
			continue
		}
		foldOrderInsensitiveAttr(def, &merged, variants, cells, allCellNames, sink)
	}
	return &merged, nil
}

// foldOrderInsensitiveAttr folds one order-insensitive attr (srcs, hdrs,
// includes, defines, deps) via set-membership partition: items every cell
// carries land in the flat baseline; items unique to a subset route to
// PerPlatform[attr][SelectKey] arms.
//
// A cell can arrive with PRE-EXISTING PerPlatform[def.name] deltas — the
// lower-side #217 platform-conditional partition splits out
// per-`@platforms//os:*` sources before the fold runs, and the option lift
// lands //options:* (and //config:*) arms. Arm items that agree across every
// present cell pass straight through to the merged target's arms (cmake
// derives an if()-guard identically on every configure platform; an option
// delta shared by every platform is platform-independent). Arm items present
// in SOME cells only are option×platform-conditional and route through
// selects.config_setting_group AND-arms (foldPreexistingArms). Items that
// only ever appear FLAT go through the cell-membership partition below
// (baseline = in every cell). Without the pass-through, folding partitioned
// cells would drop every arm item — losing all conditional facts from the
// unified BUILD.
func foldOrderInsensitiveAttr(def attrDef, merged *ir.Target, variants map[string]ir.Target, cells []Cell, allCellNames []string, sink *groupSink) {
	preArms, armItemKeys := foldPreexistingArms(def, variants, cells, sink)

	facts := map[string]map[string]bool{}
	for _, c := range cells {
		items := def.get(variants[c.Platform.Name])
		for _, item := range items {
			// Skip items already routed to a platform arm — they're
			// conditional, not flat-baseline candidates. (A source that is flat
			// in one cell but arm-routed in another belongs in the arm; the
			// #217 partition is the authority on conditionality.)
			if armItemKeys[item] {
				continue
			}
			if facts[item] == nil {
				facts[item] = map[string]bool{}
			}
			facts[item][c.Platform.Name] = true
		}
	}
	baseline, deltas := empfold.Partition(allCellNames, facts)

	flat := sliceutil.SortedKeys(baseline)
	def.set(merged, flat)

	// Emit the pre-existing arm union first, then the cell-membership deltas.
	// Both feed merged.PerPlatform[def.name].
	setArm := func(selKey string, items []string) {
		if len(items) == 0 {
			return
		}
		sort.Strings(items)
		if merged.PerPlatform == nil {
			merged.PerPlatform = map[string]map[string][]string{}
		}
		if merged.PerPlatform[def.name] == nil {
			merged.PerPlatform[def.name] = map[string][]string{}
		}
		// Merge into any existing arm (a selectKey can come from both an
		// absorbed arm and a membership delta).
		existing := merged.PerPlatform[def.name][selKey]
		merged.PerPlatform[def.name][selKey] = sortedUnion([][]string{existing, items})
	}
	for selKey, items := range preArms {
		setArm(selKey, append([]string(nil), items...))
	}

	for _, c := range cells {
		d := deltas[c.Platform.Name]
		if len(d) == 0 {
			continue
		}
		items := make([]string, 0, len(d))
		for k := range d {
			items = append(items, k)
		}
		setArm(c.Platform.SelectKey, items)
	}
}

// foldOrderSensitiveAttr handles copts / linkopts. When every
// present cell carries the same sequence AND the target is NOT
// phantom (i.e. every cell in the full matrix declares it), the
// merged target gets that sequence as a flat baseline. When
// sequences diverge OR the target is phantom (declared by only
// a subset), each present cell's full sequence is stored verbatim
// under PerPlatform[attr][SelectKey] with the flat baseline
// cleared, so emit renders a bare select() that preserves
// per-platform ordering exactly. Phantom forces the per-platform
// shape even when present cells agree, so absent platforms don't
// inherit a baseline describing a target they don't have.
// Pre-existing arms (the option lift's //options / //config copts and
// linkopts arms) fold FIRST, by whole-arm agreement: an arm whose
// sequence is byte-identical in every present cell passes through
// under its own label; an arm missing from some cells or differing
// per cell routes each present cell's verbatim sequence through the
// (cell, armLabel) AND-group — order preserved within each arm either
// way.
func foldOrderSensitiveAttr(def attrDef, merged *ir.Target, variants map[string]ir.Target, cells []Cell, phantom bool, sink *groupSink) {
	foldOrderSensitivePreArms(def, merged, variants, cells, sink)
	first := def.get(variants[cells[0].Platform.Name])
	allEqual := !phantom
	if allEqual {
		for _, c := range cells[1:] {
			if !sliceEqual(first, def.get(variants[c.Platform.Name])) {
				allEqual = false
				break
			}
		}
	}
	if allEqual {
		def.set(merged, append([]string(nil), first...))
		return
	}
	def.set(merged, nil)
	// All present cells carry an empty sequence (common for
	// phantom targets that just don't set copts/linkopts) →
	// don't allocate per-platform arms. Emit treats nil and
	// empty PerPlatform[name] identically, so this avoids
	// rendering noise like `copts = select({"<plat>": [],
	// "//conditions:default": []})` and keeps the IR shape
	// "attribute simply isn't set."
	anyNonEmpty := false
	for _, c := range cells {
		if len(def.get(variants[c.Platform.Name])) > 0 {
			anyNonEmpty = true
			break
		}
	}
	if !anyNonEmpty {
		return
	}
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

// foldOrderSensitivePreArms folds the cells' pre-existing
// order-sensitive arms (see foldOrderSensitiveAttr's doc): whole-arm
// byte-agreement across every present cell → pass-through; anything
// else → per-cell AND-group arms with each cell's verbatim sequence.
func foldOrderSensitivePreArms(def attrDef, merged *ir.Target, variants map[string]ir.Target, cells []Cell, sink *groupSink) {
	labels := map[string]bool{}
	for _, c := range cells {
		for armLabel := range variants[c.Platform.Name].PerPlatform[def.name] {
			labels[armLabel] = true
		}
	}
	if len(labels) == 0 {
		return
	}
	setArm := func(label string, seq []string) {
		if len(seq) == 0 {
			return
		}
		if merged.PerPlatform == nil {
			merged.PerPlatform = map[string]map[string][]string{}
		}
		if merged.PerPlatform[def.name] == nil {
			merged.PerPlatform[def.name] = map[string][]string{}
		}
		merged.PerPlatform[def.name][label] = append([]string(nil), seq...)
	}
	for armLabel := range labels {
		agreed := true
		first := variants[cells[0].Platform.Name].PerPlatform[def.name][armLabel]
		for _, c := range cells {
			seq, ok := variants[c.Platform.Name].PerPlatform[def.name][armLabel]
			if !ok || !sliceEqual(first, seq) {
				agreed = false
				break
			}
		}
		if agreed {
			setArm(armLabel, first)
			continue
		}
		for _, c := range cells {
			if seq, ok := variants[c.Platform.Name].PerPlatform[def.name][armLabel]; ok {
				setArm(sink.group(c, armLabel), seq)
			}
		}
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

// scalarAttrDef declares one single-string attribute the fold
// partitions per platform. The cc_import path attrs (static_library
// / shared_library) are the v1 use case: the install root's path
// layout diverges by platform (multiarch lib dirs, .so vs .dylib),
// so two cells legitimately carry different values for the same
// target. Folding them into PerPlatformScalar lets emit render a
// select() instead of demanding cross-cell agreement.
type scalarAttrDef struct {
	name string
	get  func(ir.Target) string
	set  func(*ir.Target, string)
}

var scalarTargetAttrs = []scalarAttrDef{
	{"static_library", func(t ir.Target) string { return t.StaticLibrary }, func(t *ir.Target, v string) { t.StaticLibrary = v }},
	{"shared_library", func(t ir.Target) string { return t.SharedLibrary }, func(t *ir.Target, v string) { t.SharedLibrary = v }},
}

// foldScalarAttr partitions one scalar attribute across cells.
// All present cells agree on the same non-empty value AND the
// target is NOT phantom (declared by every cell in the matrix) →
// that value lands in the flat field, PerPlatformScalar
// untouched. All present cells agree on the empty value (the
// attribute simply isn't relevant — e.g. a cc_import that only
// carries shared_library leaves static_library empty) → flat
// stays empty, no delta. Present cells disagree OR the target is
// phantom → flat clears and each present cell's non-empty value
// moves into PerPlatformScalar[attr][SelectKey]. Cells that
// lacked the path simply omit their arm from the delta map; emit
// renders the resulting select() with a trailing
// `"//conditions:default": None` arm, so in-matrix platforms
// that omitted an arm AND out-of-matrix platforms fall through
// to "attribute unset" (Bazel's treatment of None for an
// optional path attr like cc_import.static_library) — exactly
// the right outcome for the partial-platform cc_import shape and
// for the phantom-target case alike.
//
// Phantom forces the per-platform shape even when present cells
// agree, so absent platforms don't see a flat scalar describing
// a target they don't have.
func foldScalarAttr(def scalarAttrDef, merged *ir.Target, variants map[string]ir.Target, cells []Cell, phantom bool) {
	first := def.get(variants[cells[0].Platform.Name])
	allEqual := !phantom
	if allEqual {
		for _, c := range cells[1:] {
			if def.get(variants[c.Platform.Name]) != first {
				allEqual = false
				break
			}
		}
	}
	if allEqual {
		def.set(merged, first)
		return
	}
	def.set(merged, "")
	// All present cells carry an empty value (common for
	// phantom cc_import targets that just don't populate this
	// path attr) → don't allocate a per-platform arm map. Emit
	// treats nil and empty PerPlatformScalar[name] identically;
	// short-circuiting here keeps the IR shape "attribute
	// simply isn't set" rather than leaving a spurious empty
	// entry that future readers might mistake for a populated
	// delta map.
	for _, c := range cells {
		v := def.get(variants[c.Platform.Name])
		if v == "" {
			continue
		}
		if merged.PerPlatformScalar == nil {
			merged.PerPlatformScalar = map[string]map[string]string{}
		}
		if merged.PerPlatformScalar[def.name] == nil {
			merged.PerPlatformScalar[def.name] = map[string]string{}
		}
		merged.PerPlatformScalar[def.name][c.Platform.SelectKey] = v
	}
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
		if _, dup := out[p.Name]; dup {
			return nil, fmt.Errorf("elementfold: platform %q appears twice in PickSelectKeys input", p.Name)
		}
		if p.SelectKey != "" {
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
	out := sliceutil.SortedKeys(seen)
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
