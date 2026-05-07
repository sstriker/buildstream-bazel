package lower

import (
	"path"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

// fallbackStub pairs a per-target placeholder rule with the
// install-tree-relative path the extract genrule must
// produce so the rule's static_library / shared_library /
// srcs reference resolves. INTERFACE_LIBRARY targets carry an
// empty InstallPath since they're header-only and don't need
// extraction.
type fallbackStub struct {
	Target      ir.Target
	InstallPath string
}

// emitFallbackPlaceholder builds the placeholder ir.Package
// returned by ToIR when Phase B's
// UnsupportedExecuteProcessFallback flag is on and the
// classifier produced refusals.
//
// Shape (per
// docs/design/cmake-execute-process-round2-fallback.md):
//   - one extract genrule that untars install_tree.tar into
//     per-file outs derived from the codemodel
//   - per-target stubs dispatched on Target.Type:
//     STATIC_LIBRARY → cc_import + static_library; SHARED /
//     MODULE → cc_import + shared_library; EXECUTABLE →
//     sh_binary; INTERFACE_LIBRARY → cc_library hdrs-only;
//     UTILITY skipped.
//
// Path conventions: install paths derive from
// Target.Install.Destinations[0].Path + Target.NameOnDisk
// (e.g. STATIC_LIBRARY thelib with install destination "lib"
// and NameOnDisk "libthelib.a" → "install_tree/lib/libthelib.a").
// The "install_tree/" prefix names the extract genrule's output
// directory; downstream rules reference paths inside it.
//
// The extract genrule's src is the literal label
// "install_tree.tar". Resolution: when A's BUILD.bazel.out
// gets symlinked into Project B's package, the placeholder
// co-locates with B's install genrule, which produces
// install_tree.tar as one of its outs (Step 3, write-a side
// — wraps cmake configure + ninja + install under
// build-tracer + inline trace-publish). Same Bazel package =
// label resolution succeeds. convert-element's executor
// toolchain stays cmake-only; the build work lives in B.
//
// Targets with no Install block — utility, internal-only
// libraries, the project's private build artefacts — are
// omitted. Downstream consumers referencing such labels get a
// Bazel "label not found" error that's a clear signal: the
// target wasn't part of the install contract; either expose
// it via install() upstream or stop depending on it across
// the round-2 boundary.
//
// Marker tag cmake-codegen-execute-process-fallback flags every
// emitted rule for audit queries; cmake-codegen-execute-process-fallback-extract
// distinguishes the extract genrule from the per-target stubs
// in case operators want to query just the stubs.
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

	var stubs []fallbackStub

	for _, tref := range cfg.Targets {
		t, ok := r.Targets[tref.Id]
		if !ok {
			// Native render's main walk returns FileAPIMalformed
			// (a typed Tier-1 failure code) here; the fallback
			// intentionally relaxes that to a silent skip. The
			// goal in fallback mode is "emit the largest set of
			// resolvable labels we can", and turning a single
			// dangling ref into a Tier-1 failure would lose
			// every other target on the same codemodel —
			// defeating the whole point of the placeholder. The
			// same dangling ref still surfaces loudly the moment
			// the operator drops the fallback flag and re-runs
			// against the same reply (where the stricter native
			// walk takes over).
			continue
		}
		if t.IsGeneratorProvided {
			continue
		}
		// Skip targets without install destinations — they
		// aren't exposed across the round-2 boundary, so the
		// placeholder shouldn't claim them as labels.
		// INTERFACE_LIBRARY can be header-only and might not
		// have an Install block; we still surface those as a
		// header-only cc_library since downstream consumers
		// treat them as deps.
		if t.Type != "INTERFACE_LIBRARY" {
			if t.Install == nil || len(t.Install.Destinations) == 0 {
				continue
			}
		}

		base := ir.Target{
			Name: t.Name,
			Tags: []string{"cmake-codegen-execute-process-fallback"},
			// Public visibility for every stub — even
			// not-installed targets that native render would
			// have marked private. The goal in fallback mode is
			// label resolvability: any cross-element consumer
			// referring to `:thelib` should resolve regardless
			// of whether the upstream element installed the
			// target. This is a deliberate divergence from
			// native render's per-target visibility (where
			// Visibility comes from cmake's INTERFACE
			// declarations); operators who want native render's
			// visibility semantics drop the fallback flag.
			Visibility: []string{"//visibility:public"},
		}

		switch t.Type {
		case "STATIC_LIBRARY", "OBJECT_LIBRARY":
			rel := installPathFor(t)
			if rel == "" {
				continue
			}
			base.Kind = ir.KindCCImport
			base.StaticLibrary = rel
			// OBJECT_LIBRARY in native lowering sets
			// alwayslink=True so $<TARGET_OBJECTS:obj> link
			// contributions don't get pruned by Bazel's link
			// archiver. cc_import (used for the fallback's
			// installed-archive shape) doesn't expose an
			// alwayslink attribute — treating an OBJECT lib
			// as a static-archive cc_import is already a
			// type-shape change vs native; alwayslink would
			// be a no-op at the rule level. If a fixture
			// surfaces OBJECT_LIBRARY round-2-fallback link
			// breakage, the right fix is a wrapper rule
			// (cc_library + linkstatic + alwayslink wrapping
			// the cc_import), not a no-op attribute on
			// cc_import.
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: rel})
		case "SHARED_LIBRARY", "MODULE_LIBRARY":
			rel := installPathFor(t)
			if rel == "" {
				continue
			}
			base.Kind = ir.KindCCImport
			base.SharedLibrary = rel
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: rel})
		case "EXECUTABLE":
			rel := installPathFor(t)
			if rel == "" {
				continue
			}
			base.Kind = ir.KindShBinary
			base.Srcs = []string{rel}
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: rel})
		case "INTERFACE_LIBRARY":
			base.Kind = ir.KindCCInterface
			// Header-only: the InstallPath is empty for the
			// extract genrule's purposes; downstream
			// consumers wire hdrs separately (out-of-scope
			// for v1 placeholder).
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: ""})
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
	}

	// Emit one extract genrule wrapping every per-target
	// install path. cmake's tar archive layout mirrors
	// CMAKE_INSTALL_PREFIX; we strip the leading "/" and
	// place outputs under "install_tree/<dest>". The genrule's
	// cmd untars install_tree.tar into the package's output
	// dir under the install_tree/ subdirectory.
	if extract := buildExtractGenrule(stubs); extract != nil {
		pkg.Targets = append(pkg.Targets, *extract)
	}

	for _, s := range stubs {
		pkg.Targets = append(pkg.Targets, s.Target)
	}
	return pkg, nil
}

// installPathFor derives the package-relative path inside the
// extract genrule's outputs for a single target. Combines
// Target.Install.Destinations[0].Path (the dest directory,
// e.g. "lib") with Target.NameOnDisk (the artifact filename,
// e.g. "libthelib.a") under the "install_tree/" prefix.
//
// The first destination wins. cmake's install(TARGETS) can
// declare multiple destinations (ARCHIVE / LIBRARY / RUNTIME),
// but the codemodel doesn't tag them; the canonical artifact
// for a given target type lives in exactly one of those
// (.a → ARCHIVE / lib, .so → LIBRARY / lib, .exe → RUNTIME /
// bin), and projects that declare all three for symmetry list
// the same lib= path in each. v1 uses the first; real
// fixtures with diverging destinations can drive a per-type
// disambiguation.
//
// Returns "" if the target has no install destination or no
// NameOnDisk — caller skips emitting a stub for that target.
func installPathFor(t fileapi.Target) string {
	if t.Install == nil || len(t.Install.Destinations) == 0 {
		return ""
	}
	if t.NameOnDisk == "" {
		return ""
	}
	dest := t.Install.Destinations[0].Path
	dest = strings.TrimPrefix(dest, "/")
	return path.Join("install_tree", dest, t.NameOnDisk)
}

// buildExtractGenrule emits the single tar-extract genrule
// that produces every install path the per-target stubs
// reference. Returns nil when there are no extractable paths
// (every stub was INTERFACE_LIBRARY, which doesn't need
// extraction).
//
// The genrule reads "install_tree.tar" — a literal label that
// write-a wires to the appropriate source (project A's
// converter-genrule output vs a sibling install genrule). The
// extract cmd untars into $(@D)/install_tree, matching the
// "install_tree/" prefix the per-target paths share.
func buildExtractGenrule(stubs []fallbackStub) *ir.Target {
	var outs []string
	seen := map[string]bool{}
	for _, s := range stubs {
		if s.InstallPath == "" {
			continue
		}
		if seen[s.InstallPath] {
			continue
		}
		seen[s.InstallPath] = true
		outs = append(outs, s.InstallPath)
	}
	if len(outs) == 0 {
		return nil
	}
	return &ir.Target{
		Name:        "_install_tree_extract",
		Kind:        ir.KindGenrule,
		Srcs:        []string{"install_tree.tar"},
		GenruleOuts: outs,
		GenruleCmd: `mkdir -p "$(@D)/install_tree" && ` +
			`tar -C "$(@D)/install_tree" -xf "$(location install_tree.tar)"`,
		Tags: []string{
			"cmake-codegen-execute-process-fallback",
			"cmake-codegen-execute-process-fallback-extract",
		},
		Visibility: []string{"//visibility:private"},
	}
}
