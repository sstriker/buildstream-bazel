package element

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Project is the parsed set of elements under one BuildStream root.
//
// Only the .bst files we successfully parsed are present; if some were
// malformed, ReadProject returns an error listing the offending paths
// rather than skipping silently — better to fail loud than silently
// orchestrate the wrong subset.
type Project struct {
	// Root is the absolute path passed to ReadProject (typically the
	// directory containing `elements/`).
	Root string

	// ElementsDir is the relative path under Root where .bst files live.
	// Defaults to "elements"; some projects use a different layout.
	ElementsDir string

	// Elements is keyed by Element.Name.
	Elements map[string]*Element
}

// Lookup returns an element by its filename-with-extension form
// (e.g. "components/libdrm.bst"), trimming the .bst suffix to match
// Element.Name. Returns nil if not found.
func (p *Project) Lookup(filename string) *Element {
	name := strings.TrimSuffix(filepath.ToSlash(filename), ".bst")
	if p == nil {
		return nil
	}
	return p.Elements[name]
}

// SortedNames returns element names in deterministic order. Useful for
// callers that want stable iteration without re-implementing the sort.
func (p *Project) SortedNames() []string {
	out := make([]string, 0, len(p.Elements))
	for n := range p.Elements {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ReadProject walks <root>/<elementsDir> for .bst files and parses each.
// Errors from individual files accumulate; the returned error is non-nil
// if any failed.
func ReadProject(root, elementsDir string) (*Project, error) {
	if elementsDir == "" {
		elementsDir = "elements"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("element: abs %s: %w", root, err)
	}
	walkRoot := filepath.Join(abs, elementsDir)
	info, err := os.Stat(walkRoot)
	if err != nil {
		return nil, fmt.Errorf("element: stat %s: %w", walkRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("element: %s is not a directory", walkRoot)
	}

	proj := &Project{
		Root:        abs,
		ElementsDir: elementsDir,
		Elements:    map[string]*Element{},
	}
	var parseErrs []string

	walkErr := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".bst" {
			return nil
		}
		rel, err := filepath.Rel(walkRoot, p)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".bst")
		el, perr := ParseElement(p, name)
		if perr != nil {
			parseErrs = append(parseErrs, perr.Error())
			return nil
		}
		proj.Elements[name] = el
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(parseErrs) > 0 {
		return proj, fmt.Errorf("element: %d parse error(s):\n  %s",
			len(parseErrs), strings.Join(parseErrs, "\n  "))
	}
	return proj, nil
}
