package lower

import (
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// The 2D option×config fold: when --lift-options runs under a
// multi-config configure (--build-types), a fact can vary on EITHER
// axis — or on both at once (`$<$<AND:$<CONFIG:Debug>,
// $<BOOL:${FOO}>>:…>`-shaped). Additive selects can't subtract, so
// the pure //config:* arms the base multi-config fold emitted (from
// the base reply alone, i.e. measured at the configured option
// values) are only honest for facts that are config-conditional
// under EVERY option value. This fold classifies each fact by its
// support over the full (config × option-value) cell grid:
//
//   - support == every cell            → baseline (flat attrs; skip)
//   - full grid, every value, some cfg → pure config: the base fold's
//     //config:<cfg> arm is already correct; skip.
//   - full grid, every cfg, some value → pure option: //options arms,
//     exactly the single-axis fold's shape.
//   - anything else (mixed support)    → one AND arm per supporting
//     cell, keyed by a skylib config_setting_group over
//     (//config:<cfg>, <option arm>) — and the fact is REMOVED from
//     any base-fold //config arm that would over-apply it under
//     option values outside the support.
//
// AND-group arms over one (build_type flag, option flag) pair are
// pairwise exclusive (each fixes both flags' values), so they form
// one select family per option.

// Cell2DKey names one (config, option-value) grid cell for the joint
// configfold.Project call. The option side is the value's ARM LABEL
// (already unique per option value).
func Cell2DKey(config, valueArm string) string {
	return config + "\x00" + valueArm
}

// OptionGroup is one config_setting_group the 2D fold's mixed facts
// need: Name is the //options-package target name, MatchAll the two
// conditions (the //config:<cfg> setting and the option-value
// setting) that must BOTH hold.
type OptionGroup struct {
	Name     string
	MatchAll []string
}

// GroupLabel/groupName mint the AND-arm's label from its config name
// and option-arm label: //options:<cfg>_and_<option-arm-suffix>.
func groupName(config, valueArm string) string {
	return toLower(config) + "_and_" + strings.TrimPrefix(valueArm, "//options:")
}

// ApplyOptionFold2D projects one lifted option's deltas over the
// full (config × option-value) grid into select() arms on
// pkg.Targets. byCell maps target NAME → Cell2DKey(config, valueArm)
// → that cell's codemodel view; configs are the non-feature config
// names (base reply order); valueArms the option's arm labels, the
// configured value's first. flagLabel is the option's select family
// ("//options:<name>").
//
// Returns the names of targets that gained arms, plus the
// config_setting_groups the mixed facts need (deduplicated; the
// caller emits them into the //options package and they're
// registered under the per-(build_type, option) group family).
func ApplyOptionFold2D(pkg *ir.Package, byCell map[string]map[string]fileapi.Target, configs, valueArms []string, cmakeSrc, cmakeBuild string, idToName map[string]string, flagLabel string) ([]string, []OptionGroup) {
	if pkg == nil || len(byCell) == 0 || len(configs) < 2 || len(valueArms) < 2 {
		return nil, nil
	}
	cells := make([]string, 0, len(configs)*len(valueArms))
	for _, c := range configs {
		for _, v := range valueArms {
			cells = append(cells, Cell2DKey(c, v))
		}
	}
	folds := configfold.Project(byCell, cells)
	if len(folds) == 0 {
		return nil, nil
	}
	byName := map[string]*ir.Target{}
	for i := range pkg.Targets {
		byName[pkg.Targets[i].Name] = &pkg.Targets[i]
	}

	groupFamily := "//config:build_type+" + flagLabel
	groups := map[string]OptionGroup{}
	var lifted []string
	for _, fold := range folds {
		tgt, ok := byName[fold.Name]
		if !ok {
			continue
		}
		before := perPlatformArmCount(tgt)
		f2 := fold2D{
			pkg: pkg, tgt: tgt,
			configs: configs, valueArms: valueArms,
			cmakeSrc: cmakeSrc, cmakeBuild: cmakeBuild,
			groupFamily: groupFamily, groups: groups,
		}
		f2.family("defines", fold.Defines, nil)
		f2.family("copts", fold.CompileFragments, nil)
		f2.family("linkopts", fold.LinkFragments, nil)
		f2.family("includes", fold.Includes, nil)
		f2.family("srcs", fold.Sources, nil)
		f2.family("deps", fold.Dependencies, idToName)
		for _, arms := range tgt.PerPlatform {
			for label, vs := range arms {
				sort.Strings(vs)
				arms[label] = vs
			}
		}
		if arms, ok := tgt.PerPlatform["copts"]; ok {
			for label, vs := range arms {
				arms[label] = filterPCHCoptArm(vs)
			}
		}
		dedupBaselineAgainstDeltas(tgt)
		// Reconciliation can empty a base-fold //config arm entirely
		// (every fact moved onto AND arms); an empty arm renders as
		// `"//config:debug": [],` noise, so drop emptied arms — unlike
		// pruneEmptyPerPlatform's whole-attr rule, this is per arm.
		for _, arms := range tgt.PerPlatform {
			for label, vs := range arms {
				if len(vs) == 0 {
					delete(arms, label)
				}
			}
		}
		pruneEmptyPerPlatform(tgt)
		if perPlatformArmCount(tgt) > before {
			lifted = append(lifted, fold.Name)
		}
	}
	sort.Strings(lifted)
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]OptionGroup, 0, len(names))
	for _, n := range names {
		out = append(out, groups[n])
	}
	return lifted, out
}

// fold2D carries one target's 2D classification state.
type fold2D struct {
	pkg                  *ir.Package
	tgt                  *ir.Target
	configs, valueArms   []string
	cmakeSrc, cmakeBuild string
	groupFamily          string
	groups               map[string]OptionGroup
}

// family classifies one fact family's partition. relabel, when
// non-nil, maps fact tokens (codemodel dependency ids) onto ":<name>"
// labels first — matching the single-axis fold's deps handling; ids
// without a mapping drop.
func (f *fold2D) family(attr string, p configfold.Partition, relabel map[string]string) {
	if len(p.Deltas) == 0 {
		return
	}
	// Reconstruct each fact's support set from the per-cell deltas.
	support := map[string]map[string]bool{} // fact → cell → present
	for cell, facts := range p.Deltas {
		for fact := range facts {
			if support[fact] == nil {
				support[fact] = map[string]bool{}
			}
			support[fact][cell] = true
		}
	}
	facts := make([]string, 0, len(support))
	for fact := range support {
		facts = append(facts, fact)
	}
	sort.Strings(facts)
	for _, fact := range facts {
		f.classify(attr, fact, support[fact], relabel)
	}
}

// classify routes one fact by its support shape (see the file
// header).
func (f *fold2D) classify(attr, fact string, cells map[string]bool, relabel map[string]string) {
	cfgSet := map[string]bool{}
	valSet := map[string]bool{}
	for cell := range cells {
		c, v, _ := strings.Cut(cell, "\x00")
		cfgSet[c] = true
		valSet[v] = true
	}
	fullGrid := len(cells) == len(cfgSet)*len(valSet)
	allCfgs := len(cfgSet) == len(f.configs)
	allVals := len(valSet) == len(f.valueArms)

	token := fact
	if relabel != nil {
		name, ok := relabel[fact]
		if !ok || name == "" {
			return
		}
		token = ":" + name
	} else {
		var keep bool
		token, keep = filterFactForAttr(attr, fact, f.cmakeSrc, f.cmakeBuild)
		if !keep {
			return
		}
	}

	switch {
	case fullGrid && allVals && allCfgs:
		return // baseline: flat attrs already carry it
	case fullGrid && allVals:
		// Pure config fact: config-conditional under every option
		// value — the base multi-config fold's //config:<cfg> arms
		// (measured at the configured values) are already correct.
		return
	case fullGrid && allCfgs:
		// Pure option fact: same shape as the single-axis fold.
		for v := range valSet {
			f.addArm(attr, v, token)
		}
	default:
		// Mixed support: one AND arm per supporting cell, and the
		// base fold's //config arms must stop over-applying the fact
		// under option values outside the support.
		for cell := range cells {
			c, v, _ := strings.Cut(cell, "\x00")
			label := "//options:" + groupName(c, v)
			if _, ok := f.groups[label]; !ok {
				f.groups[label] = OptionGroup{
					Name:     groupName(c, v),
					MatchAll: []string{"//config:" + toLower(c), v},
				}
				registerSelectArmFamily(f.pkg, label, f.groupFamily)
			}
			f.addArm(attr, label, token)
		}
		baseArm := f.valueArms[0]
		for c := range cfgSet {
			if cells[Cell2DKey(c, baseArm)] {
				f.removeFromArm(attr, configLabel(c), token)
				if attr == "defines" {
					// The base fold's interface-scope pass can route a
					// private define's //config arm onto local_defines;
					// the mixed fact must leave that spelling too or the
					// plain config arm still over-applies it.
					f.removeFromArm("local_defines", configLabel(c), token)
				}
			}
		}
	}
}

// addArm appends token under the arm label (lazy map init; sorting
// happens once per target after classification).
func (f *fold2D) addArm(attr, label, token string) {
	if f.tgt.PerPlatform == nil {
		f.tgt.PerPlatform = map[string]map[string][]string{}
	}
	if f.tgt.PerPlatform[attr] == nil {
		f.tgt.PerPlatform[attr] = map[string][]string{}
	}
	f.tgt.PerPlatform[attr][label] = append(f.tgt.PerPlatform[attr][label], token)
}

// removeFromArm drops token from an existing arm (the base
// multi-config fold's //config:<cfg> arm) — additive selects can't
// subtract, so a fact that turns out to be option-conditional must
// leave the plain config arm and live on the AND arms instead.
func (f *fold2D) removeFromArm(attr, label, token string) {
	arms := f.tgt.PerPlatform[attr]
	if arms == nil {
		return
	}
	vs := arms[label]
	out := vs[:0]
	for _, v := range vs {
		if v != token {
			out = append(out, v)
		}
	}
	arms[label] = out
}
