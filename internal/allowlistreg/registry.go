// Package allowlistreg manages the per-element allowlist registry: the
// orchestrator's persistent record of which source-tree paths each
// converted element's cmake actually read at configure time.
//
// On every successful conversion the converter writes read_paths.json
// listing the source-relative paths cmake's --trace-format=json-v1 trace
// captured. The orchestrator merges that list into a per-element JSON
// file under <out>/registry/allowlists/<elem>.json and on the *next* run
// passes those paths through to shadow.Build's matcher so the shadow
// tree carries real content for files cmake actually consumes.
//
// First run starts cold (registry is empty; only the default allowlist
// applies). If cmake reads a non-allowlisted file then the converter
// fails — at convert time CMakeLists are real-content already; the
// trace-driven augmentation kicks in only when the registry covers a
// later iteration's behavior. M4's regression detector uses the registry
// as the second-half of its content fingerprint.
package allowlistreg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// File is the on-disk schema. Versioned so M4 can fence on incompatible
// reads.
type File struct {
	Version int      `json:"version"`
	Element string   `json:"element"`
	Paths   []string `json:"paths"`
}

// Registry holds the in-memory union of every per-element file read so
// far this run, plus a memory of which file each set came from so Update
// can write back deterministically.
type Registry struct {
	Root   string // <out>/registry/allowlists
	byElem map[string]map[string]struct{}
}

// New returns an empty Registry rooted at root. Loaders populate byElem
// lazily (on first Update or Load call per element).
func New(root string) *Registry {
	return &Registry{
		Root:   root,
		byElem: map[string]map[string]struct{}{},
	}
}

// Load populates the registry from <root>/<elem>.json if it exists.
// Missing file is not an error (first run for this element).
func (r *Registry) Load(elem string) error {
	path := r.pathFor(elem)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("allowlistreg: load %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("allowlistreg: parse %s: %w", path, err)
	}
	if f.Version != 1 {
		return fmt.Errorf("allowlistreg: %s: unsupported version %d", path, f.Version)
	}
	set := map[string]struct{}{}
	for _, p := range f.Paths {
		set[filepath.ToSlash(p)] = struct{}{}
	}
	r.byElem[elem] = set
	return nil
}

// Update merges the given paths into element's allowlist and writes the
// per-element JSON file. Idempotent: identical input produces an
// identical output file.
func (r *Registry) Update(elem string, paths []string) error {
	set, ok := r.byElem[elem]
	if !ok {
		set = map[string]struct{}{}
		r.byElem[elem] = set
	}
	for _, p := range paths {
		set[filepath.ToSlash(p)] = struct{}{}
	}

	out := sliceutil.SortedKeys(set)

	doc := File{Version: 1, Element: elem, Paths: out}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dst := r.pathFor(elem)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, append(body, '\n'), 0o644)
}

// Paths returns the current path set for element, sorted. Empty if
// uninitialized.
func (r *Registry) Paths(elem string) []string {
	set := r.byElem[elem]
	out := sliceutil.SortedKeys(set)
	return out
}

// Matcher returns a shadow.Matcher that allows DefaultAllowlist OR any
// path the registry has recorded for this element.
func (r *Registry) Matcher(elem string) shadow.Matcher {
	def := shadow.DefaultAllowlist()
	set := r.byElem[elem]
	return shadow.MatcherFunc(func(rel string) bool {
		if def.Allowed(rel) {
			return true
		}
		_, ok := set[filepath.ToSlash(rel)]
		return ok
	})
}

func (r *Registry) pathFor(elem string) string {
	return filepath.Join(r.Root, filepath.FromSlash(elem)+".json")
}
