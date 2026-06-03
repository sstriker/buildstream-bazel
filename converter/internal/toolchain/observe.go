// Package toolchain's observer: take N ProbeResults and partition
// them into a baseline (every fact identical across all probes)
// plus per-variant deltas (everything else), purely from
// observation. No name-pattern matching ("CMAKE_<LANG>_FLAGS_DEBUG
// is a build-type flag") is hardcoded; we derive classifications
// from the data the probes produce.
//
// This makes the toolchain pipeline robust to:
//
//   - Custom build types not in CMake's standard quartet.
//   - Sanitizer / coverage / LTO flags injected into base
//     CMAKE_<LANG>_FLAGS rather than the build-type slot.
//   - Project-side toolchain files that override unexpected
//     cache vars.
//   - Future cmake versions that rename or split variables.
//
// The Bazel-side mapping (which delta routes to dbg / opt / a
// custom feature) is a separate concern handled at emit time via a
// VariantToBazel function — see emit/bazeltoolchain.

package toolchain

import "github.com/sstriker/buildstream-bazel/internal/empfold"

// ResolvedToolchain is the empirical fold of N ProbeResults. The
// baseline slot holds everything observed identically across every
// variant; the per-variant slots hold only that variant's deltas.
//
// One ResolvedToolchain corresponds to one (host, target,
// compiler) cell. Cross-target / cross-compiler combinations
// produce multiple ResolvedToolchains, one per cell — the cells
// don't share a baseline because their compilers / system roots
// differ.
type ResolvedToolchain struct {
	// Base is the always-on layer. Languages, tools, and platform
	// are populated from the values that were identical across
	// every probe variant. Per-variant cache entries (anything
	// that differed) are zeroed out — they appear in Variants
	// instead.
	Base *Model

	// Variants keyed by Variant.Name. Each entry holds only the
	// per-variant delta (the cache entries / flag sets where this
	// variant differed from the consensus). A variant whose
	// CacheVars produced exactly the baseline (e.g. an empty
	// "baseline" variant whose values match every other probe's
	// shared subset) gets an empty delta; we keep the entry so
	// the emitter knows the variant was observed.
	Variants map[string]*VariantDelta
}

// VariantDelta is the per-variant changeset relative to Base.
type VariantDelta struct {
	// Spec is the original Variant input (Name + CacheVars).
	// Retained so the emit layer can map variant identity to
	// Bazel features without re-deriving it from the delta.
	Spec Variant

	// LanguageFlags keyed by CMake language ("C", "CXX"). Each
	// value is the per-variant compile-flag delta — flags this
	// variant produced that the baseline didn't see.
	LanguageFlags map[string][]string

	// LinkFlags is the per-variant linker-flag delta merged from
	// CMAKE_EXE_LINKER_FLAGS_<x> + CMAKE_SHARED_LINKER_FLAGS_<x>
	// (when the variant sets a build type) or other
	// cache-var-driven link-flag changes.
	LinkFlags []string

	// CacheVarOverrides records cache entries whose values
	// differed in this variant. Useful for diagnostics ("this
	// variant changed CMAKE_C_COMPILER from X to Y") and for
	// feature classification at emit time.
	CacheVarOverrides map[string]string
}

// Observe folds probe results into a ResolvedToolchain. Returns
// nil for an empty input. With a single result Observe still
// returns a usable ResolvedToolchain — Base mirrors that result,
// the lone variant's delta is empty.
//
// The cache-var partition is delegated to empfold.Partition: it
// splits per-variant cache observations into a baseline (every
// variant agreed) and per-variant deltas. Observe then layers
// the toolchain-specific shape (per-language flag deltas, the
// ResolvedToolchain wrapper) on top of that primitive.
func Observe(results []ProbeResult) *ResolvedToolchain {
	if len(results) == 0 {
		return nil
	}

	// Lift each cache entry to facts[name][variantName] = value
	// for empfold.Partition. Variants without a Reply contribute
	// no facts but still appear in the cells slice so missing-in-
	// some-variants is detected correctly.
	cells := make([]string, 0, len(results))
	cacheValues := map[string]map[string]string{}
	for _, r := range results {
		cells = append(cells, r.Variant.Name)
		if r.Reply == nil {
			continue
		}
		for _, e := range r.Reply.Cache.Entries {
			if cacheValues[e.Name] == nil {
				cacheValues[e.Name] = map[string]string{}
			}
			cacheValues[e.Name][r.Variant.Name] = e.Value
		}
	}
	baselineCache, perVariantOverrides := empfold.Partition(cells, cacheValues)

	// Build Base from the first result's Model with cache-driven
	// fields trimmed to the baseline subset.
	primary := results[0].Model
	base := cloneAsBaseline(primary)
	rebaseLanguageFlags(base, baselineCache)

	rt := &ResolvedToolchain{
		Base:     base,
		Variants: map[string]*VariantDelta{},
	}

	for _, r := range results {
		delta := &VariantDelta{
			Spec:              r.Variant,
			LanguageFlags:     map[string][]string{},
			CacheVarOverrides: perVariantOverrides[r.Variant.Name],
		}
		// Per-language flag delta: take this variant's
		// (BaseFlags + BuildTypeFlags) for each language and
		// subtract the baseline's BaseFlags. Anything left is
		// what this variant adds.
		baseLangFlags := languageFlagSets(base)
		for lang, l := range r.Model.Languages {
			combined := append([]string(nil), l.BaseFlags...)
			combined = append(combined, l.BuildTypeFlags...)
			contribution := stripDuplicates(combined, baseLangFlags[lang])
			if len(contribution) > 0 {
				delta.LanguageFlags[lang] = contribution
			}
			if len(l.LinkBuildTypeFlags) > 0 {
				delta.LinkFlags = append(delta.LinkFlags, l.LinkBuildTypeFlags...)
			}
		}
		rt.Variants[r.Variant.Name] = delta
	}
	return rt
}

// rebaseLanguageFlags trims each language's BaseFlags slice to only
// the tokens that were observed identically in every variant. This
// is a flag-level analogue of the cache-var partitioning above.
//
// Since Model.Languages already reflects ONE variant's data (the
// first probe's), we don't need to recompute the universe — we
// just need to drop tokens that other variants disagreed about.
// In practice CMAKE_<LANG>_FLAGS is the same across build-type
// variants, so this is usually a no-op; it matters for
// compiler-axis variants where flags can shift.
func rebaseLanguageFlags(base *Model, baselineCache map[string]string) {
	for lang, l := range base.Languages {
		// CMAKE_<LANG>_FLAGS is the source of truth for BaseFlags.
		// If every variant produced the same value, the cache
		// partitioning above already kept it as baseline.
		// Otherwise tokens that are unique to one variant should
		// not leak into base.
		baseToken := baselineCache["CMAKE_"+lang+"_FLAGS"]
		if baseToken == "" {
			// Either the cache var truly was empty or it differed
			// across variants. In either case base shouldn't
			// claim flags that aren't observed everywhere; trim.
			l.BaseFlags = nil
		} else {
			l.BaseFlags = tokenizeString(baseToken)
		}
		l.BuildTypeFlags = nil
		l.LinkBuildTypeFlags = nil
		base.Languages[lang] = l
	}
}

func tokenizeString(s string) []string {
	// Mirrors tokenizeCacheFlags but operates on a string we
	// already have in hand.
	if s == "" {
		return nil
	}
	var out []string
	field := []byte{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if len(field) > 0 {
				out = append(out, string(field))
				field = field[:0]
			}
			continue
		}
		field = append(field, c)
	}
	if len(field) > 0 {
		out = append(out, string(field))
	}
	return out
}

// languageFlagSets returns a map keyed by language with the set of
// flags currently in Base. Used to compute per-variant deltas as
// "this variant produced - what's already in baseline".
func languageFlagSets(base *Model) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(base.Languages))
	for lang, l := range base.Languages {
		set := make(map[string]bool, len(l.BaseFlags))
		for _, f := range l.BaseFlags {
			set[f] = true
		}
		out[lang] = set
	}
	return out
}

// cloneAsBaseline creates a Model with its build-type-specific
// fields zeroed out. The remaining fields (compiler info, builtin
// includes, base flags) are populated by the caller from the
// observed-as-common cache subset.
func cloneAsBaseline(src *Model) *Model {
	if src == nil {
		return nil
	}
	out := &Model{
		HostPlatform:   src.HostPlatform,
		TargetPlatform: src.TargetPlatform,
		BuildType:      "",
		Sysroot:        src.Sysroot,
		Tools:          src.Tools,
		Languages:      map[string]Language{},
	}
	for k, l := range src.Languages {
		copy := l
		copy.BuildTypeFlags = nil
		copy.LinkBuildTypeFlags = nil
		copy.BuiltinIncludeDirs = append([]string(nil), l.BuiltinIncludeDirs...)
		copy.BuiltinLinkDirs = append([]string(nil), l.BuiltinLinkDirs...)
		copy.BaseFlags = append([]string(nil), l.BaseFlags...)
		copy.LinkFlags = append([]string(nil), l.LinkFlags...)
		copy.SourceFileExtensions = append([]string(nil), l.SourceFileExtensions...)
		out.Languages[k] = copy
	}
	return out
}
