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
	"path/filepath"
	"sort"
	"strings"
)

// Imports is the top-level manifest object.
//
// Version is required; readers must reject unknown major versions. Minor
// version bumps add fields; old readers ignore unknown fields. Today's
// schema is version 1.
type Imports struct {
	Version  int        `json:"version"`
	Elements []*Element `json:"elements"`

	// Tools maps host CODEGEN tools that have no native Bazel rule onto the
	// label that provides them, so a recovered genrule drives the hermetic
	// Bazel tool instead of cmake's configure-time host binary. See Tool.
	// Top-level (not per-element) because a codegen tool is a build-time
	// dependency of the conversion, not an export of any one converted
	// element. Optional; empty for the common case.
	Tools []*Tool `json:"tools,omitempty"`
}

// Tool maps a host codegen tool — one with no idiomatic native Bazel rule
// (a project's own python/perl generator, flatc, thrift, an absolute
// host-install binary) — onto the Bazel label that provides it. The genrule
// recovery paths consult it through the single tool-swap chokepoint
// (lower.rewriteToolFromTarget): a command token matching a Tool is rewritten
// to `$(execpath <Label>)` and the label is added to the genrule's `tools`,
// so the action runs the hermetic Bazel tool and Bazel stages it into the
// sandbox. Without this an operator could only hermeticize a tool by abusing
// an Export's LinkPaths with Kind=executable, and only when the command
// referenced the tool by its absolute host path — basename-driven tools
// (`flatc`, `python3`, `perl`) had no swap at all.
//
// This is the codegen counterpart to the recognizer registry: recognizers
// cover tools WITH a native rule (protoc → proto_library); the tools map
// covers tools WITHOUT one (they stay genrules, but hermetic ones).
type Tool struct {
	// Match is how a command token is recognized as this tool. Two forms:
	//   - a bare BASENAME (no "/", e.g. "flatc", "python3") matches any
	//     command token whose basename equals it — a PATH-resolved driver
	//     or an absolute host path to the same program.
	//   - an ABSOLUTE path (e.g. "/opt/host/bin/gen.py") matches that exact
	//     token — the in-tree-script-by-absolute-path shape.
	// A relative multi-component match is matched verbatim. Case-sensitive.
	Match string `json:"match"`

	// Label is the $(execpath)-able Bazel label that provides the tool, e.g.
	// "@flatbuffers//:flatc" or "//tools:gen". Replaces the matched token and
	// is added to the genrule's tools attribute.
	Label string `json:"label"`
}

// Element represents one CMake source element (a converted package). Each
// exports zero or more targets that downstream elements may import.
type Element struct {
	Name    string    `json:"name"`              // matches Bazel external repo name
	Exports []*Export `json:"exports,omitempty"` // exported imported-targets

	// UmbrellaLabel + UmbrellaIncludeRoots model find_package(<Pkg>)'s
	// whole-include-tree behavior. A cmake `find_package(P)` puts P's
	// INTERFACE_INCLUDE_DIRECTORIES on every consumer's compile line, so
	// ALL of P's headers are reachable even though the consumer links only
	// a subset of P's targets (protobuf links 37 absl targets but its
	// sources #include ~70 absl headers, e.g. absl/functional/overload.h
	// from absl::overload — never declared). Bazel's strict per-target
	// headers reject that. When lower drops one of these include roots (an
	// absolute find_package include dir outside the source/build tree), it
	// instead adds UmbrellaLabel to the consumer's deps — a single
	// cc_library that re-exports P's full public header surface. Empty for
	// elements without a find_package whole-tree include (the common case).
	UmbrellaLabel        string   `json:"umbrella_label,omitempty"`
	UmbrellaIncludeRoots []string `json:"umbrella_include_roots,omitempty"`

	// UmbrellaDeps is the dependency closure of the umbrella target —
	// the labels a synthesized umbrella cc_library re-exports (the
	// structured home of what used to live in a side-channel deps.txt;
	// the build-lens .conf umbrella synthesis reads it from here).
	// Informational for lower; consumed by workspace-synthesis tooling.
	UmbrellaDeps []string `json:"umbrella_deps,omitempty"`
}

// PrefixAnchor is the canonical virtual token that anchors cross-element
// prefix paths in manifests (link_paths and kin): no filesystem path of
// that name exists; consumers remap real prefix paths onto it before
// lookup, and producers emit anchored paths so the two sides meet. The
// value is part of the manifest CONTRACT — this is its single home
// (converter/internal/lower.ManifestPrefixAnchor aliases it).
const PrefixAnchor = "/opt/prefix/"

// KindExecutable marks an Export as an installed program rather than a
// linkable library — see Export.Kind.
const KindExecutable = "executable"

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

	// Kind distinguishes non-library exports. Empty means a linkable
	// library (the default). KindExecutable ("executable") marks an
	// installed program — a cmake `add_executable(... IMPORTED)` bundle
	// target or a bare bin/ tool: consumers resolve it through
	// LinkPaths for genrule tool lifts, and wrapper generators must
	// emit a file-shaped target (filegroup) for it, never a cc_library
	// (an ELF program is not a static_library, and a cc_import over
	// one breaks at the consumer's link).
	Kind string `json:"kind,omitempty"`

	// InterfaceIncludes are package-relative include directories the
	// import contributes to consumers. Lower copies these into the
	// consumer's `includes` attribute when needed.
	InterfaceIncludes []string `json:"interface_includes,omitempty"`

	// LinkLibraries are additional libraries (typically `-l<name>` flag
	// fragments or pkg-config-like names) the import expands into. Most
	// imports won't set this; included for completeness.
	LinkLibraries []string `json:"link_libraries,omitempty"`

	// Deps are absolute Bazel labels the consumer must wire ALONGSIDE
	// BazelLabel when it imports this export — the import's own
	// requirements that Bazel transitivity cannot recover on its own:
	// the labels that carry no dep modeling (a bare cc_import over a
	// prebuilt archive), where cmake's flattened link line is
	// deliberately dropped for transitive-only archives (the
	// trace-gated drop) and a STATIC consumer has no link line at all.
	// Wired with the same PUBLIC/PRIVATE scope as the export itself.
	//
	// INVARIANT: Deps non-empty ⇔ BazelLabel does NOT model its own
	// deps. Producer-emitted manifests for converted elements leave it
	// EMPTY — their labels are real rules whose deps Bazel resolves;
	// filling it would double-wire consumers with direct edges to the
	// export's internals (the over-emit shape the trace-gated drop
	// exists to avoid). Hand-written host-install manifests list the
	// closure explicitly; a wrapper-synthesis generator that
	// materializes the closure as real cc_library deps must CLEAR
	// Deps in its output manifest to preserve the invariant.
	//
	// Resolution is ONE level — the consumer wires this list verbatim
	// and never chases a listed label's own Export.Deps (a label that
	// arrives via another export's closure is not re-consulted). A
	// hand-written manifest must therefore list each export's FULL
	// transitive closure, not just direct deps.
	Deps []string `json:"deps,omitempty"`

	// LinkPaths is the set of absolute paths the cmake codemodel records
	// for this import in `target.link.commandFragments[role="libraries"]`.
	// The orchestrator populates these when it stages the synth-prefix
	// tree: each IMPORTED_LOCATION_<CONFIG> path resolved against the
	// prefix root. Lower matches link-fragment paths against this list to
	// rewrite them as the export's BazelLabel.
	LinkPaths []string `json:"link_paths,omitempty"`

	// ExportedHeaders lists the header include-path keys gazelle's cc
	// header-scan resolver consults — each string as it would appear
	// in a downstream `#include "..."` / `#include <...>` line, e.g.
	// "openssl/ssl.h". build-cc-index folds each into cc_index.json
	// mapped to BazelLabel. This is the resolver-shaped counterpart
	// of InterfaceIncludes (which carries include *directories*, not
	// the per-header keys gazelle indexes): it closes the
	// external-repo edge where a genuinely-external dep's header
	// universe lives outside project B and only the manifest knows
	// the label. Sibling-element headers don't need this — they
	// already land in cc_index.json via build-cc-index's BUILD walk.
	ExportedHeaders []string `json:"exported_headers,omitempty"`

	// ImportModules lists the Python import / distribution names a
	// downstream `import <name>` resolves to this export. build-cc-
	// index folds each into python_modules.json mapped to BazelLabel.
	// Same role for the python resolver that ExportedHeaders plays
	// for the cc one: the resolver-shaped keys, distinct from
	// LinkLibraries' flag-fragment / distribution-name values.
	ImportModules []string `json:"import_modules,omitempty"`

	// CMakeConfigBundleLabel, when non-empty, is the absolute Bazel
	// label of the `cmake_config_bundle` filegroup the producer's
	// converted BUILD emits for the install(EXPORT) bundle that
	// surfaces this export. Phase 6 of the generator-parity uplift
	// (ROADMAP.md) writes this when the orchestrator's manifest
	// synthesizer can derive the bundle label from a declarative
	// install(EXPORT) verdict. Downstream consumers that resolve
	// the export via `find_package(<Pkg>)` against the producer
	// element point cmake's CMAKE_PREFIX_PATH at the directory the
	// label resolves to. Empty for imperative bundles (those stay
	// on the round-2 _install_tree_extract fallback, which doesn't
	// surface a stable bundle label) and for non-cmake exports
	// (autotools / meson / pyproject).
	CMakeConfigBundleLabel string `json:"cmake_config_bundle_label,omitempty"`

	// CMakeImportLabels lists the absolute Bazel labels of the
	// `<name>_import` cc_import targets the producer's converted
	// BUILD emits for each declarative install(EXPORT) target.
	// Sibling to CMakeConfigBundleLabel: where the bundle label
	// points at the lib/cmake/<Pkg>/ filegroup the orchestrator's
	// find_package wraps, these labels point at the per-artifact
	// cc_import facades for consumers who want to link directly
	// against the producer's pre-built without going through the
	// cmake-config find_package machinery. Empty for imperative
	// bundles. Order matches the declarative installer's
	// ExportTargets order so consumers iterating both lists see
	// stable pairings.
	CMakeImportLabels []string `json:"cmake_import_labels,omitempty"`
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

// LoadDoc reads and parses an imports manifest without indexing it.
// Used by callers that merge several docs before building one
// Resolver (e.g. a base --imports-manifest plus N --exports-in
// producer manifests).
func LoadDoc(path string) (*Imports, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	var im Imports
	if err := json.Unmarshal(b, &im); err != nil {
		return nil, fmt.Errorf("manifest: parse %s: %w", path, err)
	}
	return &im, nil
}

// LoadMerged reads several manifest docs (skipping empty paths) and
// indexes them with last-wins precedence on key collisions, rather
// than Index's strict duplicate-is-an-error. Paths are processed in
// order, so a caller that lists the render-time convention base
// first and producer-emitted --exports-in docs after gets the
// producer's real export surface winning over the convention guess
// for any shared cmake_target / link key. Each doc must be schema
// version 1; bazel_label must be non-empty (a mapping to nothing is
// always an authoring error, never an intended override).
func LoadMerged(paths ...string) (*Resolver, error) {
	r := &Resolver{
		byCMakeTarget:   map[string]*Export{},
		byElement:       map[string]*Element{},
		byLinkPath:      map[string]*Export{},
		byLinkLib:       map[string]*Export{},
		byUmbrellaIncRt: map[string]string{},
		byToolBasename:  map[string]string{},
		byToolPath:      map[string]string{},
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		doc, err := LoadDoc(p)
		if err != nil {
			return nil, err
		}
		if doc.Version != 1 {
			return nil, fmt.Errorf("manifest: %s unsupported version %d (want 1)", p, doc.Version)
		}
		for _, el := range doc.Elements {
			if el == nil || el.Name == "" {
				continue
			}
			r.byElement[el.Name] = el
			if el.UmbrellaLabel != "" {
				for _, root := range el.UmbrellaIncludeRoots {
					if root != "" {
						r.byUmbrellaIncRt[root] = el.UmbrellaLabel
					}
				}
			}
			for _, ex := range el.Exports {
				if ex == nil || ex.CMakeTarget == "" {
					continue
				}
				if ex.BazelLabel == "" {
					return nil, fmt.Errorf("manifest: %s element %q export %q: empty bazel_label", p, el.Name, ex.CMakeTarget)
				}
				r.byCMakeTarget[ex.CMakeTarget] = ex
				for _, lp := range ex.LinkPaths {
					r.byLinkPath[lp] = ex
				}
				for _, ll := range ex.LinkLibraries {
					r.byLinkLib[ll] = ex
				}
			}
		}
		// Last-wins on tool-match collisions across merged docs (the same
		// precedence the merge gives every other key): a producer --exports-in
		// doc overrides the convention base.
		for _, tl := range doc.Tools {
			if err := r.addTool(tl, false); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
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
		byCMakeTarget:   map[string]*Export{},
		byElement:       map[string]*Element{},
		byLinkPath:      map[string]*Export{},
		byLinkLib:       map[string]*Export{},
		byUmbrellaIncRt: map[string]string{},
		byToolBasename:  map[string]string{},
		byToolPath:      map[string]string{},
	}
	for _, el := range im.Elements {
		if el == nil || el.Name == "" {
			return nil, fmt.Errorf("manifest: element with empty name")
		}
		if _, dup := r.byElement[el.Name]; dup {
			return nil, fmt.Errorf("manifest: duplicate element %q", el.Name)
		}
		r.byElement[el.Name] = el
		if el.UmbrellaLabel != "" {
			for _, root := range el.UmbrellaIncludeRoots {
				if root != "" {
					r.byUmbrellaIncRt[root] = el.UmbrellaLabel
				}
			}
		}
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
	// Strict: a duplicate tool match is an authoring ambiguity, surfaced here
	// rather than silently won by last-write (mirrors the cmake_target rule).
	for _, tl := range im.Tools {
		if err := r.addTool(tl, true); err != nil {
			return nil, err
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
	byCMakeTarget   map[string]*Export
	byElement       map[string]*Element
	byLinkPath      map[string]*Export
	byLinkLib       map[string]*Export
	byUmbrellaIncRt map[string]string // find_package include root → umbrella label
	byToolBasename  map[string]string // codegen tool basename → bazel label
	byToolPath      map[string]string // codegen tool abs/relative-multi path → label
}

// addTool registers one Tool entry into the resolver's by-basename / by-path
// indexes. A bare basename (no "/") indexes by basename so any token whose
// basename matches lifts; an absolute or relative-multi-component match
// indexes verbatim. strict makes a duplicate match an error (Index); LoadMerged
// passes strict=false for last-wins precedence.
func (r *Resolver) addTool(t *Tool, strict bool) error {
	if t == nil || t.Match == "" {
		return fmt.Errorf("manifest: tool with empty match")
	}
	if t.Label == "" {
		return fmt.Errorf("manifest: tool %q: empty label", t.Match)
	}
	dst := r.byToolPath
	if !strings.Contains(t.Match, "/") {
		dst = r.byToolBasename
	}
	if strict {
		if _, dup := dst[t.Match]; dup {
			return fmt.Errorf("manifest: duplicate tool match %q", t.Match)
		}
	}
	dst[t.Match] = t.Label
	return nil
}

// LookupTool returns the Bazel label registered for a command token naming a
// codegen tool, or ("", false) if none. An absolute or relative-multi token
// matches a verbatim path entry; an absolute path or a bare basename also
// matches a registered basename. A relative multi-component token (typically
// an in-tree output path) is NOT basename-matched — only its verbatim form —
// so a tool name doesn't accidentally rewrite a same-basenamed output.
func (r *Resolver) LookupTool(token string) (string, bool) {
	if r == nil || token == "" {
		return "", false
	}
	if label, ok := r.byToolPath[token]; ok {
		return label, true
	}
	if len(r.byToolBasename) == 0 {
		return "", false
	}
	if !strings.Contains(token, "/") || filepath.IsAbs(token) {
		if label, ok := r.byToolBasename[filepath.Base(token)]; ok {
			return label, true
		}
	}
	return "", false
}

// HasTools reports whether the resolver carries any codegen-tool mappings.
// The genrule tool-swap fast-path uses it to proceed even for a manifest that
// has ONLY a tools section (no exports, so Empty() is true).
func (r *Resolver) HasTools() bool {
	if r == nil {
		return false
	}
	return len(r.byToolBasename) > 0 || len(r.byToolPath) > 0
}

// NewResolver returns an empty, ready-to-use resolver. Used by callers that
// build a resolver from in-memory data rather than a manifest file — e.g.
// registering built-in tool conventions when no --imports-manifest was given.
func NewResolver() *Resolver {
	return &Resolver{
		byCMakeTarget:   map[string]*Export{},
		byElement:       map[string]*Element{},
		byLinkPath:      map[string]*Export{},
		byLinkLib:       map[string]*Export{},
		byUmbrellaIncRt: map[string]string{},
		byToolBasename:  map[string]string{},
		byToolPath:      map[string]string{},
	}
}

// AddToolConventions adds FALLBACK tool mappings (built-in conventions) that do
// NOT override an existing match — an operator's explicit `tools` entry (or an
// earlier convention) wins. Each match/label must be non-empty.
func (r *Resolver) AddToolConventions(tools []Tool) error {
	if r == nil {
		return fmt.Errorf("manifest: AddToolConventions on a nil resolver")
	}
	for i := range tools {
		t := tools[i]
		if t.Match == "" {
			return fmt.Errorf("manifest: tool convention with empty match")
		}
		if t.Label == "" {
			return fmt.Errorf("manifest: tool convention %q: empty label", t.Match)
		}
		dst := r.byToolPath
		if !strings.Contains(t.Match, "/") {
			dst = r.byToolBasename
		}
		if _, exists := dst[t.Match]; exists {
			continue // operator / prior mapping wins
		}
		dst[t.Match] = t.Label
	}
	return nil
}

// UmbrellaForIncludeDir returns the umbrella label registered for an
// absolute find_package include root, or "" if none. lower calls this
// when it would otherwise drop an out-of-tree include dir, so a
// find_package(<Pkg>) whose whole-include-tree behavior an element
// declared (Element.UmbrellaIncludeRoots) wires the umbrella dep
// instead of silently losing the headers.
func (r *Resolver) UmbrellaForIncludeDir(path string) string {
	if r == nil {
		return ""
	}
	return r.byUmbrellaIncRt[path]
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

// AllExports returns every export across all elements in a
// deterministic order — by element name, then by each element's
// declared export order. build-cc-index uses this to fold
// exported-header / import-module entries into the gazelle resolver
// files with stable first-write-wins semantics on manifest-internal
// key collisions.
func (r *Resolver) AllExports() []*Export {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.byElement))
	for name := range r.byElement {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*Export
	for _, name := range names {
		out = append(out, r.byElement[name].Exports...)
	}
	return out
}

// Empty reports whether the resolver carries any imports. Used by callers
// that take a different fast-path when no manifest is loaded.
func (r *Resolver) Empty() bool {
	if r == nil {
		return true
	}
	return len(r.byCMakeTarget) == 0
}
