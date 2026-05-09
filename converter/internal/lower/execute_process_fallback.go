package lower

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

// fallbackStub pairs a per-target placeholder rule with the
// install-tree-relative paths the extract genrule must
// produce so the rule's static_library / shared_library /
// srcs / hdrs references resolve.
//
// InstallPath is the artifact (.a / .so / executable) install
// path; empty for INTERFACE_LIBRARY (header-only).
//
// HeaderPaths are FileSet HEADERS install paths derived from
// Target.FileSets where Type == "HEADERS" plus the matching
// Target.Sources entries (correlated via TargetSource.FileSetIndex).
// The list seeds both the extract genrule's outs and the per-target
// stub's hdrs.
type fallbackStub struct {
	Target      ir.Target
	InstallPath string
	HeaderPaths []string
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
			// Public visibility for every emitted stub. The
			// goal in fallback mode is label resolvability:
			// any cross-element consumer referring to
			// `:thelib` should resolve. Note this only applies
			// to targets we actually emit — non-INTERFACE
			// targets without an Install block are skipped
			// above (they aren't crossing the round-2
			// boundary). This is a deliberate divergence from
			// native render's per-target visibility (where
			// Visibility comes from cmake's INTERFACE
			// declarations); operators who want native render's
			// visibility semantics drop the fallback flag.
			Visibility: []string{"//visibility:public"},
		}

		hdrs := installHeadersFor(t)

		switch t.Type {
		case "STATIC_LIBRARY", "OBJECT_LIBRARY":
			rel := installPathFor(t)
			if rel == "" {
				continue
			}
			base.Kind = ir.KindCCImport
			base.StaticLibrary = rel
			base.Hdrs = hdrs
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
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: rel, HeaderPaths: hdrs})
		case "SHARED_LIBRARY", "MODULE_LIBRARY":
			rel := installPathFor(t)
			if rel == "" {
				continue
			}
			base.Kind = ir.KindCCImport
			base.SharedLibrary = rel
			base.Hdrs = hdrs
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: rel, HeaderPaths: hdrs})
		case "EXECUTABLE":
			rel := installPathFor(t)
			if rel == "" {
				continue
			}
			base.Kind = ir.KindShBinary
			base.Srcs = []string{rel}
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: rel, HeaderPaths: nil})
		case "INTERFACE_LIBRARY":
			base.Kind = ir.KindCCInterface
			base.Hdrs = hdrs
			// Header-only: the InstallPath is empty for the
			// extract genrule's artefact-side outs; the
			// HeaderPaths field is what carries the FileSet
			// headers into the genrule's outs list.
			stubs = append(stubs, fallbackStub{Target: base, InstallPath: "", HeaderPaths: hdrs})
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
// Returns "" if the target has no install destination, no
// NameOnDisk, or its install destination would escape the
// "install_tree/" prefix (an absolute path or one whose
// path.Clean form starts with "..") — caller skips emitting a
// stub for that target. The escape check matters because
// path.Join with "install_tree/" + "../lib" would silently
// resolve to "lib/...", which the extract genrule (which
// always extracts under "install_tree/") wouldn't satisfy
// and could push outs out of the genrule's declared output
// directory. Refusing such targets is safer than emitting
// stubs whose paths the genrule can never produce.
func installPathFor(t fileapi.Target) string {
	if t.Install == nil || len(t.Install.Destinations) == 0 {
		return ""
	}
	if t.NameOnDisk == "" {
		return ""
	}
	dest := t.Install.Destinations[0].Path
	// Reject absolute destinations BEFORE TrimPrefix so the
	// path.IsAbs check actually fires. Strip-then-check would
	// always pass (the TrimPrefix removes the leading "/" that
	// IsAbs needs to fire on POSIX paths). cmake's install
	// destinations are conventionally relative (e.g. "lib",
	// "include/foo"); an absolute one would either escape the
	// install_tree/ prefix on path.Join or stomp the genrule's
	// output dir, neither of which the extract genrule
	// produces.
	if path.IsAbs(dest) {
		return ""
	}
	dest = strings.TrimPrefix(dest, "/")
	// path.Clean collapses any `..` segments. After cleaning,
	// reject destinations that traverse upward — `..` alone or
	// anything starting with `../` (NOT plain `..foo` which
	// is a legitimate single-component name even though it
	// happens to start with two dots).
	cleaned := path.Clean(dest)
	if cleaned == "." {
		// destination was empty / "./"; place artefact
		// directly under install_tree/.
		cleaned = ""
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return path.Join("install_tree", cleaned, t.NameOnDisk)
}

// installHeadersFor enumerates the install-tree-relative
// paths of public headers for one target. Walks
// Target.FileSets where Type == "HEADERS" and Visibility ∈
// {"PUBLIC", "INTERFACE"} — PRIVATE FileSets are
// internal-only headers (target_sources(... FILE_SET
// HEADERS PRIVATE ...)) that aren't part of the install
// contract; surfacing them as cc_import.hdrs would expose
// internal-only headers to consumers AND have the extract
// genrule claim outs that install_tree.tar never produces.
// For each PUBLIC/INTERFACE HEADERS FileSet, walks
// Target.Sources looking for entries whose FileSetIndex
// points back at it; for each match, computes the path
// under the first matching BaseDirectory (iteration order
// of fs.BaseDirectories) and prefixes "install_tree/include/".
//
// The "install_tree/include/" convention reflects cmake's
// GNUInstallDirs default (CMAKE_INSTALL_INCLUDEDIR == "include").
// Projects that override this either via per-FileSet
// install destinations or non-default install prefixes will
// produce placeholder hdrs that don't match the actual
// install_tree.tar layout. The mismatch surfaces at consumer
// build time as a missing-header error rather than silently;
// a real fixture forcing the divergence drives codemodel
// FileSet install-destination support as a follow-on.
//
// Returns the deduped, sorted list of header install paths.
// Nil for targets without any PUBLIC/INTERFACE HEADERS-typed
// FileSet — those targets get an artefact-only stub.
//
// Known gap: a target that DECLARES PUBLIC/INTERFACE FILE_SET
// HEADERS via target_sources but doesn't actually pass those
// FileSets to install(TARGETS ... FILE_SET HEADERS) will
// still produce hdrs entries here — the function gates on
// FileSet visibility, not on whether the FileSet appears in
// the install contract. The extract genrule then declares
// outs that install_tree.tar won't carry, surfacing as a
// missing-out failure at A's build time. Honest behaviour
// vs. silently dropping the headers; a real fixture forcing
// the divergence drives FileSet install-membership lookup as
// a follow-on (Target.Install.FileSets / a codemodel field
// we don't yet decode).
func installHeadersFor(t fileapi.Target) []string {
	if len(t.FileSets) == 0 {
		return nil
	}
	headerSets := map[int]fileapi.TargetFileSet{}
	for i, fs := range t.FileSets {
		if fs.Type != "HEADERS" {
			continue
		}
		// Allowlist PUBLIC / INTERFACE explicitly rather than
		// blocklisting PRIVATE. Allowlist semantics make us
		// safe against new visibility values cmake may add to
		// the codemodel — an unknown visibility doesn't slip
		// through as "exposed" by default. PRIVATE is the
		// internal-only case (target_sources(... FILE_SET
		// HEADERS PRIVATE ...)); other unknown values that
		// might mean "internal but new" also drop out here.
		// Empty Visibility (older codemodel-v2 minor without
		// the field populated) also drops, which is the
		// conservative default.
		if fs.Visibility != "PUBLIC" && fs.Visibility != "INTERFACE" {
			continue
		}
		headerSets[i] = fs
	}
	if len(headerSets) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var hdrs []string
	for _, src := range t.Sources {
		if src.FileSetIndex == nil {
			continue
		}
		fs, ok := headerSets[*src.FileSetIndex]
		if !ok {
			continue
		}
		// src.Path is project-source-root-relative. Strip
		// the first matching BaseDirectory prefix (the
		// public header root) to get the install-relative
		// name; if no base dir matches, fall back to the
		// source basename — wrong but at least non-crashing.
		rel := stripFileSetBase(src.Path, fs.BaseDirectories, t.Paths.Source)
		if rel == "" {
			continue
		}
		installPath := path.Join("install_tree", "include", rel)
		if seen[installPath] {
			continue
		}
		seen[installPath] = true
		hdrs = append(hdrs, installPath)
	}
	sort.Strings(hdrs)
	return hdrs
}

// stripFileSetBase returns the relative path of src under the
// first BaseDirectory that contains it, in slash form. srcPath
// is project-source-root-relative; we resolve it to absolute
// form by joining with srcRoot when present, then ToSlash for
// cross-platform comparison. BaseDirectories from cmake's File
// API are recorded absolutely in practice, so the prefix match
// runs against absolute slash form on both sides. A
// hypothetical relative base dir falls through to the basename
// fallback (no Join against srcRoot is performed for bases —
// adding that needs a fixture pinning down what semantics
// cmake actually uses for relative bases, which we haven't
// observed in tree).
//
// When no base dir contains src, falls back to filepath.Base(src)
// — better than dropping the header entirely (the consumer
// would then have a broken hdrs reference at consumer build
// time, which surfaces the issue loudly rather than silently
// missing the header). The fallback is best-effort: if the
// project's install_tree.tar doesn't actually carry that
// basename at install_tree/include/<basename>, the consumer's
// build fails with a missing-file error pointing at the right
// path. Returns "" only when src is empty or has no basename.
func stripFileSetBase(srcPath string, baseDirs []string, srcRoot string) string {
	if srcPath == "" {
		return ""
	}
	abs := srcPath
	if !filepath.IsAbs(abs) && srcRoot != "" {
		abs = filepath.Join(srcRoot, abs)
	}
	abs = filepath.ToSlash(abs)
	for _, base := range baseDirs {
		if base == "" {
			continue
		}
		// Normalise to forward-slash form FIRST so
		// Windows-style paths (backslash separators) collapse
		// to slash before we trim the trailing separator.
		// TrimRight on `/` then handles both `path/` and
		// double-slash trailing forms uniformly. Trimming
		// before ToSlash would miss `path\` (TrimSuffix("/")
		// doesn't see the backslash) and produce `path/` →
		// double-slash prefix that never matches.
		baseTrim := strings.TrimRight(filepath.ToSlash(base), "/")
		if abs == baseTrim {
			return path.Base(abs)
		}
		prefix := baseTrim + "/"
		if strings.HasPrefix(abs, prefix) {
			return abs[len(prefix):]
		}
	}
	// No base dir matched — return the source basename so
	// the consumer at least gets `install_tree/include/<name>`
	// (which install_tree.tar typically does carry under cmake's
	// GNUInstallDirs default). A miss surfaces as a missing-
	// header error at consumer build time rather than silent
	// drop.
	return path.Base(filepath.ToSlash(srcPath))
}

// buildExtractGenrule emits the single tar-extract genrule
// that produces every install path the per-target stubs
// reference (artefacts + headers). Returns nil when there
// are no extractable paths.
//
// The genrule reads "install_tree.tar" — a literal label
// that resolves once A's BUILD.bazel.out is symlinked into
// Project B's package and co-locates with B's install
// genrule. The extract cmd untars into $(RULEDIR)/install_tree,
// matching the "install_tree/" prefix the per-target paths
// share. $(RULEDIR) (not $(@D)) so the base stays the package
// output root regardless of which `outs` entry comes first
// (a nested first out under install_tree/lib would make
// $(@D) point one level too deep).
func buildExtractGenrule(stubs []fallbackStub) *ir.Target {
	var outs []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		outs = append(outs, p)
	}
	for _, s := range stubs {
		add(s.InstallPath)
		for _, h := range s.HeaderPaths {
			add(h)
		}
	}
	if len(outs) == 0 {
		return nil
	}
	return &ir.Target{
		Name:        "_install_tree_extract",
		Kind:        ir.KindGenrule,
		Srcs:        []string{"install_tree.tar"},
		GenruleOuts: outs,
		GenruleCmd: `mkdir -p "$(RULEDIR)/install_tree" && ` +
			`tar -C "$(RULEDIR)/install_tree" -xf "$(location install_tree.tar)"`,
		Tags: []string{
			"cmake-codegen-execute-process-fallback",
			"cmake-codegen-execute-process-fallback-extract",
		},
		Visibility: []string{"//visibility:private"},
	}
}
