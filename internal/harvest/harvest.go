// Package harvest produces an imports manifest from an install-shaped
// prefix tree — a bst artifact checkout, a host-install dir, any
// CMAKE_PREFIX_PATH-shaped root. It parses what cmake itself trusts
// when consuming the prefix:
//
//   - lib/cmake/<Pkg>/*Targets*.cmake — cmake's serialized export
//     graph: IMPORTED targets (libraries AND executables, plus
//     aliases), IMPORTED_LOCATION_<CONFIG> (→ anchored link_paths),
//     INTERFACE_INCLUDE_DIRECTORIES, and INTERFACE_LINK_LIBRARIES —
//     the DIRECT dependency edges;
//   - lib/pkgconfig/*.pc + share/pkgconfig/*.pc — Libs/Cflags/Requires
//     for libraries that ship no cmake bundle;
//   - bin/* — executables no bundle exports, so genrule tool lifts
//     still resolve them by path.
//
// Deps stay DIRECT, never flattened: the harvested manifest is
// wrapper-generator INPUT (internal/wrappergen), whose synthesized
// cc_library wrappers give Bazel transitivity the closure. Labels are
// synthesized against the wrapper package up front using the
// generator's own naming (wrappergen.WrapperName), so generation is
// label-idempotent: the rewritten manifest's labels equal the
// harvested ones, with Deps cleared per the Export.Deps invariant.
package harvest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/wrappergen"
)

// builtinPseudoTargets maps cmake's own imported pseudo-targets — names
// that resolve to no prefix file — onto link-library names. Unknown
// `NS::x` references that match no harvested target warn and drop.
var builtinPseudoTargets = map[string]string{
	"Threads::Threads": "pthread",
}

// Harvest walks prefixDir and returns the manifest (one element named
// element, labels under "//"+labelPkg+":") plus human-readable
// warnings for everything conservatively skipped.
func Harvest(prefixDir, element, labelPkg string) (*manifest.Imports, []string, error) {
	return HarvestWithRegistry(prefixDir, element, labelPkg, nil)
}

// HarvestWithRegistry is Harvest plus a cross-element LABEL REGISTRY: a map from
// a foreign cmake target name (e.g. "OtherPkg::foo") to its absolute Bazel label.
// A single prefix's harvest can only resolve INTERFACE_LINK_LIBRARIES / .pc
// Requires refs to targets IN THAT PREFIX (h.byName); a ref to a target another
// element exports isn't there, so resolveDeps would warn-drop it — silently
// losing the link edge (a static consumer's undefined symbol is legal in the .a
// and only surfaces at the far-downstream executable link). The registry — built
// by the orchestrator from the OTHER elements' exports.json manifests — is
// consulted before that drop, so a cross-element dep resolves to the real
// sibling label instead of vanishing. Nil registry ⇒ Harvest's historical
// single-prefix behavior.
func HarvestWithRegistry(prefixDir, element, labelPkg string, registry map[string]string) (*manifest.Imports, []string, error) {
	st, err := os.Stat(prefixDir)
	if err != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("prefix %s is not a directory", prefixDir)
	}
	h := &harvester{
		prefix:     prefixDir,
		realPrefix: prefixDir,
		labelPkg:   labelPkg,
		registry:   registry,
		byName:     map[string]*row{},
		byPath:     map[string]*row{},
	}
	if rp, err := filepath.EvalSymlinks(prefixDir); err == nil {
		h.realPrefix = rp
	}
	if err := h.parseBundles(); err != nil {
		return nil, nil, err
	}
	h.dedupBundleRows()
	if err := h.parsePkgConfig(); err != nil {
		return nil, nil, err
	}
	h.collectBareBinaries(element)
	h.resolveDeps()
	h.breakDepCycles()
	return h.manifest(element), h.warnings, nil
}

// row is one export under construction.
type row struct {
	cmakeTarget string
	includes    []string
	linkPaths   []string // anchored
	linkLibs    []string
	depRefs     []string // cmake `NS::x` or pc names, resolved late
	deps        []string // resolved labels
	aliasOf     string   // alias rows point at their underlying target
	origin      string   // provenance for diagnostics ("bundle", "pkgconfig <name>", "bin")
	kind        string   // "" (library) or manifest.KindExecutable
}

// mergeInto folds a same-library row harvested from another channel
// into its claimant: the union of both channels' keys survives on ONE
// row (the dropped channel's -l name keeps the LookupLinkLibrary
// redirect alive; its Requires deps keep resolving), and the merged
// row's name registers as an ALIAS in byName so dep references to it
// resolve to the claimant.
func (h *harvester) mergeInto(claimant, dup *row) {
	for _, l := range dup.linkLibs {
		claimant.linkLibs = appendUnique(claimant.linkLibs, l)
	}
	for _, inc := range dup.includes {
		claimant.includes = appendUnique(claimant.includes, inc)
	}
	for _, lp := range dup.linkPaths {
		claimant.linkPaths = appendUnique(claimant.linkPaths, lp)
		if key := h.canonicalKey(lp); h.byPath[key] == nil {
			h.byPath[key] = claimant
		}
	}
	claimant.depRefs = append(claimant.depRefs, dup.depRefs...)
	h.byName[dup.cmakeTarget] = claimant
}

// dedupBundleRows folds bundle rows that describe ONE library under
// two exported names — the old-style + namespaced export pair (cmake's
// `install(EXPORT)` with both a NAMESPACE and bare OLD_STYLE names, or
// a bundle shipping `foo` alongside `foo::foo`). Without the fold the
// pair survives as two rows whose wrapper names collide at generation
// time instead of resolving as one library with two names.
//
// Two identity signals, same vocabulary as sameLibraryClaimant:
//
//   - shared artifact: a row whose IMPORTED_LOCATION canonicalizes to
//     a path another row already claims is the same library, whatever
//     the names say;
//   - the bare/namespaced name pair (`foo` + `ns::foo`) when artifact
//     evidence doesn't CONTRADICT — both sides carrying distinct
//     artifacts means two real libraries, left to the collision
//     warning.
//
// The duplicate survives as an ALIAS row (both names stay visible in
// the manifest, one label), with its keys folded into the claimant.
func (h *harvester) dedupBundleRows() {
	for _, r := range h.rows {
		if r.aliasOf != "" {
			continue
		}
		var owner *row
		for _, lp := range r.linkPaths {
			if prev := h.claimantOf(h.byPath[h.canonicalKey(lp)]); prev != nil && prev != r {
				owner = prev
				break
			}
		}
		if owner == nil {
			continue
		}
		h.warnf("%s: same artifact as %s (%s); names folded onto one export", r.cmakeTarget, owner.cmakeTarget, owner.origin)
		h.foldDuplicate(owner, r)
	}
	for _, r := range h.rows {
		if r.aliasOf != "" || strings.Contains(r.cmakeTarget, "::") {
			continue
		}
		for _, prev := range h.rows {
			if prev == r || prev.aliasOf != "" || !strings.HasSuffix(prev.cmakeTarget, "::"+r.cmakeTarget) {
				continue
			}
			if prev.kind != r.kind {
				// A bare library and a namespaced executable (or vice
				// versa) sharing a stem are not the same export.
				continue
			}
			if len(prev.linkPaths) > 0 && len(r.linkPaths) > 0 {
				// Both carry artifacts: the path pass above already
				// folded the same-artifact case, so these are two real
				// libraries — leave them to the collision warning.
				continue
			}
			claimant, dup := prev, r
			if len(r.linkPaths) > 0 {
				claimant, dup = r, prev
			}
			h.warnf("%s: old-style name for %s; names folded onto one export", dup.cmakeTarget, claimant.cmakeTarget)
			h.foldDuplicate(claimant, dup)
			break
		}
	}
}

// foldDuplicate merges dup's keys into claimant and demotes dup to an
// alias row: its export survives (the duplicate NAME stays resolvable,
// pointing at the claimant's label) but carries no keys of its own.
func (h *harvester) foldDuplicate(claimant, dup *row) {
	h.mergeInto(claimant, dup)
	dup.aliasOf = claimant.cmakeTarget
	dup.includes, dup.linkLibs, dup.linkPaths = nil, nil, nil
	dup.depRefs, dup.deps = nil, nil
}

// claimantOf chases an alias row to its underlying claimant — byPath
// entries registered before a fold can still point at the demoted row.
// nil passes through; an unresolvable alias name returns the row as-is.
func (h *harvester) claimantOf(r *row) *row {
	if r == nil || r.aliasOf == "" {
		return r
	}
	if u, ok := h.byName[r.aliasOf]; ok {
		return u
	}
	return r
}

type harvester struct {
	prefix     string
	realPrefix string // EvalSymlinks(prefix) — base for canonicalized anchoring
	labelPkg   string
	registry   map[string]string // foreign cmake target → absolute Bazel label (cross-element deps)
	rows       []*row
	byName     map[string]*row // cmake target / pc name → row
	byPath     map[string]*row // canonicalKey(anchored path) → row (dedup pc-vs-bundle)
	warnings   []string
}

func (h *harvester) warnf(format string, args ...any) {
	h.warnings = append(h.warnings, fmt.Sprintf(format, args...))
}

func (h *harvester) label(cmakeTarget string) string {
	return "//" + h.labelPkg + ":" + wrappergen.WrapperName(cmakeTarget)
}

// sameLibraryClaimant returns the existing row that already describes
// r's library, through either identity signal: a shared artifact path
// (byPath) or a shared consumer-facing wrapper name — the same
// collision imports-wrapper-gen would reject, here recognized EARLY
// as "one library, two channels" when exactly one side carries an
// artifact-less description (the header-only / unresolvable-probe
// shapes that path identity can't see).
func (h *harvester) sameLibraryClaimant(r *row) *row {
	for _, lp := range r.linkPaths {
		if prev, ok := h.byPath[h.canonicalKey(lp)]; ok {
			return prev
		}
	}
	want := wrappergen.WrapperName(r.cmakeTarget)
	for _, prev := range h.rows {
		if prev.aliasOf == "" && wrappergen.WrapperName(prev.cmakeTarget) == want {
			// Same wrapper name across channels: only treat as the
			// same library when the artifact evidence doesn't
			// CONTRADICT (both carrying distinct artifacts means two
			// real libraries that happen to collide — surfaced by the
			// collision check instead).
			if len(prev.linkPaths) == 0 || len(r.linkPaths) == 0 {
				return prev
			}
		}
	}
	return nil
}

func (h *harvester) addRow(r *row) *row {
	if prev, ok := h.byName[r.cmakeTarget]; ok {
		return prev
	}
	h.rows = append(h.rows, r)
	h.byName[r.cmakeTarget] = r
	for _, lp := range r.linkPaths {
		if key := h.canonicalKey(lp); h.byPath[key] == nil {
			h.byPath[key] = r
		}
	}
	return r
}

// resolveDeps maps each row's collected references onto labels:
// harvested targets → their synthesized labels, builtin pseudo-targets
// → link_libraries, everything else warns and drops (conservative —
// a dropped dep surfaces at the consumer's link, not as a silently
// wrong edge).
func (h *harvester) resolveDeps() {
	for _, r := range h.rows {
		seen := map[string]bool{}
		for _, ref := range r.depRefs {
			if target, ok := h.byName[ref]; ok {
				name := target.cmakeTarget
				if target.aliasOf != "" {
					name = target.aliasOf
				}
				l := h.label(name)
				if l != h.label(r.cmakeTarget) && !seen[l] {
					seen[l] = true
					r.deps = append(r.deps, l)
				}
				continue
			}
			if lib, ok := builtinPseudoTargets[ref]; ok {
				r.linkLibs = appendUnique(r.linkLibs, lib)
				continue
			}
			// Cross-element label registry: a ref this prefix didn't harvest
			// may be a target ANOTHER element exports. Resolve it to the
			// registered sibling label instead of dropping the edge — the fix
			// for cross-element link deps silently vanishing (a static
			// consumer's undefined symbol only surfaces at the downstream exe
			// link). Guard against self- and dup-edges like the byName arm.
			if l := h.registry[ref]; l != "" {
				if l != h.label(r.cmakeTarget) && !seen[l] {
					seen[l] = true
					r.deps = append(r.deps, l)
				}
				continue
			}
			h.warnf("%s: dep %q resolves to no harvested target or registry entry; dropped", r.cmakeTarget, ref)
		}
		sort.Strings(r.deps)
	}
}

// breakDepCycles drops dependency edges that would close a cycle.
// Circular references survive harvesting otherwise (mutual .pc
// Requires, an INTERFACE_LINK_LIBRARIES loop), and Bazel rejects
// cyclic deps outright once wrappergen materializes Deps as real
// cc_library edges — a load-time failure far from the cause. The DFS
// walks rows in insertion order (file-walk order, deterministic) over
// already-sorted dep lists, so WHICH edge breaks is stable across
// runs; each break warns with the full cycle path so the operator can
// fuse or restructure the wrappers instead of relying on the
// arbitrary-but-stable choice.
func (h *harvester) breakDepCycles() {
	byLabel := map[string]*row{}
	for _, r := range h.rows {
		if r.aliasOf == "" {
			byLabel[h.label(r.cmakeTarget)] = r
		}
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	state := map[*row]int{}
	var stack []string
	var visit func(r *row)
	visit = func(r *row) {
		state[r] = gray
		stack = append(stack, r.cmakeTarget)
		kept := r.deps[:0]
		for _, d := range r.deps {
			target := byLabel[d]
			if target != nil && state[target] == gray {
				cyc := stack
				for i, n := range stack {
					if n == target.cmakeTarget {
						cyc = stack[i:]
						break
					}
				}
				h.warnf("%s: dep %s closes a dependency cycle (%s); edge dropped — Bazel rejects cyclic deps, fuse or restructure these exports",
					r.cmakeTarget, d, strings.Join(append(append([]string{}, cyc...), target.cmakeTarget), " -> "))
				continue
			}
			if target != nil && state[target] == white {
				visit(target)
			}
			kept = append(kept, d)
		}
		r.deps = kept
		stack = stack[:len(stack)-1]
		state[r] = black
	}
	for _, r := range h.rows {
		if r.aliasOf == "" && state[r] == white {
			visit(r)
		}
	}
}

func (h *harvester) manifest(element string) *manifest.Imports {
	// Consumer-facing wrapper names must be unique for the generator;
	// genuinely distinct targets that collide post-sanitization are
	// surfaced HERE with channel provenance — a far earlier and richer
	// diagnostic than the generator's late name-collision error.
	byWrapper := map[string]*row{}
	for _, r := range h.rows {
		if r.aliasOf != "" {
			continue
		}
		name := wrappergen.WrapperName(r.cmakeTarget)
		if prev, dup := byWrapper[name]; dup {
			h.warnf("wrapper name %q collides: %s (%s) vs %s (%s) — imports-wrapper-gen will refuse; disambiguate before generating",
				name, prev.cmakeTarget, prev.origin, r.cmakeTarget, r.origin)
			continue
		}
		byWrapper[name] = r
	}
	rows := append([]*row(nil), h.rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].cmakeTarget < rows[j].cmakeTarget })
	exports := make([]*manifest.Export, 0, len(rows))
	for _, r := range rows {
		underlying := r.cmakeTarget
		kind := r.kind
		if r.aliasOf != "" {
			underlying = r.aliasOf
			// Alias rows mirror the underlying's kind so a consumer
			// resolving the alias name still learns the export shape.
			if u, ok := h.byName[r.aliasOf]; ok {
				kind = u.kind
			}
		}
		exports = append(exports, &manifest.Export{
			CMakeTarget:       r.cmakeTarget,
			BazelLabel:        h.label(underlying),
			Kind:              kind,
			InterfaceIncludes: r.includes,
			LinkLibraries:     r.linkLibs,
			LinkPaths:         r.linkPaths,
			Deps:              r.deps,
		})
	}
	return &manifest.Imports{
		Version:  1,
		Elements: []*manifest.Element{{Name: element, Exports: exports}},
	}
}

// collectBareBinaries adds rows for bin/ executables no bundle
// exported: the genrule tool lift keys on link_paths, so a synthesized
// name suffices ("<element>::bin/<base>" — self-describing, scoped).
func (h *harvester) collectBareBinaries(element string) {
	binDir := filepath.Join(h.prefix, "bin")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err != nil || info.Mode()&0o111 == 0 {
			continue
		}
		anchored := manifest.PrefixAnchor + "bin/" + e.Name()
		if h.byPath[h.canonicalKey(anchored)] != nil {
			continue
		}
		h.addRow(&row{
			cmakeTarget: element + "::bin/" + e.Name(),
			linkPaths:   []string{anchored},
			origin:      "bin",
			kind:        manifest.KindExecutable,
		})
	}
}

// anchoredFromImportPrefix maps a `${_IMPORT_PREFIX}/<rel>` value (or
// an absolute path under the harvested prefix) onto the manifest's
// anchored form; ("", false) for anything else. The OBSERVED spelling
// is kept — consumer-side LookupLinkPath matches trace spellings
// verbatim, so the manifest must carry whatever the channel saw;
// same-library identity canonicalizes separately via canonicalKey.
func (h *harvester) anchoredFromImportPrefix(v string) (string, bool) {
	if rel, ok := strings.CutPrefix(v, "${_IMPORT_PREFIX}/"); ok {
		return manifest.PrefixAnchor + rel, true
	}
	if filepath.IsAbs(v) {
		if rel, err := filepath.Rel(h.prefix, v); err == nil && !strings.HasPrefix(rel, "..") {
			return manifest.PrefixAnchor + filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// canonicalKey resolves an anchored path's symlinks against the
// harvested tree, returning the canonical anchored spelling for byPath
// identity. A symlinked soname otherwise gives the same file two keys
// (the libfoo.so dev link a .pc probe finds vs the libfoo.so.1.2.3
// realpath a bundle's IMPORTED_LOCATION carries) — path identity
// misses, and since both rows then "carry artifacts", the name-match
// guard reads a true duplicate as a genuine collision. Paths that
// don't resolve on disk (partial trees) or escape the prefix keep
// their literal spelling as the key.
func (h *harvester) canonicalKey(anchored string) string {
	rel := strings.TrimPrefix(anchored, manifest.PrefixAnchor)
	resolved, err := filepath.EvalSymlinks(filepath.Join(h.prefix, filepath.FromSlash(rel)))
	if err != nil {
		return anchored
	}
	if rrel, err := filepath.Rel(h.realPrefix, resolved); err == nil && !strings.HasPrefix(rrel, "..") {
		return manifest.PrefixAnchor + filepath.ToSlash(rrel)
	}
	return anchored
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// walkFiles lists regular files under root matching match, sorted.
func walkFiles(root string, match func(string) bool) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if match(p) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
