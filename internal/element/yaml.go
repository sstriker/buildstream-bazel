// Package element parses BuildStream-style element YAML files into a flat
// data model the orchestrator graph-builder consumes.
//
// Scope: the strict subset FreeDesktop SDK uses for `kind: cmake` elements
// and their immediate dependencies. We model `kind`, `depends`, and
// `sources`. Variants, junctions, conditional inheritance, and project-wide
// `variables:` blocks are out of scope until a real FDSDK element forces
// them in.
//
// Reading multiple YAMLs (`Project`) is intentionally separate from parsing
// one (`ParseElement`) so the orchestrator can layer in its own discovery
// logic (subdirectory walks, junction resolution) without re-running the
// per-file parser.
package element

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Element is one parsed `<name>.bst` file.
type Element struct {
	// Name is the element's logical identifier — by convention the file
	// name with the .bst extension stripped, with directories preserved
	// using forward slashes (e.g. "components/libdrm" for
	// elements/components/libdrm.bst).
	Name string

	// SourcePath is the absolute path to the YAML file we read.
	SourcePath string

	// Kind matches the BuildStream `kind:` field. Values we care about:
	// "cmake", "autotools", "meson", "manual". Other kinds are recorded
	// faithfully so the graph builder can treat them as opaque deps.
	Kind string

	// Depends is the parsed `depends:` list. A bare string in YAML is
	// equivalent to {filename: <s>, type: all}; we normalize either form
	// to Dep.
	Depends []Dep

	// Sources is the parsed `sources:` list. M3 doesn't act on sources
	// (the user provides pre-checked-out trees) but we keep the list so
	// later milestones can drive `bst source checkout`.
	Sources []Source
}

// Dep is one entry under `depends:`. BuildStream allows either a bare
// filename string or a dict with filename + type + junction. We normalize
// to this shape.
type Dep struct {
	// Filename is the depended-upon element's path, e.g. "base.bst" or
	// "components/libdrm.bst". Normalized to forward slashes.
	Filename string

	// Type is one of "build", "runtime", or "all" (default).
	Type string

	// Junction is the optional cross-project junction name. Most FDSDK
	// kind:cmake elements don't use junctions; we preserve the field so
	// the orchestrator can reject unsupported junction crossings with a
	// clear message.
	Junction string
}

// Source is one entry under `sources:`. We only need enough fields to
// fingerprint sources for the action-key cache (M3a step 6); everything
// else is preserved in Extra for forward compatibility.
type Source struct {
	Kind  string         // "git" / "tar" / "local" / etc.
	URL   string         // git url or tarball location
	Ref   string         // git ref / tarball sha
	Extra map[string]any // any other keys, preserved verbatim
}

// ParseElement reads a single .bst file and returns its parsed shape.
// `name` is the logical name (path-relative-to-elements-root, no .bst
// suffix); we don't infer it from path because the caller has more
// context.
func ParseElement(path, name string) (*Element, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("element: open %s: %w", path, err)
	}
	defer f.Close()
	return parseElementReader(f, path, name)
}

func parseElementReader(r io.Reader, path, name string) (*Element, error) {
	var raw map[string]yaml.Node
	dec := yaml.NewDecoder(r)
	dec.KnownFields(false)
	if err := dec.Decode(&raw); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("element: %s: empty document", path)
		}
		return nil, fmt.Errorf("element: %s: %w", path, err)
	}

	el := &Element{
		Name:       name,
		SourcePath: path,
	}

	if n, ok := raw["kind"]; ok {
		if err := n.Decode(&el.Kind); err != nil {
			return nil, fmt.Errorf("element: %s: kind: %w", path, err)
		}
	}

	if n, ok := raw["depends"]; ok {
		deps, err := decodeDepends(n, path)
		if err != nil {
			return nil, err
		}
		el.Depends = deps
	}

	if n, ok := raw["sources"]; ok {
		srcs, err := decodeSources(n, path)
		if err != nil {
			return nil, err
		}
		el.Sources = srcs
	}

	return el, nil
}

// decodeDepends handles the bare-string-or-dict polymorphism BuildStream
// allows under `depends:`.
func decodeDepends(n yaml.Node, path string) ([]Dep, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("element: %s: depends must be a list, got %s",
			path, kindName(n.Kind))
	}
	out := make([]Dep, 0, len(n.Content))
	for i, item := range n.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, Dep{
				Filename: normalizeFilename(item.Value),
			})
		case yaml.MappingNode:
			d, err := decodeDepDict(*item, path, i)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		default:
			return nil, fmt.Errorf("element: %s: depends[%d]: unexpected node kind %s",
				path, i, kindName(item.Kind))
		}
	}
	return out, nil
}

func decodeDepDict(m yaml.Node, path string, idx int) (Dep, error) {
	var raw struct {
		Filename string `yaml:"filename"`
		Type     string `yaml:"type"`
		Junction string `yaml:"junction"`
	}
	if err := m.Decode(&raw); err != nil {
		return Dep{}, fmt.Errorf("element: %s: depends[%d]: %w", path, idx, err)
	}
	if raw.Filename == "" {
		return Dep{}, fmt.Errorf("element: %s: depends[%d]: missing filename", path, idx)
	}
	return Dep{
		Filename: normalizeFilename(raw.Filename),
		Type:     raw.Type,
		Junction: raw.Junction,
	}, nil
}

func decodeSources(n yaml.Node, path string) ([]Source, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("element: %s: sources must be a list", path)
	}
	out := make([]Source, 0, len(n.Content))
	for i, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("element: %s: sources[%d]: not a mapping", path, i)
		}
		var raw map[string]any
		if err := item.Decode(&raw); err != nil {
			return nil, fmt.Errorf("element: %s: sources[%d]: %w", path, i, err)
		}
		s := Source{Extra: map[string]any{}}
		for k, v := range raw {
			switch k {
			case "kind":
				s.Kind, _ = v.(string)
			case "url":
				s.URL, _ = v.(string)
			case "ref":
				s.Ref, _ = v.(string)
			default:
				s.Extra[k] = v
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func normalizeFilename(s string) string {
	return filepath.ToSlash(strings.TrimSpace(s))
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
}
