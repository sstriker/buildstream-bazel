package lower

import (
	"sort"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// Auto-deriving the LABEL for a host codegen tool is the last gap in
// host-codegen-tool hermeticization. The imports-manifest `tools` map lets an
// operator map a tool onto a label, and the converter already auto-DETECTS the
// tools still needing a mapping (the `host-codegen-tool` conversion-todo). What
// the operator still authors by hand is the LABEL — yet for well-known
// generators the canonical Bazel label is a fixed convention. This registry is
// that knowledge: a curated tool-basename → canonical label + BCR module map.
//
// It feeds two uses, both keyed on the same data:
//   - The `host-codegen-tool` todo upgrades its suggested `tools` entry to the
//     REAL label (and names the bazel_dep to add) for a known tool, instead of
//     a `//path/to:...` placeholder. Always on, zero BUILD-output change.
//   - Under the opt-in --tool-conventions flag, the conventions are registered
//     into the imports resolver's tools map, so a recovered genrule driving a
//     known host tool auto-hermeticizes through the existing tool-swap — no
//     manual manifest authoring. Off by default; an operator `tools` entry for
//     the same tool always wins (the convention is a fallback).
//
// Correctness over breadth: an entry asserts a real BCR label, so the registry
// stays small and verified. protoc's label is the one the converter already
// hardcodes elsewhere (reanchorBuildDirCopyGenrule's `@protobuf//:protoc` swap
// for the grpc cd-shape) — this generalizes that beyond that one shape.

// ToolConvention is the canonical Bazel provider for a well-known host codegen
// tool.
type ToolConvention struct {
	// Label is the $(execpath)-able Bazel label that provides the tool.
	Label string
	// Module is the BCR module (bazel_dep name) that supplies Label — the
	// MODULE.bazel dependency an operator must add for the swapped genrule to
	// build. Surfaced in the todo's hint.
	Module string
}

// builtinToolConventions maps a codegen tool's driver basename to its canonical
// Bazel provider. Keep entries verified — each must name a real BCR label.
var builtinToolConventions = map[string]ToolConvention{
	// protoc: the protobuf BCR module exposes //:protoc; the converter already
	// swaps a host protoc to this label on the grpc cd-shape path.
	"protoc": {Label: "@protobuf//:protoc", Module: "protobuf"},
}

// toolConventionFor returns the convention for a driver basename, or ok=false.
func toolConventionFor(driver string) (ToolConvention, bool) {
	c, ok := builtinToolConventions[driver]
	return c, ok
}

// ToolConventionTools returns the built-in conventions as manifest tool
// mappings (basename matches), for registering into the imports resolver under
// --tool-conventions. Deterministic order (by match) for a stable resolver.
func ToolConventionTools() []manifest.Tool {
	names := make([]string, 0, len(builtinToolConventions))
	for n := range builtinToolConventions {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]manifest.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, manifest.Tool{Match: n, Label: builtinToolConventions[n].Label})
	}
	return out
}
