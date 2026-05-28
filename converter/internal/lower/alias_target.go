package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// lowerAliasTargets recovers cmake's `add_library(<alias> ALIAS
// <target>)` shape from the trace and emits Bazel-native
// `alias(name=, actual=)` rules so operator-written Bazel code
// (cross-package consumers, scripts, .bzl files) can reference
// either name interchangeably.
//
// cmake's File API codemodel-v2 omits ALIAS targets entirely —
// they're a name-level redirect that cmake resolves at configure
// time, so codemodel.targets[] only lists the underlying target.
// Bazel deps don't need the alias rule (the codemodel-recorded
// TargetDependency.Id resolves to the underlying target directly),
// but cross-tree consumers that hardcode the alias name break
// without it.
//
// Skipped shapes:
//
//   - Namespaced aliases (`Pkg::Target` — find_package's
//     IMPORTED-target surface). Bazel rejects `::` in target
//     names; namespaced consumers ride through the imports
//     manifest path, where the manifest's bazel_label field maps
//     `Pkg::Target` → the operator-supplied Bazel label.
//
//   - Aliases pointing at targets cmake doesn't expose (the
//     underlying name isn't in `knownTargets`). Bazel `alias`
//     requires a resolvable `actual` label; emitting a dangling
//     reference would just produce a Bazel load error.
//
//   - Re-declarations of names already in the codemodel
//     (`knownTargets[call.Name]`). The same name owned by both
//     the codemodel AND an ALIAS would produce a duplicate-target
//     diagnostic; conservative skip preserves the strict shape.
//
// cmakeSrc is the cmake source root used to reanchor absolute
// trace-recorded file paths to workspace-relative provenance,
// matching the reanchor pass the codemodel-driven lowerTarget
// performs.
//
// Returns nil when no usable ALIAS targets are found — empty
// input or all-namespaced/dangling/dup cases stay no-op.
func lowerAliasTargets(decoded *shadow.Decoded, knownTargets map[string]bool, cmakeSrc string) []ir.Target {
	if decoded == nil {
		return nil
	}
	var out []ir.Target
	emitted := map[string]bool{}
	for _, call := range decoded.AddLibraries {
		if call.Type != "ALIAS" {
			continue
		}
		if len(call.Aliases) == 0 {
			continue
		}
		actual := call.Aliases[0]
		// Bazel rejects `::` in target names; namespaced ALIAS
		// rides through the imports manifest path instead.
		if strings.Contains(call.Name, "::") {
			continue
		}
		// Underlying target must exist in the codemodel — Bazel
		// alias requires a resolvable `actual` label.
		if !knownTargets[actual] {
			continue
		}
		// Don't shadow a codemodel-emitted target of the same
		// name.
		if knownTargets[call.Name] {
			continue
		}
		// Dedup: cmake may declare the same alias from multiple
		// CMakeLists if the macro fans out.
		if emitted[call.Name] {
			continue
		}
		emitted[call.Name] = true
		// Reanchor source-tree-absolute file paths to
		// workspace-relative provenance.
		file := call.File
		if cmakeSrc != "" && filepath.IsAbs(file) {
			if rel, ok := relativeIfInside(cmakeSrc, file); ok {
				file = rel
			}
		}
		out = append(out, ir.Target{
			Name:        call.Name,
			Kind:        ir.KindAlias,
			AliasActual: ":" + actual,
			Provenance: ir.Provenance{
				File:    file,
				Line:    call.Line,
				Command: "add_library",
			},
		})
	}
	// Deterministic emit order — alphabetic by alias name.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
