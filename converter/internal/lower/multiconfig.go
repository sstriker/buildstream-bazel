package lower

import (
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
func lowerMultiConfigDeltas(pkg *ir.Package, byConfig map[string]map[string]fileapi.Target, configNames []string, cmakeSrc, cmakeBuild string) {
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

	// Index pkg.Targets by name for fast match.
	byName := map[string]*ir.Target{}
	for i := range pkg.Targets {
		byName[pkg.Targets[i].Name] = &pkg.Targets[i]
	}

	for _, fold := range folds {
		tgt, ok := byName[fold.Name]
		if !ok {
			continue
		}
		applyPartition(tgt, "defines", fold.Defines, cmakeSrc, cmakeBuild)
		applyPartition(tgt, "copts", fold.CompileFragments, cmakeSrc, cmakeBuild)
		applyPartition(tgt, "linkopts", fold.LinkFragments, cmakeSrc, cmakeBuild)
		// Includes are routed to the "includes" Bazel attribute
		// rather than copts -I, matching the rest of the lift's
		// includes handling.
		applyPartition(tgt, "includes", fold.Includes, cmakeSrc, cmakeBuild)
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

// dedupBaselineAgainstDeltas removes entries from tgt.Copts /
// tgt.Defines / tgt.LinkOpts whose value appears in any of the
// per-config delta arms in tgt.PerPlatform for the same attr.
// The select() arm is the authoritative source for cross-config-
// varying values; the flat baseline ends up reflecting only
// truly-baseline values (those common to every config).
func dedupBaselineAgainstDeltas(tgt *ir.Target) {
	if tgt == nil || tgt.PerPlatform == nil {
		return
	}
	dedup := func(attr string, baseline []string) []string {
		deltas, ok := tgt.PerPlatform[attr]
		if !ok || len(deltas) == 0 {
			return baseline
		}
		inDelta := map[string]bool{}
		for _, arm := range deltas {
			for _, v := range arm {
				inDelta[v] = true
			}
		}
		out := baseline[:0]
		for _, v := range baseline {
			if !inDelta[v] {
				out = append(out, v)
			}
		}
		return out
	}
	tgt.Copts = dedup("copts", tgt.Copts)
	tgt.Defines = dedup("defines", tgt.Defines)
	tgt.LinkOpts = dedup("linkopts", tgt.LinkOpts)
}

// applyPartition writes the per-cell deltas of one fact family
// into tgt.PerPlatform[attr]. The fact-family key (e.g. "C|-O2"
// for CompileFragments) is decomposed: for compile / link
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
//   - copts / includes: unchanged. copts tokens are short flags
//     without embedded paths after splitCompileFragments;
//     includes are paths the existing includes handler normalises.
func applyPartition(tgt *ir.Target, attr string, p configfold.Partition, cmakeSrc, cmakeBuild string) {
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
		label := configLabel(cell)
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
			default:
				values = append(values, tok)
			}
		}
		sort.Strings(values)
		// Merge: a target already populated with per-platform
		// deltas keeps those; per-config deltas append. The
		// emitter handles the combined map as a single select()
		// with arms from both axes.
		tgt.PerPlatform[attr][label] = append(tgt.PerPlatform[attr][label], values...)
	}
}

// configLabel maps a cmake config name into the Bazel
// config_setting label the convention surfaces. The actual
// config_setting must be declared by the operator (or a future
// converter slice that emits them automatically); the label is
// the agreed-upon contract: `//config:<name-lowercased>`.
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
