package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// lowerMultiConfigDeltas projects Reply.TargetsByConfig into
// PerPlatform-shaped attribute deltas on pkg.Targets. Phase 5 of
// the generator-parity uplift (ROADMAP.md).
//
// Wires the configfold.Partition cross-config split into the
// existing per-platform fold infrastructure: PerPlatform's
// documented contract is "constraint_value OR config_setting
// labels", and per-config selects are exactly the config_setting
// case. The emitter renders the same select() shape it would for
// per-platform deltas — no emit changes needed.
//
// Sanitizer-shaped config names (per configfold.SanitizerFeature)
// are deliberately NOT routed through PerPlatform. The
// Bazel-idiomatic shape for sanitizer flag deltas is a
// cc_toolchain feature plus --features=<name>; emitting a select()
// over //config:asan would be the anti-pattern the Phase 7 audit
// catches. v1 skips sanitizer configs silently; downstream
// audit/info passes can surface the skipped set so operators see
// what's expected to flow through features instead.
//
// Returns when byConfig is empty (single-config reply) or when no
// target has cross-config deltas (every fact agreed across cells).
// Pure function on pkg.Targets except for PerPlatform mutation.
func lowerMultiConfigDeltas(pkg *ir.Package, byConfig map[string]map[string]fileapi.Target, configNames []string, cmakeSrc, cmakeBuild string, idToName map[string]string, pch *pchLiftCtx) {
	if len(byConfig) == 0 || len(configNames) < 2 {
		return
	}
	// Filter out sanitizer-shaped configs; they're routed through
	// --features by a future slice. Including them here would
	// produce the select() the audit treats as anti-pattern.
	nonFeatureConfigs := nonFeatureConfigNames(configNames)
	if len(nonFeatureConfigs) < 2 {
		// Every non-primary config is a feature variant; no
		// cross-config Partition useful at the IR level.
		return
	}

	folds := configfold.Project(byConfig, nonFeatureConfigs)
	if len(folds) == 0 {
		return
	}

	// Index pkg.Targets by name for fast match, and invert idToName for
	// the per-config PCH owner lookups (byConfig is keyed by target ID).
	byName := map[string]*ir.Target{}
	for i := range pkg.Targets {
		byName[pkg.Targets[i].Name] = &pkg.Targets[i]
	}
	nameToID := map[string]string{}
	for id, name := range idToName {
		nameToID[name] = id
	}

	for _, fold := range folds {
		tgt, ok := byName[fold.Name]
		if !ok {
			continue
		}
		applyPartition(tgt, "defines", fold.Defines, cmakeSrc, cmakeBuild, configLabel)
		applyPartition(tgt, "copts", fold.CompileFragments, cmakeSrc, cmakeBuild, configLabel)
		if pch != nil {
			if id, ok := nameToID[fold.Name]; ok {
				perConfigPCHArms(tgt, byConfig[id], nonFeatureConfigs, byConfig, nameToID, *pch)
			}
		}
		applyPartition(tgt, "linkopts", fold.LinkFragments, cmakeSrc, cmakeBuild, configLabel)
		// Includes are routed to the "includes" Bazel attribute
		// rather than copts -I, matching the rest of the lift's
		// includes handling.
		applyPartition(tgt, "includes", fold.Includes, cmakeSrc, cmakeBuild, configLabel)
		// Phase 5 target-graph fold: per-config srcs / deps.
		// Source files gated on `if(CMAKE_BUILD_TYPE STREQUAL
		// "X") target_sources(... ${SRC})` end up with different
		// Sources[] across configs — without per-config srcs
		// routing they'd silently drop to cfg[0]'s view. Same
		// for codemodel-deps gated on build-type. Source paths
		// and target IDs are already package-relative-ish in the
		// codemodel; no reanchor needed here.
		applyPartition(tgt, "srcs", fold.Sources, cmakeSrc, cmakeBuild, configLabel)
		// Deps need an id→label translation pass before applyPartition
		// — configfold's Dependencies partition is keyed on cmake
		// target IDs (matching codemodel TargetDependency.Id), while
		// the IR's PerPlatform["deps"] needs Bazel-format labels.
		// Substitute IDs with their target names (":<name>") where
		// idToName covers the ID; drop unresolvable IDs (out-of-tree
		// targets the codemodel saw but the lower path didn't lower).
		applyPartition(tgt, "deps", relabelDependencyPartition(fold.Dependencies, idToName), cmakeSrc, cmakeBuild, configLabel)
		// Single-config baseline (the IR's flat copts / defines /
		// linkopts populated by lowerTarget's first-config view)
		// can carry the same value a per-config delta added —
		// because lowerTarget runs on CompileGroups[0] (typically
		// Release), it picks up Release-only flags as "baseline".
		// The multi-config delta then re-adds them in the Release
		// select arm. Drop the duplicates from the flat baseline
		// when they appear in any per-config delta — the select()
		// arm is the canonical source for cross-config-varying
		// values.
		dedupBaselineAgainstDeltas(tgt)
		// applyPartition can leave behind empty per-config arms
		// when its inner filter (libraries-role skip, build-dir
		// reanchor drop) removes every value the cell had. An
		// empty select() arm renders as `"//config:debug": [],`
		// which is verbose noise — the operator-visible select
		// becomes a no-op. Prune any attr in PerPlatform whose
		// arms are all empty.
		pruneEmptyPerPlatform(tgt)
	}
}

// perConfigPCHArms closes the config-varying PCH gap: when a target's
// declared target_precompile_headers LIST differs across configurations
// (a $<CONFIG:...> genex in the declaration), the baseline mirror —
// built from the primary configuration's list — is wrong for the other
// configs. Detect the divergence from the per-config codemodel views,
// synthesize a per-config mirror for each non-primary list, MOVE the
// forced include from the baseline copts into per-config select() arms
// (primary arm reuses the baseline mirror — its list IS the primary
// list, so ensureMirror dedups to the registered rule), and stage every
// config's mirror unconditionally in baseline srcs (cheap inputs;
// per-config srcs arms would buy nothing).
//
// The overwhelmingly common case — same list in every config, only the
// cmake_pch artifact PATH differing by config dir — is detected as
// non-divergent and stays entirely on the baseline mirror
// (filterPCHCoptArm already strips the per-config artifact tokens from
// the arms), making the baseline coverage COMPLETE rather than assumed.
//
// views is byConfig[<this target's id>]; the full byConfig + nameToID
// pair resolves a REUSE_FROM consumer's owner lists (the consumer's own
// PrecompileHeaders is null in every config — the owner's divergence is
// its divergence).
func perConfigPCHArms(tgt *ir.Target, views map[string]fileapi.Target, configNames []string, byConfig map[string]map[string]fileapi.Target, nameToID map[string]string, pch pchLiftCtx) {
	if len(views) == 0 || len(configNames) < 2 {
		return
	}
	primary := configNames[0]
	primView, ok := views[primary]
	if !ok {
		return
	}
	for _, cg := range primView.CompileGroups {
		owner, perCfg := pchPerConfigEntries(tgt.Name, cg.Language, views, configNames, byConfig, nameToID, pch.cmakeBuild)
		if owner == "" || !pchListsDiverge(perCfg, configNames) {
			continue
		}
		// Strip the baseline pair (primary-list mirror) from the flat
		// copts; it moves into the primary config's arm below.
		baseArg := pch.execRootPath(pchMirrorOut(owner, cg.Language, ""))
		tgt.Copts = stripIncludePair(tgt.Copts, baseArg)
		if tgt.PerPlatform == nil {
			tgt.PerPlatform = map[string]map[string][]string{}
		}
		if tgt.PerPlatform["copts"] == nil {
			tgt.PerPlatform["copts"] = map[string][]string{}
		}
		for _, cfg := range configNames {
			entries := perCfg[cfg]
			if len(entries) == 0 {
				continue // PCH absent in this config: no forced include.
			}
			mirrorCfg := cfg
			if cfg == primary {
				mirrorCfg = "" // reuse the registered baseline mirror
			}
			outRel, stageHdrs := pch.ensureMirror(owner, cg.Language, mirrorCfg, entries)
			label := configLabel(cfg)
			tgt.PerPlatform["copts"][label] = append(tgt.PerPlatform["copts"][label],
				"-include", pch.execRootPath(outRel))
			for _, s := range append([]string{outRel}, stageHdrs...) {
				if !stringSliceContains(tgt.Srcs, s) && !stringSliceContains(tgt.Hdrs, s) {
					tgt.Srcs = append(tgt.Srcs, s)
				}
			}
		}
	}
}

// pchPerConfigEntries resolves a target's effective PCH owner and its
// declared header list in EVERY configuration for one language: the
// target's own CompileGroup.PrecompileHeaders when declared, else (the
// REUSE_FROM shape) the owner named by the cmake_pch artifact in that
// config's compile fragments, resolved through byConfig. Returns
// owner == "" when no configuration carries any PCH signal for the
// language.
func pchPerConfigEntries(name, language string, views map[string]fileapi.Target, configNames []string, byConfig map[string]map[string]fileapi.Target, nameToID map[string]string, cmakeBuild string) (string, map[string][]fileapi.CompilePCH) {
	owner := ""
	perCfg := map[string][]fileapi.CompilePCH{}
	for _, cfg := range configNames {
		cg, ok := compileGroupForLanguage(views[cfg], language)
		if !ok {
			continue
		}
		if len(cg.PrecompileHeaders) > 0 {
			perCfg[cfg] = cg.PrecompileHeaders
			if owner == "" {
				owner = name
			}
			continue
		}
		_, _, arts := splitCompileFragments(cg.CompileCommandFragments)
		for _, art := range arts {
			o := pchArtifactOwner(art, cmakeBuild)
			if o == "" {
				continue
			}
			if owner == "" {
				owner = o
			}
			if ocg, ok := compileGroupForLanguage(byConfig[nameToID[o]][cfg], language); ok {
				perCfg[cfg] = ocg.PrecompileHeaders
			}
			break
		}
	}
	return owner, perCfg
}

// compileGroupForLanguage returns the target's first compile group of the
// given language (group order can differ across per-config views, so
// index-based matching would mispair).
func compileGroupForLanguage(t fileapi.Target, language string) (fileapi.CompileGroup, bool) {
	for _, cg := range t.CompileGroups {
		if cg.Language == language {
			return cg, true
		}
	}
	return fileapi.CompileGroup{}, false
}

// pchListsDiverge reports whether the per-config declared lists differ
// anywhere: a config whose list deviates from the primary's (including
// present-vs-absent) makes the baseline mirror wrong for that config.
func pchListsDiverge(perCfg map[string][]fileapi.CompilePCH, configNames []string) bool {
	base := perCfg[configNames[0]]
	for _, cfg := range configNames[1:] {
		if !equalPCHLists(base, perCfg[cfg]) {
			return true
		}
	}
	return false
}

func equalPCHLists(a, b []fileapi.CompilePCH) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Header != b[i].Header {
			return false
		}
	}
	return true
}

// stripIncludePair removes the `-include <arg>` pair naming arg from a
// flat copts slice (first occurrence; the lift emits at most one).
func stripIncludePair(copts []string, arg string) []string {
	for i := 0; i+1 < len(copts); i++ {
		if copts[i] == "-include" && copts[i+1] == arg {
			return append(copts[:i], copts[i+2:]...)
		}
	}
	return copts
}

// pruneEmptyPerPlatform drops any tgt.PerPlatform[attr] map whose
// per-cell value slices are all empty. Leaves attrs with at least
// one non-empty cell intact.
func pruneEmptyPerPlatform(tgt *ir.Target) {
	if tgt == nil || tgt.PerPlatform == nil {
		return
	}
	for attr, arms := range tgt.PerPlatform {
		allEmpty := true
		for _, vs := range arms {
			if len(vs) > 0 {
				allEmpty = false
				break
			}
		}
		if allEmpty {
			delete(tgt.PerPlatform, attr)
		}
	}
	if len(tgt.PerPlatform) == 0 {
		tgt.PerPlatform = nil
	}
}

// dedupBaselineAgainstDeltas removes entries from the flat baseline
// attributes whose value appears in any of the per-cell delta arms
// in tgt.PerPlatform for the same attr. The select() arm is the
// authoritative source for cross-cell-varying values; the flat
// baseline ends up reflecting only truly-baseline values (those
// common to every cell).
//
// srcs / deps matter beyond hygiene: the primary-cell arm re-adds
// the primary view's value, so leaving it flat would DOUBLE it there
// (Bazel rejects duplicate labels in srcs/deps) and wrongly include
// it under every other arm. LocalDefines dedups against the
// "defines" arms — the codemodel's compile-view defines fold onto
// the propagating `defines` attribute (matching this fold's routing;
// applyInterfaceScopeToDefines re-scopes them afterwards on the
// traced path), so a define moved into an arm must leave the flat
// local_defines too or non-matching arms would still carry it.
func dedupBaselineAgainstDeltas(tgt *ir.Target) {
	if tgt == nil || tgt.PerPlatform == nil {
		return
	}
	inDelta := func(attr string) map[string]bool {
		set := map[string]bool{}
		for _, arm := range tgt.PerPlatform[attr] {
			for _, v := range arm {
				set[v] = true
			}
		}
		return set
	}
	dedup := func(baseline []string, delta map[string]bool) []string {
		if len(delta) == 0 {
			return baseline
		}
		out := baseline[:0]
		for _, v := range baseline {
			if !delta[v] {
				out = append(out, v)
			}
		}
		return out
	}
	tgt.Copts = dedup(tgt.Copts, inDelta("copts"))
	defineArms := inDelta("defines")
	tgt.Defines = dedup(tgt.Defines, defineArms)
	tgt.LocalDefines = dedup(tgt.LocalDefines, defineArms)
	tgt.LinkOpts = dedup(tgt.LinkOpts, inDelta("linkopts"))
	tgt.Srcs = dedup(tgt.Srcs, inDelta("srcs"))
	tgt.Deps = dedup(tgt.Deps, inDelta("deps"))
}

// applyPartition writes the per-cell deltas of one fact family
// into tgt.PerPlatform[attr]. labelFor maps a partition cell name
// onto the select() arm label: the multi-config fold passes
// configLabel (cells are cmake config names → //config:<name>),
// the option fold passes identity (its cells are already full
// //options:<name>_{on,off} labels). The fact-family key (e.g.
// "C|-O2" for CompileFragments) is decomposed: for compile / link
// fragments we strip the role/language prefix before emit so the
// select() arm carries the flag itself, not the disambiguator key.
//
// Per-attribute re-anchor pass:
//
//   - linkopts: drop / re-anchor tokens that embed convert-time
//     absolute paths via reanchorLinkOptToken (same policy as the
//     single-config baseline path in lower.go's Link fragment
//     handling).
//   - defines: drop / re-anchor define values that embed convert-
//     time absolute paths via reanchorDefineValue.
//   - copts: cmake PCH machinery tokens are stripped per arm
//     (filterPCHCoptArm) — the per-config cmake_pch.hxx path is a
//     convert-time build-dir leak; the PCH forced-include lift rides
//     the baseline copts. Everything else passes through (copts
//     tokens are short flags without embedded paths after
//     splitCompileFragments).
//   - includes: paths the existing includes handler normalises.
func applyPartition(tgt *ir.Target, attr string, p configfold.Partition, cmakeSrc, cmakeBuild string, labelFor func(string) string) {
	if len(p.Deltas) == 0 {
		return
	}
	if tgt.PerPlatform == nil {
		tgt.PerPlatform = map[string]map[string][]string{}
	}
	if tgt.PerPlatform[attr] == nil {
		tgt.PerPlatform[attr] = map[string][]string{}
	}
	for cell, facts := range p.Deltas {
		if len(facts) == 0 {
			continue
		}
		label := labelFor(cell)
		values := make([]string, 0, len(facts))
		for fact := range facts {
			tok := stripFactPrefix(fact)
			switch attr {
			case "linkopts":
				// Skip "libraries"-role link fragments — those
				// are static archives / cmake import targets that
				// belong in `deps`, not linkopts. The single-
				// config baseline path at lower.go's t.Link.
				// CommandFragments loop already routes them
				// through imports.LookupLinkPath into the IR's
				// deps slice; per-config delta entries that
				// reflect different build-dir paths per config
				// would otherwise leak in as bogus
				// `linkopts = ["Debug/lib/libfoo.a", ...]` select
				// arms (LLVM-shape).
				if strings.HasPrefix(fact, "libraries|") ||
					strings.HasPrefix(fact, "libraryPath|") {
					continue
				}
				if rewritten, keep := reanchorLinkOptToken(tok, cmakeSrc, cmakeBuild); keep {
					values = append(values, rewritten)
				}
			case "defines":
				if rewritten, keep := reanchorDefineValue(tok, cmakeSrc, cmakeBuild); keep {
					values = append(values, rewritten)
				}
			case "includes":
				// Per-config include deltas can be absolute build-dir or
				// source-dir paths the single-config include handler never
				// saw (it only walks the primary config). Relativize them
				// the same way: a build-dir include (a `$<CONFIG>`-dependent
				// file(GENERATE) output dir like
				// include-config-debug/build_config) becomes build-dir-
				// relative — which is exactly where the recovered per-config
				// genrule places its output, so the select arm's `-I` finds
				// the generated header. A source-tree include becomes src-
				// relative. System / out-of-tree absolutes drop (the
				// toolchain supplies those). Without this the select kept
				// absolute throwaway-build-dir paths
				// (/tmp/convert-element-build-*/...) that resolve to nothing
				// at Bazel time (SDL's per-config SDL_build_config.h dir).
				if rel, ok := relativeIfInsideRelaxed(cmakeBuild, tok); ok {
					values = append(values, rel)
				} else if rel, ok := relativeIfInside(cmakeSrc, tok); ok {
					values = append(values, rel)
				} else if !filepath.IsAbs(tok) {
					values = append(values, tok)
				}
			default:
				values = append(values, tok)
			}
		}
		if attr == "copts" {
			// Strip cmake PCH machinery from per-config arms: the per-config
			// cmake_pch path (`CMakeFiles/<t>.dir/<Config>/cmake_pch.hxx`)
			// differs per cell, so it lands here as a raw convert-time
			// build-dir path token. The forced-include semantics ride the
			// baseline copts via the pchForcedIncludeCopts lift, so the arm
			// token is pure leakage. See filterPCHCoptArm (pch.go).
			values = filterPCHCoptArm(values)
		}
		sort.Strings(values)
		// Merge: a target already populated with per-platform
		// deltas keeps those; per-config deltas append. The
		// emitter handles the combined map as a single select()
		// with arms from both axes.
		tgt.PerPlatform[attr][label] = append(tgt.PerPlatform[attr][label], values...)
	}
}

// configOnlyTargetNames returns the names of targets that appear in some
// non-primary configuration (Configurations[1:]) but not in the primary
// one (Configurations[0]). lowerMultiConfigDeltas only augments targets
// the primary config's walk emitted, so a config-only target is the
// genuine residual of the first-config-primary fold — it's silently
// dropped. Names are de-duplicated and sorted for deterministic output.
// Returns nil for single-config (or empty) codemodels.
func configOnlyTargetNames(configs []fileapi.Configuration) []string {
	if len(configs) < 2 {
		return nil
	}
	primary := map[string]bool{}
	for _, tr := range configs[0].Targets {
		primary[tr.Id] = true
	}
	seen := map[string]bool{}
	var names []string
	for _, cfg := range configs[1:] {
		for _, tr := range cfg.Targets {
			if primary[tr.Id] || seen[tr.Id] {
				continue
			}
			seen[tr.Id] = true
			names = append(names, tr.Name)
		}
	}
	sort.Strings(names)
	return names
}

// configLabel maps a cmake config name into the Bazel
// config_setting label the convention surfaces:
// `//config:<name-lowercased>`. The backing config_settings are
// emitted by convert-element-cmake --out-config-settings (the
// emit/configsettings package); the
// TestConfigLabel_MatchesConfigSettingsEmit parity test pins that
// the two sides agree on naming.
func configLabel(cellName string) string {
	// Lowercased so the same config (Release, RELEASE, release) maps
	// to a single label regardless of cmake's surface case.
	return "//config:" + toLower(cellName)
}

// toLower is a fast lowercase for ASCII config names.
// Avoids strings.ToLower's table lookups for the typical short
// names cmake configs use (Debug, Release, RelWithDebInfo, etc.).
func toLower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// nonFeatureConfigNames filters out config names recognized by
// configfold.SanitizerFeature — those route through --features
// rather than per-config selects.
func nonFeatureConfigNames(configs []string) []string {
	var out []string
	for _, c := range configs {
		if _, ok := configfold.SanitizerFeature(c); ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

// stripFactPrefix removes the role/language disambiguator
// configfold prepends to compile-fragment ("C|-O2") and link-
// fragment ("libraries|-lz") keys, returning just the flag/path
// payload.
func stripFactPrefix(fact string) string {
	for i := 0; i < len(fact); i++ {
		if fact[i] == '|' {
			return fact[i+1:]
		}
	}
	return fact
}

// relabelDependencyPartition rewrites a partition keyed on cmake
// target IDs into one keyed on Bazel labels (":<name>"). IDs
// without an entry in idToName are dropped — these are typically
// IMPORTED targets the codemodel saw but that the lower path
// didn't synthesize an IR target for (their proper Bazel
// resolution rides through the imports.Manifest path, not the
// id→label codemodel translation).
func relabelDependencyPartition(p configfold.Partition, idToName map[string]string) configfold.Partition {
	out := configfold.Partition{
		Baseline: map[string]bool{},
		Deltas:   map[string]map[string]bool{},
	}
	for id := range p.Baseline {
		if name, ok := idToName[id]; ok && name != "" {
			out.Baseline[":"+name] = true
		}
	}
	for cell, ids := range p.Deltas {
		for id := range ids {
			if name, ok := idToName[id]; ok && name != "" {
				if out.Deltas[cell] == nil {
					out.Deltas[cell] = map[string]bool{}
				}
				out.Deltas[cell][":"+name] = true
			}
		}
	}
	return out
}
