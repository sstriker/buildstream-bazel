// Package translate defines the contract per-kind translators implement
// when converting one bst element of a particular kind to BUILD.bazel +
// cmake-config bundle.
//
// The host of these translators changed: the original draft plan ran
// them inside a Go orchestrator's processElement loop, dispatching by
// kind via a Registry built at startup. The current plan
// (docs/whole-project-plan.md, Bazel-as-orchestrator / two-pass
// meta-project) moves the host to **Bazel itself**: project A's
// per-element genrule invokes the per-kind translator as a binary
// (typically `cmd/convert-element-cmake` for kind:cmake, sibling binaries
// for kind:meson and friends), with Bazel's action graph + cache
// taking over what the in-process orchestrator's executor + cache used
// to do. The Translate signature in this package is what each
// translator binary's main() does internally for its single element.
//
// What this package still provides:
//
//   - Inputs / Outputs / Failure / Result: the per-element struct
//     types every translator binary's main() reads from flags + writes
//     to disk. Stable shape across kinds keeps the writer-of-A's
//     starlark templates uniform.
//   - Translator interface: `Translate(ctx, Inputs) (*Result, error)`.
//     Each translator binary wires its main() onto a concrete
//     implementation; in-process unit tests use the same interface
//     against fakes.
//   - Registry: kind -> Translator dispatch. Now used by (a) tests
//     that drive multiple kinds in one process, and (b) a possible
//     multi-kind `convert-element-cmake` binary that flag-dispatches via
//     `--kind=<X>` to keep CI's tool-image footprint small. The
//     production project-A genrule references a specific binary; the
//     Registry is not its primary user.
//
// What this package no longer does: gate orchestrator-side scheduling,
// action-cache lookup, REAPI executor abstraction, regression diff,
// or cross-element fan-out. Those responsibilities belong to Bazel in
// the meta-project shape.
//
// Trivial kinds (stack, filter, import, compose, flatpak_image, …)
// don't need a translator binary at all — the writer-of-A renders
// them as pure starlark filegroup composition. They never reach this
// interface; only kinds that produce a per-element BUILD.bazel from
// a build action do.
package translate

import (
	"context"
	"fmt"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// Inputs are the per-element bits a Translator needs to produce
// BUILD.bazel + cmake-config bundle in ElemOut. The orchestrator
// populates these from its per-element pipeline (source resolution,
// shadow tree, synth-prefix, imports manifest) before dispatch.
type Inputs struct {
	// ElementName is the bst element name (e.g. "components/fmt").
	// Used for logging and Action correlation; not read by the
	// converter binary.
	ElementName string

	// ShadowDir is the local path to the source-root view the
	// converter sees. Required.
	ShadowDir string

	// ImportsManifest is the local path to the imports.json file or
	// "" when the element has no cross-element deps.
	ImportsManifest string

	// PrefixDir is the local path to the synth-prefix tree or ""
	// when the element has no cross-element deps.
	PrefixDir string

	// ElemOut is where the translator writes BUILD.bazel +
	// cmake-config/ + read_paths.json. The orchestrator clears it
	// on cache miss before calling Translate.
	ElemOut string

	// EnvVars are extra env entries to set in the action's Command
	// (e.g. ORCHESTRATOR_ELEMENT_NAME). Translators may pass them
	// through verbatim or ignore them; the cmake translator passes
	// them through.
	EnvVars map[string]string
}

// Outputs is the post-Translate per-element state the orchestrator
// needs for downstream cross-element work (synth-prefix updates,
// imports propagation). Populated only on success.
type Outputs struct {
	// ReadPaths are the package-relative source paths the converter
	// actually read (loaded from read_paths.json). Used to update the
	// allowlist registry.
	ReadPaths []string

	// RawExports is the parsed export list extracted from the
	// cmake-config/ bundle (one entry per add_library(... IMPORTED)).
	RawExports []*manifest.Export

	// PrefixRelLinkPaths maps each exported library name to its
	// prefix-relative linker paths.
	PrefixRelLinkPaths map[string][]string

	// Pkg is the cmake package name this element contributes
	// (e.g. "fmt", "boost") — derived from the cmake-config/
	// directory name. Empty when the element exports nothing.
	Pkg string
}

// Failure is a Tier-1 conversion failure (the element couldn't be
// translated; the orchestrator records it without aborting the run).
// Translators don't know about the orchestrator's FailureRecord shape;
// the caller wraps Failure into FailureRecord with Element + Tier set.
type Failure struct {
	Code    string
	Message string
}

// Result carries the post-Translate disposition of one element.
// Exactly one of Outputs / Failure is non-nil on a normal return;
// errors from Translate (Tier-2/3) are returned as `error` instead.
type Result struct {
	// CacheHit is true when the translator served the result from a
	// prior action-cache entry. Translators that don't use the action
	// cache (in-process kinds) leave it false.
	CacheHit bool

	// Failure is non-nil when conversion produced a Tier-1 failure.
	Failure *Failure

	// Outputs is non-nil on success.
	Outputs *Outputs
}

// Translator converts one bst element of a particular kind.
type Translator interface {
	// Kind returns the bst element kind this translator handles
	// (matches the `kind:` field in a .bst). One translator per kind.
	Kind() string

	// Translate produces BUILD.bazel + cmake-config/ + read_paths.json
	// in in.ElemOut for one element. Returns a Result whose Outputs or
	// Failure describes the per-element disposition; non-nil error is
	// reserved for Tier-2/3 conditions that cancel the orchestrator
	// run (REAPI gRPC failures, store I/O errors, …).
	Translate(ctx context.Context, in Inputs) (*Result, error)
}

// Registry maps element kinds to their Translator. Built at
// orchestrator startup; immutable after Run begins.
type Registry struct {
	byKind map[string]Translator
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byKind: map[string]Translator{}}
}

// Register associates a translator with its Kind(). Returns an error
// when the kind is already registered — a single binary can't have
// two translators for the same kind, and silent overwrite would hide
// configuration bugs.
func (r *Registry) Register(t Translator) error {
	k := t.Kind()
	if _, ok := r.byKind[k]; ok {
		return fmt.Errorf("translate: kind %q already registered", k)
	}
	r.byKind[k] = t
	return nil
}

// Lookup returns the translator for the given kind, or (nil, false)
// when no translator is registered for it. Callers surface a Tier-1
// "unsupported-kind" failure on miss; the orchestrator should not
// abort the whole run because one kind has no translator yet.
func (r *Registry) Lookup(kind string) (Translator, bool) {
	t, ok := r.byKind[kind]
	return t, ok
}

// Kinds returns the registered kinds in deterministic order. Used by
// startup logging so operators can see which translators are loaded.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	// Stable order via sort imported lazily; small slice, not perf-
	// critical. Don't pull "sort" just for this; do an in-place
	// insertion sort.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
