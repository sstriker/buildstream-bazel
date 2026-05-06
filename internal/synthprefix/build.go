// Package synthprefix builds a per-element CMAKE_PREFIX_PATH tree from the
// converter's synthesized cmake-config bundles for the dep closure.
//
// Architectural claim (validated end-to-end in M2's drop-in test): cmake's
// `if(NOT EXISTS)` import-check loop in <Pkg>Targets.cmake passes against
// zero-byte files. So for cross-element find_package resolution at convert
// time we don't need real built artifacts — only the bundle .cmake files
// + filesystem stubs at every IMPORTED_LOCATION_<CONFIG> path the bundles
// reference.
//
// One synth-prefix per downstream element keeps action keys narrow: B's
// prefix contains only B's transitive deps, not the whole universe of
// previously-converted elements.
package synthprefix

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DepBundle is one dep's contribution to the synth-prefix.
type DepBundle struct {
	// Pkg is the cmake project name as it appears in the bundle filename
	// (`<Pkg>Config.cmake`). Drives the lib/cmake/<Pkg>/ subdirectory.
	// Used for stable iteration ordering only; the layout itself is
	// determined by the bundle contents under SourceDir.
	Pkg string

	// SourceDir is the absolute path to the converter-emitted cmake-config
	// directory for this dep (`<out>/elements/<elem>/cmake-config/`). The
	// directory is in synth-prefix shape — its contents merge directly
	// onto the destination prefix without re-nesting.
	SourceDir string
}

// BuildSlice creates dst as a single-package CMAKE_PREFIX_PATH-shaped
// slice from a FLAT input directory of `<Pkg>*.cmake` files plus the
// stubs scanned out of those files. dst must not exist; BuildSlice
// creates it. Used by `convert-element --out-bundle-dir` to produce
// the per-element bundle.
//
// For each input (typically one per call):
//   - `<Pkg>*.cmake` files are copied to <dst>/lib/cmake/<Pkg>/.
//   - Every IMPORTED_LOCATION_<CONFIG> path under ${_IMPORT_PREFIX}
//     becomes a zero-byte stub at <dst>/<rel>.
//   - INTERFACE_INCLUDE_DIRECTORIES paths under ${_IMPORT_PREFIX} get
//     mkdir-stubs (cmake configure doesn't validate include dir
//     contents, only existence).
func BuildSlice(dst string, deps []DepBundle) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("synthprefix: dst already exists: %s", dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	cp := append([]DepBundle(nil), deps...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Pkg < cp[j].Pkg })

	for _, d := range cp {
		bundleDest := filepath.Join(dst, "lib", "cmake", d.Pkg)
		if err := os.MkdirAll(bundleDest, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(d.SourceDir)
		if err != nil {
			return fmt.Errorf("synthprefix: read %s: %w", d.SourceDir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if filepath.Ext(e.Name()) != ".cmake" {
				continue
			}
			if err := copyFile(filepath.Join(d.SourceDir, e.Name()), filepath.Join(bundleDest, e.Name())); err != nil {
				return err
			}
		}
		stubs, err := scanImportedPaths(d.SourceDir)
		if err != nil {
			return err
		}
		for _, s := range stubs {
			if err := stubAt(filepath.Join(dst, s.relPath), s.kind); err != nil {
				return err
			}
		}
	}
	return nil
}

// Build creates dst as a CMAKE_PREFIX_PATH-shaped synth-prefix tree by
// merging each dep's already-synth-prefix-shaped bundle directory into
// dst. dst must not exist; Build creates it.
//
// Each bundle (produced by `convert-element --out-bundle-dir`, which
// runs BuildSlice internally) carries its own
// `lib/cmake/<Pkg>/<Pkg>{Config,Targets,Targets-<config>}.cmake` plus
// zero-byte stubs at the IMPORTED_LOCATION paths and mkdir-stubs at the
// INTERFACE_INCLUDE_DIRECTORIES paths. Build copies each bundle's tree
// over dst; with one Pkg per bundle, the lib/cmake/<Pkg>/ dirs don't
// collide.
func Build(dst string, deps []DepBundle) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("synthprefix: dst already exists: %s", dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	cp := append([]DepBundle(nil), deps...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Pkg < cp[j].Pkg })

	for _, d := range cp {
		if err := mergeTree(d.SourceDir, dst); err != nil {
			return fmt.Errorf("synthprefix: merge %s: %w", d.SourceDir, err)
		}
	}
	return nil
}

// mergeTree copies every regular file and directory from src into dst,
// preserving the relative path layout. Directories already present in
// dst are extended; files are not overwritten if the same relative path
// already exists (caller's responsibility to ensure non-overlapping
// bundles). Symlinks are followed.
func mergeTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Regular file (or zero-byte stub). Skip if already present.
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

// importedPathKind tells stubAt whether to create a zero-byte file or a
// directory at the prefix-relative path.
type importedPathKind int

const (
	stubFile importedPathKind = iota
	stubDir
)

type stubSpec struct {
	relPath string // path relative to the synth-prefix root (no leading slash)
	kind    importedPathKind
}

// importedLocationRe matches lines like
//
//	IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/lib/libhello.a"
//
// emitted by the converter's per-config Targets-*.cmake files.
var importedLocationRe = regexp.MustCompile(`IMPORTED_LOCATION_[A-Z]+\s+"\$\{_IMPORT_PREFIX\}([^"]+)"`)

// interfaceIncludeRe matches the cmake-side
//
//	INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include"
//
// shape (and similar with multiple `;`-separated entries inside the quotes).
var interfaceIncludeRe = regexp.MustCompile(`INTERFACE_INCLUDE_DIRECTORIES\s+"([^"]+)"`)

// scanImportedPaths walks bundleDir for *.cmake files and extracts every
// imported-target path under ${_IMPORT_PREFIX}/, classifying each as a
// file (IMPORTED_LOCATION_<CONFIG>) or directory
// (INTERFACE_INCLUDE_DIRECTORIES).
//
// Paths outside ${_IMPORT_PREFIX} (host-leaks; system libs) are silently
// skipped — not our concern at this layer.
func scanImportedPaths(bundleDir string) ([]stubSpec, error) {
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []stubSpec
	add := func(relPath string, kind importedPathKind) {
		clean := strings.TrimPrefix(relPath, "/")
		if seen[clean] {
			return
		}
		seen[clean] = true
		out = append(out, stubSpec{relPath: clean, kind: kind})
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".cmake" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(bundleDir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, m := range importedLocationRe.FindAllSubmatch(body, -1) {
			add(string(m[1]), stubFile)
		}
		for _, m := range interfaceIncludeRe.FindAllSubmatch(body, -1) {
			// May contain `;`-separated entries; cmake also splits on
			// embedded paths starting with ${_IMPORT_PREFIX}.
			for _, p := range strings.Split(string(m[1]), ";") {
				p = strings.TrimSpace(p)
				rest, ok := strings.CutPrefix(p, "${_IMPORT_PREFIX}")
				if !ok {
					continue
				}
				add(rest, stubDir)
			}
		}
	}
	return out, nil
}

func stubAt(absPath string, kind importedPathKind) error {
	switch kind {
	case stubFile:
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(absPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		return f.Close()
	case stubDir:
		return os.MkdirAll(absPath, 0o755)
	}
	return fmt.Errorf("synthprefix: unknown stub kind %v", kind)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// PkgFromBundle returns the cmake project name of a synth-prefix-shaped
// bundle. Returns "" if the bundle has no Config.cmake (shouldn't
// happen for converter output but tolerated).
//
// The bundle is laid out as
//
//	<bundleDir>/lib/cmake/<Pkg>/<Pkg>Config.cmake
//
// (the shape `BuildSlice` produces inside `convert-element`'s
// `--out-bundle-dir`). PkgFromBundle finds the unique
// `<Pkg>Config.cmake` under `lib/cmake/*/` and returns `<Pkg>`.
func PkgFromBundle(bundleDir string) (string, error) {
	cmakeRoot := filepath.Join(bundleDir, "lib", "cmake")
	entries, err := os.ReadDir(cmakeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkgDir := filepath.Join(cmakeRoot, e.Name())
		inner, err := os.ReadDir(pkgDir)
		if err != nil {
			return "", err
		}
		for _, f := range inner {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if strings.HasSuffix(name, "Config.cmake") {
				return strings.TrimSuffix(name, "Config.cmake"), nil
			}
		}
	}
	return "", nil
}

// walkErr is a small adaptor used by tests that want to verify a
// constructed synth-prefix walks cleanly. Not exported; tests use it
// alongside fs.WalkDir.
var _ fs.WalkDirFunc = func(string, fs.DirEntry, error) error { return nil }
