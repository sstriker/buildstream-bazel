// Package manifest defines the per-orchestration imports manifest schema and
// resolver. The manifest tells lower how to translate a cross-element CMake
// target (one that doesn't appear in the current element's codemodel) into a
// Bazel label and its interface metadata.
//
// The orchestrator (M3) produces this file from its element registry; M2
// uses hand-written manifests for tests and the M2-step-5 acceptance gate.
//
// Schema stability: same append-only rule as failure-schema.md and
// codegen-tags.md. Add new optional fields freely; renaming or removing
// existing ones is a breaking change for every element pipeline that's
// consumed a manifest written before the change.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
)

// Imports is the top-level manifest object.
//
// Version is required; readers must reject unknown major versions. Minor
// version bumps add fields; old readers ignore unknown fields. Today's
// schema is version 1.
type Imports struct {
	Version  int        `json:"version"`
	Elements []*Element `json:"elements"`
}

// Element represents one CMake source element (a converted package). Each
// exports zero or more targets that downstream elements may import.
type Element struct {
	Name    string    `json:"name"`              // matches Bazel external repo name
	Exports []*Export `json:"exports,omitempty"` // exported imported-targets
}

// Export wires one CMake imported target name to a Bazel label.
type Export struct {
	// CMakeTarget is the namespaced name a downstream consumer's
	// `target_link_libraries(... CMakeTarget)` references, e.g.
	// "Glibc::c". Match is case-sensitive (CMake's behavior).
	CMakeTarget string `json:"cmake_target"`

	// BazelLabel is the absolute Bazel label that replaces the import in
	// generated BUILD.bazel deps lists, e.g.
	// "//elements/components/glibc:c". Resolves against the orchestrator-
	// emitted bzlmod project rooted at <out>/.
	BazelLabel string `json:"bazel_label"`

	// InterfaceIncludes are package-relative include directories the
	// import contributes to consumers. Lower copies these into the
	// consumer's `includes` attribute when needed.
	InterfaceIncludes []string `json:"interface_includes,omitempty"`

	// LinkLibraries are additional libraries (typically `-l<name>` flag
	// fragments or pkg-config-like names) the import expands into. Most
	// imports won't set this; included for completeness.
	LinkLibraries []string `json:"link_libraries,omitempty"`

	// LinkPaths is the set of absolute paths the cmake codemodel records
	// for this import in `target.link.commandFragments[role="libraries"]`.
	// The orchestrator populates these when it stages the synth-prefix
	// tree: each IMPORTED_LOCATION_<CONFIG> path resolved against the
	// prefix root. Lower matches link-fragment paths against this list to
	// rewrite them as the export's BazelLabel.
	LinkPaths []string `json:"link_paths,omitempty"`
}

// Load reads and parses an imports manifest from disk. Returns a Resolver
// keyed for fast lookup.
func Load(path string) (*Resolver, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	var im Imports
	if err := json.Unmarshal(b, &im); err != nil {
		return nil, fmt.Errorf("manifest: parse %s: %w", path, err)
	}
	return Index(&im)
}

// Index validates the manifest and returns a Resolver.
//
// Validation:
//   - Version must be exactly 1 (M2). Unknown versions get a typed error.
//   - Each Export.CMakeTarget must be unique across all elements; duplicates
//     are ambiguous and fail loudly here rather than silently winning by
//     last-write.
func Index(im *Imports) (*Resolver, error) {
	if im.Version != 1 {
		return nil, fmt.Errorf("manifest: unsupported version %d (want 1)", im.Version)
	}
	r := &Resolver{
		byCMakeTarget: map[string]*Export{},
		byElement:     map[string]*Element{},
		byLinkPath:    map[string]*Export{},
		byLinkLib:     map[string]*Export{},
	}
	for _, el := range im.Elements {
		if el == nil || el.Name == "" {
			return nil, fmt.Errorf("manifest: element with empty name")
		}
		if _, dup := r.byElement[el.Name]; dup {
			return nil, fmt.Errorf("manifest: duplicate element %q", el.Name)
		}
		r.byElement[el.Name] = el
		for _, ex := range el.Exports {
			if ex == nil || ex.CMakeTarget == "" {
				return nil, fmt.Errorf("manifest: element %q: export with empty cmake_target", el.Name)
			}
			if ex.BazelLabel == "" {
				return nil, fmt.Errorf("manifest: element %q export %q: empty bazel_label", el.Name, ex.CMakeTarget)
			}
			if existing, dup := r.byCMakeTarget[ex.CMakeTarget]; dup {
				return nil, fmt.Errorf("manifest: cmake_target %q exported by %q and %q",
					ex.CMakeTarget, el.Name, findElementForExport(im, existing))
			}
			r.byCMakeTarget[ex.CMakeTarget] = ex
			for _, lp := range ex.LinkPaths {
				r.byLinkPath[lp] = ex
			}
			for _, ll := range ex.LinkLibraries {
				// First-write-wins on link-library collisions:
				// two elements both exposing `-lz` is a manifest
				// authoring concern, not something we want to
				// surface as a hard error here. The cmake side
				// already has a similar tolerance (link_paths
				// can collide too).
				if _, dup := r.byLinkLib[ll]; !dup {
					r.byLinkLib[ll] = ex
				}
			}
		}
	}
	return r, nil
}

func findElementForExport(im *Imports, ex *Export) string {
	for _, el := range im.Elements {
		for _, e := range el.Exports {
			if e == ex {
				return el.Name
			}
		}
	}
	return "<unknown>"
}

// Resolver is the indexed manifest. Query methods are pure and concurrency-
// safe (no mutation post-Load).
type Resolver struct {
	byCMakeTarget map[string]*Export
	byElement     map[string]*Element
	byLinkPath    map[string]*Export
	byLinkLib     map[string]*Export
}

// LookupCMakeTarget returns the export for a CMake namespaced target name
// like "Glibc::c", or nil if no element exports it.
func (r *Resolver) LookupCMakeTarget(name string) *Export {
	if r == nil {
		return nil
	}
	return r.byCMakeTarget[name]
}

// LookupLinkPath returns the export that owns a given absolute link-fragment
// path, or nil if none. Used by lower to map cross-element library
// fragments (CMake records IMPORTED_LOCATION_<CONFIG> file paths in
// `target.link.commandFragments[role="libraries"]`) onto Bazel labels.
func (r *Resolver) LookupLinkPath(path string) *Export {
	if r == nil {
		return nil
	}
	return r.byLinkPath[path]
}

// LookupLinkLibrary returns the export that owns a `-l<name>` link
// flag's <name>, or nil if no element claims it. Used by
// convert-element-trace to resolve link commands' -l<lib>
// args (e.g., -lz → //elements/zlib:zlib) when the trace
// itself doesn't produce a matching archive in-graph.
func (r *Resolver) LookupLinkLibrary(name string) *Export {
	if r == nil {
		return nil
	}
	return r.byLinkLib[name]
}

// LookupElement returns an element by name, or nil if none.
func (r *Resolver) LookupElement(name string) *Element {
	if r == nil {
		return nil
	}
	return r.byElement[name]
}

// Empty reports whether the resolver carries any imports. Used by callers
// that take a different fast-path when no manifest is loaded.
func (r *Resolver) Empty() bool {
	if r == nil {
		return true
	}
	return len(r.byCMakeTarget) == 0
}
