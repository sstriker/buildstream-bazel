package lower

import (
	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

// emitFallbackPlaceholder builds the placeholder ir.Package
// returned by ToIR when Phase B's
// UnsupportedExecuteProcessFallback flag is on and the
// classifier produced refusals. The placeholder enumerates
// every non-UTILITY codemodel target as an empty per-target
// stub — same Kind dispatch as native render
// (lowerTarget), same public visibility, but no srcs / hdrs
// / copts / deps so the BUILD analyzes cleanly without
// committing to artifact paths.
//
// The intent is "labels resolve at analysis time so downstream
// consumers see :thelib / :thetool / etc." even though the
// rules are empty bodies. Step 2.5 (queued behind IR support
// for cc_import.static_library / .shared_library) wires the
// stubs to the round-2 install_tree.tar's per-target paths
// derived from Target.Install.Destinations + NameOnDisk —
// after which downstream consumers' compile actions can pull
// linkable artifacts through the placeholder. For Step 2 the
// stubs are deliberately load-bearing only at the
// label-resolution layer; the artifact-wiring follow-on is
// queued in docs/design/cmake-execute-process-round2-fallback.md
// "Staged implementation."
//
// Marker tag cmake-codegen-execute-process-fallback flags every
// stub for audit queries, so operators can see at a glance
// which BUILD.bazel.out files are placeholders rather than
// fully-converted packages.
func emitFallbackPlaceholder(r *fileapi.Reply, hostSrc string) (*ir.Package, error) {
	if got := len(r.Codemodel.Configurations); got != 1 {
		return nil, failure.New(failure.UnsupportedTargetType,
			"M1 supports exactly one configuration; got %d", got)
	}
	pkg := &ir.Package{
		Name:       projectName(r),
		SourceRoot: hostSrc,
	}
	cfg := r.Codemodel.Configurations[0]
	for _, tref := range cfg.Targets {
		t, ok := r.Targets[tref.Id]
		if !ok {
			// Same defensive check as ToIR's main walk; skip
			// silently rather than turning a placeholder
			// emission into a Tier-2 failure. A dangling target
			// ref is a real bug, but in fallback mode the goal
			// is "produce something analyzable"; the fully-
			// converted path's stricter validation catches the
			// bug there.
			continue
		}
		if t.IsGeneratorProvided {
			// ZERO_CHECK / INSTALL / PACKAGE / RUN_TESTS — IDE
			// integration; no Bazel equivalent.
			continue
		}
		stub := ir.Target{
			Name: t.Name,
			Tags: []string{"cmake-codegen-execute-process-fallback"},
			// Public visibility mirrors what native render
			// gives an installed target; downstream consumers
			// reference these labels exactly as they would
			// against a fully-converted element.
			Visibility: []string{"//visibility:public"},
		}
		switch t.Type {
		case "STATIC_LIBRARY", "OBJECT_LIBRARY", "SHARED_LIBRARY", "MODULE_LIBRARY":
			stub.Kind = ir.KindCCLibrary
			if t.Type == "STATIC_LIBRARY" {
				stub.Linkstatic = true
			}
		case "EXECUTABLE":
			stub.Kind = ir.KindCCBinary
		case "INTERFACE_LIBRARY":
			stub.Kind = ir.KindCCInterface
		default:
			// UTILITY targets (add_custom_target / dependency
			// grouping) and unknown types: skip. Native render
			// already filters UTILITY; we mirror that here so
			// the placeholder doesn't introduce labels native
			// render wouldn't have emitted.
			continue
		}
		pkg.Targets = append(pkg.Targets, stub)
	}
	return pkg, nil
}
