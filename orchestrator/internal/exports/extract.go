// Package exports reads imported-target declarations out of an element's
// synthesized cmake-config bundle and turns them into manifest.Export
// entries the orchestrator can register in the dep-export registry.
//
// Source of truth is the bundle's <Pkg>Targets.cmake. Parsing it (rather
// than modeling the converter's IR a second time) keeps the contract on
// the cmake-side surface — the same surface a real find_package consumer
// would see.
package exports

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

// addLibraryRe matches lines like
//
//	add_library(hello::hello STATIC IMPORTED)
//	add_library(FMT::fmt SHARED IMPORTED)
//	add_library(absl::strings INTERFACE IMPORTED)
//
// Group 1 = namespace, group 2 = target name, group 3 = type keyword.
// Whitespace and trailing-arg variations within the parens are tolerated.
// CMake allows hyphens in both namespace and target ID. Convention is
// underscores-only for namespaces but real-world bundles (e.g. one whose
// project name itself contains a hyphen) emit hyphenated namespaces.
var addLibraryRe = regexp.MustCompile(`^add_library\(\s*([A-Za-z0-9_-]+)\s*::\s*([A-Za-z0-9_-]+)\s+(STATIC|SHARED|MODULE|INTERFACE)\s+IMPORTED`)

// importedLocationStanzaRe captures `set_target_properties(<NS>::<target>
// PROPERTIES ... IMPORTED_LOCATION_<CONFIG> "${_IMPORT_PREFIX}<rest>" ...)`.
// The match spans multi-line stanzas that converter cmakecfg emits; we
// run it over the file-as-a-whole rather than line-by-line.
var importedLocationStanzaRe = regexp.MustCompile(`set_target_properties\(\s*([A-Za-z0-9_-]+::[A-Za-z0-9_-]+)\s+PROPERTIES[\s\S]*?IMPORTED_LOCATION_[A-Z]+\s+"\$\{_IMPORT_PREFIX\}([^"]+)"`)

// FromBundle scans a synth-prefix-shaped cmake-config bundle directory
// for imported targets and returns one manifest.Export per declaration.
// Element name is NOT applied to BazelLabel here — that's the caller's
// job (the orchestrator stamps the `//elements/<name>:<target>` label
// after picking up the raw exports).
//
// The bundle layout is what `convert-element --out-bundle-dir` writes
// via `synthprefix.BuildSlice`:
//
//	<bundleDir>/lib/cmake/<Pkg>/<Pkg>Targets.cmake
//	<bundleDir>/lib/cmake/<Pkg>/<Pkg>Targets-<config>.cmake
func FromBundle(bundleDir string) ([]*manifest.Export, error) {
	targetsFiles, err := listTargetsFiles(bundleDir)
	if err != nil {
		return nil, err
	}
	if len(targetsFiles) == 0 {
		return nil, nil // nothing to export; not an error
	}

	seen := map[string]bool{}
	var out []*manifest.Export
	for _, path := range targetsFiles {
		exps, err := parseFile(path)
		if err != nil {
			return nil, err
		}
		for _, e := range exps {
			if seen[e.CMakeTarget] {
				continue
			}
			seen[e.CMakeTarget] = true
			out = append(out, e)
		}
	}
	return out, nil
}

// PrefixRelativeLinkPaths reads every per-config Targets-*.cmake from
// the bundle (synth-prefix or flat layout — see FromBundle's comment)
// and returns a map of cmake_target -> []prefix-relative paths. Each
// path is the IMPORTED_LOCATION_<CONFIG> stanza's value with the
// leading `${_IMPORT_PREFIX}` stripped. Caller (orchestrator) joins
// these against the consumer's prefix root to produce absolute
// LinkPaths for the imports manifest.
func PrefixRelativeLinkPaths(bundleDir string) (map[string][]string, error) {
	cmakeFiles, err := listAllCmakeFiles(bundleDir)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, path := range cmakeFiles {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, m := range importedLocationStanzaRe.FindAllSubmatch(body, -1) {
			tgt, rel := string(m[1]), string(m[2])
			out[tgt] = append(out[tgt], rel)
		}
	}
	return out, nil
}

// listTargetsFiles returns the absolute paths to every
// `<Pkg>Targets.cmake` under
// `<bundleDir>/lib/cmake/<Pkg>/`.
func listTargetsFiles(bundleDir string) ([]string, error) {
	cmakeRoot := filepath.Join(bundleDir, "lib", "cmake")
	pkgDirs, err := os.ReadDir(cmakeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("exports: read %s: %w", cmakeRoot, err)
	}
	var out []string
	for _, p := range pkgDirs {
		if !p.IsDir() {
			continue
		}
		pkgDir := filepath.Join(cmakeRoot, p.Name())
		entries, err := os.ReadDir(pkgDir)
		if err != nil {
			return nil, fmt.Errorf("exports: read %s: %w", pkgDir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), "Targets.cmake") {
				out = append(out, filepath.Join(pkgDir, e.Name()))
			}
		}
	}
	return out, nil
}

// listAllCmakeFiles is the IMPORTED_LOCATION-source variant of
// listTargetsFiles: every `*.cmake` regardless of suffix, since
// per-config overlays (`Targets-<config>.cmake`) carry the
// IMPORTED_LOCATION_<CONFIG> values.
func listAllCmakeFiles(bundleDir string) ([]string, error) {
	cmakeRoot := filepath.Join(bundleDir, "lib", "cmake")
	pkgDirs, err := os.ReadDir(cmakeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, p := range pkgDirs {
		if !p.IsDir() {
			continue
		}
		pkgDir := filepath.Join(cmakeRoot, p.Name())
		entries, err := os.ReadDir(pkgDir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".cmake" {
				continue
			}
			out = append(out, filepath.Join(pkgDir, e.Name()))
		}
	}
	return out, nil
}

// parseFile extracts imported-target declarations from one Targets.cmake.
//
// The orchestrator ignores per-config overlays (Targets-release.cmake) —
// those carry IMPORTED_LOCATION_RELEASE and similar but no new add_library
// calls.
func parseFile(path string) ([]*manifest.Export, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var exps []*manifest.Export
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		m := addLibraryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ns, target := m[1], m[2]
		exps = append(exps, &manifest.Export{
			CMakeTarget: ns + "::" + target,
		})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return exps, nil
}

// AsElement stamps a per-element BazelLabel onto raw exports and wraps
// them as a manifest.Element. The label form `//elements/<element-name>:<target>`
// addresses each converted element as a Bazel package within the
// orchestrator-emitted bzlmod project rooted at <out>/. The `<target>`
// half is the second segment of the cmake-target namespaced name
// (`Pkg::target` -> `target`).
//
// LinkPaths, when non-nil, supplies the absolute link-fragment paths the
// orchestrator computed for this element under the consumer's synth-
// prefix tree (one set per consumer because the prefix root differs).
// linkPathsFor maps cmake_target -> []absolute paths.
func AsElement(elementName string, raw []*manifest.Export, linkPathsFor map[string][]string) *manifest.Element {
	out := make([]*manifest.Export, 0, len(raw))
	for _, ex := range raw {
		// CMakeTarget is "Pkg::target"; the second half becomes the label.
		idx := strings.Index(ex.CMakeTarget, "::")
		if idx < 0 || idx+2 >= len(ex.CMakeTarget) {
			continue
		}
		target := ex.CMakeTarget[idx+2:]
		stamped := &manifest.Export{
			CMakeTarget: ex.CMakeTarget,
			BazelLabel:  fmt.Sprintf("//elements/%s:%s", elementName, target),
		}
		if linkPathsFor != nil {
			if paths, ok := linkPathsFor[ex.CMakeTarget]; ok {
				stamped.LinkPaths = append([]string(nil), paths...)
			}
		}
		out = append(out, stamped)
	}
	return &manifest.Element{
		Name:    elementName,
		Exports: out,
	}
}
