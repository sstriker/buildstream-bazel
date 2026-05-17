// Package lower converts a parsed CMake File API reply into the IR consumed by
// emit/. It is the conversion brain; most semantic decisions (rule kind
// classification, header discovery, flag splitting) live here.
//
// M1 scope: single-config (Release), single-language compile groups, no
// add_custom_command (genrule recovery is M2). Anything outside this scope
// returns a Tier-1 failure via failure.Error so the caller can surface it
// without aborting the conversion run.
package lower

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Options controls behavior that the orchestrator (M3) overrides per-package.
// M1 callers can pass the zero value.
type Options struct {
	// HostSourceRoot is the on-disk path to the source tree, used for
	// filesystem walks (e.g. header discovery under each include directory).
	// Defaults to the source path recorded in the File API codemodel.
	HostSourceRoot string

	// HostPrefixDir, when set, is the absolute on-disk path to the
	// synthesized prefix tree cmake configured against (CMAKE_PREFIX_PATH).
	// Codemodel link.commandFragments paths anchored at this dir are
	// remapped to manifestPrefixAnchor before manifest lookup so the
	// host-independent imports manifest matches regardless of where on
	// disk the prefix tree happens to live.
	HostPrefixDir string

	// Imports resolves out-of-tree imported targets (find_package-style
	// names that aren't part of the current codebase) to Bazel labels.
	// Optional; nil disables manifest lookup, in which case unresolved
	// link deps trigger unresolved-link-dep.
	Imports *manifest.Resolver

	// CTest, when non-nil, lets ToIR classify EXECUTABLE targets that
	// were registered via add_test() as cc_test rules. The registry is
	// produced by parsing CTestTestfile.cmake out of the cmake build
	// dir; nil means no test classification (every EXECUTABLE stays
	// cc_binary, matching pre-CTest-support behavior).
	CTest *ctest.Registry

	// BuildDir is the host-real path of the cmake build directory
	// where configured outputs (configure_file's targets, version
	// headers cmake writes at configure time, etc.) live on this
	// machine. Used together with TraceRaw to read the rendered
	// bytes of configure_file outputs and embed them into Bazel
	// genrules. Empty disables configure_file recovery.
	//
	// Production: the live cmake build dir convert-element-cmake just
	// configured.
	// Offline tests: the fixture dir, which record-fileapi.sh
	// stashes configured outputs into mirroring the build-dir
	// layout.
	BuildDir string

	// TraceRaw is cmake's --trace-expand --trace-format=json-v1
	// output (one JSON event per line). When non-empty, lower
	// uses it to surface signals the codemodel doesn't expose:
	//   - target_include_directories PUBLIC/PRIVATE arms
	//     (visibility delta — the codemodel flattens both
	//     into compileGroups[].includes[]).
	//   - target_link_libraries IMPORTED-target deps for
	//     STATIC libs (find-package STATIC delta — codemodel
	//     records nothing for static-lib link inputs).
	//   - configure_file(<input> <output> ...) input/output
	//     pairings (configure_file delta — cmakeFiles records
	//     the input only).
	//
	// Empty disables the trace-driven enrichment paths;
	// codemodel-only behavior matches pre-trace lower output.
	TraceRaw []byte

	// UnsupportedExecuteProcessFallback toggles
	// recoverExecuteProcess's refusal handling. When false
	// (the default — preserves Phase A behaviour),
	// classifier refusals (stamp / probe / unknown buckets)
	// produce a typed unsupported-execute-process Tier-1
	// failure. When true, refusals don't error; ToIR returns
	// a placeholder ir.Package whose targets are **empty
	// cc_library / cc_binary / cc_library-interface stubs**
	// (one per non-UTILITY codemodel target, public
	// visibility) so downstream label references still
	// resolve at analysis time. The per-target artifact
	// wiring (cc_import / sh_binary referencing install_tree.tar
	// paths reconstructed from Target.Install.Destinations
	// + NameOnDisk) lands in Step 2.5 — until then,
	// downstream consumers' compile/link actions against the
	// stubs fail. See
	// docs/design/cmake-execute-process-round2-fallback.md
	// for the architectural shape.
	UnsupportedExecuteProcessFallback bool

	// LiftConfigureFile toggles the configure_file recovery's
	// lifted shape. When true (and a values namespace is
	// available — either via CMakeVars below or via per-template
	// Extract recovery), the emitted genrule has the .h.in
	// template as a real srcs input and invokes
	// //tools:cmake-configure-file at Bazel build time. When
	// false (the default — preserves pre-lift behaviour for
	// callers that don't yet stage the tool), the legacy
	// base64-of-rendered-bytes cmd shape is always emitted. See
	// internal/configurefile package doc for the full lift
	// rationale + cache-key analysis.
	LiftConfigureFile bool

	// CMakeVars is the full cmake variable namespace captured
	// at end of configure (cmakerun.Reply.Vars). Used by the
	// configure_file lift as the values map for the lifted
	// genrule's Substitute, replacing per-template Extract.
	// With every cmake variable in hand, .h.in edits that
	// introduce new @VAR@/${VAR} markers always resolve
	// correctly without convert-element-cmake rerunning — closes the
	// soundness gap the per-template Extract had. Empty (e.g.
	// offline --reply-dir tests, or a configure that fatal-
	// erred before the dump-vars hook fired) falls back to
	// Extract per-template; if Extract also fails the lift
	// falls back to legacy.
	CMakeVars map[string]string
}

// manifestPrefixAnchor is the canonical token the orchestrator's imports
// manifest uses to anchor cross-element link paths (see
// orchestrator.sandboxPrefix). The token is virtual — no filesystem path
// of that name exists; lower remaps real prefix paths onto it before
// LookupLinkPath.
const manifestPrefixAnchor = "/opt/prefix/"

// Header file extensions we treat as `hdrs` candidates when walking include
// directories. Lowercase comparison.
var headerExts = map[string]bool{
	".h":   true,
	".hh":  true,
	".hpp": true,
	".hxx": true,
	".inl": true,
}

// ToIR lowers a parsed reply into a Package. The optional ninja graph
// enables genrule recovery for targets with isGenerated sources; pass nil to
// disable (M1-style behavior — generated sources then trigger
// unsupported-custom-command).
func ToIR(r *fileapi.Reply, g *ninja.Graph, opts Options) (*ir.Package, error) {
	if got := len(r.Codemodel.Configurations); got != 1 {
		return nil, failure.New(failure.UnsupportedTargetType,
			"M1 supports exactly one configuration; got %d", got)
	}
	cfg := r.Codemodel.Configurations[0]

	// Pre-parse trace records once so lowerTarget can consult
	// per-target maps without re-walking the trace bytes per
	// target. Empty TraceRaw → empty maps, no behavior change.
	//
	// knownTargets lets the trace extractors keep calls that
	// originate in producer-element cmake macros (outside the
	// consumer source tree) but act on consumer-defined
	// targets — the macro-from-import case. Without this,
	// inSourceTree alone would drop those calls and lower
	// would lose visibility/link-libs information they
	// contribute.
	knownTargets := map[string]bool{}
	for _, tref := range cfg.Targets {
		knownTargets[tref.Name] = true
	}

	var privateIncludeDirs map[string]map[string]bool // target → set of absolute private dir paths
	var traceLinkLibs map[string][]string             // target → ordered list of cmake lib names from target_link_libraries (all visibility arms, dedup-preserved)
	// traceLinkScope maps target → libName → cmake keyword
	// ("PUBLIC", "PRIVATE", "INTERFACE", or "" for the legacy
	// keyword-less positional shape — which cmake treats as
	// PUBLIC). Populated alongside traceLinkLibs from the same
	// shadow.Decode pass; consumed by lowerTarget to route
	// PRIVATE deps to ir.Target.ImplementationDeps per Phase 4
	// (parsed-from-trace signal).
	//
	// First-write-wins on duplicate (target, lib) pairs across
	// arms: cmake itself allows multiple target_link_libraries
	// calls with conflicting keywords, but the upstream-most
	// scope governs propagation. The codemodel's
	// commandFragments resolve to the same final link line
	// regardless; the keyword recovery here is best-effort.
	var traceLinkScope map[string]map[string]string
	// traceDecoded tracks whether shadow.Decode ran; when true,
	// decodedConfigureFiles holds the configure_file extractions
	// from that single pass and the configure_file recovery
	// reuses them rather than re-parsing the trace. A
	// nil-slice sentinel wouldn't suffice — a trace with zero
	// configure_file events leaves the slice nil, which would
	// otherwise look identical to "decode never ran".
	var traceDecoded bool
	var decodedConfigureFiles []shadow.ConfigureFileCall
	var decodedFileGenerates []shadow.FileGenerateCall
	var decodedExecuteProcesses []shadow.ExecuteProcessCall
	if len(opts.TraceRaw) > 0 {
		cmakeSrcForTrace := r.Codemodel.Paths.Source
		decoded := shadow.Decode(opts.TraceRaw, cmakeSrcForTrace, knownTargets)
		traceDecoded = true
		privateIncludeDirs = map[string]map[string]bool{}
		for _, call := range decoded.Includes {
			for _, grp := range call.Groups {
				if grp.Visibility != "PRIVATE" {
					continue
				}
				if _, ok := privateIncludeDirs[call.Target]; !ok {
					privateIncludeDirs[call.Target] = map[string]bool{}
				}
				for _, dir := range grp.Dirs {
					privateIncludeDirs[call.Target][dir] = true
				}
			}
		}
		traceLinkLibs = map[string][]string{}
		traceLinkScope = map[string]map[string]string{}
		for _, call := range decoded.Links {
			seen := map[string]bool{}
			if _, ok := traceLinkScope[call.Target]; !ok {
				traceLinkScope[call.Target] = map[string]string{}
			}
			for _, grp := range call.Groups {
				for _, lib := range grp.Libs {
					if seen[lib] {
						continue
					}
					seen[lib] = true
					traceLinkLibs[call.Target] = append(traceLinkLibs[call.Target], lib)
					// First-write-wins so an earlier
					// target_link_libraries(t PUBLIC bar) call
					// doesn't get overwritten by a later
					// target_link_libraries(t PRIVATE bar) — the
					// effective semantics in cmake itself are
					// not well-defined when the same library is
					// listed twice with different keywords, but
					// the upstream-most call governs header
					// propagation in the typical case.
					if _, ok := traceLinkScope[call.Target][lib]; !ok {
						traceLinkScope[call.Target][lib] = grp.Visibility
					}
				}
			}
		}
		decodedConfigureFiles = decoded.ConfigFiles
		decodedFileGenerates = decoded.FileGenerates
		decodedExecuteProcesses = decoded.ExecuteProcesses
	}

	cmakeSrc := r.Codemodel.Paths.Source
	cmakeBuild := r.Codemodel.Paths.Build
	hostSrc := opts.HostSourceRoot
	if hostSrc == "" {
		hostSrc = cmakeSrc
	}
	// hostSrcOnDisk gates the per-source existence check used for
	// the #209 missing-source elision. Reply-dir-only replay runs
	// (golden tests, offline fixtures) point hostSrc at a path the
	// recording machine had but this host doesn't, and the elision
	// against an absent root would drop every source. Stat once
	// here; the loop reads the bool.
	hostSrcOnDisk := false
	if info, statErr := os.Stat(hostSrc); statErr == nil && info.IsDir() {
		hostSrcOnDisk = true
	}

	pkg := &ir.Package{
		Name:       projectName(r),
		SourceRoot: hostSrc,
	}

	cc := newCodegenContext()

	// execute_process recovery. Configure-time subprocess
	// invocations are a hermeticity violation by Bazel's
	// analysis-then-action model. Some calls are liftable
	// (cmake -E builtins; file-producing tools with declared
	// OUTPUT_FILE — translated to build-time genrules) and some
	// aren't (version stamps, toolchain probes, opaque
	// pipelines).
	//
	// Disposition of the unliftable bucket depends on
	// opts.UnsupportedExecuteProcessFallback:
	//   - off (Phase A): aggregate into a single typed
	//     unsupported-execute-process Tier-1 failure so
	//     projects with several offending calls get one
	//     triage report rather than N converter runs
	//     uncovering them one at a time.
	//   - on (Phase B fallback): emitFallbackPlaceholder
	//     enumerates every non-UTILITY codemodel target as
	//     an empty cc_library / cc_binary / cc_library-
	//     interface stub so downstream label references
	//     resolve at analysis time. Step 2.5 (PR #98)
	//     extends the placeholder to wire those stubs to
	//     Target.Install.Destinations via cc_import /
	//     sh_binary; this PR (Step 2) only delivers the
	//     analysis-time label-resolution shape.
	//
	// Liftable buckets always append to cc.Genrules +
	// cc.OutToGenrule before lowerTarget runs so consumer
	// attribution can attach generated artefacts to cc
	// targets that include the build dir. The
	// []executeProcessOut return is parallel to
	// configureFileOut and feeds the same per-target
	// attribution loop in lowerTarget below.
	executeProcesses, executeProcessRefusals := recoverExecuteProcess(decodedExecuteProcesses, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, cc)
	if len(executeProcessRefusals) > 0 {
		if !opts.UnsupportedExecuteProcessFallback {
			return nil, formatExecuteProcessFailure(executeProcessRefusals)
		}
		// Phase B fallback: emit a placeholder ir.Package
		// rather than continuing into the native lowering
		// path. The native path would either redo the
		// refusal analysis or trip on the unliftable call
		// later in lowerTarget; the placeholder is the
		// cleaner cut, and it lets downstream consumers see
		// per-target labels at analysis time even when the
		// element itself can't be fine-converted.
		return emitFallbackPlaceholder(r, hostSrc)
	}

	// Recover configure_file outputs from trace before lowering
	// targets. Each call surfaces as an ir.Target{KindGenrule}
	// on cc.Genrules; the returned slice tells lowerTarget which
	// recorded outputs (and their build-dir-relative paths) to
	// attach to consuming targets that include the cmake build
	// dir in their codemodel-recorded Includes.
	var configureFiles []configureFileOut
	if traceDecoded {
		var err error
		configureFiles, err = recoverConfigureFilesFromCalls(decodedConfigureFiles, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, cc)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		configureFiles, err = recoverConfigureFiles(opts.TraceRaw, hostSrc, opts.BuildDir, cmakeSrc, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, cc)
		if err != nil {
			return nil, err
		}
	}

	// file(GENERATE) recovery — sister to configure_file's lift.
	// Outputs land in cc.Genrules / cc.OutToGenrule before
	// lowerTarget runs; the returned slice feeds lowerTarget's
	// consumer-attribution loop the same way configureFiles does.
	// Reuses opts.LiftConfigureFile as the lift opt-in: both
	// lifters require the same Bazel-time tool (//tools:cmake-
	// configure-file) and the same cmakeVars namespace dump, so
	// flipping them together keeps the operator's flag surface
	// minimal. Only the decoded-trace path drives this — the
	// pre-trace fallback that re-parses opts.TraceRaw doesn't
	// surface a file(GENERATE) extractor today (none of the
	// pre-Decode callers need it).
	var fileGenerates []fileGenerateOut
	if traceDecoded {
		var err error
		fileGenerates, err = recoverFileGenerate(decodedFileGenerates, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, buildGenexTargets(r, cmakeBuild), opts.Imports, cc)
		if err != nil {
			return nil, err
		}
	}

	// Build the in-codebase id -> Bazel-rule-name map up front so dep
	// lowering can map t.Dependencies[].Id to a label without re-walking
	// configurations. UTILITY targets (add_custom_target nodes) are
	// excluded — they have no Bazel equivalent, so depending on them is a
	// no-op; the underlying add_custom_command's outputs are referenced
	// via srcs/hdrs instead. utilityIDs records them separately so dep
	// resolution can distinguish "skip utility" from "unresolved".
	idToName := map[string]string{}
	utilityIDs := map[string]bool{}
	for _, tref := range cfg.Targets {
		if t, ok := r.Targets[tref.Id]; ok && t.Type == "UTILITY" {
			utilityIDs[tref.Id] = true
			continue
		}
		idToName[tref.Id] = tref.Name
	}

	for _, tref := range cfg.Targets {
		t, ok := r.Targets[tref.Id]
		if !ok {
			return nil, failure.New(failure.FileAPIMalformed,
				"target id %q in codemodel but not loaded", tref.Id)
		}
		irt, err := lowerTarget(&t, cmakeSrc, cmakeBuild, hostSrc, opts.HostPrefixDir, hostSrcOnDisk, g, cc, idToName, utilityIDs, opts.Imports, opts.CTest, privateIncludeDirs[tref.Name], traceLinkLibs[tref.Name], traceLinkScope[tref.Name], configureFiles, fileGenerates, executeProcesses)
		if err != nil {
			return nil, err
		}
		if irt == nil {
			// lowerTarget returned (nil, nil) to skip — UTILITY targets
			// (add_custom_target nodes) and similar that have no Bazel
			// equivalent. Also fires when an EXECUTABLE was rewritten
			// into one or more cc_test entries on cc.Tests.
			continue
		}
		pkg.Targets = append(pkg.Targets, *irt)
	}

	// Append recovered genrules then per-language sub-libraries
	// then cc_test rules in deterministic order; each slot is
	// appended in target-walk order during lowerTarget, which is
	// itself stable.
	pkg.Targets = append(pkg.Targets, cc.Genrules...)
	pkg.Targets = append(pkg.Targets, cc.Subs...)
	pkg.Targets = append(pkg.Targets, cc.Tests...)
	return pkg, nil
}

func projectName(r *fileapi.Reply) string {
	if e := r.Cache.Get("CMAKE_PROJECT_NAME"); e != nil {
		return e.Value
	}
	return ""
}

func lowerTarget(t *fileapi.Target, cmakeSrc, cmakeBuild, hostSrc, hostPrefix string, hostSrcOnDisk bool, g *ninja.Graph, cc *codegenContext, idToName map[string]string, utilityIDs map[string]bool, imports *manifest.Resolver, tests *ctest.Registry, privateIncludeDirs map[string]bool, traceLinkLibs []string, traceLinkScope map[string]string, configureFiles []configureFileOut, fileGenerates []fileGenerateOut, executeProcesses []executeProcessOut) (*ir.Target, error) {
	// Generator-provided targets (ZERO_CHECK, INSTALL, PACKAGE,
	// RUN_TESTS, etc.) are inserted by cmake itself for IDE
	// integration and have no Bazel equivalent. Skip them silently.
	if t.IsGeneratorProvided {
		return nil, nil
	}

	irt := &ir.Target{Name: t.Name}

	switch t.Type {
	case "STATIC_LIBRARY":
		irt.Kind = ir.KindCCLibrary
		irt.Linkstatic = true
	case "OBJECT_LIBRARY":
		// cmake OBJECT libs compile sources to .o without
		// archiving. Consumers reference $<TARGET_OBJECTS:t>
		// to inline the objects into a downstream artifact.
		// Bazel analog: cc_library with alwayslink=True so
		// the objects always link into transitive consumers
		// (matches cmake's "every consumer drags every
		// object" semantics). linkstatic stays false — there's
		// no archive; alwayslink is what carries the inline
		// behavior.
		irt.Kind = ir.KindCCLibrary
		irt.Alwayslink = true
	case "SHARED_LIBRARY", "MODULE_LIBRARY":
		irt.Kind = ir.KindCCLibrary
	case "EXECUTABLE":
		irt.Kind = ir.KindCCBinary
	case "INTERFACE_LIBRARY":
		irt.Kind = ir.KindCCInterface
	case "UTILITY":
		// add_custom_target / add_dependencies grouping. The underlying
		// add_custom_command is recovered separately via genrule lookup;
		// the utility node itself has no Bazel equivalent.
		return nil, nil
	default:
		return nil, failure.New(failure.UnsupportedTargetType,
			"target %q has unsupported type %q", t.Name, t.Type)
	}

	// Build the source-index → in-compile-group set once per target.
	// Walking t.CompileGroups[].SourceIndexes per source visit was
	// O(N*M) where N is sources and M is compile-group entries; for
	// large targets (mesa, gcc, clang) that's the dominant cost in
	// lowerTarget.
	inCompileGroup := buildCompileGroupSet(t)

	consumesCodegen := false
	elidedBuildDirSrc := false
	elidedMissingSrc := false
	elidedCompilerArtifact := false
	for i, src := range t.Sources {
		// CMake's bookkeeping `<build>/version.h.rule` files are internal
		// re-run markers; skip them silently.
		if strings.HasSuffix(src.Path, ".rule") {
			continue
		}

		if src.IsGenerated {
			// $<TARGET_OBJECTS:other_target> shows up in
			// codemodel as "<build>/CMakeFiles/<other>.dir/<src>.o"
			// generated sources. The other target's compile is
			// already captured as its own cc_library; the
			// inlining relationship surfaces via t.Dependencies
			// + the OBJECT lib's alwayslink=True. Skip these
			// here — passing them through recoverGenrule would
			// fail because cmake's C compile rule isn't a
			// CUSTOM_COMMAND.
			if isTargetObjectsRef(src.Path, cmakeBuild, idToName) {
				continue
			}
			// Unity builds and other compile-rule-produced .o
			// files appear as generated sources but aren't
			// CUSTOM_COMMAND outputs — they're cc-compile
			// artifacts already captured by the same target's
			// compile group. Skip silently with an audit tag so
			// the recoverGenrule call below doesn't refuse them;
			// #206. Conservative gate: must look like a compile
			// artifact (.o/.obj under CMakeFiles/<x>.dir) AND
			// the producing ninja rule must be a known compiler
			// rule shape. Either signal alone is too permissive.
			if isCompilerObjectArtifact(src.Path, cmakeBuild, g) {
				elidedCompilerArtifact = true
				continue
			}
			relOut, _, err := cc.recoverGenrule(src.Path, cmakeSrc, cmakeBuild, g)
			if err != nil {
				return nil, err
			}
			consumesCodegen = true
			ext := strings.ToLower(filepath.Ext(relOut))
			switch {
			case inCompileGroup[i]:
				irt.Srcs = append(irt.Srcs, relOut)
			case headerExts[ext]:
				irt.Hdrs = append(irt.Hdrs, relOut)
			default:
				// Non-header, not compiled: still belongs in srcs so the
				// genrule's output is included in the package's input set.
				irt.Srcs = append(irt.Srcs, relOut)
			}
			continue
		}

		if !inCompileGroup[i] {
			// Not assigned to a compile group: probably a header in
			// target_sources(); we'll discover hdrs via include-dir
			// walking below. Skip here.
			continue
		}
		// Configure-time-created files living under the build dir
		// (e.g. `file(WRITE ${CMAKE_BINARY_DIR}/dummy.cpp "")` +
		// `add_library(foo ${CMAKE_BINARY_DIR}/dummy.cpp)` — a
		// common header-only-library shim) get recorded with an
		// absolute path under cmakeBuild but without the
		// IsGenerated flag (cmake only sets that for sources
		// produced by add_custom_command / configure_file etc.).
		// Their absolute paths point at this run's tmp build dir,
		// which is gone before Bazel ever executes the rule —
		// emitting them produces a cc_library whose srcs label
		// resolves to a nonexistent file. Drop them silently and
		// tag the consuming target so audit queries can find the
		// elided sources; downstream the cc_library renders with
		// the remaining (real) sources, or hdrs-only if this was
		// the only source.
		//
		// Scope: the elision is intentionally build-dir-specific.
		// Absolute paths under cmakeSrc (or any other root) fall
		// through to the append below; Bazel rejects absolute
		// labels at load time with a clear error, which is the
		// right surfacing for "consumer named a source outside
		// its package" — a different bug shape that an audit-tag
		// silent-drop would obscure.
		if cmakeBuild != "" && filepath.IsAbs(src.Path) {
			if _, inside := relativeIfInside(cmakeBuild, src.Path); inside {
				elidedBuildDirSrc = true
				continue
			}
		}
		// Confirm the source actually exists on disk at convert
		// time. cmake's target model lists sources as add_executable
		// / add_library / target_sources(...) receive them, without
		// checking existence; a static path can legitimately enter
		// the model and survive configure even when the file isn't
		// in the source tree the converter sees (e.g. the producer's
		// tarball pruned the tests/ subtree but kept the
		// add_executable(test_x tests/...) entry). Letting them
		// through emits a BUILD whose `srcs = ["tests/x.cpp"]`
		// label Bazel rejects at build time with "missing input
		// file". Drop the missing source here with an audit tag
		// so the surviving cc_library still builds; #209.
		//
		// The check is gated on hostSrcOnDisk because reply-dir-only
		// runs (golden tests, offline replay) point cmakeSrc at a
		// path the recording machine had but this host doesn't, and
		// elision against that absent root would drop every source.
		if hostSrcOnDisk {
			onDisk := src.Path
			if !filepath.IsAbs(onDisk) {
				onDisk = filepath.Join(hostSrc, src.Path)
			}
			if _, statErr := os.Stat(onDisk); statErr != nil && errors.Is(statErr, fs.ErrNotExist) {
				elidedMissingSrc = true
				continue
			}
		}
		irt.Srcs = append(irt.Srcs, src.Path)
	}
	if consumesCodegen {
		irt.Tags = append(irt.Tags, "has-cmake-codegen")
	}
	if elidedBuildDirSrc {
		irt.Tags = append(irt.Tags, "cmake-elided-build-dir-source")
	}
	if elidedMissingSrc {
		irt.Tags = append(irt.Tags, "cmake-elided-missing-source")
	}
	if elidedCompilerArtifact {
		irt.Tags = append(irt.Tags, "cmake-elided-compiler-artifact")
	}

	// Build-dir-rooted includes (relative to the cmake build
	// dir). Populated alongside the source-tree includes in
	// the CompileGroup walk; used afterward for configure_file
	// consumer attribution. Empty for targets that don't
	// include any build-dir path — they consume no
	// configure_file outputs.
	targetBuildIncs := map[string]bool{}

	if len(t.CompileGroups) > 0 {
		// M1 assumption: at most one language per target. Aggregate the
		// first compile group's flags/includes/defines.
		cg := t.CompileGroups[0]
		copts, defs := splitCompileFragments(cg.CompileCommandFragments)
		irt.Copts = copts

		for _, d := range cg.Defines {
			defs = append(defs, d.Define)
		}
		irt.Defines = defs

		// Dedup includes: cmake's codemodel emits one entry per
		// PUBLIC include propagation, so a target whose own
		// target_include_directories names "include" plus a PUBLIC
		// dep that also names "include" surfaces with two identical
		// entries. The emitter sorts but doesn't dedup; bazel
		// accepts duplicates but they're cosmetic noise. Dedup-while-
		// preserving-order at IR-build time so any downstream
		// consumer of irt.Includes sees a clean list.
		//
		// PRIVATE-include partition: when a trace record (loaded
		// in ToIR) marks an absolute include path as PRIVATE for
		// this target, the dir flows into the cc_library's compile-
		// only `copts = ["-I<dir>"]` rather than the
		// consumer-visible `includes` attribute. cmake's PUBLIC
		// keyword propagates to consumers; PRIVATE doesn't —
		// Bazel's `includes` is consumer-visible by default, so
		// PRIVATE has to ride on -I in copts to preserve
		// encapsulation.
		seenInc := map[string]bool{}
		for _, inc := range cg.Includes {
			if rel, ok := relativeIfInsideRelaxed(cmakeBuild, inc.Path); ok {
				// Build-dir include — codemodel records this
				// for targets that target_include_directories'd
				// $<BUILD_INTERFACE:${CMAKE_CURRENT_BINARY_DIR}>
				// (typical configure_file consumer pattern).
				// Track for the configure_file consumer
				// attribution below; don't surface in
				// irt.Includes (source-tree-relative only).
				targetBuildIncs[rel] = true
				continue
			}
			rel, ok := relativeIfInside(cmakeSrc, inc.Path)
			if !ok {
				continue
			}
			if seenInc[rel] {
				continue
			}
			seenInc[rel] = true
			if privateIncludeDirs[inc.Path] {
				// Compile-only — don't propagate to consumers.
				irt.Copts = append(irt.Copts, "-I"+rel)
				continue
			}
			irt.Includes = append(irt.Includes, rel)
		}
	}

	// configure_file consumer attribution. Any target whose
	// codemodel-recorded includes contain the cmake build dir
	// (or a subdir thereof) is a candidate consumer of
	// configure_file outputs that landed inside that include
	// path. We add each matching output to the target's hdrs;
	// the genrule that produces it is already on cc.Genrules
	// from recoverConfigureFiles. cmake-codegen tag mirrors the
	// existing CUSTOM_COMMAND-recovered consumer's shape.
	if len(configureFiles) > 0 && len(targetBuildIncs) > 0 {
		var addedHdrs []string
		for _, cfgOut := range configureFiles {
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, cfgOut.RelOutput) {
					addedHdrs = append(addedHdrs, cfgOut.RelOutput)
					break
				}
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

	// file(GENERATE) consumer attribution. Sister block to the
	// configure_file walk above; file(GENERATE) outputs can be
	// any extension (config-shaped .h, license blobs, generated
	// source, .json manifests, etc.) so we branch on extension
	// like the execute_process walk below — headers go to hdrs,
	// other artefacts go to srcs.
	if len(fileGenerates) > 0 && len(targetBuildIncs) > 0 {
		var addedHdrs, addedSrcs []string
		seenHdr := map[string]bool{}
		seenSrc := map[string]bool{}
		for _, fg := range fileGenerates {
			match := false
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, fg.RelOutput) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			ext := strings.ToLower(filepath.Ext(fg.RelOutput))
			if headerExts[ext] {
				if !seenHdr[fg.RelOutput] {
					seenHdr[fg.RelOutput] = true
					addedHdrs = append(addedHdrs, fg.RelOutput)
				}
				continue
			}
			if !seenSrc[fg.RelOutput] {
				seenSrc[fg.RelOutput] = true
				addedSrcs = append(addedSrcs, fg.RelOutput)
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
		}
		if len(addedSrcs) > 0 {
			irt.Srcs = append(irt.Srcs, addedSrcs...)
		}
		if len(addedHdrs) > 0 || len(addedSrcs) > 0 {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

	// execute_process consumer attribution. Sister block to the
	// configure_file walk above, applied to the recovered
	// execute_process outputs (cmake -E touch / copy /
	// copy_if_different + the file-producing hoist's
	// OUTPUT_FILE). Without this, a target that
	// target_include_directories'd ${CMAKE_CURRENT_BINARY_DIR}
	// and #includes (or #compiles) a file an
	// execute_process(... OUTPUT_FILE generated.h) call produces
	// would resolve at analysis time but fail at action time —
	// the genrule landed on cc.Genrules but no cc target's
	// hdrs/srcs referenced it, so Bazel's sandbox wouldn't stage
	// the recovered artefact for the consumer's compile. Branch
	// on extension so headers go to hdrs (publicly exposed) and
	// other generated artefacts go to srcs (private compile
	// inputs); same shape as the IsGenerated branch in the
	// CompileGroup walk above.
	if len(executeProcesses) > 0 && len(targetBuildIncs) > 0 {
		var addedHdrs, addedSrcs []string
		seenHdr := map[string]bool{}
		seenSrc := map[string]bool{}
		for _, ep := range executeProcesses {
			match := false
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, ep.RelOutput) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			ext := strings.ToLower(filepath.Ext(ep.RelOutput))
			if headerExts[ext] {
				if !seenHdr[ep.RelOutput] {
					seenHdr[ep.RelOutput] = true
					addedHdrs = append(addedHdrs, ep.RelOutput)
				}
				continue
			}
			if !seenSrc[ep.RelOutput] {
				seenSrc[ep.RelOutput] = true
				addedSrcs = append(addedSrcs, ep.RelOutput)
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
		}
		if len(addedSrcs) > 0 {
			irt.Srcs = append(irt.Srcs, addedSrcs...)
		}
		if len(addedHdrs) > 0 || len(addedSrcs) > 0 {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

	hdrs, err := discoverHeaders(hostSrc, irt.Includes, cc.HeaderWalkCache)
	if err != nil {
		return nil, err
	}
	// FileSets give cmake's authoritative list of HEADERS-typed sources
	// declared via `target_sources(... FILE_SET HEADERS ...)`. The
	// filesystem walk above catches anything under irt.Includes, but a
	// FILE_SET can name a header outside any declared include dir;
	// merging the two lists ensures those don't get dropped. Sources
	// without a FileSetIndex (the common case in M1 fixtures) leave
	// fileSetHdrs empty and behavior matches the pre-FileSets path.
	var fileSetHdrs []string
	if len(t.FileSets) > 0 {
		for _, src := range t.Sources {
			if src.FileSetIndex == nil {
				continue
			}
			idx := *src.FileSetIndex
			if idx < 0 || idx >= len(t.FileSets) {
				continue
			}
			if t.FileSets[idx].Type != "HEADERS" {
				continue
			}
			fileSetHdrs = append(fileSetHdrs, src.Path)
		}
	}
	merged := append(irt.Hdrs, hdrs...)
	merged = append(merged, fileSetHdrs...)
	sort.Strings(merged)
	irt.Hdrs = dedupeStrings(merged)

	// Lower dependencies. In-codebase target ids look like `<name>::@<hash>`
	// where <name> is the CMake target name; out-of-tree find_package-
	// imported targets carry a namespaced name like `Pkg::tgt::@<hash>`.
	// Resolution order:
	//
	//   1. In-codebase non-UTILITY target -> ":<name>"
	//   2. In-codebase UTILITY target -> skip silently (no Bazel equivalent)
	//   3. CMake target name in imports manifest -> bazel_label
	//   4. Otherwise -> Tier-1 unresolved-link-dep.
	//
	// Phase 4 enrichment: when traceLinkScope (the cmake-side
	// PUBLIC/PRIVATE/INTERFACE keyword recovered by shadow.Decode
	// from the trace) records this dep as PRIVATE, route the
	// resolved Bazel label to irt.ImplementationDeps rather than
	// irt.Deps — matching `implementation_deps = [...]` on the
	// emitted cc_library, which prevents the dep's headers from
	// propagating to consumers. Unknown scope (no trace, or the
	// dep wasn't named in any target_link_libraries call —
	// possible for cmake-generated dependency edges that aren't
	// user-authored, like cyclical-static-archive helpers) falls
	// through to the historical `irt.Deps` routing — strictly
	// safe (PUBLIC default forwards headers, which is the cmake
	// default for non-keyword target_link_libraries usage).
	//
	// Rule-kind gating: only cc_library supports
	// `implementation_deps` in stock rules_cc. cc_binary /
	// cc_test / cc_import don't accept the attribute, and bazel
	// rejects it at analysis time. For non-library kinds the
	// scope distinction is moot — a binary has no consumers
	// (it's a leaf), and a cc_import wraps a pre-built archive
	// whose link interface is fixed at build time — so fold
	// PRIVATE deps into `irt.Deps` for those kinds.
	allowsImplementationDeps := irt.Kind == ir.KindCCLibrary
	// Issue #194: cmake's codemodel can report the same
	// dependency twice — e.g. a target referenced by
	// target_link_libraries with both INTERFACE and PRIVATE
	// visibility, or referenced by separate
	// target_link_libraries invocations. Without a seen-set,
	// both entries land in irt.Deps and the rendered BUILD.bazel
	// carries a duplicate label that Bazel rejects ("Label '...'
	// is duplicated in the 'deps' attribute"). The Link.CommandFragments
	// loop below already has a seen-set spanning Deps +
	// ImplementationDeps (to avoid cross-bucket dupes); this
	// one covers within-loop duplicates that come straight from
	// t.Dependencies. Both maps fold into the same routing-
	// decision shape so a dep already in either bucket isn't
	// re-appended.
	seenDep := map[string]bool{}
	for _, dep := range t.Dependencies {
		// Resolve label; routing decision (Deps vs
		// ImplementationDeps) folds in after.
		var label string
		if name, ok := idToName[dep.Id]; ok {
			label = ":" + name
		} else if utilityIDs[dep.Id] {
			continue
		} else {
			cmakeName := stripIDHash(dep.Id)
			if export := imports.LookupCMakeTarget(cmakeName); export != nil {
				label = export.BazelLabel
			} else {
				return nil, failure.New(failure.UnresolvedLinkDep,
					"target %q depends on %q which is neither in-codebase nor in the imports manifest",
					t.Name, cmakeName)
			}
		}
		if seenDep[label] {
			continue
		}
		seenDep[label] = true
		if allowsImplementationDeps && depScopeIsPrivate(traceLinkScope, dep, idToName) {
			irt.ImplementationDeps = append(irt.ImplementationDeps, label)
		} else {
			irt.Deps = append(irt.Deps, label)
		}
	}

	// Out-of-tree link fragments. CMake records IMPORTED_LOCATION paths
	// in t.Link.CommandFragments[role="libraries"] as resolved absolute
	// paths under the synth-prefix tree. The orchestrator's imports
	// manifest carries each export's link paths so we can rewrite those
	// fragments to Bazel labels.
	//
	// The seen-set spans Deps + ImplementationDeps so a dep already
	// routed to either bucket by the t.Dependencies loop above
	// doesn't get re-appended to Deps here (which would duplicate
	// it across both buckets and produce an invalid BUILD).
	if t.Link != nil {
		seen := map[string]bool{}
		for _, d := range irt.Deps {
			seen[d] = true
		}
		for _, d := range irt.ImplementationDeps {
			seen[d] = true
		}
		for _, frag := range t.Link.CommandFragments {
			if frag.Role != "libraries" {
				continue
			}
			path := strings.TrimSpace(frag.Fragment)
			if path == "" || !filepath.IsAbs(path) {
				continue
			}
			if hostPrefix != "" && strings.HasPrefix(path, hostPrefix+string(filepath.Separator)) {
				path = manifestPrefixAnchor + path[len(hostPrefix)+1:]
			}
			if export := imports.LookupLinkPath(path); export != nil {
				if !seen[export.BazelLabel] {
					seen[export.BazelLabel] = true
					// Trace-scope routing: if the
					// underlying cmake lib name is recorded
					// PRIVATE in traceLinkScope AND the
					// target accepts implementation_deps
					// (cc_library only), route to
					// ImplementationDeps. Otherwise fold
					// into Deps — cc_binary / cc_test /
					// cc_import don't accept the attribute.
					if allowsImplementationDeps && traceLinkScope != nil && scopeForLabelLib(traceLinkScope, export.CMakeTarget) == "PRIVATE" {
						irt.ImplementationDeps = append(irt.ImplementationDeps, export.BazelLabel)
					} else {
						irt.Deps = append(irt.Deps, export.BazelLabel)
					}
				}
			}
		}
	}

	// STATIC IMPORTED dep recovery from trace. STATIC archives don't
	// run a link step at build time, so cmake's codemodel records
	// no Link.CommandFragments and no IMPORTED-target Dependencies
	// for them — both upstream channels are empty. Trace's
	// target_link_libraries calls are the only ground truth for a
	// static lib's IMPORTED deps. For each lib name the trace
	// records, look it up in the imports manifest; resolve hits
	// are appended (deduped). Non-IMPORTED libs (in-codebase target
	// names) already came in via t.Dependencies above and are
	// covered by the seen-map.
	//
	// The seen-set spans Deps + ImplementationDeps so a dep already
	// routed to ImplementationDeps by the t.Dependencies loop above
	// doesn't get re-appended to Deps here.
	if t.Type == "STATIC_LIBRARY" && len(traceLinkLibs) > 0 {
		seen := map[string]bool{}
		for _, d := range irt.Deps {
			seen[d] = true
		}
		for _, d := range irt.ImplementationDeps {
			seen[d] = true
		}
		for _, lib := range traceLinkLibs {
			if export := imports.LookupCMakeTarget(lib); export != nil {
				if !seen[export.BazelLabel] {
					seen[export.BazelLabel] = true
					// Trace-scope routing: route PRIVATE
					// imports to ImplementationDeps only on
					// targets that accept the attribute
					// (cc_library — the STATIC_LIBRARY guard
					// above already ensures this for the
					// outer if, but make the rule-kind check
					// explicit for parallel structure with
					// the t.Dependencies + Link.CommandFragments
					// paths). Otherwise fold into Deps.
					if allowsImplementationDeps && traceLinkScope != nil && traceLinkScope[lib] == "PRIVATE" {
						irt.ImplementationDeps = append(irt.ImplementationDeps, export.BazelLabel)
					} else {
						irt.Deps = append(irt.Deps, export.BazelLabel)
					}
				}
			}
		}
	}

	if t.Install != nil && len(t.Install.Destinations) > 0 {
		irt.Visibility = []string{"//visibility:public"}
		irt.InstallDest = t.Install.Destinations[0].Path
	}

	if len(t.Artifacts) > 0 {
		irt.ArtifactName = t.Artifacts[0].Path
	} else if t.NameOnDisk != "" {
		irt.ArtifactName = t.NameOnDisk
	}

	switch {
	case t.Link != nil && t.Link.Language != "":
		irt.LinkLanguage = t.Link.Language
	case len(t.CompileGroups) > 0:
		irt.LinkLanguage = t.CompileGroups[0].Language
	}

	// CTest classification. An EXECUTABLE registered via add_test() is
	// rewritten as one or more cc_test rules — one per registration —
	// each sharing the cc_binary's srcs/hdrs/copts/deps. The cc_binary
	// itself is dropped (return nil) since the test executable is
	// addressable as a cc_test label after rewriting.
	if irt.Kind == ir.KindCCBinary && tests != nil {
		regs := tests.Lookup(t.Name)
		if len(regs) > 0 {
			for _, reg := range regs {
				cct := *irt
				cct.Name = reg.Name
				cct.Kind = ir.KindCCTest
				cct.TestArgs = append([]string(nil), reg.Args...)
				cct.TestEnv = append([]string(nil), reg.Env...)
				cct.TestData = append([]string(nil), reg.Data...)
				cct.TestTimeout = reg.Timeout
				if len(reg.Tags) > 0 {
					seen := make(map[string]bool, len(cct.Tags)+len(reg.Tags))
					merged := append([]string(nil), cct.Tags...)
					for _, x := range cct.Tags {
						seen[x] = true
					}
					for _, x := range reg.Tags {
						if seen[x] {
							continue
						}
						seen[x] = true
						merged = append(merged, x)
					}
					cct.Tags = merged
				}
				cc.Tests = append(cc.Tests, cct)
			}
			return nil, nil
		}
	}

	// Multi-language structural split: cmake records one
	// CompileGroup per language with that language's flags. A
	// single Bazel cc_library can't carry per-source-language
	// copts, so split into a wrapper cc_library (the user-
	// visible target name, deps-only) plus one private
	// sub-library per language with that language's srcs +
	// flags. Single-language targets stay one cc_library; this
	// branch only fires for ≥ 2 distinct compile-group
	// languages.
	if shouldSplitMultiLanguage(t) {
		if err := splitMultiLanguage(t, irt, cc); err != nil {
			return nil, err
		}
	}

	return irt, nil
}

// shouldSplitMultiLanguage reports whether the target carries
// sources from ≥ 2 distinct compile-group languages and is a
// rule kind where the per-language flag delta could surface as
// a build-time correctness issue. Header-only INTERFACE
// libraries don't compile sources here; UTILITY targets we
// already filtered out before reaching this point.
func shouldSplitMultiLanguage(t *fileapi.Target) bool {
	if len(t.CompileGroups) < 2 {
		return false
	}
	langs := map[string]bool{}
	for _, cg := range t.CompileGroups {
		if cg.Language == "" {
			continue
		}
		langs[cg.Language] = true
	}
	return len(langs) >= 2
}

// splitMultiLanguage rewrites irt as a deps-only wrapper and
// appends one per-language ir.Target to cc.Subs. Each sub-
// library carries:
//
//   - Srcs filtered to the sources cmake assigned to that
//     compile group.
//   - Copts + Defines extracted from that compile group's
//     command fragments (each cmake CG carries its own full
//     flag set including general flags like -O3, so per-
//     language flag isolation is automatic).
//   - The same Hdrs / Includes as the wrapper would have had
//     (cmake doesn't language-tag headers; the public include
//     surface is shared).
//   - Private visibility: only the wrapper consumes them.
//
// The wrapper drops Srcs / Copts / Defines, retains the
// public surface (hdrs, includes, visibility, install
// metadata), and adds a Deps edge to each sub-library.
func splitMultiLanguage(t *fileapi.Target, irt *ir.Target, cc *codegenContext) error {
	// Sort CompileGroups by language for deterministic sub-
	// library ordering across runs (the codemodel records them
	// in source-declaration order, which is stable but harder
	// to reason about).
	groups := append([]fileapi.CompileGroup(nil), t.CompileGroups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Language < groups[j].Language })

	sharedHdrs := append([]string(nil), irt.Hdrs...)
	sharedIncludes := append([]string(nil), irt.Includes...)
	wrapperSrcs := irt.Srcs

	var subDeps []string
	for _, cg := range groups {
		if cg.Language == "" {
			continue
		}
		// Source partition: keep the entries the wrapper
		// already accepted (those that survived
		// shouldIncludeSource etc.) AND that the codemodel
		// assigned to this CG.
		srcIndex := map[int]bool{}
		for _, idx := range cg.SourceIndexes {
			srcIndex[idx] = true
		}
		var subSrcs []string
		for i, s := range t.Sources {
			if !srcIndex[i] {
				continue
			}
			// Only keep srcs that landed on the wrapper's Srcs
			// slice — the existing source-walk filtering
			// (.rule files, generated sources we couldn't
			// recover, etc.) already trimmed them.
			rel := relForSource(s.Path, t)
			if !stringSliceContains(wrapperSrcs, rel) {
				continue
			}
			subSrcs = append(subSrcs, rel)
		}
		if len(subSrcs) == 0 {
			continue
		}
		copts, defs := splitCompileFragments(cg.CompileCommandFragments)
		for _, d := range cg.Defines {
			defs = append(defs, d.Define)
		}

		sub := ir.Target{
			Name:       irt.Name + "_" + langSuffix(cg.Language),
			Kind:       irt.Kind,
			Srcs:       subSrcs,
			Hdrs:       sharedHdrs,
			Includes:   sharedIncludes,
			Copts:      copts,
			Defines:    defs,
			Linkstatic: irt.Linkstatic,
			Alwayslink: irt.Alwayslink,
			Visibility: []string{"//visibility:private"},
		}
		cc.Subs = append(cc.Subs, sub)
		subDeps = append(subDeps, ":"+sub.Name)
	}

	if len(subDeps) == 0 {
		// Nothing actually split (e.g. all CGs ended up
		// empty); leave the wrapper alone so the existing
		// output stays valid.
		return nil
	}

	// Wrapper: deps-only. Strip srcs/copts/defines (now on
	// sub-libs); keep hdrs/includes/visibility/install/etc.
	irt.Srcs = nil
	irt.Copts = nil
	irt.Defines = nil
	irt.Deps = append(irt.Deps, subDeps...)
	return nil
}

// langSuffix maps a cmake compile-group language string to the
// short suffix used in the synthesized sub-library name.
func langSuffix(lang string) string {
	switch lang {
	case "C":
		return "c"
	case "CXX":
		return "cxx"
	case "OBJC":
		return "objc"
	case "OBJCXX":
		return "objcxx"
	case "Fortran":
		return "fortran"
	case "ASM":
		return "asm"
	}
	return strings.ToLower(lang)
}

// relForSource returns the package-relative path the wrapper
// stored for this codemodel source. Sources are mostly
// relative paths the codemodel records verbatim; absolute
// paths are uncommon but possible. Mirrors how the source
// walk in lowerTarget computes the path used in irt.Srcs.
func relForSource(p string, t *fileapi.Target) string {
	_ = t
	return p
}

// stringSliceContains is a tiny linear-search helper. The
// source slices are short enough (typically <50 entries) that
// a map+rebuild is overkill.
func stringSliceContains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// scopeForLabelLib looks up a cmake lib name in the
// trace-recovered link-scope map and returns the keyword
// recorded for it (PUBLIC / PRIVATE / INTERFACE), or "" when
// the lib isn't recorded.
//
// Distinct from depScopeIsPrivate's idToName-aware lookup
// because callers here already have the cmake name from
// `manifest.Export.CMakeTarget`, not a codemodel
// `TargetDependency` id.
func scopeForLabelLib(traceLinkScope map[string]string, cmakeName string) string {
	if cmakeName == "" {
		return ""
	}
	return traceLinkScope[cmakeName]
}

// depScopeIsPrivate reports whether a dep entry was recorded
// as PRIVATE in the trace-recovered target_link_libraries
// call. Returns false when:
//   - traceLinkScope is nil (no trace was decoded — codemodel-
//     only path),
//   - the dep's name wasn't found in any keyword-scoped trace
//     entry for this target (the call used the legacy
//     keyword-less positional shape, OR the dep was a cmake-
//     synthesized edge that doesn't surface in trace),
//   - the recorded keyword is PUBLIC, INTERFACE, or empty.
//
// The cmake-name match strips the codemodel id hash; for
// in-codebase deps it also tries the literal target name
// (which is what cmake records in the trace). For external
// imports the namespaced cmake name (`Foo::bar`) is what
// appears in trace and what stripIDHash produces — direct
// match.
func depScopeIsPrivate(traceLinkScope map[string]string, dep fileapi.TargetDependency, idToName map[string]string) bool {
	if len(traceLinkScope) == 0 {
		return false
	}
	cmakeName := stripIDHash(dep.Id)
	// Try the literal cmake name first (covers
	// imports-manifest deps that carry their namespace).
	if scope, ok := traceLinkScope[cmakeName]; ok {
		return scope == "PRIVATE"
	}
	// Also try the per-codemodel target-name registry: cmake's
	// trace records in-codebase deps under the bare target name
	// (e.g. "uses_hello"), not the codemodel id form. idToName
	// gives us that bare name; look it up.
	if name, ok := idToName[dep.Id]; ok {
		if scope, ok2 := traceLinkScope[name]; ok2 {
			return scope == "PRIVATE"
		}
	}
	return false
}

// stripIDHash returns the CMake target name from a File-API target id of the
// form `<name>::@<hash>`. If the id has no hash suffix it is returned
// unchanged; namespaced names (Foo::bar::@<hash>) collapse to "Foo::bar".
func stripIDHash(id string) string {
	if i := strings.Index(id, "::@"); i >= 0 {
		return id[:i]
	}
	return id
}

// isTargetObjectsRef reports whether srcPath is a
// $<TARGET_OBJECTS:t> reference — cmake records these in a
// consumer's sources[] as "<buildDir>/CMakeFiles/<t>.dir/.../*.o".
// We recognize that shape and check whether <t> is a known
// in-codebase target. When it is, the consumer's deps already
// carry an edge to <t>; lower's OBJECT_LIBRARY emit gives that
// target alwayslink=True, so the objects flow into the consumer
// archive transitively. The .o path itself shouldn't go through
// recoverGenrule (cmake's compile rule isn't a CUSTOM_COMMAND).
func isTargetObjectsRef(srcPath, buildDir string, idToName map[string]string) bool {
	rel, ok := relativeIfInsideRelaxed(buildDir, srcPath)
	if !ok {
		return false
	}
	const prefix = "CMakeFiles/"
	if !strings.HasPrefix(rel, prefix) {
		return false
	}
	tail := rel[len(prefix):]
	dirEnd := strings.Index(tail, ".dir/")
	if dirEnd < 0 {
		return false
	}
	otherName := tail[:dirEnd]
	for _, name := range idToName {
		if name == otherName {
			return true
		}
	}
	return false
}

// isCompilerObjectArtifact reports whether srcPath is a cc-compile
// output (typically from a unity build or other shape that surfaces
// the .o as a "generated source"), as opposed to a real
// CUSTOM_COMMAND-produced file. Two signals must both fire:
//
//  1. The path resolves under buildDir/CMakeFiles/<x>.dir/... and
//     ends in a compile-output extension (.o, .obj, .lo).
//  2. The producing ninja Build's rule has a known compiler-rule
//     prefix (CXX_COMPILER__, C_COMPILER__, etc.; the cmake-side
//     ninja generator decorates compile rules with this shape).
//
// Either signal alone is too permissive: a real CUSTOM_COMMAND could
// happen to write a .o (signal 1 alone) and a compiler rule could
// emit a header in a code-gen toolchain (signal 2 alone). #206.
func isCompilerObjectArtifact(srcPath, buildDir string, g *ninja.Graph) bool {
	rel, ok := relativeIfInsideRelaxed(buildDir, srcPath)
	if !ok {
		return false
	}
	if !strings.HasPrefix(rel, "CMakeFiles/") || !strings.Contains(rel, ".dir/") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(rel))
	if ext != ".o" && ext != ".obj" && ext != ".lo" {
		return false
	}
	if g == nil {
		return false
	}
	b := g.BuildFor(rel)
	if b == nil {
		b = g.BuildFor(srcPath)
	}
	if b == nil {
		return false
	}
	// cmake's modern ninja generator names compile rules
	// `<LANG>_COMPILER__<target>[<suffix>]_<config>` (e.g.
	// `CXX_COMPILER__foo_unscanned_Release`); the `_COMPILER__`
	// infix is the stable signal across every fixture in tree
	// (see converter/testdata/fileapi/*/CMakeFiles/rules.ninja).
	//
	// The bare `<LANG>_COMPILER` suffix branch covers the
	// non-per-target-decorated rule shape — appears in
	// hand-rolled / older-cmake ninja graphs that don't fan out
	// the `__<target>_<config>` tail (the form
	// `converter/internal/ninja/parsefile_test.go` exercises).
	// Kept as a defensive second match: ignoring it would let a
	// non-decorated graph's compile artifact slip past the gate
	// and back into recoverGenrule's CUSTOM_COMMAND refusal,
	// re-introducing the #206 failure shape we just fixed.
	return strings.Contains(b.Rule, "_COMPILER__") || strings.HasSuffix(b.Rule, "_COMPILER")
}

// isPathPrefix reports whether prefix is an ancestor of (or
// equal to the parent dir of) path, where both are
// slash-separated path strings. Empty prefix matches anything
// (means "the whole containing dir"). Used by the configure_file
// consumer attribution to test whether a target's build-dir
// include covers a configured-file output. Pure path semantics —
// no filesystem access.
func isPathPrefix(prefix, path string) bool {
	if prefix == "" || prefix == "." {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

// dedupeStrings returns a copy of in with consecutive duplicates removed. The
// caller is expected to have sorted in.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	out = append(out, in[0])
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// buildCompileGroupSet returns the set of source indexes that any of
// the target's compileGroups references. The CompileGroupIndex field on
// TargetSource can't be trusted on its own — it's an int with default 0,
// indistinguishable from "in group 0" vs "absent". The returned set is
// queried once per source in the per-target source loop; building it
// once flattens what was an O(sources * compileGroupEntries) inner loop.
func buildCompileGroupSet(t *fileapi.Target) map[int]bool {
	if len(t.CompileGroups) == 0 {
		return nil
	}
	out := make(map[int]bool)
	for _, cg := range t.CompileGroups {
		for _, idx := range cg.SourceIndexes {
			out[idx] = true
		}
	}
	return out
}

// splitCompileFragments parses each whitespace-delimited fragment piece. -D
// pieces are returned as defines (with the leading -D stripped); everything
// else is a copt. -I and -isystem are dropped — those come through
// compileGroup.includes structurally.
func splitCompileFragments(frags []fileapi.CommandFragment) (copts, defines []string) {
	for _, f := range frags {
		if f.Role != "" {
			// Reserved for link fragments; ignore on the compile side.
			continue
		}
		for _, p := range strings.Fields(f.Fragment) {
			switch {
			case strings.HasPrefix(p, "-D"):
				defines = append(defines, strings.TrimPrefix(p, "-D"))
			case strings.HasPrefix(p, "-I"), strings.HasPrefix(p, "-isystem"):
				// dropped: see compileGroup.includes
			default:
				copts = append(copts, p)
			}
		}
	}
	return copts, defines
}

// relativeIfInside returns (rel-path, true) if abs is at or below root, else
// ("", false). Returned slash style: filepath.ToSlash for portability of
// emitted BUILD files (Bazel labels use forward slashes always).
func relativeIfInside(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", true
	}
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", false
	}
	return rel, true
}

// discoverHeaders walks each include dir under sourceRoot and returns a sorted
// deduplicated list of header files (package-relative). M1 walks recursively;
// per-file granularity (excluding subdirs) can come later.
//
// Walks are memoized through cache (keyed on absolute include-dir path)
// so that multiple targets sharing an include root don't re-walk the
// same filesystem subtree. Pass nil for cache to disable memoization.
func discoverHeaders(sourceRoot string, includeDirs []string, cache map[string][]string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, inc := range includeDirs {
		absDir := filepath.Join(sourceRoot, inc)
		if cache != nil {
			if hdrs, ok := cache[absDir]; ok {
				for _, h := range hdrs {
					seen[h] = struct{}{}
				}
				continue
			}
		}
		var perDir []string
		walkErr := filepath.WalkDir(absDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				// An include dir that doesn't exist on disk is an error
				// worth surfacing; this is rare (CMake validates include
				// dirs on PUBLIC), but possible if the shadow tree is
				// out of sync.
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if !headerExts[ext] {
				return nil
			}
			rel, err := filepath.Rel(sourceRoot, p)
			if err != nil {
				return err
			}
			slash := filepath.ToSlash(rel)
			seen[slash] = struct{}{}
			perDir = append(perDir, slash)
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk include dir %q: %w", absDir, walkErr)
		}
		if cache != nil {
			cache[absDir] = perDir
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}
