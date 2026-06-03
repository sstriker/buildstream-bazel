package toolchain

// VariantMatrix composes two variant axes into the full probe matrix:
// the compiler axis (kits, from cmake-kits.json) and the build axis
// (variants, typically build types / sanitizers from CMakePresets.json
// or DefaultVariants). It returns the cross-product — one cell per
// (kit, variant) pair — with each cell's CacheVars merged and its Kit
// stamped so unify-toolchains can group cells back into one toolchain
// per (platform, kit).
//
// Degenerate axes pass through so callers can wire kits in
// unconditionally:
//
//   - No kits: the build variants are returned unchanged (Kit stays
//     empty). This is the pre-kits behavior — one toolchain per
//     platform — so wiring VariantMatrix into a kit-less pipeline is a
//     no-op.
//   - No build variants: each kit becomes its own cell. (render's
//     callers require at least one build variant today, but the
//     library stays total.)
//
// Merge precedence on a CacheVars key collision: the KIT wins. A kit's
// reason for existing is to pin the compiler (CMAKE_C_COMPILER /
// CMAKE_CXX_COMPILER / CMAKE_TOOLCHAIN_FILE), so when a build variant
// also sets one of those, the operator's explicit kit choice is
// authoritative. Build variants own the orthogonal axis
// (CMAKE_BUILD_TYPE, sanitizer flags), which kits don't normally touch,
// so collisions are rare in practice.
//
// Cell naming: "<kit>-<variant>" (both already label-safe). The Kit
// field carries the grouping key separately, so unify never has to
// parse it back out of the name.
func VariantMatrix(kits, variants []Variant) []Variant {
	if len(kits) == 0 {
		return variants
	}
	if len(variants) == 0 {
		out := make([]Variant, 0, len(kits))
		for _, k := range kits {
			out = append(out, Variant{
				Name:      k.Name,
				Kit:       k.Name,
				CacheVars: cloneCacheVars(k.CacheVars),
			})
		}
		return out
	}
	out := make([]Variant, 0, len(kits)*len(variants))
	for _, k := range kits {
		for _, v := range variants {
			out = append(out, Variant{
				Name:      k.Name + "-" + v.Name,
				Kit:       k.Name,
				CacheVars: mergeCacheVars(v.CacheVars, k.CacheVars),
			})
		}
	}
	return out
}

// mergeCacheVars returns base overlaid by over (over wins on key
// collision). Returns nil when the result is empty so an all-empty
// merge matches the baseline nil-map convention.
func mergeCacheVars(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// cloneCacheVars copies m so cells never alias a source variant's map.
func cloneCacheVars(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
