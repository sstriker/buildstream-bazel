package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func init() { registerHandler(cmakeHandler{}) }

// cmakeConfig holds the per-run config the kind:cmake handler
// needs from CLI flags. Populated by main.go on --cmake-configure-
// file-bin; consumed by the per-element render to opt
// convert-element-cmake into the configure_file lift (see
// internal/configurefile package doc + ROADMAP.md).
var cmakeConfig struct {
	// configureFileBin is the absolute path to the
	// cmake-configure-file binary. When non-empty:
	//   - writeProjectA / writeProjectB stage the binary into
	//     their tools/ dir and add it to exports_files.
	//   - The per-cmake-element genrule cmd includes
	//     `--lift-configure-file=true` so convert-element-cmake emits
	//     the lifted shape (.h.in as srcs +
	//     //tools:cmake-configure-file invocation) when its
	//     reverse-extract succeeds.
	// Empty (default) preserves the legacy base64-of-rendered
	// shape — keeps orchestrator-driven flows that don't stage
	// the tool working unchanged.
	configureFileBin string

	// ccEmbedBin is the absolute path to the cc-embed binary. When
	// non-empty it mirrors configureFileBin for the cc_embed lift:
	//   - writeProjectA / writeProjectB stage the binary into their
	//     tools/ dir and add it to exports_files.
	//   - The per-cmake-element genrule cmd includes
	//     `--lift-cc-embed=true` so convert-element-cmake recognizes a
	//     known file-embedding cmake -P encoder (VTK's vtkEncodeString)
	//     and lowers it to the native cc_embed rule
	//     (//tools:cc-embed) instead of refusing / running cmake at
	//     build time.
	// Empty (default) leaves the recognizer off — the encoder edge
	// falls through to the runner/bake/refuse path. See
	// docs/research/codegen-idiom-coverage.md.
	ccEmbedBin string

	// ccHashBin is the staged path to cmd/cc-hash. When set, kind:cmake
	// elements thread --lift-cc-hash so convert-element-cmake lowers a
	// known file-hashing cmake -P script (vtkHashSource) to the native
	// cc_hash rule. Empty (default) leaves the hashing edge on the
	// runner/bake/refuse path. See docs/research/codegen-idiom-coverage.md.
	ccHashBin string

	// round2FallbackEnabled toggles the kind:cmake round-2
	// fallback shape (Phase B; see
	// docs/design/rendezvous.md).
	// When true:
	//   - Project A's converter genrule threads
	//     `--unsupported-execute-process-fallback=true` into
	//     convert-element-cmake's cmd, so classifier refusals
	//     produce the placeholder shape instead of a Tier-1
	//     exit.
	//   - Project B emits a real install genrule (cmake
	//     configure + ninja + install + tar under
	//     build-tracer, plus inline trace-publish) — replaces
	//     today's RenderB placeholder.
	// Off (default) preserves the existing kind:cmake shape.
	// Requires --build-tracer-bin + --trace-publish-bin so the
	// install genrule can wrap the build and publish to the
	// REAPI AC.
	round2FallbackEnabled bool

	// fidelity / bakeIn / diagnostics are the operator-facing dial
	// values resolved by deriveModes (see modes.go) and threaded
	// from main.go into the cmake-converter genrule cmd. Empty
	// strings (the zero value, never set in production but possible
	// in tests) elide the matching --fidelity / --bake-in /
	// --diagnostics flag in the converter cmd so the cache key
	// stays stable for the legacy callsite shape.
	fidelity    string
	bakeIn      string
	diagnostics bool

	// splitPackages toggles the per-directory BUILD split (see
	// docs/design/cmake-split-packages.md). When true the
	// converter genrule threads `--split-packages`, writes the
	// per-sub-package BUILD tree into a temp dir, and tars it into
	// a single declared `build-packages.tar` output (a genrule
	// cannot declare the sub-package set statically); stage-b
	// unpacks that tar into project B's elements/<name>/ tree.
	// Off (default) keeps the single BUILD.bazel.out shape.
	splitPackages bool

	// buildTypes, when non-empty, threads --build-types=<a,b,c> into
	// every kind:cmake converter genrule so cmake runs under the Ninja
	// Multi-Config generator and BUILD.bazel.out carries the per-config
	// //config:<name> select() arms (Phase 5 multi-config fold). write-a
	// renders the matching //config package (string_flag + config_settings
	// via emit/configsettings) into project B so the labels
	// resolve — see writeConfigSettingsPackage. Empty (default) keeps the
	// single-config render byte-stable.
	buildTypes []string

	// autoBuildTypes selects --build-types=auto: the converter genrule
	// gets --build-types=auto (convert-element-cmake configures Ninja
	// Multi-Config without forcing CMAKE_CONFIGURATION_TYPES, so the
	// project's own declared configs stand — detection happens in A's
	// conversion action at build time, never in write-a). write-a emits
	// the //config package over cmake's standard config set. Mutually
	// exclusive with an explicit buildTypes list.
	autoBuildTypes bool
}

// cmakeHandler renders a kind:cmake element. The project-A side is a
// genrule invoking convert-element-cmake under Bazel's action graph; the
// project-B side is a placeholder the driver script overwrites with
// project A's converted BUILD.bazel.out plus the user's full source
// tree (project B's cc_library compiles against the real source bytes).
type cmakeHandler struct{}

func (cmakeHandler) Kind() string           { return "cmake" }
func (cmakeHandler) NeedsSources() bool     { return true }
func (cmakeHandler) HasProjectABuild() bool { return true }

// DefaultReadPathsPatterns returns the cmake-converter default
// shadow-tree narrowing rules. Per-element <element>.read-paths.txt
// rules layer on top.
//
// Today: empty (no defaults). The patterns mechanism is in place
// but the cmake defaults aren't tuned yet — empirical narrowing
// data from the FDSDK reality-check probe will inform what's
// safe to default-include. Until that lands, every cmake element
// without an explicit read-paths.txt stages everything as real
// (the conservative pre-narrowing behaviour); per-element files
// remain the only narrowing path.
//
// Pinning the converter version pins these defaults, so cache-
// key stability follows the converter release contract.
func (cmakeHandler) DefaultReadPathsPatterns() *readPathsPatterns {
	return nil
}

func (cmakeHandler) RenderA(elem *element, elemPkg string) error {
	// Round-2 fallback: pre-compute the srckey hex once; pass it
	// through to cmakeElementBuild for the trace_load rule attr.
	// We don't render srckey.txt in project A (the publish side in
	// project B owns srckey.txt), so this is just the hex value.
	// The cmake-round2-fallback flag is incompatible with FUSE mode
	// (see the validation in main.go), so the FUSE branch below
	// doesn't need the hash — its fall-through to the staging path
	// covers any kind:local source that didn't have a sourceKey.
	srckeyHash := ""
	if cmakeConfig.round2FallbackEnabled {
		hash, _, err := computeSrckey(elem, cmakeSrckeyPatterns())
		if err != nil {
			return fmt.Errorf("element %q: compute srckey for trace_load: %w", elem.Name, err)
		}
		srckeyHash = hash
	}

	// FUSE-sources mode (--use-fuse-sources): skip on-disk staging
	// entirely; the per-element BUILD references @src_<key>//:tree
	// directly. The repo rule (rules/sources.bzl) ctx.symlinks the
	// file tree from the cas-fuse mount, so the genrule's $(SRCS)
	// resolves to bazel-bin paths that the kernel serves through
	// FUSE. Only viable for single-source-no-directory cmake elements
	// today; multi-source / directory-suffix elements fall back to
	// the staging path for now (additional repo composition needed).
	if useFuseSourcesGlobal && !cmakeMultiSource(elem) {
		k := sourceKey(elem.Sources[0])
		if k != "" {
			// Run the same partitionSources walk as the staging
			// path: the source-cache local tree gives us the
			// universe; converter defaults + per-element patterns
			// partition it into RealPaths / ZeroPaths. Real entries
			// flow as enumerated @src_<k>//:tree_dir/<path> labels;
			// zero entries flow through the same zero_files starlark
			// rule the staging path uses. cmake walks SHADOW inside
			// the genrule action, which matches: real bytes for real
			// files (streamed from CAS via @src_<k>//), empty bytes
			// for zero stubs. Narrowing applies; bytes flow only
			// when the action reads them.
			if err := partitionSources(elem); err != nil {
				return fmt.Errorf("partition sources (fuse mode): %w", err)
			}
			cmakePatterns := composeReadPathsPatterns(cmakeHandler{}.DefaultReadPathsPatterns(), elem.Patterns)
			if err := writeNarrowingPatterns(elemPkg, withCMakeListsRule(cmakePatterns)); err != nil {
				return err
			}
			return writeFile(filepath.Join(elemPkg, "BUILD.bazel"),
				cmakeElementBuildFuse(elem, k))
		}
		// Fall through: kind:local sources have no source-key, so
		// they can't be served via @src_<key>//. They still take
		// the staging path below.
	}

	srcStage := filepath.Join(elemPkg, "sources")
	if err := os.RemoveAll(srcStage); err != nil {
		return err
	}

	// Read-set narrowing only applies to single-source-no-directory
	// elements (the v1 fixture shape). Multi-source elements or any
	// source with a Directory subpath fall back to "stage everything
	// as real" — narrowing across multiple source roots needs
	// additional bookkeeping that lands when an FDSDK fixture forces
	// it.
	if cmakeMultiSource(elem) {
		elem.RealPaths = nil
		elem.ZeroPaths = nil
		if err := stageAllSources(elem, srcStage); err != nil {
			return err
		}
		if err := writeCmakeImportsManifest(elem, elemPkg); err != nil {
			return err
		}
		// Multi-source / Directory-suffix: no narrowing applied;
		// audit consumes an empty pattern set as "everything
		// covered" (the conservative default).
		if err := writeNarrowingPatterns(elemPkg, nil); err != nil {
			return err
		}
		return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), cmakeElementBuild(elem, srckeyHash))
	}

	if err := partitionSources(elem); err != nil {
		return fmt.Errorf("partition sources: %w", err)
	}
	// Emit the resolved pattern set for cmd/audit-narrowing.
	// The CMakeLists.txt-always-real rule that partitionSources
	// applies as a special case is made explicit here so the
	// audit's plain Match() produces the same coverage answer
	// applyReadPathsPatterns produces in the staging path.
	cmakePatterns := composeReadPathsPatterns(cmakeHandler{}.DefaultReadPathsPatterns(), elem.Patterns)
	if err := writeNarrowingPatterns(elemPkg, withCMakeListsRule(cmakePatterns)); err != nil {
		return err
	}
	// Real sources are staged as files in project A's source tree;
	// the glob in the per-element BUILD picks them up. Zero stubs are
	// NOT staged on disk — they're produced at action time by the
	// zero_files starlark rule and merged into $SRC_ROOT inside the
	// genrule's cmd. The rendered project-A tree only contains the
	// files the user can actually inspect; the empty stubs are an
	// action-graph detail Bazel handles.
	src := elem.Sources[0].AbsPath
	for _, rel := range elem.RealPaths {
		if err := copyFile(filepath.Join(src, rel), filepath.Join(srcStage, rel)); err != nil {
			return fmt.Errorf("stage real source %s: %w", rel, err)
		}
	}
	if err := writeCmakeImportsManifest(elem, elemPkg); err != nil {
		return err
	}
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), cmakeElementBuild(elem, srckeyHash))
}

// writeCmakeImportsManifest renders an imports.json next to the
// element's BUILD.bazel when the element has kind:cmake deps.
// One Element entry per dep, with a single Export per dep
// following the convention `<dep>::<dep>` → //elements/<dep>:<dep>.
//
// This is a best-effort convention bind. Real-world cmake
// projects whose namespace/target shape diverges from
// `<elem>::<elem>` won't resolve. A follow-up pass should let
// convert-element-cmake emit per-element exports metadata that
// write-a stitches in here at action time, replacing the
// convention guess.
func writeCmakeImportsManifest(elem *element, elemPkg string) error {
	deps := cmakeDepBundleLabels(elem)
	if len(deps) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`{
  "version": 1,
  "elements": [
`)
	for i, dep := range deps {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, `    {
      "name": %q,
      "exports": [
        {
          "cmake_target": %q,
          "bazel_label": "//elements/%s:%s",
          "link_paths": ["${_IMPORT_PREFIX}/lib/lib%s.a"]
        }
      ]
    }`, dep.DepName,
			dep.DepName+"::"+dep.DepName,
			dep.DepName, dep.DepName,
			dep.DepName)
	}
	b.WriteString(`
  ]
}
`)
	return writeFile(filepath.Join(elemPkg, "imports.json"), b.String())
}

// cmakeMultiSource reports whether this cmake element's sources
// are in any shape that prevents the single-source-tree narrowing
// path: >1 source declared, the lone source has a non-empty
// Directory subpath, or the source has no on-disk tree to walk
// (kind:git_repo / kind:tar / etc. with no --source-cache hit —
// AbsPath is empty). All these shapes flow through stageAllSources
// without path-narrowing.
func cmakeMultiSource(elem *element) bool {
	if len(elem.Sources) != 1 {
		return true
	}
	if elem.Sources[0].Directory != "" {
		return true
	}
	return elem.Sources[0].AbsPath == ""
}

func (cmakeHandler) RenderB(elem *element, elemPkg string) error {
	// Stage the FULL source tree (no narrowing). Project B's
	// cc_library needs the real source bytes to compile, so this is
	// the user's tree verbatim. (writeProjectB already cleared and
	// re-created elemPkg before calling us.) Multi-source elements
	// honor each source's Directory subpath via stageAllSources.
	if err := stageAllSources(elem, elemPkg); err != nil {
		return err
	}

	// Round-2 fallback shape: write the install genrule as
	// the package's BUILD.bazel directly — the placeholder is
	// NOT emitted in this branch. Post-build the driver
	// stages A's BUILD.bazel.out alongside this BUILD.bazel
	// (same package), so labels declared in BUILD.bazel.out
	// (cc_import / sh_binary stubs fed by pick_file over the
	// install-root TreeArtifact) resolve to this element's
	// pipeline_install output via same-package label
	// resolution. See docs/design/rendezvous.md.
	if cmakeConfig.round2FallbackEnabled {
		// cmakeSrckeyPatterns() already includes "CMakeLists.txt"
		// + "**/CMakeLists.txt" rules, so withCMakeListsRule
		// would duplicate them in srckey-patterns.txt. Pass the
		// pattern set straight through.
		if err := renderSrckey(elem, elemPkg, cmakeSrckeyPatterns()); err != nil {
			return err
		}
		return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), renderCmakeRound2B(elem))
	}

	// Placeholder BUILD; the driver script overwrites this after
	// project-A's bazel build produces the converter's
	// BUILD.bazel.out. Without the placeholder, Bazel would try to
	// load() rules_cc against an empty package and fail with a
	// confusing error before the stage step ran; the placeholder
	// makes the staging-not-yet-run state explicit.
	placeholder := projectBPlaceholder(elem.Name, "")
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), placeholder)
}

// partitionSources walks the element's source tree and decides which
// paths flow as real files vs zero stubs into project A.
//
//   - With no <element>.read-paths.txt patterns file
//     (elem.Patterns == nil), every file is real. The conservative
//     "no narrowing" default; matches pre-#61 behaviour without
//     opt-in.
//   - With patterns present, applyReadPathsPatterns partitions the
//     source-relative path universe per the include / exclude
//     rules. CMakeLists.txt files always stay real (cmake parses
//     the entry CMakeLists before any narrowing has a chance to
//     matter; auto-including them keeps cmake configure correct).
//
// Caller (cmakeHandler.RenderA) gates this on the single-source-no-
// directory case (cmakeMultiSource(elem) == false), so reading
// elem.Sources[0].AbsPath here is unconditional.
func partitionSources(elem *element) error {
	root := elem.Sources[0].AbsPath
	universe := []string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		universe = append(universe, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(universe)

	// Compose: converter defaults first, per-element override
	// rules concatenated after. applyReadPathsPatterns evaluates
	// rules left-to-right so defaults set the conservative
	// baseline and per-element entries refine.
	patterns := composeReadPathsPatterns(cmakeHandler{}.DefaultReadPathsPatterns(), elem.Patterns)
	elem.RealPaths, elem.ZeroPaths = applyReadPathsPatterns(patterns, universe)
	return nil
}

// composeReadPathsPatterns layers a per-element override file on
// top of converter defaults. nil + nil → nil (default-no-narrow);
// nil + b → b; a + nil → a; a + b → concatenated rules with
// defaults first.
func composeReadPathsPatterns(defaults, overrides *readPathsPatterns) *readPathsPatterns {
	if defaults == nil && overrides == nil {
		return nil
	}
	if defaults == nil {
		return overrides
	}
	if overrides == nil {
		return defaults
	}
	out := &readPathsPatterns{}
	out.Rules = append(out.Rules, defaults.Rules...)
	out.Rules = append(out.Rules, overrides.Rules...)
	return out
}

func cmakeElementBuild(elem *element, srckeyHash string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Generated by cmd/write-a. Do not edit by hand.

package(default_visibility = ["//visibility:public"])
`)

	// Round-2 fallback: emit a trace_load target whose action shells
	// to trace-lookup at action time. The element's converter
	// genrule consumes :<elem>_trace_load via srcs (filegroup-style);
	// an AC miss surfaces as zero-byte trace.log which the converter
	// treats as "no trace yet". The srckey hex is passed in by the
	// caller (RenderA computes it via computeSrckey alongside the
	// project-B srckey.txt that's rendered for the publishing
	// side). expect_make_db=False because cmake's converter derives
	// IR from the trace + cmake File API — no make-db needed.
	if cmakeConfig.round2FallbackEnabled && srckeyHash != "" {
		b.WriteString(traceLoadBlock(elem.Name, srckeyHash))
	}

	// Render the zero_files load + target only when feedback narrowed
	// the source set. First-run / no-feedback elements have an empty
	// ZeroPaths and don't need the rule at all — keeps the BUILD
	// minimal in that common case.
	if len(elem.ZeroPaths) > 0 {
		fmt.Fprintf(&b, `
load("@rules_buildstream_bazel//rules:zero_files.bzl", "zero_files")

# Files cmake's directory walks see but don't read. Materialized
# at action time as zero-length stubs whose merkle is the empty
# SHA — the action input remains content-stable across edits to
# any of these paths in the user's source tree.
zero_files(
    name = "%[1]s_zero_stubs",
    paths = [
`, elem.Name)
		for _, p := range elem.ZeroPaths {
			fmt.Fprintf(&b, "        %q,\n", "sources/"+p)
		}
		fmt.Fprintf(&b, `    ],
)
`)
	}

	// Real sources flow through a glob; CMakeLists.txt is included
	// like any other entry — the cmd's shadow merge handles every
	// source uniformly via $(SRCS).
	fmt.Fprintf(&b, `
filegroup(
    name = "%[1]s_real",
    srcs = glob(["sources/**"]),
)
`, elem.Name)

	// Cross-cmake-element deps: every kind:cmake dep of this
	// element exposes a cmake_config_bundle filegroup
	// (declared at the bottom of its own BUILD); extracting
	// each into $PREFIX/lib/cmake/<dep>/ at action time makes
	// `find_package(<Pkg> CONFIG)` resolve against the synth
	// bundle. Non-cmake deps don't ship a cmake-config bundle
	// today (Phase 4 typed filegroup work) and are skipped
	// here.
	cmakeDepLabels := cmakeDepBundleLabels(elem)

	// Compose the genrule's srcs (real-files filegroup, optional
	// zero_stubs, dep bundles + imports.json, dep exports.json, round-2
	// trace_load) and the matching --exports-in flag string.
	depExportsLabels := cmakeDepExportsLabels(elem)
	srcsList, exportsInFlag := composeCmakeGenruleSrcs(elem, cmakeDepLabels, depExportsLabels)

	// Cross-element bundle extraction: every kind:cmake dep's
	// cmake-config-bundle.tar already carries its full synth-
	// prefix slice (lib/cmake/<DepPkg>/*.cmake plus zero-byte
	// IMPORTED_LOCATION stubs and INTERFACE_INCLUDE_DIRECTORIES
	// directories — produced by synthprefix.Build inside
	// convert-element-cmake). One `tar -xf` per dep into a shared
	// $PREFIX overlays each slice; cmake's
	// find_package(<DepPkg> CONFIG) resolves against the
	// stitched tree and the IMPORTED-target EXISTS checks
	// pass against the stubs.
	flags := buildCmakeConverterFlags(cmakeDepLabels)

	// Split-packages reshapes the converter's BUILD output: instead of
	// a single BUILD.bazel.out genrule, the element is converted by the
	// cmake_split_convert custom rule (rules/cmake_packages.bzl), whose
	// action runs the converter in --split-packages mode and declares
	// the discovered-at-action-time per-sub-package BUILD tree as a
	// TreeArtifact directory (content-addressed per file — no opaque
	// build-packages.tar). stage-b merges that directory into project
	// B's elements/<name>/ tree by per-file content compare. Default
	// (off) keeps the single-file genrule shape byte-for-byte.
	if cmakeConfig.splitPackages {
		b.WriteString(cmakeSplitConvertBlock(elem, cmakeDepLabels, depExportsLabels, flags.lift, flags.fallback, flags.fidelity, flags.bakeIn, flags.buildTypes))
		return b.String()
	}

	primaryOut := `"BUILD.bazel.out",`
	outBuildSetup := ""
	outBuildFlag := `--out-build="$(location BUILD.bazel.out)"`
	splitFlag := ""
	buildPackagingStep := ""
	buildBazelSrcs := `"BUILD.bazel.out"`

	fmt.Fprintf(&b, `
genrule(
    name = "%[1]s_converted",
    srcs = [%[2]s],
    outs = [
        %[13]s
        "read_paths.json",
        "cmake-config-bundle.tar",
        "exports.json",
        "conversion-todos.json",%[11]s
    ],
    cmd = """
        # Build a unified source-root by merging real srcs (workspace
        # paths under elements/<name>/sources/) and zero stubs (under
        # bazel-bin/.../sources/) into a fresh shadow dir. Both share
        # a "sources/" segment in their path; strip up to the last one
        # to recover the source-relative suffix. Cross-element bundle
        # tars from kind:cmake deps come in via $(SRCS) too; skip
        # them here since the dep-extract loop below handles them.
        SHADOW="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                *cmake-config-bundle.tar) continue ;;
                */imports.json) continue ;;
                */exports.json) continue ;;
            esac
            rel="$${src##*sources/}"
            mkdir -p "$$SHADOW/$$(dirname "$$rel")"
            cp -L "$$src" "$$SHADOW/$$rel"
        done
        # Stage each kind:cmake dep's synth bundle under $$PREFIX
        # so find_package(<Pkg> CONFIG) in this consumer's
        # CMakeLists resolves against it. No-op when the element
        # has no kind:cmake deps.
%[3]s        BUNDLE_DIR="$$(mktemp -d)"
%[14]s        $(location //tools:convert-element-cmake) \\
            --source-root="$$SHADOW" \\
            %[15]s \\
            --out-bundle-dir="$$BUNDLE_DIR" \\
            --out-read-paths="$(location read_paths.json)" \\
            --out-exports="$(location exports.json)" \\
            --conversion-todos-report="$(location conversion-todos.json)" \\
            --bazel-package-path="elements/%[1]s"%[4]s%[5]s%[6]s%[7]s%[8]s%[9]s%[10]s%[12]s%[16]s%[19]s
        tar -cf "$(location cmake-config-bundle.tar)" -C "$$BUNDLE_DIR" .%[17]s
    """,
    tools = ["//tools:convert-element-cmake"],
)

# Typed exports project B consumes. Phase 1/2 emit the converter's
# raw outputs; later phases expand cmake-config-bundle.tar into
# the typed slices (cmake_config / pkg_config / headers / libs).
# Under --split-packages this is the build-packages.tar tree that
# stage-b unpacks into elements/<name>/.
filegroup(
    name = "build_bazel",
    srcs = [%[18]s],
)

# Cross-element handle: downstream cmake elements reference this
# label in their own genrule srcs, which extracts the tar into
# $PREFIX/lib/cmake/<this>/ at convert-element-cmake action time.
filegroup(
    name = "cmake_config_bundle",
    srcs = ["cmake-config-bundle.tar"],
)
`, elem.Name, srcsList, flags.depExtract, flags.prefix, flags.imports, flags.lift, flags.fallback, flags.fidelity, flags.bakeIn, flags.diagnostics, flags.diagnosticOuts, exportsInFlag, primaryOut, outBuildSetup, outBuildFlag, splitFlag, buildPackagingStep, buildBazelSrcs, flags.buildTypes)
	return b.String()
}

// composeCmakeGenruleSrcs builds the converter genrule's srcs list and the
// matching --exports-in flag string for one kind:cmake element: the real-files
// filegroup, the optional zero_stubs target (when feedback narrowed the source
// set), each kind:cmake dep's config bundle plus imports.json (so
// convert-element-cmake's manifest lookup resolves IMPORTED-target names to
// Bazel labels), each dep's narrow exports.json (a dep edit that leaves its
// export surface unchanged doesn't re-invalidate this consumer), and — under
// round-2 fallback — the action-time :<elem>_trace_load target.
func composeCmakeGenruleSrcs(elem *element, cmakeDepLabels []cmakeDepBundleLabel, depExportsLabels []string) (srcsList, exportsInFlag string) {
	srcsList = fmt.Sprintf(`":%s_real"`, elem.Name)
	if len(elem.ZeroPaths) > 0 {
		srcsList += fmt.Sprintf(`, ":%s_zero_stubs"`, elem.Name)
	}
	for _, depLabel := range cmakeDepLabels {
		srcsList += fmt.Sprintf(`, %q`, depLabel.Label)
	}
	if len(cmakeDepLabels) > 0 {
		srcsList += `, "imports.json"`
	}
	for _, lbl := range depExportsLabels {
		srcsList += fmt.Sprintf(`, %q`, lbl)
		exportsInFlag += fmt.Sprintf(` \
            --exports-in="$(location %s)"`, lbl)
	}
	if cmakeConfig.round2FallbackEnabled {
		srcsList += fmt.Sprintf(`, ":%s_trace_load"`, elem.Name)
	}
	return srcsList, exportsInFlag
}

// cmakeConverterFlags holds the per-element convert-element-cmake command
// fragments derived from the operator dials (cmakeConfig) + the element's
// kind:cmake dep set. Each field is empty when its dial is at the default /
// inactive, keeping the rendered genrule byte-stable for legacy callsites that
// never set the dial.
type cmakeConverterFlags struct {
	fidelity       string
	bakeIn         string
	buildTypes     string
	diagnostics    string
	diagnosticOuts string
	lift           string
	fallback       string
	prefix         string
	imports        string
	depExtract     string
}

// buildCmakeConverterFlags computes the converter command fragments for one
// kind:cmake element from the operator dials and its kind:cmake deps.
func buildCmakeConverterFlags(cmakeDepLabels []cmakeDepBundleLabel) cmakeConverterFlags {
	var f cmakeConverterFlags
	// Operator-facing dial pass-through: thread --fidelity, --bake-
	// in, --diagnostics into the converter cmd. Each is elided when
	// the value is empty or matches the converter's own default, so
	// the cache key / golden-byte shape stays stable for legacy
	// callsites that never set the dial.
	f.fidelity = fidelityFlagFragment(cmakeConfig.fidelity)
	if cmakeConfig.bakeIn != "" && cmakeConfig.bakeIn != bakeInWarn {
		f.bakeIn = fmt.Sprintf(` \
            --bake-in=%s`, cmakeConfig.bakeIn)
	}
	// Multi-config: thread --build-types so cmake runs under Ninja
	// Multi-Config and BUILD.bazel.out carries the //config:<name>
	// select() arms. write-a renders the matching //config package into
	// project B (writeConfigSettingsPackage). Empty (default) elides the
	// flag, keeping the single-config render byte-stable.
	if cmakeConfig.autoBuildTypes {
		f.buildTypes = ` \
            --build-types=auto`
	} else if len(cmakeConfig.buildTypes) > 0 {
		f.buildTypes = fmt.Sprintf(` \
            --build-types=%s`, strings.Join(cmakeConfig.buildTypes, ","))
	}
	// Diagnostics dial: thread --diagnostics + a per-element
	// rejections.json output. The converter writes the file (empty
	// when no rejections fired) so the declared output always
	// exists for Bazel; the operator can read elements/<name>/
	// rejections.json post-build to see the structured rejection
	// list. Without the --rejections-report path, --diagnostics
	// would silently collect rejections but write nothing — the
	// dial's whole point is producing readable output for
	// surveys.
	if cmakeConfig.diagnostics {
		f.diagnostics = ` \
            --diagnostics=true \
            --rejections-report="$(location rejections.json)"`
		f.diagnosticOuts = `
        "rejections.json",`
	}
	if cmakeConfig.configureFileBin != "" {
		f.lift = ` \
            --lift-configure-file=true`
	}
	// cc_embed lift rides the same liftFlag string so it threads into
	// both the split (cmakeSplitConvertBlock) and single-BUILD genrule
	// paths without a separate parameter.
	if cmakeConfig.ccEmbedBin != "" {
		f.lift += ` \
            --lift-cc-embed=true`
	}
	if cmakeConfig.ccHashBin != "" {
		f.lift += ` \
            --lift-cc-hash=true`
	}
	// Phase B round-2 fallback: when enabled, convert-element-cmake
	// is told to turn classifier refusals into the placeholder
	// shape (cc_import / sh_binary stubs fed by pick_file over
	// the install-root TreeArtifact) instead of exiting Tier-1.
	// Those pick_file targets resolve against Project B's
	// pipeline_install rule (emitted by RenderB when the same
	// flag is enabled) once the BUILD.bazel.out gets symlinked
	// into B's package.
	if cmakeConfig.round2FallbackEnabled {
		f.fallback = ` \
            --unsupported-execute-process-fallback=true`
	}
	if len(cmakeDepLabels) > 0 {
		var depExtract strings.Builder
		depExtract.WriteString(`        PREFIX="$$(mktemp -d)"
`)
		for _, dep := range cmakeDepLabels {
			// Filter by basename + non-empty: the
			// :<dep>_trace_load label expands to multiple
			// outputs (trace.log, marker, make-db.txt,
			// cmake-config-bundle.tar) and only the tar is a
			// valid archive. The non-empty check also handles
			// AC-miss zero-byte bundles cleanly (consumers
			// "detect empty bundles and skip dep-stage" per the
			// design doc). The kind:cmake :cmake_config_bundle
			// filegroup is single-file so the basename filter
			// is a no-op there.
			fmt.Fprintf(&depExtract, `        for tar in $(locations %s); do
            if [ "$$(basename "$$tar")" = "cmake-config-bundle.tar" ] && [ -s "$$tar" ]; then
                tar -xf "$$tar" -C "$$PREFIX"
            fi
        done
`, dep.Label)
		}
		f.depExtract = depExtract.String()
		f.prefix = ` \
            --prefix-dir="$$PREFIX"`
		f.imports = ` \
            --imports-manifest="$(location imports.json)"`
	}
	return f
}

// cmakeSplitConvertBlock renders the --split-packages delivery shape
// for one kind:cmake element: a cmake_split_convert custom rule
// (rules/cmake_packages.bzl) whose action runs convert-element-cmake in
// --split-packages mode and declares the per-sub-package BUILD tree as
// a TreeArtifact directory (content-addressed per file — no
// build-packages.tar). This replaces the BUILD.bazel.out genrule on the
// split path; the off (default) path keeps the genrule byte-for-byte.
//
// The rule's attrs partition the genrule's $(SRCS) by role:
//   - srcs: shadow source-root inputs (:<name>_real + optional
//     :<name>_zero_stubs).
//   - dep_bundles: each kind:cmake dep's cmake_config_bundle filegroup,
//     untarred into $PREFIX inside the action; presence adds
//     --prefix-dir. The round-2 :<name>_trace_load (whose outputs
//     include a cmake-config-bundle.tar) rides here too when enabled.
//   - aux: imports.json + dep exports.json — staged into the action but
//     referenced via converter_args by path. See the v1 boundary note
//     below.
//
// converter_args carries the flag LOGIC write-a owns (lift / fallback /
// fidelity / bake-in) so the Starlark rule stays mechanical.
//
// v1 boundary: --diagnostics (--rejections-report), --imports-manifest,
// and --exports-in all reference genrule $(location) outputs/inputs that
// have no $(location) analogue in a custom-rule string, so they are NOT
// threaded into converter_args on the split path yet. dep_bundles
// extraction (the prefix wiring) IS supported. See
// docs/design/cmake-split-packages.md for the follow-on.
func cmakeSplitConvertBlock(elem *element, cmakeDepLabels []cmakeDepBundleLabel, depExportsLabels []string, liftFlag, fallbackFlag, fidelityFlag, bakeInFlag, buildTypesFlag string) string {
	// srcs: real sources + (when narrowed) zero stubs only — the
	// shadow source-root inputs. Dep bundles / aux ride separate attrs.
	srcs := fmt.Sprintf(`":%s_real"`, elem.Name)
	if len(elem.ZeroPaths) > 0 {
		srcs += fmt.Sprintf(`, ":%s_zero_stubs"`, elem.Name)
	}

	// dep_bundles: each kind:cmake dep's cmake_config_bundle filegroup,
	// plus the round-2 trace_load (its outputs include a config bundle).
	var depBundles []string
	for _, depLabel := range cmakeDepLabels {
		depBundles = append(depBundles, depLabel.Label)
	}
	if cmakeConfig.round2FallbackEnabled {
		depBundles = append(depBundles, fmt.Sprintf(":%s_trace_load", elem.Name))
	}
	depBundlesAttr := ""
	if len(depBundles) > 0 {
		var q []string
		for _, l := range depBundles {
			q = append(q, fmt.Sprintf("%q", l))
		}
		depBundlesAttr = fmt.Sprintf("\n    dep_bundles = [%s],", strings.Join(q, ", "))
	}

	// Cross-element dep channel (#310): imports.json (when the element
	// has kind:cmake deps) rides imports_manifest; each dep's exports.json
	// rides exports_in. The rule turns these into --imports-manifest /
	// --exports-in flags by action-input path.
	depChannelAttrs := ""
	if len(cmakeDepLabels) > 0 {
		depChannelAttrs += "\n    imports_manifest = \"imports.json\","
	}
	if len(depExportsLabels) > 0 {
		var q []string
		for _, lbl := range depExportsLabels {
			q = append(q, fmt.Sprintf("%q", lbl))
		}
		depChannelAttrs += fmt.Sprintf("\n    exports_in = [%s],", strings.Join(q, ", "))
	}
	if cmakeConfig.diagnostics {
		depChannelAttrs += "\n    emit_rejections = True,"
	}

	// converter_args: the flag LOGIC write-a owns. Each piece is the
	// same string the genrule appended; here they're concatenated into
	// one space-separated attr value (no leading-backslash line
	// continuations — those are genrule-bash artifacts). Trim each
	// fragment's leading " \\\n            " wrapper down to the bare
	// flag tokens.
	converterArgs := strings.TrimSpace(strings.Join([]string{
		flagTokens(liftFlag),
		flagTokens(fallbackFlag),
		flagTokens(fidelityFlag),
		flagTokens(bakeInFlag),
		flagTokens(buildTypesFlag),
	}, " "))
	converterArgs = strings.Join(strings.Fields(converterArgs), " ")

	return fmt.Sprintf(`
load("@rules_buildstream_bazel//rules:cmake_packages.bzl", "cmake_split_convert")

cmake_split_convert(
    name = "%[1]s_converted",
    srcs = [%[2]s],%[3]s%[4]s
    converter_args = %[5]q,
    bazel_package_path = "elements/%[1]s",
    converter = "//tools:convert-element-cmake",
)

# Typed exports project B consumes. Under --split-packages the
# converted output is the cmake_split_convert rule's TreeArtifact
# packages/ directory (content-addressed per file); stage-b reads
# bazel-bin directly and merges it into elements/%[1]s/, so this
# filegroup is informational — it points at the rule's DefaultInfo
# (which includes the packages directory).
filegroup(
    name = "build_bazel",
    srcs = [":%[1]s_converted"],
)

# Cross-element handle: downstream cmake elements reference this
# label in their own srcs, which extracts the tar into
# $PREFIX/lib/cmake/<this>/ at convert-element-cmake action time.
filegroup(
    name = "cmake_config_bundle",
    srcs = [":%[1]s_converted"],
)
`, elem.Name, srcs, depBundlesAttr, depChannelAttrs, converterArgs)
}

// flagTokens strips a genrule flag fragment's bash line-continuation
// wrapper (` \\\n            `) down to bare " --flag=value" tokens
// suitable for the cmake_split_convert converter_args string attr.
func flagTokens(frag string) string {
	frag = strings.ReplaceAll(frag, "\\\n", " ")
	return strings.TrimSpace(frag)
}

// cmakeDepBundleLabel pairs a cross-element dep's name with the
// Bazel label of its `cmake_config_bundle` filegroup. Used by
// the cmake handler to stage one cmake-config tar per dep
// under $PREFIX/lib/cmake/<dep>/ inside the consumer's
// convert-element-cmake action.
type cmakeDepBundleLabel struct {
	DepName string
	Label   string
}

// cmakeDepBundleLabels returns the cross-element bundle labels
// the consumer's genrule should stage. Two paths:
//
//   - kind:cmake deps: reference the dep's `:cmake_config_bundle`
//     filegroup, which packs a converter-synthesized bundle from
//     cmake's File API codemodel (pass-2 artifact, available
//     whether or not the dep has been built yet).
//   - Trace-driven deps with the round-2 path active:
//     reference the dep's `:<dep>_trace_load` rule, whose action-
//     time AC lookup materializes a `cmake-config-bundle.tar`
//     synthesized from the install tree at pass-3. The cross-
//     element configure-step bootstrap rendezvous (see
//     docs/design/rendezvous.md): pass-3
//     publishes, pass-2 consumes via the AC.
//
// Both paths land a cmake-config-bundle.tar file in the consumer's
// $(SRCS); the consumer's existing dep-extract shell loop matches
// by basename and untars uniformly. The trace-driven dep's
// trace_load also drops trace.log + marker (+ optionally make-db)
// into $(SRCS) but those files have different basenames, so the
// extract loop ignores them.
//
// Deps with no bundle-producing path (filegroup kinds, stack,
// compose, etc.) are skipped silently. Order is dep-walk order
// so the rendered BUILD is deterministic.
//
// The trace-driven case keys off traceDrivenSrckeyPatternsForKind:
// the same gate that drives whether the dep's RenderB emits the
// trace_load target. A dep's round-2 path is operative iff
// traceDrivenSrckeyPatternsForKind returns non-nil for its kind —
// and that's exactly when the dep has published bundle bytes
// available via :<dep>_trace_load.
// cmakeDepExportsLabels returns the exports.json labels for this
// element's kind:cmake deps only. Each kind:cmake producer emits an
// exports.json (via --out-exports); the consumer stages them via
// --exports-in so its lower pass resolves the producer's real
// namespaced targets to real Bazel labels — the action-time
// replacement for write-a's render-time "<dep>::<dep>" convention
// guess. Trace-driven deps don't run convert-element-cmake, so they
// have no exports.json and are excluded.
func cmakeDepExportsLabels(elem *element) []string {
	var out []string
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst == nil || dep.Bst.Kind != "cmake" {
			continue
		}
		out = append(out, fmt.Sprintf("//elements/%s:exports.json", dep.Name))
	}
	return out
}

func cmakeDepBundleLabels(elem *element) []cmakeDepBundleLabel {
	var out []cmakeDepBundleLabel
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst == nil {
			continue
		}
		if dep.Bst.Kind == "cmake" {
			out = append(out, cmakeDepBundleLabel{
				DepName: dep.Name,
				Label:   fmt.Sprintf("//elements/%s:cmake_config_bundle", dep.Name),
			})
			continue
		}
		if traceDrivenSrckeyPatternsForKind(dep.Bst.Kind) != nil {
			out = append(out, cmakeDepBundleLabel{
				DepName: dep.Name,
				Label:   fmt.Sprintf("//elements/%s:%s_trace_load", dep.Name, dep.Name),
			})
			continue
		}
	}
	return out
}

// cmakeElementBuildFuse renders the FUSE-sources variant of the
// per-element BUILD.bazel: srcs come from @src_<key>//:tree
// (which the sources extension's repo rule symlinks into the
// cas-fuse mount), and the genrule's cmd strips up to and
// including "tree_dir/" — matching the symlink target name
// the rule (rules/sources.bzl) creates.
//
// v1 doesn't emit zero_files in this mode: read-paths narrowing
// across a CAS-served tree needs additional plumbing
// (the universe is the FileNodes in the Directory proto, not a
// glob over an on-disk staging dir). All sources flow as real;
// the action-cache stability story for FUSE mode is "the
// Directory digest changes only when the source bytes change",
// which is already strictly stronger than today's glob().
func cmakeElementBuildFuse(elem *element, sourceKey string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Generated by cmd/write-a (--use-fuse-sources). Do not edit by hand.

package(default_visibility = ["//visibility:public"])
`)
	// zero_files target: paths cmake's directory walk sees but
	// doesn't read. Materialised at action time as zero-length
	// stubs whose merkle is the empty SHA — the action input
	// stays content-stable across edits to non-real source files.
	if len(elem.ZeroPaths) > 0 {
		fmt.Fprintf(&b, `
load("@rules_buildstream_bazel//rules:zero_files.bzl", "zero_files")

zero_files(
    name = "%[1]s_zero_stubs",
    paths = [
`, elem.Name)
		for _, p := range elem.ZeroPaths {
			fmt.Fprintf(&b, "        %q,\n", "tree_dir/"+p)
		}
		fmt.Fprintf(&b, `    ],
)
`)
	}

	// Real-files srcs: enumerate per-file labels into the
	// @src_<key>// repo (each file reachable as a digest-stable
	// Bazel label, exports_files'd by the repo rule). When no
	// patterns narrow the universe, partitionSources puts every
	// path in RealPaths; when patterns are active, only the
	// narrowed-real subset lands here and the rest flows through
	// the zero_stubs target above.
	srcsList := ""
	if len(elem.RealPaths) > 0 {
		// Single-line srcs list with @src_<k>//:tree_dir/<path>
		// labels. Sorted for determinism (RealPaths is already
		// sorted by partitionSources).
		var labels []string
		for _, p := range elem.RealPaths {
			labels = append(labels, fmt.Sprintf("%q", "@src_"+sourceKey+"//:tree_dir/"+p))
		}
		srcsList = strings.Join(labels, ", ")
	}
	if len(elem.ZeroPaths) > 0 {
		zeroRef := fmt.Sprintf("%q", ":"+elem.Name+"_zero_stubs")
		if srcsList == "" {
			srcsList = zeroRef
		} else {
			srcsList += ", " + zeroRef
		}
	}
	// Fallback: when the element has no patterns + no source-cache
	// hit (so partitionSources didn't run / produced nothing),
	// reach for the opaque :tree filegroup so we still feed
	// convert-element-cmake a non-empty input set. This matches the
	// pre-narrowing "everything real" default.
	if srcsList == "" {
		srcsList = fmt.Sprintf("%q", "@src_"+sourceKey+"//:tree")
	}

	fmt.Fprintf(&b, `
genrule(
    name = "%[1]s_converted",
    srcs = [%[3]s],
    outs = [
        "BUILD.bazel.out",
        "read_paths.json",
        "cmake-config-bundle.tar",
    ],
    cmd = """
        # Materialise the narrowed source root inside the action
        # sandbox: real srcs (CAS-served via @src_<key>//) and
        # zero stubs (rule-generated empties) both arrive under
        # path components ending in tree_dir/<rel>. Strip up to
        # and including the last tree_dir/ to recover the
        # source-relative suffix.
        #
        # SHADOW is a tree of symlinks pointing at the original
        # Bazel-supplied paths in the action's working directory.
        # No byte materialisation: when sources flow from the
        # FUSE mount, cmake reads through SHADOW symlink → Bazel
        # sandbox path → external-repo symlink → FUSE mount, all
        # resolved by the kernel on demand. cp -L would render
        # the bytes (defeats the no-bytes-on-dev-disk story the
        # FUSE-sources route exists to deliver, plus on FUSE has
        # cp's "replaced while being copied" race).
        #
        # We prefix $$src with $$PWD rather than resolving via
        # readlink so the kernel walks Bazel's own symlink chain
        # at read time — the final FUSE-mount path may not be in
        # the Bazel sandbox's mount namespace, so resolving the
        # absolute target up front would point at a path the
        # action can't open.
        SHADOW="$$(mktemp -d)"
        for src in $(SRCS); do
            rel="$${src##*tree_dir/}"
            mkdir -p "$$SHADOW/$$(dirname "$$rel")"
            ln -s "$$PWD/$$src" "$$SHADOW/$$rel"
        done
        BUNDLE_DIR="$$(mktemp -d)"
        $(location //tools:convert-element-cmake) \\
            --source-root="$$SHADOW" \\
            --source-key="%[2]s" \\
            --out-build="$(location BUILD.bazel.out)" \\
            --out-bundle-dir="$$BUNDLE_DIR" \\
            --out-read-paths="$(location read_paths.json)" \\
            --bazel-package-path="elements/%[1]s"%[4]s%[5]s
        tar -cf "$(location cmake-config-bundle.tar)" -C "$$BUNDLE_DIR" .
    """,
    tools = ["//tools:convert-element-cmake"],
)

filegroup(
    name = "build_bazel",
    srcs = ["BUILD.bazel.out"],
)

filegroup(
    name = "cmake_config_bundle",
    srcs = ["cmake-config-bundle.tar"],
)
`, elem.Name, sourceKey, srcsList, fuseFidelityFlag(), fuseDiagnosticsFlag())
	return b.String()
}

// fuseFidelityFlag / fuseDiagnosticsFlag thread the operator-facing
// dials into the FUSE cmake template. --bake-in is intentionally
// NOT threaded here today (deriveModes surfaces a downgrade note
// when --bake-in=reject + --use-fuse-sources both fire) — the FUSE
// template's existing limitations around --lift-configure-file etc.
// argue for keeping new dial integration minimal until the FUSE
// template grows feature parity with the staging template.
func fuseFidelityFlag() string {
	return fidelityFlagFragment(cmakeConfig.fidelity)
}

func fuseDiagnosticsFlag() string {
	return diagnosticsFlagFragment(cmakeConfig.diagnostics)
}
