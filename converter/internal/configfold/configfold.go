// Package configfold projects fileapi.Reply.TargetsByConfig
// into per-config flag deltas via empfold. It's the building block
// Phase 5 of the generator-parity uplift (ROADMAP.md) uses to fold
// multi-config codemodel data into select() arms keyed on
// //config:<name>.
//
// The cross-cutting question the package answers: given N target
// JSONs for the same cmake target (one per Configuration name),
// which compile fragments / defines / includes / link fragments
// are common to every config and which differ per-config?
//
// Output shape:
//
//   - Baseline: facts (defines / includes / fragments) every config
//     agrees on. Lower can flatten these into the rule's primary
//     attribute (e.g. cc_library.defines).
//
//   - Deltas: per-config facts. Lower lifts these into
//     select({"//config:<name>": [...]}) arms wrapping the same
//     attribute.
//
// The package is pure data — no IR, no Bazel, no policy. Callers
// (lower/) decide how to translate the partition into IR.
package configfold

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/internal/empfold"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// TargetFold is one cmake target's cross-config partition.
type TargetFold struct {
	// Name is the cmake target name.
	Name string

	// Defines, Includes, LinkFragments, CompileFragments
	// carry the post-empfold-Partition split for each fact
	// family. Each map's outer key is the cell (config) name;
	// Baseline holds facts every cell agreed on.
	Defines  Partition
	Includes Partition
	// LinkFragments is the role-tagged link line: each entry's
	// key is the verbatim fragment (e.g. "-lz" or "-Wl,--as-needed"
	// or "/abs/libz.a"), prefixed by role to disambiguate cmake's
	// commandFragments[].role (libraries / flags / libraryPath /
	// frameworkPath). Prefix lets a "-lz" library fragment and a
	// "-lz" flag fragment partition independently.
	LinkFragments Partition
	// CompileFragments is the per-language compile flag set
	// (CompileGroup.CompileCommandFragments[]). One entry per
	// (language, fragment) pair, prefixed by language to
	// disambiguate.
	CompileFragments Partition

	// Sources is the per-target source file set keyed on
	// TargetSource.Path. Tracks per-config source-list deltas —
	// projects whose CMakeLists.txt gates `target_sources(X
	// PRIVATE ${SRC})` on the build configuration (e.g.
	// `if(CMAKE_BUILD_TYPE STREQUAL "Debug") target_sources(X
	// PRIVATE debug_only.c) endif()`) emit different Sources[]
	// per config; the partition routes the deltas to
	// PerPlatform["srcs"] select() arms instead of dropping
	// to cfg[0]'s view.
	Sources Partition
	// Dependencies is the per-target codemodel-deps set keyed on
	// TargetDependency.Id. Same shape as Sources, gated on
	// `target_link_libraries(X PRIVATE ${LIB})` under build-
	// config-conditional cmake blocks; routes per-config dep
	// deltas to PerPlatform["deps"] arms.
	Dependencies Partition
}

// Partition is one fact family's cross-cell split. Baseline keeps
// facts every declared cell agrees on (value identical across all);
// Deltas[cell] holds facts that cell observed differently or
// uniquely.
type Partition struct {
	Baseline map[string]bool
	Deltas   map[string]map[string]bool
}

// Project folds Reply.TargetsByConfig into per-target cross-config
// partitions. configs lists every configuration name participating
// in the fold (typically Reply's Codemodel.Configurations[].Name in
// declared order). The returned slice is sorted by target name for
// deterministic output.
//
// Targets without a matching entry in every config are still
// returned — the missing-cell case shows up as "this fact is in
// every cell that observed the target, but not every declared
// cell", which empfold.Partition correctly routes to the
// Deltas[cell] arm of the cells that did observe.
//
// Pass r.TargetsByConfig; for single-config replies, r.Targets
// can be promoted by wrapping in a one-config map externally —
// but Project on single-config returns no useful split (every
// fact lands in baseline), so callers should gate on
// len(configs) > 1.
// configTables holds one target's per-fact tables, each keyed
// (factValue → configName → present), accumulated across configs by
// recordConfigTarget and then partitioned into baseline + per-config deltas.
type configTables struct {
	defines  map[string]map[string]bool
	includes map[string]map[string]bool
	link     map[string]map[string]bool
	compile  map[string]map[string]bool
	sources  map[string]map[string]bool
	deps     map[string]map[string]bool
}

func newConfigTables() *configTables {
	return &configTables{
		defines:  map[string]map[string]bool{},
		includes: map[string]map[string]bool{},
		link:     map[string]map[string]bool{},
		compile:  map[string]map[string]bool{},
		sources:  map[string]map[string]bool{},
		deps:     map[string]map[string]bool{},
	}
}

// mark records that fact `value` is present under `cfgName` in table m.
func mark(m map[string]map[string]bool, value, cfgName string) {
	if m[value] == nil {
		m[value] = map[string]bool{}
	}
	m[value][cfgName] = true
}

// recordConfigTarget folds one config's view of a target (t, seen under
// cfgName) into tbl: its non-generated sources, codemodel dependencies, the
// per-compile-group defines / includes / tokenised compile flags, and the
// tokenised link fragments.
func recordConfigTarget(tbl *configTables, t fileapi.Target, cfgName string) {
	// Per-config sources. Skip generated sources (cmake records `<gen>` flag on
	// sources from configure_file / add_custom_command outputs; those are
	// handled by the genrule lift, not the per-config srcs fold).
	for _, src := range t.Sources {
		if src.IsGenerated {
			continue
		}
		mark(tbl.sources, src.Path, cfgName)
	}
	// Per-config codemodel dependencies (target IDs the lower path later
	// resolves to Bazel labels).
	for _, d := range t.Dependencies {
		mark(tbl.deps, d.Id, cfgName)
	}
	for _, cg := range t.CompileGroups {
		for _, def := range cg.Defines {
			mark(tbl.defines, def.Define, cfgName)
		}
		for _, inc := range cg.Includes {
			mark(tbl.includes, inc.Path, cfgName)
		}
		for _, frag := range cg.CompileCommandFragments {
			// cmake's File API serialises compile flags as ONE
			// whitespace-joined fragment per compile group (the verbatim
			// `CMAKE_CXX_FLAGS_<CFG>` + per-target value). Tokenise on
			// whitespace so each flag lands as its own select() arm element —
			// Bazel passes each list entry as a separate argv to gcc; without
			// this split gcc receives the entire string as one (invalid) flag.
			//
			// Disambiguate same-string fragments across languages — a `-O2`
			// under C compiles differently from a `-O2` under CXX in practice,
			// so partition per (language, token).
			for _, tok := range strings.Fields(frag.Fragment) {
				mark(tbl.compile, cg.Language+"|"+tok, cfgName)
			}
		}
	}
	if t.Link != nil {
		for _, frag := range t.Link.CommandFragments {
			// Same tokenisation as compile fragments. The "flags" role
			// typically carries multiple `-Wl,...` joined; "libraries" /
			// "libraryPath" / "frameworkPath" are usually single tokens
			// already so Fields is a no-op for those.
			for _, tok := range strings.Fields(frag.Fragment) {
				mark(tbl.link, frag.Role+"|"+tok, cfgName)
			}
		}
	}
}

func Project(byConfig map[string]map[string]fileapi.Target, configs []string) []TargetFold {
	// Accumulate per-target fact tables across every (config, target) cell.
	perTarget := map[string]*configTables{}
	targetNames := map[string]string{} // id → display name

	for id, byCell := range byConfig {
		for cfgName, t := range byCell {
			tbl, ok := perTarget[id]
			if !ok {
				tbl = newConfigTables()
				perTarget[id] = tbl
			}
			if t.Name != "" {
				targetNames[id] = t.Name
			}
			recordConfigTarget(tbl, t, cfgName)
		}
	}

	// Partition each fact family per target.
	ids := sliceutil.SortedKeys(perTarget)

	out := make([]TargetFold, 0, len(ids))
	for _, id := range ids {
		tbl := perTarget[id]
		name := targetNames[id]
		if name == "" {
			name = id
		}
		out = append(out, TargetFold{
			Name:             name,
			Defines:          partitionToShape(empfold.Partition(configs, tbl.defines)),
			Includes:         partitionToShape(empfold.Partition(configs, tbl.includes)),
			LinkFragments:    partitionToShape(empfold.Partition(configs, tbl.link)),
			CompileFragments: partitionToShape(empfold.Partition(configs, tbl.compile)),
			Sources:          partitionToShape(empfold.Partition(configs, tbl.sources)),
			Dependencies:     partitionToShape(empfold.Partition(configs, tbl.deps)),
		})
	}
	return out
}

// partitionToShape lifts the (baseline, deltas) tuple empfold
// returns into the Partition struct that's easier for callers
// to handle as a single return value.
func partitionToShape(baseline map[string]bool, deltas map[string]map[string]bool) Partition {
	return Partition{Baseline: baseline, Deltas: deltas}
}
