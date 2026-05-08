package lower

import (
	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

// emitFallbackPlaceholder builds the placeholder ir.Package
// returned by ToIR when Phase B's
// UnsupportedExecuteProcessFallback flag is on and the
// classifier produced refusals. At this stage (Step 2 / PR
// #97), the placeholder enumerates every non-UTILITY codemodel
// target as an **empty** per-target stub — same Kind dispatch
// as native render (lowerTarget), same public visibility, but
// no srcs / hdrs / copts / deps. The BUILD analyzes cleanly,
// so downstream label references like `:thelib` / `:thetool`
// resolve, but compile/link actions against the stubs fail
// because nothing on the rule body claims an artifact path.
//
// Artifact wiring is the next step. Step 2.5 (queued behind IR
// support for cc_import.static_library / .shared_library)
// extends emitFallbackPlaceholder to:
//
//   - dispatch STATIC_LIBRARY / SHARED_LIBRARY → cc_import
//     with `static_library` / `shared_library` pointing at
//     install_tree.tar paths derived from
//     Target.Install.Destinations + Target.NameOnDisk;
//   - dispatch EXECUTABLE → sh_binary with srcs at the same
//     install_tree.tar paths;
//   - leave INTERFACE_LIBRARY as cc_library hdrs-only;
//   - emit a sister extract genrule that untars
//     install_tree.tar into the paths the stubs reference.
//
// After Step 2.5, downstream consumers' compile/link actions
// pull real artifacts through the placeholder. For Step 2
// the stubs are deliberately load-bearing only at the
// label-resolution layer; the artifact wiring follow-on is
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
			// Native render's main walk returns FileAPIMalformed
			// here; the fallback intentionally relaxes that to
			// a silent skip. The goal in fallback mode is "emit
			// the largest set of resolvable labels we can", and
			// turning a single dangling ref into a Tier-2 failure
			// would lose every other target on the same
			// codemodel — defeating the whole point of the
			// placeholder. The same dangling ref still surfaces
			// loudly the moment the operator drops the fallback
			// flag and re-runs against the same reply (where the
			// stricter native walk takes over).
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
			// grouping) have no Bazel equivalent — both native
			// render and the fallback skip them. Other unknown
			// types (target shapes the codemodel adds in newer
			// cmake versions before lower.go grows a case)
			// also fall here. Native render returns
			// UnsupportedTargetType for those; the fallback
			// instead silently drops them so the rest of the
			// codemodel's labels still resolve. Trade-off: an
			// element that depends on the dropped label fails
			// loudly at consumer build time rather than at
			// analysis. Operators who need analysis-time
			// strictness drop the fallback flag and rerun.
			continue
		}
		pkg.Targets = append(pkg.Targets, stub)
	}
	return pkg, nil
}
