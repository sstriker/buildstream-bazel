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
	"path"
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
	st, err := os.Stat(prefixDir)
	if err != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("prefix %s is not a directory", prefixDir)
	}
	h := &harvester{
		prefix:   prefixDir,
		labelPkg: labelPkg,
		byName:   map[string]*row{},
		byPath:   map[string]*row{},
	}
	if err := h.parseBundles(); err != nil {
		return nil, nil, err
	}
	if err := h.parsePkgConfig(); err != nil {
		return nil, nil, err
	}
	h.collectBareBinaries(element)
	h.resolveDeps()
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
}

type harvester struct {
	prefix   string
	labelPkg string
	rows     []*row
	byName   map[string]*row // cmake target / pc name → row
	byPath   map[string]*row // anchored artifact path → row (dedup pc-vs-bundle)
	warnings []string
}

func (h *harvester) warnf(format string, args ...any) {
	h.warnings = append(h.warnings, fmt.Sprintf(format, args...))
}

func (h *harvester) label(cmakeTarget string) string {
	return "//" + h.labelPkg + ":" + wrappergen.WrapperName(cmakeTarget)
}

func (h *harvester) addRow(r *row) *row {
	if prev, ok := h.byName[r.cmakeTarget]; ok {
		return prev
	}
	h.rows = append(h.rows, r)
	h.byName[r.cmakeTarget] = r
	for _, lp := range r.linkPaths {
		if _, claimed := h.byPath[lp]; !claimed {
			h.byPath[lp] = r
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
			h.warnf("%s: dep %q resolves to no harvested target; dropped", r.cmakeTarget, ref)
		}
		sort.Strings(r.deps)
	}
}

func (h *harvester) manifest(element string) *manifest.Imports {
	rows := append([]*row(nil), h.rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].cmakeTarget < rows[j].cmakeTarget })
	exports := make([]*manifest.Export, 0, len(rows))
	for _, r := range rows {
		underlying := r.cmakeTarget
		if r.aliasOf != "" {
			underlying = r.aliasOf
		}
		exports = append(exports, &manifest.Export{
			CMakeTarget:       r.cmakeTarget,
			BazelLabel:        h.label(underlying),
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
		if _, claimed := h.byPath[anchored]; claimed {
			continue
		}
		h.addRow(&row{
			cmakeTarget: element + "::bin/" + e.Name(),
			linkPaths:   []string{anchored},
		})
	}
}

// anchoredFromImportPrefix maps a `${_IMPORT_PREFIX}/<rel>` value (or
// an absolute path under the harvested prefix) onto the manifest's
// anchored form; ("", false) for anything else.
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

// trimQuotes strips one layer of double quotes.
func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

var _ = path.Join // keep path imported for siblings
