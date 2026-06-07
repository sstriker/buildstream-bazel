# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **Refactor: one source-classification chokepoint in `lower`.** The "is this
  path a cc compile/link/header input, and which attribute does it go in
  (srcs/hdrs/data/drop)?" decision is duplicated across ~6 sites in
  `converter/internal/lower/lower.go` — the main per-source switch, the
  recovered-genrule branch, the GENERATED-not-on-disk branch, the inCompileGroup
  branch, the file(GENERATE) consumer-attribution block, and the execute_process
  sister block. The VTK wrap-hierarchy `.args/.data` bug had to be fixed at
  several of these independently before the actual entry path (file(GENERATE)
  attribution) was found — a clear "same fix in N places" smell. `isCcSrcEntry`
  centralized the *predicate*; the *routing* (append to hdrs vs srcs vs data,
  drop cross-package non-cc, dedup, CcEmbed header pairing, the has-cmake-codegen
  tag) is still scattered. Consolidate into a single
  `classifyAndAttach(irt, path, policy)` helper that every source-producing site
  calls, so a new non-cc/odd-extension shape is handled once. Audit the wider
  converter for the same pattern (cross-package relabeling, visibility
  publicizing, and exports_files also recur at multiple sites) and capture any
  further consolidations. Guard with the existing goldens + the abseil/glm/VTK
  surveys so the refactor is behavior-preserving.

- **Generator-parity uplift for the cmake converter.** The
  current cmake converter reads File API codemodel-v2 +
  `--trace-expand` and emits BUILD files; that recovers
  ~67% of FDSDK with fidelity gaps catalogued in
  `docs/cmake-conversion-deltas.md` and the genex / install
  bullets under `Later`. A hypothetical `cmake -G Bazel`
  generator running inside cmake's generation pass would
  resolve most of those gaps for free by virtue of being
  cmake. We don't need to build the generator — the same
  fidelity is reachable by extending hooks already in tree
  (`CMAKE_PROJECT_TOP_LEVEL_INCLUDES`, the `build.ninja`
  parser under `converter/internal/ninja/`, the
  `elementfold` fan-out, the `shadow.Extract*` trace
  decoders) so that cmake itself is the oracle for the
  residue the File API doesn't expose. **Bazel-idiom
  shaping is a first-class goal**, not a post-pass:
  sanitizer build types become `--features` on the
  cc_toolchain rather than raw per-config `select()`s;
  install(EXPORT) bundles become `cc_import` + `pkg_files`
  rather than tar-and-extract; custom commands become
  `genrule`s with depfile-derived `srcs`. `buildifier
  --mode=fix` + `gazelle fix` must remain no-ops over the
  output (the existing Phase 8 gazelle-roundtrip contract
  in `ROADMAP.md`).

  Multi-package split (opt-in, shipped): `convert-element-cmake
  --split-packages` emits one BUILD.bazel per directory (the
  "gazelle model") mirroring the CMakeLists/add_subdirectory
  layout instead of a single monolithic BUILD. Targets land in
  the package matching their declaring cmake dir; each include-
  root becomes a synthesized header `cc_library` (so cmake's
  single-`-I`-root include semantics survive the split), and when
  include-roots nest, an ancestor header lib forwards (via `deps`)
  to its descendant-root header libs so the recursive reachability
  monolithic emit gets from a single `-I<root>` is preserved
  (VTK's `vtk_module_third_party` forwarders); intra-
  element deps and cross-package sources are rewritten to label
  form; `exports.json` carries the sub-package label so cross-
  element consumers resolve the right package. `buildifier
  -mode=diff` is a no-op over every emitted BUILD (render gate
  `scripts/meta-cmake-split-packages.sh`). v1 boundaries: OFF by
  default and byte-identical to the single-BUILD emit when off;
  both the local and `--source-key` (orchestrator FUSE-sources)
  regimes are supported for the header-library idiom; mutually
  exclusive with `--out-ir-json` (the multi-platform fold path);
  install-derived / synthesized targets (filegroups, `cc_import`,
  `cmake_config_bundle`, aliases, interface libs) stay in the root
  package. Synthesized **output-producing rules** are the exception:
  a genrule, `write_file` bake, or `cmake_configure_file` lift whose
  recovered output lands in a sub-package is placed in that package
  (with the output path re-relativized package-local), because Bazel
  requires a rule's output to live in the rule's own package — a root
  rule declaring `outs = ["sub/x.cpp"]` (or `out =
  "include/llvm/Config/config.h"`) both collides with the deeper
  package's boundary and is unreachable from a consumer there that
  lists the file as a generated `hdr`. This is what lets a generated
  compiled source (e.g. eigen's `configure_file`-produced
  `compile_<snippet>.cpp`) feed a cc_binary that moved to its own
  sub-package, and a `write_file`-baked `llvm/Config/config.h` satisfy
  the `//include` header library LLVM's per-directory libraries depend
  on. Wired
  end-to-end through the
  orchestrator: `cmd/write-a --split-packages` converts the
  element with the `cmake_split_convert` custom rule
  (`rules_buildstream_bazel/rules/cmake_packages.bzl`), whose
  action declares the per-sub-package BUILD tree as a Bazel
  **TreeArtifact** (`ctx.actions.declare_directory`,
  content-addressed per file — no opaque tar; a genrule can't
  declare the discovered-at-action-time sub-package set as static
  `outs`), and `cmd/stage-b` merges that materialized directory
  into project B's `elements/<name>/` by per-file digest. See
  `docs/design/cmake-split-packages.md`.

  Phasing (each phase is a self-contained PR stack with its
  own render gate):

  - **Phase 1 — read what we already loaded (SHIPPED).**
    Consume the `backtraceGraph` indices on
    `Target.Dependencies[]` to recover PUBLIC/PRIVATE/INTERFACE
    keywords without re-parsing `--trace-expand` (trace stays
    as fallback for cmake < 3.21 where backtraces are
    incomplete) — **shipped (1a)**. Plumb
    `DirectoryInstaller.Type == "file"` / `"directory"` from
    `directory-*.json` into `ir.Package` so install(FILES) /
    install(DIRECTORY) lower to `pkg_files` (rules_pkg) at
    convert time, carrying the install DESTINATION as the
    `pkg_files` `prefix`, instead of an opaque filegroup /
    the round-2 install-tree.tar path — **shipped (1b)**.
    `convert-element-cmake` emits
    `load("@rules_pkg//pkg:mappings.bzl", "pkg_files")` and
    `cmd/write-a` adds `bazel_dep(rules_pkg)` to project B's
    MODULE.bazel when the graph has any kind:cmake element
    (coarse gate — write-a renders MODULE.bazel before the
    per-element converter runs, so it can't see whether a
    given element emits a pkg_files target; mirrors the
    rules_python precedent). install(DIRECTORY) lowers to
    `pkg_files(srcs = glob(["<dir>/**"]), prefix = "<dest>",
    strip_prefix = strip_prefix.from_pkg("<dir>"))` — a glob over
    the source directory's contents, not a bare directory in
    `srcs` (a bare dir doesn't package its files; a consuming
    `pkg_tar` fails with `IsADirectoryError`). install(FILES)
    keeps the literal `srcs` list. **Install granularity — done:**
    (a) per-file destination renames (cmake
    `install(FILES ... RENAME ...)`) now lift onto `pkg_files`
    `renames`. The File API records a renamed FILES installer as a
    `{"from","to"}` object (vs. the plain string of an un-renamed one),
    with `to` the destination name under DESTINATION; previously the
    object form was only decoded for *directory* installers, so a
    renamed file was silently dropped — it's now a `renames` entry
    (`{"<src>": "<rename>"}`, dest relative to the prefix). (b)
    install(DIRECTORY)'s two shapes are distinguished: trailing-slash
    "contents of `<dir>/` into DESTINATION" (`{"from","to":"."}` object)
    strips the whole source dir, while the no-trailing-slash form
    (`install(DIRECTORY include DESTINATION include)` →
    `include/include/...`, recorded as a plain string) strips only the
    dir's *parent* so the dir name survives under the prefix. Render
    gate `scripts/meta-cmake-install-files-pkg.sh` covers both (RENAME +
    no-slash directory).
    `shadow.ExtractSourceFileProperties` decodes per-file
    `set_source_files_properties` and lowering now consumes it
    — **shipped (1c)**: `HEADER_FILE_ONLY` → hdrs reclassify;
    `OBJECT_DEPENDS` → consuming target's hdrs (rebuild-trip
    edge); `LANGUAGE` → audit tag (Bazel has no per-source
    language override); `GENERATED` → a manually-marked
    missing source is kept (not elided) as a generator-output
    edge and tagged `cmake-declared-generated-source`;
    `COMPILE_DEFINITIONS` → folded into the target's `defines`
    when uniform across the target's sources, else tagged
    `cmake-per-source-compile-definitions-divergent`.
    *Limitation:* per-file COMPILE_DEFINITIONS that genuinely
    differ between sources in one target are not expressible in
    a single cc_library (Bazel's defines/copts are per-target);
    the operator's remedy is splitting the divergent sources
    into separate cc_library targets. No new cmake hooks; pure
    consumer-side wins on data the converter already pulls in.

  - **Phase 2 — request `configureLog-v1`.** Add a fourth
    File API object kind alongside codemodel / cache /
    cmakeFiles / toolchains in `fileapi.Index.requestQuery`
    (cmake 3.26+; gracefully absent on older). Decode
    `try_compile` / `find_package` / `check_*` outcomes into
    `fileapi.ConfigureLog`. **Shipped:** the probe-bucket
    `unsupported-execute-process` Tier-1 refusals are retired
    where the OUTPUT_VARIABLE is already a recorded
    try_compile / try_run result (`configureLogVars` projects
    each `buildResult.variable` / `runResult.variable` to
    cmake's "1"/"0", merged into the rescue `cmakeVars` in
    `lower.ToIR`) — and now also where it's a `find_package`
    outcome (`<Pkg>_FOUND` reconstructed from the event's
    `found.package` + `isFound`). A rescued probe's value
    flows to downstream configure_file / file(GENERATE)
    consumers through the same merged var map; nothing fails.
    **Deferred to Phase 5:** the cross-platform `select()`
    over `@platforms` config_settings. A single configure run
    yields ONE platform's configureLog, so there is no
    per-platform probe data to fold into a `select()` here —
    the value is baked from the single resolved outcome. The
    multi-config probe fold (per-config `CMakeConfigureLog`
    re-keyed alongside `Reply.Targets`, collapsed via the
    `empfold` cross-config primitive) lands with Phase 5's
    Ninja Multi-Config work; building a degenerate single-arm
    `select()` here would be dishonest about the data we have.

  - **Phase 3 — genex-probe TOP_LEVEL_INCLUDES extension.**
    The probe-staging hook (`probe-genex.cmake`, injected via
    `CMAKE_PROJECT_TOP_LEVEL_INCLUDES`) speculatively emits a
    `file(GENERATE)` per cmake target resolving the
    structurally-important genex shapes
    (`$<TARGET_FILE*:t>`, `$<TARGET_OBJECTS:t>`,
    `$<TARGET_PROPERTY:t,INTERFACE_*>`) at generation time;
    `cmakerun.ReadGenexProbe` reads the resolved bytes back
    and `lower/buildGenexTargets` folds them into the
    `genexeval.TargetInfo` the (a) Go-side evaluator
    consults. cmake is the oracle for those ops; the Go
    evaluator's `UnsupportedError` on those is now the
    no-probe-available fallback (offline / `--probe-genex=false`),
    exactly as queued. **Shipped:** the per-target probe
    feeds the lift end-to-end (live render gate
    `scripts/meta-cmake-genex-probe.sh` proves
    `$<TARGET_OBJECTS:obj>` in a `file(GENERATE)` CONTENT
    body resolves via the probe rather than refusing), and
    the audit tag set collapsed from
    `cmake-codegen-file-generate-genex{,-evaluated,-lifted,
    -cross-package}` to `cmake-codegen-genex-resolved`
    (resolved), `cmake-codegen-genex-unresolved` (legacy
    bytes-baked fallback), and `cmake-codegen-genex-cross-package`
    (refusal stub) — see `docs/codegen-tags.md`. Also fixed
    an empty-`$<CONFIG>` probe-read drop that silently lost
    every per-target value for no-build-type configures.
    **Generalized per-literal probe — mechanism shipped, via a
    warm second configure pass.** Collecting arbitrary `$<…>`
    literals (beyond the fixed per-target property set) needs a
    second configure: the exact literal strings aren't known
    until the first pass's trace is parsed, but resolving them
    means emitting `file(GENERATE CONTENT "<literal>")` which
    must be staged before configure starts. The loop is now
    closed: `cmakerun.{RenderLiteralProbeHook,ReadLiteralProbe}`
    + `Options.LiteralProbes` stage a per-literal hook
    (`cmake-to-bazel.litgenex/<hash>.$<CONFIG>.txt`, per-`$<CONFIG>`
    so it composes with Multi-Config, per-config divergence
    preserved for a future `select()`); `lower.LiteralProbeSink`
    collects unresolved literals on pass 1; and
    convert-element-cmake's `--two-pass-genex` (default ON) runs a
    *conditional, warm* second `Configure` against the same build
    dir (reuses the try_compile/find_package cache → a few % of
    the first configure; skipped entirely when pass 1 leaves
    nothing unresolved). The first consumer wired is the
    `file(GENERATE)` OUTPUT-path site (an arbitrary genex in
    OUTPUT now resolves to a static genrule `outs` instead of
    dropping the call); `scripts/meta-cmake-genex-literal-twopass.sh`
    pins it end-to-end (`$<TARGET_PROPERTY:app,APP_GENDIR>` in an
    OUTPUT → `gen_out/manifest.txt`, load-bearing under
    `--two-pass-genex=false`). The second consumer wired is the
    `file(GENERATE)` **CONTENT-body** site (tier b′ in
    `buildFileGenerateGenrule`): when the (a) Go evaluator refuses
    and the (b) structured-capture extractor can't anchor the
    per-genex values positionally — adjacent genexes with no static
    separator, ambiguous static chunks — `probeGenexValuesForBody`
    resolves each top-level `$<…>` literal individually via the warm
    second pass and replays them as a `--genex-values` literal-replace
    map, so the body lifts (`cmake-codegen-genex-resolved`) instead of
    baking the rendered bytes into srckey
    (`cmake-codegen-genex-unresolved`). The same gate pins it
    end-to-end (adjacent `$<TARGET_PROPERTY:app,GEN_PART_{A,B}>` →
    resolved, tag flips to `-unresolved` under `--two-pass-genex=false`).
    **add_custom_command argv — audit + portability landed.** Genexes
    are resolved away in `build.ninja`, so the unresolved `$<...>` only
    survives in the trace (`AddCustomCommandCall.Commands`); the genrule
    cmd is built from the resolved ninja command. `rewriteToolFromTarget`
    already lifts a `$<TARGET_FILE:t>`-derived build-dir-relative artifact
    path to a portable `$(location :t)` label + tools dep (now pinned by
    `standalone_genrules_genex_test.go`); the new
    `cmake-codegen-cmd-genex-{resolved,unresolved}` tags
    (`standalone_genrules_genex.go`) cross-reference each genrule back to
    its trace call and make that resolution visible — `-unresolved` flags
    the residue where a path-bearing genex baked a non-portable literal.
    **Per-config-divergent literals — done.** A file(GENERATE) genex
    literal that resolves to DIFFERENT bytes per build config (Ninja
    Multi-Config) no longer drops to legacy: `probeGenexValuesForBody`'s
    select()-capable sibling `probeGenexValuesPerConfigForBody` reads the
    probe's `LiteralResolution.PerConfig` and lowers the divergence to
    `CMakeConfigureFileSpec.GenexValuesPerConfig`, which the emitter
    renders as `genex_values = select({"//config:<name>": {...}, ...})`
    over the multi-config `//config` package (the rule's `string_dict`
    attr is configurable, so Bazel resolves the active config's map — no
    `.bzl` change). **Install destinations — N/A:** the File API codemodel
    is generated after cmake's generation phase, so `DirectoryInstaller`
    destinations are already resolved per-config — no `$<...>` reaches the
    converter; per-config destination divergence is the multi-config fold's
    job (Phase 5), not a genex-probe consumer. **Accepted residue:**
    `$<TARGET_OBJECTS:t>` substitution in custom-command argv (object lists
    aren't in the artifact map; the resolved-`.o`-sequence reverse-mapping
    is fragile and the pattern is rare) and cross-element
    `$<TARGET_FILE:t>` stay `-unresolved` — honestly audit-tagged rather
    than silently baked; revisit if a real project needs them. With these,
    the remaining-consumer surface is closed: every file(GENERATE) /
    custom-command genex is resolved, lowered to a `select()`, or
    audit-tagged, and the `UnsupportedError` / legacy-bake surface has
    stopped silently swallowing genex spend.

  - **Phase 4 — build.ninja custom-command walk.** Promote
    `converter/internal/ninja/` from its current
    `RERUN_CMAKE`-deps-only consumer into a full
    `CUSTOM_COMMAND` edge walker. Every edge becomes a
    `genrule`: `cmd` from the rule's resolved command
    (post-genex, post-VERBATIM-escaping), `srcs` from
    explicit inputs + depfile-derived implicit inputs,
    `outs` from the edge outputs. Standalone-edge emission
    shipped (`lowerStandaloneCustomCommands` in
    `converter/internal/lower/standalone_genrules.go`,
    behind `Options.EmitStandaloneCustomCommands`);
    trace cross-reference with
    `add_custom_command` / `add_custom_target` /
    `add_dependencies` call sites also shipped — emitted
    genrules now name themselves after the wrapping
    add_custom_target (when one exists) and open
    visibility from `//visibility:private` to `:__pkg__`
    when an in-trace consumer references the output.
    `execute_process_classify.go`'s `unknown` arm retired
    in the same slice: every refusal now carries a
    per-shape diagnosis (multi-COMMAND pipeline, opaque
    driver without OUTPUT_FILE, RESULTS_VARIABLE pipeline-
    state capture, etc.) rather than the catch-all "no
    recognized lift pattern" string. The bucket constant
    keeps the historical `unknown` value for failure.json
    triage continuity; the new `BucketRefuse` alias
    documents the role. The probe/stamp handling has since broadened
    from "rescue when captured" into a faithful split
    (`recoverExecuteProcess`): a recognized host/toolchain
    **probe** that produces no file artifact is SKIPPED
    unconditionally — it is never a build input, and its
    consequence (a captured value feeding a configure_file, a
    host triple landing in config.h, a tool capability landing
    in the recovered compile flags) is recovered independently —
    while a **stamp** still gates on the dump-vars capture. A
    stamp value that feeds a `configure_file`, though, no longer
    bakes its revision into srckey: it lifts to the
    `cmake_configure_file` rule's `stamp_values`, re-reading the
    live revision from the Bazel workspace status (`STABLE_<var>`
    in `ctx.info_file`, cache-keyed so a revision change
    re-renders) at build time under `--stamp` +
    `--workspace_status_command`, with the convert-time value kept
    as the no-`--stamp` fallback. Indirection through verbatim
    copies (`set(VERSION ${GIT_SHA})` then `@VERSION@`, the Google-
    Benchmark shape) lifts too: when pass 1 finds VCS-stamp vars,
    a warm second cmake configure captures the NON-EXPANDED trace
    (`--trace`, which keeps `${GIT_SHA}` verbatim where
    `--trace-expand` would substitute it), `ExtractSetAssignments`
    recovers the copies, and the stamp key propagates to the copy
    so the `@VERSION@` consumer lifts. A stamp wrapped in a helper
    FUNCTION — the `GetGitRevisionDescription.cmake` / `git_describe()`
    shape SDL and the hundreds of projects that copy that module use,
    where the `execute_process` OUTPUT_VARIABLE is a function-local
    handed back via `set(${_var} "${out}" PARENT_SCOPE)` — no longer
    ABORTS the clean convert: the local is the `SrcVar` of the
    recovered copy, and on a pass-1 stamp abort the converter re-runs
    the non-expanded-trace pass and re-lowers with that copy (the
    rescue is narrow to forwarded stamps; an uncaptured, unforwarded
    stamp still refuses). That forwarded value also LIFTS to
    `stamp_values` rather than baking: the same non-expanded pass feeds
    `ExtractParentScopeForwards`, which resolves the parent-scope return
    name `${_var}` to the caller argument (`get_git_sha(GIT_SHA)` →
    `GIT_SHA`) by binding the function's declared parameter to the
    call-site arg, and `applyParentScopeForwards` marks that consumer a
    stamp var (re-keyed to its own name, `STABLE_GIT_SHA`, since the
    function-local `out` is a name the operator never sets) so the
    `@GIT_SHA@` consumer re-reads the live revision from workspace
    status. A feature-
    declaration probe (`HAVE_*` / `USE_*` / `*_FOUND` / …) instead
    lifts to an operator-overridable `bool_flag` +
    `config_setting` — the Bazel idiom for "does the host have
    X?" — defaulting per writeback channel (`featureProbeDefault`:
    a RESULT_VARIABLE exit-0 means present; an OUTPUT_VARIABLE
    stdout goes through `cmakeTruthy`).
    `scripts/meta-cmake-execute-process-rescue.sh` drives a
    top-level `execute_process` probe whose value re-renders
    through a lifted `configure_file` at Bazel time, and its
    `--dump-vars=false` arm now pins the broadened skip — the
    uncaptured probe is benign (convert still exits 0), and the
    value still byte-recovers into the re-render. The originally-queued *sibling
    `file(GENERATE)` OUTPUT_VARIABLE hook* is resolved as
    follows: for the common top-level config-invariant probe
    it is **redundant** — dump-vars already DEFER-dumps every
    variable at end-of-directory, so the value is captured
    without a per-variable `file(GENERATE)`. Its only
    non-redundant value (capturing a *named* OUTPUT_VARIABLE
    set in a nested `add_subdirectory()` scope) carries the
    same second-configure-pass dependency as the Phase 3
    generalized per-literal probe — the hook must know the
    variable names before configure starts, but the names
    come from the post-configure trace — and folds into that
    two-pass work rather than duplicating it. The `stamp = 1`
    idea does not map to a standard `genrule` attribute (no
    `stamp` attr on `genrule`; a true non-baking stamp needs
    a custom status/repo rule), so BucketStamp's non-hermetic
    values correctly stay on the round-2 fallback. The probe
    `select()` is over `@platforms` host/toolchain
    `config_setting`s per the classifier's own contract;
    `select()` over build-config is Phase 5's job.

  - **Phase 5 — Ninja Multi-Config + sanitizer-as-feature.**
    `cmakerun.Options.BuildType` → `BuildTypes []string`;
    when `len(BuildTypes) > 1`, the argv builder switches to
    `-G "Ninja Multi-Config" -DCMAKE_CONFIGURATION_TYPES=…`.
    `fileapi.Reply.Targets` re-keys to `map[targetId]map
    [config]Target`; the existing `internal/empfold`
    cross-config fold collapses per-config compile/link
    fragments via `select()` over `//config:<name>`
    `config_setting`s, reusing the phantom-target select
    shape that handles per-platform absence today. **Bazel-
    idiom shaping**: for config names matching a known
    sanitizer set (`*San`, `MSan`, `TSan`, `UBSan`, `ASan`)
    or LTO / debug-info variants, lower the per-config
    fragments to `--features` on the cc_toolchain rather
    than raw selects — emit `//features:tsan_enabled`
    config_settings the operator wires to
    `--features=tsan`, and let the toolchain's feature
    definitions carry the `-fsanitize=thread` flags. Refuse
    projects where the trace shows `if(CMAKE_BUILD_TYPE
    STREQUAL "…")` branches affecting target-graph shape
    (silently no-op under multi-config; would produce
    wrong output). **Shipped.** `cmakerun.BuildTypes` drives
    the Ninja Multi-Config argv (`--build-types`, including the
    `auto` sentinel that lets the project's own configs stand
    without `-DCMAKE_CONFIGURATION_TYPES`; detection runs in the
    conversion action, not write-a); the `internal/configfold`
    cross-config primitive collapses per-config src/dep/flag
    deltas (`lowerMultiConfigDeltas` / `configLabel`) into
    `select()` arms over `//config:<name>`, backed by the
    `//config` package (`string_flag build_type` + config_settings)
    emitted by `converter/emit/configsettings`. The Bazel-idiom
    sanitizer lowering landed too — `configfold/features.go` +
    `sanitizer_flags.go` recognize the sanitizer/LTO config set and
    route their flags to `//features:*` cc_toolchain feature
    definitions emitted by `internal/emit/sanitizerfeatures` rather
    than raw selects. The graph-shape refusal is precise:
    `configOnlyTargetNames` flags targets that exist in only some
    configs and `--fidelity=strict` refuses exactly those (rather
    than the whole element). Wired end-to-end through write-a
    (`--build-types`, `multiConfigEnabled()` gating the
    bazel_skylib dep + `//config` package emit) and the
    `--split-packages` path. Render gates
    `scripts/meta-cmake-sanitizer-features.sh` (sanitizer feature
    lift) + `scripts/meta-cmake-split-multiconfig.sh` (split +
    multi-config TreeArtifact `//config` arms, stage-b, and a
    project-B `bazel build --//config:build_type=debug`).

  - **Phase 6 — install(EXPORT) declarative IR
    projection.** Classifier (`internal/exportshape.Classify`)
    + IR projection (`exportshape.EmitDeclarative`) +
    codemodel-only `EmitInputs` wiring
    (`exportshape.BuildInputs`) landed: declarative bundles
    produce a `<name>_import` `cc_import` per exported
    library + per-target header filegroups + a bundle-wide
    `cmake_config_bundle` filegroup, all from
    `Target.NameOnDisk`/`Target.Install.Destinations`/
    `Target.FileSets` HEADERS without running
    `cmake --install` at convert time. The
    resolved-lift manifest-synth (M3) step shipped:
    `buildExportsDoc` (convert-element-cmake) now
    populates the `*manifest.Export` `omitempty`
    `cmake_config_bundle_label` + `cmake_import_labels`
    fields from the lowered codemodel verdict — it scans
    `pkg.Targets` for the `cmake_config_bundle` filegroup
    + the `<lib>_import` cc_import facades (tagged
    `cmake-codegen-install-export-import`) and frames
    their labels with `--bazel-package-path`, so
    cross-element `find_package` consumers can resolve
    directly to the synthesized bundle. Non-bundle
    elements leave both fields empty (byte-identical
    exports.json). **Hard constraint preserved: convert
    is metadata-only — no `cmake --build` /
    `cmake --install` runs at convert time.** Earlier WIP
    that wired convert-time build was backed out (it would
    have changed the project's runtime model from
    sandboxable-and-cheap to "build farms"). The
    non-declarative residue stays on the round-2
    pick_file-over-install-root fallback. Consumer-side
    resolver — evaluated, declined: the manifest's
    `cmake_config_bundle_label` / `cmake_import_labels` are
    populated for external / future resolvers and the manifest
    loader parses them (they round-trip in
    `internal/manifest/imports_test.go`), but the converter's
    cross-element resolution intentionally does **not** consult
    them. Cross-element `find_package(<Pkg> CONFIG)` already
    resolves correctly via the build-tree `BazelLabel` dep +
    the synth-prefix bundle (see `scripts/meta-cross-cmake.sh`);
    re-pointing consumers at the `<target>_import` facade is a
    lateral form-change (installed-export vs build-tree, both
    valid in Bazel), not a correctness gain, and adds
    target→label mapping fragility (the flat, unkeyed
    `cmake_import_labels` list). Phase 6 is complete.

  - **Phase 7 — Bazel-idiom shaping audit.** A final-
    emission pass that audits the converter's IR against a
    Bazel-idiom checklist extending
    `ROADMAP.md`: known-config
    selects routed through `select_to_features`; install
    bundles routed through `pkg_files` / `pkg_tar`;
    IMPORTED targets emitted as `cc_import` rather than
    `cc_library(srcs=[…lib…])` placeholders; headers from
    `Target.FileSets HEADERS` routed through
    `cc_library(hdrs = …, includes = …)` with the right
    `strip_include_prefix` derived from
    `BUILD_INTERFACE`/`INSTALL_INTERFACE`; gazelle-friendly
    `# keep` placement on the residue that gazelle would
    otherwise drop. The `gazelle-roundtrip` conformance
    gate guards the contract; `cmd/relax-keeps` learns the
    new shapes.

    **Landed (slice 1) — header-only `strip_include_prefix`
    shaping.** A final-emission IR pass
    (`shapeHeaderOnlyStripIncludePrefix`,
    `converter/internal/lower/fileset_strip_prefix.go`, run in
    `ToIR`'s tail) lifts a header-only interface library's single
    include directory from the broad `includes = ["<d>"]` (`-I<d>`)
    form to the precise `strip_include_prefix = "<d>"` form when every
    exported header lives under `<d>` — only the declared headers are
    then visible, matching the FILE_SET HEADERS contract. Operating on
    the lowered package (not inside `lowerTarget`) catches interface
    libs from BOTH the codemodel path (`KindCCInterface`) and the
    trace-synth path (`lowerInterfaceLibraries`, which emits
    `cc_library` + the `cmake-codegen-interface-library-from-trace`
    tag and is where a consumed `INTERFACE_LIBRARY` actually lands).
    Conservative gates: genuine interface libs only (a plain
    `cc_library` whose srcs were elided keeps its compile-time
    `includes`); single include dir; all hdrs under it. The
    `interface-library` golden + a direct pass unit test pin it.
    **Landed (slice 2) — compiled-lib `strip_include_prefix`.** The
    companion `liftCompiledLibFileSetStripIncludePrefix` (run in
    `lowerTarget`) lifts the same FILE_SET HEADERS export dir to
    `strip_include_prefix` for libraries that ALSO compile sources.
    Unlike the header-only IR pass it MUST key on FileSet metadata — a
    compiled lib's `includes` come from CompileGroups and can be
    arbitrary `-I` roots, so it lifts only the include dir that is
    demonstrably a single FILE_SET HEADERS base dir with every header
    under it, keeping other `-I`s. The lift's regression guard runs in CI
    via the Go unit test + the `interface-library` golden (`go test`); a
    supplementary local render+build check
    (`scripts/meta-cmake-fileset-compiled-lib.sh`, `fileset-compiled-lib`
    sample — a standalone render gate like `meta-cmake-genex-probe.sh`,
    not wired into the `e2e-meta-*` CI list) does a real bazel-9 build
    confirming the lib's OWN sources and a consumer both still resolve
    `#include <pkg/hdr.h>` via the virtual include root — the risk that
    strip_include_prefix on a srcs-bearing target breaks its own
    compilation does not materialize.

    **Sanitizer `select` → `--features` — already done (no slice).**
    Investigated and confirmed implemented across earlier phases:
    multi-config sanitizer-shaped configs are filtered out of the
    `PerPlatform` select emission (`multiconfig.go` `nonFeatureConfigNames`)
    so no hand-rolled `select` is emitted, and single-config raw
    `-fsanitize=*` (with `-fPIC`/`-flto`/`-fvisibility=*`) in copts/linkopts
    are lifted to `features` by `liftRawFeatureFlags`. The `bazelidiom`
    `auditSanitizerSelects` is a backstop for operator-hand-rolled BUILD
    edits, not a converter gap.

    With both, **Phase 7's checklist is complete**: `select`→features,
    install→`pkg_files`, IMPORTED→`cc_import`, FILE_SET HEADERS→
    `strip_include_prefix` (header-only + compiled), and `# keep`
    placement all ship, guarded by the `bazelidiom` audit + the
    gazelle-roundtrip gate.

  Phase shape: overview / phase boundaries / acceptance criteria
  are tracked here. The hook protocols + fold semantics + classifier
  rules land as code comments on the implementation PRs rather than
  separate design docs.

  Acceptance: FDSDK kind:cmake coverage delta drops to
  near-zero (the structural residue is `try_compile`-keyed
  target-graph shape per `docs/research/cmake_analysis.md`
  §7, which the round-2 fallback covers by construction);
  the `cmake-codegen-*-genex*` audit tag set collapses to
  one `-resolved` tag (shipped — see Phase 3);
  `internal/genexeval`'s `UnsupportedError` surface shrinks
  toward gone (the structural per-target ops now resolve via
  the probe; the generalized per-literal probe for arbitrary
  `$<…>` literals remains queued — see Phase 3 "Not yet done");
  `cmake-conversion-deltas.md` "open deltas" closes the
  configurable items; render-gate output for known
  sanitizer configs uses `--features` rather than raw
  per-config selects. The genex / TARGET_FILE / TARGET_OBJECTS
  / INTERFACE_* aggregation items currently under `Later`
  retire as Phase 3 lands.

- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`) fail intermittently for
  environment reasons (cmake-config bundle staging on the CI
  runner; userns / fuse permissions on Ubuntu 24.04 runners;
  bazel 9 toolchain expectations). These don't reflect
  product issues but they make PR review noisier than it
  should be. The previously-listed `hello-fuse pipeline` and
  `cas-fuse against fake CAS` jobs were retired alongside
  `cmd/cas-fuse` itself; bb_clientd is the production
  CAS-aware mount story now.

- **Wire the cmake render gates into CI — partly landed.** The
  `e2e-meta-cmake-render-gates` aggregate target (Makefile `RENDER_GATES`)
  now runs the core render gates in the CI `bazel-e2e` job —
  `meta-cmake-genex-probe`, `meta-file-generate`,
  `meta-cmake-genex-literal-twopass`, `meta-cmake-fileset-compiled-lib`,
  `meta-cmake-stamp-volatile`, and the two `meta-cmake-vcs-stamp{,-indirect}`
  gates (previously Makefile-targeted but never CI-invoked) — so their
  convert→render→`bazel build` contracts (the load-bearing halves the
  Go-level unit tests + goldens don't cover) guard regressions on every PR.
  The aggregate guards on cmake + ninja up front (each gate self-skips its
  bazel≥9 build half), so it no-ops cleanly without the toolchain.
  **Follow-up:**
  the broader `meta-cmake-*.sh` render-gate family (install-export
  declarative, sanitizer-features, interface-genex-defines,
  probe-genex-object/utility, platform-partition-tier2, …) is still
  local-only; add each to `RENDER_GATES` once verified CI-safe (skip-clean +
  no heavy/special-toolchain or flaky-fetch dependence). Surfaced in #366
  review.

- **Volatile execute_process drivers (`date` + build identity).**
  Extends the #371 stamp lift beyond `{git, hg, svn}` to the other
  clearly-volatile / non-hermetic value sources, in two stacked PRs.

  **PR 1 (landed/in review) — close the hole + identity drivers.**
  Adds `date`, `whoami`, `id`, `hostid` to `stampDrivers`
  (`converter/internal/lower/execute_process_classify.go`), so their
  OUTPUT_FILE form no longer slips through to *file-producing* and gets
  HOISTED to a build-time genrule — running the tool on the executor and
  baking a non-hermetic value, the exact non-determinism the stamp bucket
  prevents for VCS (whose OUTPUT_FILE form is driver-first stamp, "can't
  hoist"). Build-identity values (`whoami`/`id`/`hostid`) are stable like
  a VCS revision → live `STABLE_` workspace-status keys (cache-keyed: a
  change re-renders). `date` is the exception: a wall-clock timestamp
  must NOT be a `STABLE_` key (it would bust the action cache every
  build), so in PR 1 its captured value *bakes* at convert time (stable,
  non-cache-busting) rather than lifting to a live stamp.

  **PR 2 (stacked, this) — live volatile `date`.** `stampStatusKey`
  is now driver-aware (`date` → a `VOLATILE_` key), and the shipped
  `cmake_configure_file` rule + `cmd/cmake-configure-file` read
  `ctx.version_file` (volatile-status.txt) IN ADDITION to
  `ctx.info_file` (stable-status) — but only when a `VOLATILE_` stamp
  key is present, so VCS/identity (`STABLE_`) stamps stay
  byte-identical. `--status-file` is now repeatable; the tool merges the
  two files' disjoint `STABLE_`/`VOLATILE_` namespaces. `date` is thus a
  live, cache-safe build-date stamp: its value changes per build without
  busting the action cache. Value source: operator-supplied via
  `--workspace_status_command` (like VCS), emitted as a volatile key —
  that script is where `SOURCE_DATE_EPOCH` belongs for reproducibility.
  Bazel's native `BUILD_TIMESTAMP` is volatile but epoch-millis, so a
  formatted `date +%F` template still wants the operator-level value; the
  converter's job is only to route `date` → the volatile key.
  (`whoami`/`id`/`hostid` need no volatile alternative — Bazel has no
  native identity key, and identity is correctly stable.) Surfaced while
  verifying the #371 vcs-stamp lift. `scripts/meta-cmake-stamp-volatile.sh`
  proves it end to end: a fixture stamping BOTH a git revision (STABLE_)
  and a build date (VOLATILE_) into one configure_file, converted and
  `bazel build --stamp`-ed with a workspace_status_command, re-reads both
  live values from stable- and volatile-status — and a no-stamp build
  drops the volatile value without leaking it.

## Next

- **Green grpc — the deepest `find_package` graph
  (abseil+protobuf+re2+c-ares+zlib+…).** protobuf is now green (libs + protoc +
  upb generators), which proves the whole find_package-availability +
  whole-include-tree machinery end-to-end; grpc is the next member to drive
  through it. The reusable mechanism, now in place:
  - **Host-install + imports-manifest → BCR.** Install each dep so
    `find_package(<Pkg> CONFIG)` SUCCEEDS at cmake time (else the project
    FetchContent-downloads it into the build dir, which the lens overlay can't
    stage), point `CMAKE_PREFIX_PATH` at the installs, and map the imported
    targets → BCR labels with `--imports-manifest` (+ `EXTRA_BAZEL_DEPS`). The
    abseil manifest auto-gen rule (name-match in `absl/*/BUILD.bazel`; strip
    `<dir>_internal_` → `//absl/<dir>/internal:<rest>`) is in
    scripts/build-lens/ and reusable; grpc needs the same for protobuf + re2 +
    c-ares targets.
  - **find_package whole-include-tree umbrella.** `find_package(<Pkg>)` puts
    Pkg's ENTIRE include dir on every consumer, so consumers `#include` headers
    for targets they never link (Bazel strict-deps rejects). The manifest's
    `umbrella_label` + `umbrella_include_roots` + the build-lens `extra_ws_setup`
    hook (generates `//absl_umbrella:absl` from
    scripts/build-lens/absl-umbrella-deps.txt = every public abseil target)
    model this. grpc will want the same umbrella for protobuf's headers.
  - REPRODUCIBILITY TODO (carried from protobuf): the .conf files hardcode
    `/tmp/absl-install` (host-installed abseil) and the umbrella deps list is a
    snapshot of the pinned abseil. Fold the abseil (and protobuf, for grpc)
    host-installs into the SessionStart hook so the lens is reproducible without
    a manual prep step.

- **Converter hang in `--diagnostics` mode on libevent's regress targets.** The
  `--diagnostics` convert of libevent spins indefinitely (observed 38+ min, no
  output) UNLESS `EVENT__DISABLE_TESTS=ON` — so the libevent lens scopes the
  regress tests off (libevent.conf), which also dodges a `test/regress.gen.c
  outside-build-dir` rejection. With tests off both converts complete in
  seconds with 0 rejections, so the loop is in the regress target graph (likely
  the custom-command / generated-source recovery over the regress test tree).
  Find + fix the loop so libevent's tests don't have to be scoped purely to
  avoid a hang. Lower priority than greening members, but a hang (vs a clean
  refusal) is a sharp edge worth removing.

- **Stage headers from a PRIVATE include dir with no public header lib.** How
  the existing machinery works (verified): lower emits a `target_include_
  directories(PRIVATE <dir>)` as an element-root-relative `-I<dir>` copt; split's
  `rewriteTarget` copt scan (split.go ~944) then keys on that `-I<dir>` and, IFF
  a header lib was synthesized for `<dir>` (i.e. `<dir>` is ALSO a public include
  root of some target, so it's in `incRoots`), wires the lib as a (private)
  header dep and drops the bare `-I`. That stages the headers + supplies the
  correct exec-root search path via the lib's `includes`. The GAP: a dir that is
  PRIVATE-only (no target lists it as a public include) gets no header lib, so
  the scan finds nothing, the `-I<dir>` stays element-relative (unresolved at the
  exec root), and the dir's headers are never staged. Blocks **mbedtls** (its
  always-on `framework` builds `mbedtls_test_helpers` → `#include
  "test/ssl_helpers.h"` from PRIVATE-only `tests/include`) and **sdl**
  (`SDL_uclibc` → `#include "SDL_internal.h"` from PRIVATE-only `src`).
  Implement (verified-safe shape — two pieces, BOTH required):
  1. **lower: discover the PRIVATE dirs' headers.** `includesForWalk`
     (lower.go ~2736, fed to `discoverHeaders`) starts from `irt.Includes`
     (the NON-private include attr) + each src file's dir — it does NOT include
     the PRIVATE `-I` copt dirs. So a PRIVATE-ONLY dir's headers never land in
     `irt.Hdrs`/`allHdrs`. Add the PRIVATE include dirs (the `emit` values from
     the `privateIncludeDirs` branch ~2459) to `includesForWalk` so their
     headers are discovered + declared.
  2. **split: synthesize + wire.** In planSplit add PRIVATE `-I` copt dirs to
     `incRoots` so `headerLibTarget` builds a lib (now non-empty thanks to #1);
     rewriteTarget's existing copt scan (~944) then wires it as a private header
     dep and drops the bare `-I`.
  WHY BOTH: with only #2, a PRIVATE-only dir gets an EMPTY header lib (its
  headers weren't discovered) AND the `-I` is dropped → regression. SDL's `src`
  happens to be safe with #2-only because another (non-private) target already
  declares `SDL_internal.h`, but mbedtls's PRIVATE-only `tests/include` is not —
  hence #1 is mandatory for a general fix. Needs full corpus re-validation (it
  touches header-lib synthesis broadly); two earlier point-fixes in this area
  regressed fmt and the cp-dir tests, so validate every green member before/after.

  RELATED mbedtls blocker (separate, AFTER header-staging — verified by getting
  past ssl_helpers.h to here): mbedtls GENERATES `query_config.c`, `error.c`,
  `version_features.c` via python scripts, then `link_to_source`-copies them,
  lowering to execute_process copy genrules with `srcs=[X]`+`outs=[X]` (in==out).
  Two failed shortcuts, both reverted: (a) "drop the dir copy when
  srcFileRel==outRel" broke legitimate `cp <srcdir> <build>` staging (the
  cp-dir-lift tests); (b) "drop the single-file copy" cleared the in==out and got
  mbedtls compiling 475/591, but then `ld: undefined symbol: query_config /
  list_config` — because X is GENERATED-ONLY (no usable committed source), so
  dropping its copy loses the content entirely. So the real fix is NOT to drop or
  rename the copy — it's to actually GENERATE these files: emit the python-script
  genrule (the genrule-script-staging item) that produces query_config.c et al.,
  and recognize the link_to_source copy as redundant with it. mbedtls needs:
  header-staging (DONE) + python-script source generation + then the in==out copy
  falls away (the generating genrule owns the output).

  REGRESSION LESSON (do not repeat): an earlier attempt "exec-root anchor the
  PRIVATE `-I` copt" (so `-Itests/libtest`→`-Ielements/<pkg>/tests/libtest`)
  made curl's unit-test build find headers it had in `srcs`, but it BROKE the
  split copt scan above for every member relying on it — the scan matches the
  element-relative form, so the exec-root form silently stopped wiring the header
  lib and regressed **fmt** (posix-mock-test: `#include <fmt/os.h>` "No such
  file", fmt's `include/` lib no longer wired). Reverted. The copt MUST stay
  element-relative; do the staging in split (header lib), not by rewriting the
  copt in lower.

- **Green the remaining heavyweight corpus members: grpc, vtk, cuda-samples.**
  23/26 are green (protobuf + sdl landed). The last three are each deep:
  - **grpc** — the deepest `find_package` graph (abseil + protobuf + re2 +
    c-ares + zlib). The whole mechanism is proven (see the grpc bullet under
    `Next`): host-install each dep so find_package succeeds, map imported
    targets → BCR labels via `--imports-manifest`, and use the find_package
    whole-include-tree umbrella (manifest `umbrella_label` +
    `//absl_umbrella:absl`-style generated lib). grpc's build is large — mind
    the disk-bounded build cycle in the large-project playbook below.
  - **vtk** — NOT disk-blocked (an earlier note wrongly claimed this; the
    container had ~22 GB of stale prior-session survey dirs under `/home/user/`
    masking ~25 GB of real free space — reclaimed). VTK configures, converts
    with **0 rejections** (vtk.conf: `--cmake-script-bake` lifts the 705
    `cmake -P vtkEncodeString.cmake` codegen commands, `VTK_GIT_DESCRIBE` skips
    the git stamp), and now LOADS + ANALYZES (1533 actions) after fixing the
    FILE_SET-HEADERS relativization bug (141 targets had `//pkg:/abs` labels).
    PRE-BUILD FIDELITY (run the compile-commands lens on the analyzed graph
    before chasing a green build — the right loop): 804 TUs matched cmake; it
    caught the string-define **quote-stripping** bug (VTK_PARSE_VERSION /
    LZ4_VERSION="1.8.0" / H5_ZLIB_HEADER reached the compiler UNQUOTED because
    Bazel's `defines` Bourne-tokenization strips single-escaped quotes — FIXED
    in emit, corpus-wide). Remaining fidelity diffs are over-broad-but-benign:
    `-I.` (package root) on every TU, HDF5 include breadth, VTK_PARSE_VERSION /
    H5_ZLIB_HEADER define over-propagation (PRIVATE→`defines`, same class as
    DEFINE_SYMBOL), and dropped `-fno-common`/`-ftrapv` (HDF5) vs added
    `-fvisibility*`.
    BUILD blockers (the real multi-step VTK lift):
    1. **Built-tool genrule recovery (proj.db) — LANDED + validated.** libproj's
       `generate_proj_db.cmake` pipes its .sql into a sqlite3 binary, and VTK
       hardcodes its OWN bundled tool via `set(EXE_SQLITE3
       "$<TARGET_FILE:VTK::sqlitebin>")` (the `find_program`/host path is
       disabled behind `if (FALSE)`, so host sqlite3 / `-DEXE_SQLITE3` is
       ignored — an earlier "needs host sqlite3" reading was wrong). The
       recovered genrule referenced the build-tree path `bin/Debug/sqlitebin-9.4`
       which doesn't exist in the sandbox. Fixed: `rewriteToolFromTarget` now
       lifts the `VAR=<artifact-path>` embedded form (gated on a new
       `ExecArtifacts` set so libs aren't mis-lifted) — proj.db now carries
       `EXE_SQLITE3=$(location …:sqlitebin)` + `tools=[…:sqlitebin]`, and the
       build advances past it. General capability (any in-tree codegen tool
       passed as a -D arg). (The bake's WORKING_DIRECTORY fix — lower's
       extractCdDir — also landed, helping OTHER relative-`include()` bake scripts.)
    2. **octree split-package `strip_include_prefix` — LANDED + validated.**
       Under `--split-packages` octree got its own package
       (`elements/vtk/Utilities/octree`) but kept the element-root-relative
       `strip_include_prefix = "Utilities/octree"`, which Bazel resolves relative
       to the PACKAGE → the doubled `…/Utilities/octree/Utilities/octree` (its
       `octree/*` headers "not under the strip prefix"). Fixed: rewriteTarget
       now emits the repo-root absolute form (`/elements/vtk/Utilities/octree`)
       for sub-package targets; root-package targets keep the relative form
       (no churn). abseil + glm re-validated green (`0 0 0 ok ok`); glm emits no
       strip_include_prefix so it's a no-op there.
    3. **KWSys cross-package header refs (NEXT blocker).** The build now aborts
       at `//…/Utilities/KWSys:Utilities_KWSys_headers` — a Visibility error:
       its synthesized `hdrs` list files (`Base64.h`, `CommandLineArguments.hxx`,
       …) that physically live in the `vtksys` SUB-package
       (`Utilities/KWSys/vtksys`), so the cross-package file refs aren't visible.
       The synthesized header-lib collection needs to either `exports_files`
       those headers in their owning sub-package or reference the sub-package's
       header-lib label — the split-emit cross-package-source handling
       (`exportsByDir`) extended to synthesized header libs. (~20 consumers all
       report the same one root cause.) The build is still in ANALYSIS — no TU
       has compiled yet; expect more split-package edge cases before the large
       compile.
    A converter-shaped lift, not disk- or scale-blocked.

    UPDATE — analysis is now FULLY GREEN (2359/2359) and the COMPILE runs.
    proj.db (built-tool recovery), octree (split strip_include_prefix), KWSys
    (cross-package generated-header publicize), and the wrap-hierarchy
    `.args/.data` (non-cc generated outputs → data, not cc srcs) all landed.
    LANDED since: the **vtkModuleAutoInit define-driven generated-header** wiring
    (501→0 — `wireDefineDrivenGeneratedHeaders` synthesizes a wrapper with the
    right `includes` so the basename include resolves), the **eigen extensionless
    headers** (Dense/Core/Eigenvalues — `discoverHeaders` now content-sniffs
    extensionless C++ headers), and `.txx/.tcc/.ipp` added to headerExts
    (vtkImageProgressIterator.txx). `bazel build //...` now compiles **~6,345 /
    6,366 (~99.6%)**.
    REMAINING TAIL (~20, well-diagnosed — the documented follow-up):
    - **configure_file config-headers not wired to consumers (~19):**
      `kwsysPrivate.h` (15), `proj_config.h` (4), `pugiconfig.hpp` (3). A header
      `configure_file(... COPYONLY)` output, #included by BARE quote name from a
      same-dir source — cmake needs no `-I` (quote resolves same-dir), so
      `targetBuildIncs` never records it and the prefix-match attribution misses
      it; the consuming multi-language SUB-library never declares the generated
      header. A same-dir-attribution pass was added but DOESN'T engage for these:
      instrumentation showed `lowerTarget`'s `t.Name` for the kwsys-consuming
      target is NOT "vtksys" (the converter renames on emit) and/or VTK's
      configure_files don't reach the `configureFiles` slice the attribution
      iterates — the precise recovery-path/target-identity needs one more
      instrumented pass. Fix lands the output in the consuming sub-lib's hdrs
      (rides `splitCompileGroups`' sharedHdrs).
    - **2 genrule-EXECUTION failures:** `proj_db` (`cmake -P generate_proj_db.cmake`
      fails at `include(sql_filelist.cmake)` — relative include not staged in the
      genrule's cwd at build time) and `vtkCommonCore-hierarchy.txt`
      (`vtkWrapHierarchy: couldn't open @…hierarchy.Debug.args` — the `.args`
      response-file, routed to `data`, isn't staged as a genrule input at the
      expected path). Both are build-time genrule input-staging fixes.
    - misc: `lz4.c` (1).
    provisioned in the default web session and a multi-GB install. (Verify by
    trying, not assuming — the vtk "disk-blocked" claim was an untested
    assumption that turned out false.)
  The converter features sdl + vtk needed are all generic and landed:
  multi-config `file(GENERATE)` glob fan-out, per-config include relativization,
  cmake PCH `-include` drop, select-arm cross-package relabel, and FILE_SET
  HEADERS path relativization. The remaining members lean on dep-availability
  (`tools/install-survey-deps.sh`) + hermetic `cmake -P` script execution +
  scale.

  DISK NOTE (corrected): the real ceiling is ~37 GB, and a clean session has
  ~25 GB free — ample for grpc/vtk builds. The earlier "~3 GB, disk-blocked"
  reading was stale prior-session survey dirs (`g-*`, `revisit`, `final-val`,
  …) accumulating under `/home/user/`; reclaim them between runs. Always
  `df /` and check `du -xsh /home/user/*` before concluding disk is the limit —
  and clean per-project `.bzcache`/`build-ws` under `--out-dir/<member>/`.

- **Faithful SHARED-library conversion (`cc_shared_library`) — Phases 1/2/2b
  LANDED & validated; remaining: corpus-wide re-green + edge cases.** The WHOLE
  POINT of shared is FIDELITY — to build what cmake would actually build. The
  survey forces `BUILD_SHARED_LIBS=OFF` for simplicity, but static is NOT
  cmake's/the project's default; that forced-static is the deviation this work
  removes. Historically lower collapsed `SHARED_LIBRARY`/`MODULE_LIBRARY` → a
  plain `cc_library`, losing the `.so` / dynamic linking / symbol-boundary
  semantics — wrong where the shared boundary is load-bearing (curl's tests
  SIGSEGV under static-collapse because the `.so` should hide the curlx
  utility symbols; `MODULE_LIBRARY` dlopen plugins; per-`.so` global state).
  **Landed:**
  - `--emit-shared-libraries` (survey: `SURVEY_SHARED=1`, which also drops the
    forced static so the project builds its NATURAL config). lower sets
    `ir.Target.SharedLibName` for SHARED/MODULE targets; emit renders a sibling
    `cc_shared_library(name=<t>_shared, shared_lib_name=<NameOnDisk>, deps=[":<t>"])`
    alongside the static impl. Default emit byte-identical (opt-in).
  - Consumer `dynamic_deps` wired (`wireDynamicDeps`): consumers keep the impl
    in deps (headers) + the `_shared` sibling in dynamic_deps → Bazel links the
    real `.so` (validated: example binary's ELF shows `NEEDED libz.so`).
  - Multi-shared-lib graphs: the wrapper gets its OWN dynamic_deps to sibling
    shared libs (`SharedLibDynamicDeps`) so it doesn't statically re-link a
    cc_library another shared lib owns ("linked more than once"); the
    shared_lib_name appends the cmake SOVERSION when the unversioned name would
    collide with the impl's auto `lib<t>.so` (brotli).
  - Split (build-lens) path: dynamic_deps + the wrapper labels relabel
    cross-package (`rewriteSharedDeps` resolves `<lib>_shared` to the impl's
    package — curl's `libcurl_shared` in `//elements/curl/lib`).
  Validated green under `SURVEY_SHARED=1` (9/9 probed): zlib, fmt, libxml2,
  brotli (multi-lib), curl (multi-package + the SIGSEGV root-cause — now fixed
  by the real `.so`), glog, spdlog, mbedtls (multi-lib), protobuf
  (find_package(absl) + umbrella + many libs).
  **Remaining:** run the WHOLE build-lens corpus under `SURVEY_SHARED=1` (incl.
  protobuf/abseil, sdl, OpenBLAS, the heavy LLVM/VTK) and fix fallout; carry the
  `.so` in runfiles for `bazel run`/test; `MODULE_LIBRARY` dlopen semantics;
  and consider flipping `SURVEY_SHARED` to the DEFAULT once the corpus is green
  under it (so green + the fidelity lens run against the config cmake produces).

- **Test-target coverage — enable the scoped-out members' tests.** The build
  lens builds `//...`, which already INCLUDES test targets where the project's
  tests need no extra infra: tests build green today for fmt (20 `cc_test`),
  libxml2 (8), glog (10, via `--dynamic_mode=off`), glm, googletest, abseil
  (test-off but the surface compiles); curl builds its test PROGRAMS (cc_binary,
  perl-harness-driven). The remaining members scope tests out via a `.conf`
  flag, each for a concrete reason — to enable, resolve that reason:
  - **spdlog** (`SPDLOG_BUILD_TESTS=OFF`): tests need `find_package(Catch2 3)`.
    Catch2 IS a corpus member (3.5.3) — wire it cross-element via the imports
    manifest + a host-install prefix (the protobuf↔absl pattern).
  - **nlohmann-json** (`JSON_BuildTests=OFF`): tests `#include` a generated
    `test_data.hpp` whose data is a `git clone` of `json_test_data` (network) —
    stage the data dir + point `JSON_TestDataDirectory` at it.
  - **mbedtls** (`ENABLE_TESTING=OFF`): test suites are `.c` generated from
    `.data` + `.function` by `generate_test_code.py` (python add_custom_command)
    — verify the converter recovers those as genrules.
  - **libevent** (`EVENT__DISABLE_TESTS=ON`): `regress` needs `regress.gen.c`
    from `event_rpcgen.py` (python codegen) — same genrule-recovery check.
  - **eigen** (`EIGEN_BUILD_TESTING=OFF`): ~900-target `-Werror` SIMD suite,
    self-contained (no ext dep/codegen) but a huge build — needs a scoped/
    sharded build, not `//...` in one shot. Deferred dev surface.
  - **openblas** (`BUILD_TESTING=OFF`): utest is C but the BLAS test surface
    pulls the Fortran reference — gated on the (deferred) Fortran ruleset.
  - **protobuf** (`protobuf_BUILD_TESTS=OFF`): needs googletest as a dep
    (BCR module / corpus member) wired like abseil's `GTest::gmock`.

- **Final corpus validation pass before declaring the converter "done."**
  Independent of any single feature: when the corpus is considered complete, do
  one clean-room full pass — every build-lens member fetched fresh, converted
  from scratch (no stale `build/bin` binary, no warm out-dir), `bazel build
  //...` green, AND the lens's run/execution checks green (the unit-test-style
  "does it actually run" probes, e.g. curl's unit tests passing) — on a machine
  with enough disk for the large members (LLVM `TOOLS=ON`, VTK) so nothing is
  scoped out for disk. Capture the result as the corpus's "all green, no cmake"
  baseline. This is the acceptance gate, distinct from the per-change
  re-validation the dev loop already does.

- **Make the host-system-library fallback EXPLICIT (hermeticity boundary).**
  When a `find_package`/`target_link_libraries` link fragment resolves to a
  standard system library (`/usr/lib*`, `/lib*`, `/usr/local/lib*`) and the
  imports manifest has no entry for it, the lower lifts it to a `-l<name>`
  linkopt (`converter/internal/lower/lower.go`: the `systemLibName(path)`
  sites — the find_package-attributed branch AND the attribution-missed
  branch). This is what makes LLVM's `opt`/`llc` link against host zlib. It
  is **not hermetic**: the build relies on the host toolchain's library
  search path containing `libz.so` etc. Today the lift is **silent** — there
  is no signal in the emitted BUILD that a target took a host dependency.
  Decide + implement the explicit contract: (a) emit a visible marker on
  every host-syslib lift (e.g. a `cmake-codegen-host-syslib=<name>` tag and
  an idiom-audit finding) so host coupling is auditable; and/or (b) gate the
  lift behind an opt-in flag (default: refuse with a typed failure pointing
  at the imports manifest), so the hermetic path (map `<Pkg>::<Pkg>` →
  a BCR module like `@zlib//:zlib` via the manifest) is the default and
  host-coupling is a conscious choice. The manifest is already the hermetic
  channel (abseil→googletest); this item is about not silently bypassing it.

- **Build curl's `docs/` manpage genrules.** curl's test surface now builds
  (`BUILD_TESTING=ON`; the ninja-recovery exec-root + cd-stripped-output
  anchoring that did it shipped — see docs/survey-corpus.md curl row), but the
  lens still scopes `docs/` off (`BUILD_LIBCURL_DOCS=OFF`). The `docs/` tree is
  manpage generation: genrules running perl helpers (`cd2nroff`/`managen`/
  `mkhelp.pl`) over ~300 `.md` files, often with a different shape than the test
  codegen (whole-directory `managen` inputs, `>`-redirect outputs, multi-input
  staging). Verify which of those build under the current anchoring and close
  the remainder, so docs build faithfully instead of being scoped away. It's a
  documentation surface, not library/test code, so lower priority than the test
  side that's now green.

- **Build-lens fidelity (compile-commands lens) — defines + -std + includes +
  copts + link-order(v1) LANDED & wired; remaining: link-order project-archive
  layer.** The build lens proves
  `bazel build //...` succeeds, but a build can succeed with the WRONG per-TU
  flags (the `BUILDING_LIBCURL` / `HAVE_ZLIB` leaks compiled fine while applying
  a macro to TUs cmake never gave it). Shipped: `cmd/compile-commands-diff` +
  `scripts/compile-commands-lens.sh`, wired into `run-survey.sh` as the 5th lens
  `SURVEY_COMPILE_DB=1` (runs after convert but BEFORE the build's compile —
  `aquery` needs only analysis — writing `<out>/<name>/fidelity.json`). It diffs
  cmake's `CMAKE_EXPORT_COMPILE_COMMANDS=ON` db against Bazel's
  `aquery 'mnemonic("CppCompile",//...)'` (built-in, hermetic) per TU on:
  **(1) defines** (set-diff, filtering Bazel's `__DATE__/__TIME__/__TIMESTAMP__`
  reproducibility stamps); **-std**; and **(2) includes** — both sides
  normalized to one source-relative space (`normalizeInclude`: cmake absolute
  paths strip cmakeSrc→source-rel / cmakeBuild→`gen:` / `/usr`→`sys:`; Bazel
  exec-root paths strip the element package→source-rel /
  `bazel-out/<cfg>/bin/<pkg>`→`gen:` / `external/`→`ext:`), so source includes
  diff exactly and the `gen:/sys:/ext:` build-layout/toolchain noise is filtered
  from the headline. First real catch: zlib's `DEFINE_SYMBOL ZLIB_DLL` leaking
  to `example.c`/`minigzip.c` — FIXED (DEFINE_SYMBOL→`local_defines`); zlib is
  now fully fidelity-clean (0/0/0/0).
  Also LANDED: **(3) copts** (project-authored flags only — `interestingCopt`
  keeps `-fvisibility=`/`-fno-rtti`/`-fopenmp`/`-march`/`-pthread`, filters
  optimization/debug/warnings/hardening/PIC toolchain + build-mode defaults),
  and **link-line ORDER v1** (`linkOrderDiff`: cmake codemodel
  `link.commandFragments` via fileapi — Ninja emits no `link.txt` — vs Bazel
  `aquery 'mnemonic("CppLink",//...)'`, output binary resolved by walking the
  pathFragment tree; reports relative-order inversions per matched executable).
  Remaining (PARKED — extend the link-order check to compare ALL libraries in
  order, not just system libs):
  - The v1 compares SYSTEM libs only (stdc++/m/pthread/dl/rt — stable identity
    both sides), which is empirically LOW-YIELD: pure-C members link no
    allowlist system libs (zlib's exes link only `libz.so`), and others link
    ssl/crypto/z as paths. The goal is to diff the FULL ordered link line —
    system libs AND project archives AND find_package/external deps — since the
    first-to-satisfy-a-symbol rule applies across all of them. That's gated on
    cross-build-system identity matching for the non-system libs: map cmake's
    link-fragment path basename → target via `NameOnDisk`, and Bazel's mangled
    `-lelements_Szlib_Slibzlib` → target by reversing the solib escape
    (`_S`→`/`, `_U`→`_`, basename, strip `lib`) — both land on the cmake
    `Target.Name`, the common key; external/find_package libs map via the
    imports manifest's BazelLabel. Also handle Bazel `.a`-path link forms
    (static mode) vs the solib `-l` form (default dynamic), and the
    static-vs-dynamic caveat (dynamic linking is order-independent, so a
    project-archive order divergence only matters where Bazel links static).
  Caveats still open: TU keying is by basename (collides across dirs in big
  trees; disambiguate by relative-suffix), config alignment (cmake db is
  single-config; defines/-std/includes are largely config-stable).

- **Derive `target_libc` / target triple from the probed sysroot.**
  `builtin_sysroot` now ships: the probe lifts `CMAKE_SYSROOT` into
  `toolchain.Model` and the emit sets `cc_toolchain_config`'s
  `builtin_sysroot` per (platform, kit), so Bazel passes `--sysroot=` to
  compile + link (host builds emit no `builtin_sysroot`, leaving their
  `toolchains/BUILD.bazel` unchanged).
  Still baked, though, are `target_libc` (the `defaultLibcFor` OS-name
  heuristic) and the `abi_version = "local"` / `*_system_name`
  placeholders — these are really "what the sysroot would tell us." Next:
  derive them from the probed sysroot/compiler triple instead. (Also worth
  pairing: `toolchain()` emits only `target_compatible_with`, never
  `exec_compatible_with`, so cross exec≠target resolution is unconstrained.)

- **Hermetic sysroot-as-toolchain-inputs.** `builtin_sysroot` tells the
  compiler *where* the sysroot is; for a sandboxed / RBE action to
  actually *contain* it the sysroot's files must be declared as toolchain
  inputs (`cc_toolchain.all_files` / `compiler_files` / `linker_files` /
  `libc_top`). The emit currently sets `all_files = ":empty"`
  (`unified.go`), i.e. a deliberately non-hermetic toolchain that leans
  on absolute host paths (`/usr/include`, `/usr/bin/gcc`) being present
  in the action — fine under local/host-mounted sandboxes, wrong for
  hermetic RBE. Materialize the sysroot tree as a Bazel repo
  (`new_local_repository` / `http_archive`) and wire `libc_top` /
  `all_files` so actions ship the sysroot. The jump from "references host
  paths" to "ships the sysroot"; larger, follows the `builtin_sysroot`
  item.

- **Agent-actionable prompts for no-mechanical-form constructs — AI
  post-pass (consumer) remains.** The deterministic **producer** + the
  consumer **contract** shipped, and the producer is now **on by default
  and wired through to project B**. `convert-element-cmake` emits a
  deterministic, byte-identical `conversion-todos.json` by default
  (`--conversion-todos=false` opts out; destination is
  `--conversion-todos-report=<p>` if set, else
  `<dir(out-build)>/conversion-todos.json`; `--conversion-todos-preamble=<f>`
  overrides the preamble) — the `todos.Collector` in
  `converter/internal/todos`, plumbed through `lower.Options.Todos`,
  carrying the operator **preamble** + one grouped
  `{id, kind, disposition, group_key, anchors, evidence, suggested_shape,
  prompt}` entry per unit. **Full coverage of the refusal + bake surfaces**
  (`converter/internal/lower/todos_producers.go` + `todos_coverage.go`): the
  three breadcrumb sites — `cmake-p-test` (`add_test(COMMAND cmake -P …)`),
  `cmake-internal-drop` (filtered command edges), and
  `install-script`/`install-code` (each alongside its retained stderr
  warning) — plus generic producers mirroring **every Tier-1 refusal**
  (`rejection:<code>`, diagnostic mode only — a refusal aborts in normal
  mode), **every convert-time bake** (`bake`, from `collectBakedEntries`),
  and **every unresolved-genex audit tag** (`genex-unresolved`). Each entry
  carries a best-guess **`disposition`** (`actionable` | `improvement` |
  `informational`) — a *fallible hint*, not a gate: the preamble invites the
  agent to upgrade an improvement/informational entry when it sees a better
  form (e.g. a baked check/try_compile probe option → a `config_setting`/
  `select` over platform/sysroot/toolchain). Defaults are per-producer with
  **per-site overrides** (a hoisted VCS/identity/date stamp bake is
  `actionable` while a baked check-probe under the same tag stays
  `improvement`). The preamble's **`environment` block** states the target
  Bazel version + canonical rule providers (`@rules_cc` / `@rules_shell` /
  `bazel_skylib` / `rules_pkg`) + the buildifier/gazelle gate, and names the
  rendered `MODULE.bazel` as a handoff input; README's *"Post-conversion
  prompts for an AI agent"* documents the handoff bundle (worklist + rendered
  `BUILD.bazel` + `MODULE.bazel` + the CMake sources the anchors point at).
  The orchestrator carries it to project B: the `<name>_converted` convert
  genrule declares `conversion-todos.json` as an output (and the
  `cmake_split_convert` rule writes it into the `packages` TreeArtifact), and
  `stage-b` lands it in `elements/<name>/conversion-todos.json`. The survey
  aggregates it alongside `rejections`/`bazel-idiom`/`coverage` (run-survey.sh
  `todos` column). Idempotency is the stable line-free `id` + the
  file-ownership split (the converter-owned `BUILD.bazel.out` stays
  byte-identical and marker-free; the post-pass authors into a separate file
  keyed by `id`).
  **What's left:** the non-deterministic **AI post-pass** that consumes
  the report to author the Bazel form (an `sh_test`/`diff_test` driving the
  built CLI, one reusable macro per shared unit) — deliberately quarantined
  out of the converter so it stays a pure replayable function. It honors
  the contract above: read `preamble` + `todos`, author one unit per `id`
  into the authored-output file (skip ids already present), turn
  `evidence.verification` into the test's assertion, emit plain idiomatic
  Bazel (no cmake re-invocation), and pass the **same render gates** as
  mechanical output (not trusted on faith). Surfaced from the brotli
  test-form discussion.
  **Follow-up — root-package source exports for the post-pass.** A
  real-corpus dry-run (glog v0.7.1) surfaced a gap in the file-ownership
  split: when the post-pass authors a test into a *sibling* package
  (`tests/BUILD.bazel`), its `cc_test` / `sh_test` call sites need the
  converter-owned root `BUILD.bazel` to `exports_files([...])` the
  cmake-test-only `.cc`/headers (sources no converted target lists, so the
  converter never exports them) — but rule (1) forbids the agent from editing
  the converter-owned BUILD. Options: (a) have the converter export
  test-referenced loose sources behind a stable `filegroup`; (b) let the
  post-pass author a `tests/` package *with its own* `exports_files` by
  staging the sources there; (c) relax rule (1) to permit append-only
  `exports_files` blocks. Pick one when the consumer ships.

- **A-B-C fidelity harness — productionized (CI-wired, BLOCKING).**
  Runs in CI as the `fidelity` job, now **blocking** — the
  `continue-on-error` soft-launch was dropped from every fixture step after
  the soft-launch period (all six run green, 0 impactful deltas), so a red
  is a real fidelity regression. The bazel half fetches BCR deps
  (rules_cc / rules_pkg / zlib), so to keep a blocking gate off the
  transient GitHub-releases 502 / TLS-timeout noise, `run-fidelity.sh`
  builds against a persistent `--repository_cache` (persisted across CI
  runs via `actions/cache`) — archives fetch once and are reused, so only
  the first cold populate touches the network, ridden out by a raised
  `--experimental_repository_downloader_retries`. Wiring it originally
  surfaced + fixed a rot: the harness
  staged a WORKSPACE-mode workspace and *stripped* the converter's
  `load("@rules_cc//…")` to use Bazel's native cc rules, but (a) the
  converter's `load()` symbol list had drifted (`+cc_test`, `+@rules_pkg`)
  so the fixed-string strip no longer matched, and (b) Bazel 9 removed the
  native cc rules outright. Reworked `run-fidelity.sh` to stage a bzlmod
  `MODULE.bazel` declaring `rules_cc` / `rules_pkg` as bazel_deps (versions
  tracking write-a's project B) and build the converter's *real* emitted
  BUILD unmodified — more faithful, and resilient to future `load()`
  drift. All five gates green after the fix (zlib lib+consumer, fmt
  lib+consumer, spdlog lib — 0 impactful deltas each). Foundation:
  `cmd/fidelity-compare` Go tool + `scripts/run-fidelity.sh` driver +
  `make e2e-fidelity-compare-{zlib,spdlog,fmt}` library-side gates +
  `make e2e-fidelity-compare-{zlib,fmt}-consumer` consumer-side gates +
  `testdata/fidelity/*.allowlist.txt` companions.
  Drives the full A-B-C cycle (cmake build → convert → bazel build →
  fidelity-compare → classify against allowlist) and exits non-zero on
  impactful deltas (unexplained symbol drops, hermeticity leaks).
  Built-in heuristics auto-classify the common benign categories:
  FORTIFY_SOURCE / stack-protector hardening symbols, C++
  template-instantiation pairs (matched on shared mangled-prefix),
  `.o` vs `.pic.o` archive-member name differences. Self-skips the
  bazel half cleanly when bazel isn't on PATH; honors RULES_CC_TARBALL
  for hosts that vendor rules_cc.

  Two complementary signals:
    - **Library-side** (default mode): diffs the cmake-built `.a` and
      Bazel-built `.a`. Verifies the converter preserves the
      project's own internal symbol set.
    - **Consumer-side** (`--consumer-file`): compiles a small
      consumer .c/.cpp twice (once against cmake's installed
      headers, once via Bazel as a `cc_library` depending on the
      converted target) and diffs the resulting `.o`s. Verifies
      the converter preserves the project's *exported contract*:
      `INTERFACE_INCLUDE_DIRECTORIES` reachability,
      `strip_include_prefix` resolution, INTERFACE_COMPILE_DEFINITIONS
      propagation. Works for any project including header-only.

  Per-project gate status:
    - **zlib v1.3.1** ✅ library + consumer both shipped — 105/105
      lib-side exact, 1/1 consumer-side exact (empty allowlists).
    - **spdlog v1.14.1** ✅ library + consumer both shipped — 1404/1404
      lib-side (5 template-instantiation allowlist entries); 63/63
      consumer-side exact, 0 impactful, empty allowlist. spdlog is a
      compiled library (`PUBLIC SPDLOG_COMPILED_LIB`), so the consumer
      compiles in compiled-lib mode against the converted target;
      `run-fidelity.sh` replays the define on the cmake side
      (`--consumer-cmake-cflags`) and compiles both sides at `-O2` so the
      template-instantiation symbol sets are comparable (the `-O0`
      fastbuild default otherwise floods the diff with unpaired weak
      symbols — a harness artifact, not a converter delta).
    - **fmt 11.0.2** ✅ library + consumer both shipped — 146/146
      lib-side, 1/1 consumer-side; 3 lib-side + 4 consumer-side
      template-instantiation allowlist entries.
    - **nlohmann-json 3.11.3** ✅ consumer-side shipped — header-only
      INTERFACE library (no static archive to diff). The earlier
      "blocked on converter" note is stale: the converter now lowers the
      `INTERFACE_LIBRARY` target to a `cc_library(hdrs = [...], includes
      = ["include"])` (the `cmake-codegen-interface-library-from-trace`
      synthesis, driven by `target_include_directories(... INTERFACE ...)`
      in the trace), so a consumer's `cc_library(deps = [":nlohmann_json"])`
      resolves the headers + include path. Verified: a consumer
      `#include <nlohmann/json.hpp>` compiles against the converted
      target, and `make e2e-fidelity-compare-nlohmann-json-consumer`
      passes (0 impactful deltas, benign auto-classified). Wired into the
      CI `fidelity` job.
    - **Catch2 3.5.3** ✅ library-side shipped — `make
      e2e-fidelity-compare-catch2` passes (0 impactful). `run-fidelity.sh`
      grew a `--convert-flags` passthrough (threads `--lift-configure-file`
      to recover `catch_user_config.hpp`) and auto-stages
      `//tools:cmake-configure-file` when the converted BUILD references
      it. Wiring it surfaced a converter bug, now fixed: the lifted
      configure_file genrule's output dir (`generated-includes/`) wasn't
      added to the consuming cc_library's `includes`, so the header
      landed in `hdrs` but `#include <catch2/catch_user_config.hpp>`
      couldn't resolve (`addBuildDirIncludes` in `lower.go`). Also a
      classifier improvement: unpaired std::/compiler-internal template
      instantiations now auto-classify benign (Catch2 emitted ~100 of
      them — toolchain variance the converter never controls); a 5-entry
      allowlist covers Catch's own template dtors.
    - **libpng 1.6.43** ✅ library-side shipped — `make
      e2e-fidelity-compare-libpng` passes (0 impactful). It exercises the
      whole deferred-blocker set, none of which needed new converter work
      once PR #350 landed: (1) the `cmake -E create_symlink` install
      aliases (libpng16.pc → libpng.pc, libpng16-config → libpng-config)
      skip via #350's install-compat-alias rule; (2) the `cmake -P`
      script-generated headers (`pnglibconf.h`, `pngprefix.h`, …) bake via
      `--cmake-script-bake` (self-contained base64 genrules, no runner
      tool); (3) `find_package(ZLIB)` resolves to `@zlib` via
      `--imports-manifest` (`testdata/fidelity/libpng-imports.json` maps
      `ZLIB::ZLIB` → `@zlib`); (4) `--bazel-external` adds the zlib BCR
      module so `@zlib` resolves. One delta — an undefined `floor`
      reference cmake's distro toolchain emits and Bazel's inlines as a
      builtin — is allowlisted. The cmake side needs zlib dev headers on
      the host for `find_package(ZLIB)`.

  Remaining work:
    - VTK / LLVM gates — need the project's specific configure flags +
      tooling and may need larger allowlists (the std::/libm-builtin
      classifier rules + the configure_file / cmake-P / imports-manifest /
      --bazel-external harness machinery the zlib…libpng fixtures built up
      should carry most of the way). **LLVM bazel-build lift progressing
      (manual):** the converter renders the LLVM monorepo in the **faithful
      end-state shape — multi-config + split-packages** (376 per-directory
      BUILDs + a `//config` package with `debug`/`release`
      `config_setting`s), and real libraries compile + archive under a
      staged bzlmod workspace **under BOTH configs** — `bazel build
      --//config:build_type={debug,release}` green on
      `//llvm/lib/Demangle:LLVMDemangle` (leaf, 123 syms),
      `//llvm/lib/Support:LLVMSupport` (foundational, ~165 compile actions,
      2328 syms), `//llvm/lib/Bitstream/Reader:LLVMBitstreamReader`, and
      `//llvm/lib/Remarks:LLVMRemarks` (notable because it sidesteps the
      broken `tools/remarks-shlib` package — proof the split isolates
      per-package breakage). The multi-config fold produced real per-config
      `select()` deltas (`-g` for debug, `-DNDEBUG -O3` for release) and
      composed with split-packages with **zero new converter code** — the
      existing fold + the split fixes below just work together. Gaps
      overcome to get the sources compiling: (1) umbrella src/hdr/include
      re-anchoring under the workspace-root promotion; (2) split-packages
      relocating `write_file`/`cmake_configure_file` outputs into their
      owning package (not just genrules); (3) `.def`/`.inc` added to the
      header-discovery extension set (LLVM's x-macro / textual-include
      idiom — `ItaniumNodes.def`, `regengine.inc`). Split mode is what
      makes per-leaf builds tractable: one malformed rule is a per-package
      loading error, not a whole-monorepo block. (On harness shape:
      **survey runs this faithful multi-config + split shape; fidelity
      deliberately runs single-config + single-BUILD** as a sharper symbol
      oracle — they're complementary, see
      `docs/fidelity-deltas.md` "Fidelity vs. survey".)

      Next frontier — the **tablegen generated-header tier**
      (`LLVMTargetParser` up): targets that `#include` a tablegen-generated
      `.inc` (e.g. `RISCVTargetParserDef.inc`). **The tablegen tool itself
      builds green** — `bazel build //llvm/utils/TableGen:llvm-min-tblgen`
      is 197 actions across its whole dep chain (Support + TableGen +
      utils) and produces the binary, so the "build the generator" half is
      done. **The genrules that run it are now hermetic too** — the
      standalone-custom-command path was emitting a *verbatim cmake command*
      broken four ways under umbrella+split; all four are fixed and the
      RISCV genrule now *runs tblgen* under `bazel build
      //include:custom_command_…_inc --//config:build_type=release`:
        - **input umbrella-anchoring** — `normalizeInput` /
          `rewriteGenruleCmd` take an `umbrellaPrefix`; source-tree srcs and
          cmd `-I`/input paths get the `llvm/` prefix
          (`//llvm/lib/Target:RISCV/RISCV.td`, `-I llvm/include`). Applied
          *during* the prefix strip so `<cmakeSrc>/include` (→
          `llvm/include`) stays distinct from `<buildDir>/include`.
        - **cross-package tool label** — split.go rewrites genrule `tools`
          (`:llvm-min-tblgen` → `//llvm/utils/TableGen:llvm-min-tblgen`) and
          the matching `$(location :…)` cmd refs, mirroring the deps rewrite.
        - **leftover prebuilt src** — `dropLiftedToolSrcs` removes the
          `<cfg>/bin/<tool>` artifact the tool-from-target lift hoisted into
          `tools`.
        - **output → `$(RULEDIR)`** — `anchorGenruleOutputsToRuledir`
          rewrites `-o include/…/X.inc` → `-o $(RULEDIR)/…/X.inc`; split.go
          re-relativizes the `$(RULEDIR)`-relative path when the genrule
          moves into its output's package.
      **The tablegen genrules now produce their headers** — `bazel build
      //include:custom_command_…_RISCVTargetParserDef_inc` is green (RISCV,
      ARM, AArch64 TargetParserDef.inc all build); the four clean libs are
      unregressed. The last piece was:
        - **(d) `.td` transitive-include closure** — DONE, as the precise
          per-genrule closure (replacing the first cut's `glob()` filegroups).
          tblgen failed `could not find include file 'llvm/Target/Target.td'`
          because the transitive `.td` includes aren't static ninja inputs.
          The faithful set is **cmake's per-output DEPFILE** — under Ninja
          (what the converter configures) `TableGen.cmake` tracks deps via the
          `.inc.d` depfile and sets the glob vars *empty*; `file(GLOB)` is only
          the **non-Ninja fallback** (the macro's own comment: "Use depfile
          instead of globbing … for Ninja"), so it never runs and a `glob()`
          over-declares (GenVT pulls in all 45 `.td` when it needs 1). We
          replicate the depfile **statically**: `recordCodegenIncludeClosure`
          (lower) follows `include "..."` directives from the genrule's primary
          input, resolving each against its own `-I` roots, and appends the
          reachable source files to srcs; split's existing cross-package src
          handling relabels each to its owning package and raises the
          `exports_files()` need. Result is minimal + transitive — GenVT.inc →
          `[ValueTypes.td]`, IntrinsicEnums.inc → the 25-`.td` Intrinsics
          closure. Scoped to genrules whose primary input sits inside one of
          their own `-I` roots (the include-resolving-codegen signal); an
          include that doesn't resolve on the source FS (a generated `.td`)
          terminates that branch.
        - **generic `file(GLOB)` threading** — the sibling capability for
          *any* globbing genrule (not just tablegen). `ExtractFileGlobs`
          (shadow) recovers each `file(GLOB)`/`file(GLOB_RECURSE)` call from
          the `--trace-expand` stream; `threadFileGlobs` (lower) computes the
          glob's match set on the source FS and, when a genrule depends on
          the *whole* set (subset guard — no false positives), folds those
          srcs into a build-time `glob()` filegroup split synthesizes in the
          glob's owning package: `GLOB` → `glob(["*.x"])`, `GLOB_RECURSE` →
          `glob(["**/*.x"])`, so it re-evaluates in project B. A no-op for
          tablegen (DEPFILE under Ninja, no `file(GLOB)` in the trace), it's
          ready for the first project that genuinely globs into a genrule.
      Tablegen **consumer** wiring (`LLVMTargetParser`-shaped) is
      **shipped (e)**. Under Bazel a consumer fails `fatal error:
      .../AArch64TargetParserDef.inc: No such file` because a generated
      `.inc` must be a *declared input*, not just an `-I` path. The signal
      is the codemodel's UTILITY (tablegen / `add_custom_target`)
      dependencies — `LLVMTargetParser`'s `dependencies` name
      `ARMTargetParserTableGen`, `AArch64TargetParserTableGen`,
      `RISCVTargetParserTableGen`. lower walks each such dep's ninja phony
      to the recovered genrule outputs it wraps (`collectCodegenHeaders`:
      bounded, seeded from every output whose final path component matches
      the target's unique name so sub-dir phonies like `gen/gen_inc`
      resolve, stopping at sibling-target boundaries) and records them on
      `pkg.CodegenHeaderConsumers`. The `--split-packages` transform then
      synthesizes one `generated_headers` `cc_library` per producing
      package (`textual_hdrs` = its `.inc`s, `includes=["."]` for the
      genfiles include root, whole-rule `# keep`) and splices a dep on it
      into each consumer with a per-item `# keep` (gazelle can't resolve a
      generated `.inc` to a target). Per-consumer, so the clean libs don't
      transitively force all of tablegen. Proven green end-to-end: a
      `cc_library` `#include`-ing a generated `.inc`, split-converted,
      builds under Bazel via the wrapper — fully automatic (no hand-edits):
      a recovered genrule's `cmake -E make_directory` of an output's subdir
      now anchors to `$(RULEDIR)/<subdir>` in lockstep with the output write
      (`anchorGenruleOutputsToRuledir` covers each output's multi-component
      parent dirs), so the genrule mkdir's the `.inc`'s parent where it
      writes it. buildifier `-mode=diff` stays a no-op.
      Net: tool builds, genrules run and emit headers, and consumers that
      `#include` those headers now build green. Still open: the
      source-tree-input == build-tree-output genrule aliasing
      (`Remarks.exports` in-place rewrite) and the `pkg_files` install-glob
      re-anchoring.
    - Promote each CI `fidelity` gate from `continue-on-error: true`
      to blocking after three consecutive green merges (the wiring +
      soft launch shipped — see the entry head).

  Acceptance: a converter regression that drops a symbol from the
  output artifact (e.g. accidentally skipping a source file in
  some edge case — see post-#258/#253 interaction caught by hand
  in PR #261) fails CI with a precise per-symbol diagnostic
  instead of being caught only when a downstream consumer breaks.

- **kind:meson Phase B multi-platform production promotion.** The
  per-platform fold for round-2 trace-driven kinds is **done** — both
  sides fan out (project-A converter genrules + fold-element; project-B
  N per-platform install genrules each publishing their platform's
  trace), and the sibling-kind contract is uniformly green across
  kind:make / autotools / cmake-fallback / meson-fallback (render gates
  `scripts/meta-{meson,cmake,autotools}-round2*-multiplatform.sh`). The
  one thing left is *production* promotion of multi-platform meson,
  gated on a real FDSDK consumer at scale (today's gate uses the
  meson-greet smoke fixture). Externally gated — promote once a real
  consumer surfaces the need; there's no converter/harness work
  outstanding.
- **Trace-side narrowing-audit coverage.** The narrowing-audit
  gate is now blocking for the cmake oracle, but the
  trace-side oracle (the build-tracer + trace.log path for round-2
  trace-driven kinds) still needs a CI fixture: `--trace-source-root`
  is wired but no e2e job exercises it yet, so the gate today only
  covers the cmake oracle. Add a build-tracer-on-CI fixture so the
  trace-driven sibling gate can run too.
- **Repo-rule install for kind:cmake round-2 fallback.**
  Phase B's round-2 fallback (per
  `docs/design/rendezvous.md`)
  transports the install tree as `install_tree.tar` between
  project B and project A's `BUILD.bazel.out` AND extracts a
  subset of its contents via a per-element `_install_tree_extract`
  genrule, costing CAS roughly tar_bytes + Σ(per-target
  artifact bytes the cc_import / sh_binary stubs reference).
  Storage duplication adds up across a fleet. Ruled out: a
  Bazel repository rule whose `repository_ctx.execute()` either
  runs cmake at loading time directly OR untars
  `install_tree.tar` into a per-element repo, exposing
  per-target labels without the extract genrule + CAS
  duplication. Precedent: `rules/traces.bzl`'s `_trace_repo`
  (loading-time AC lookup) — but that one only does AC
  `GetActionResult`, not a full build. **Rejected because it
  can't remote-execute:** loading-time work blocks Bazel
  startup; repo rules don't run on RBE (executor-pool
  advantages forfeited); hermeticity weaker (relies on
  host-side cmake/ninja). The live candidate is the
  TreeArtifact install root, which kept the dedup win without
  the RBE disqualifier and **shipped**. The repo-rule
  alternative stays rejected; this bullet is retained only as the
  record of why.
## Later (research / open questions)


- **Source-side AC narrowing for autotools.** Bazel's hermetic-action
  model says inputs in → outputs out; you can't have a byte be
  available to the action at exec time without it being in the AC
  key. So narrowing autotools is unavoidably a side-channel story.
  `docs/architecture.md` lays out three options (FUSE, host-fs
  source cache via `--repo_env`, write-a-time registry) and rules
  out two; the third is the path forward but the value-vs-complexity
  trade-off is open.
- **kind coverage — real semantics for the FDSDK-glue
  placeholders.** All four FDSDK-specific glue kinds
  (`collect_initial_scripts`, `collect_integration`,
  `check_forbidden`, `flatpak_repo`) now have v1 stub
  handlers (alongside the pre-existing `collect_manifest`
  stub) so FDSDK render reaches completion. Real plugin
  semantics deferred until an FDSDK fixture forces a
  bazel-build-time correctness need; per-kind cost-to-port
  is documented in `docs/fdsdk-coverage.md` (small
  for the install-tree-walk kinds; `flatpak_repo` is bigger
  — needs ostree at build time). `kind:flatpak_image` /
  `kind:snap_image` retain their structural treatment
  (filegroup composition over deps' install trees) which is
  the right shape regardless of upstream-plugin behaviour
  changes.
- **Dev-loop guidance for routing local Bazel at the executor.**
  Two slices landed: per-gate cmake prereq honesty +
  inline cmake-availability check in the kind:cmake render
  gates. Today only ~5 targets still pin cmake on the dev's
  box: the converter's own `-tags=e2e` Go tests
  (which call `cmakerun.Configure` directly), `e2e-audit-narrowing`
  + `e2e-meta-cmake-round2-fallback-storage-cost` (scripts that
  invoke `convert-element-cmake` directly before any bazel
  involvement), and `record-fixtures`. Every other meta-* gate
  runs render-only with just Go, including kind:cmake gates.

  Closing the gap for the bazel-build half — "dev with bazel
  installed but no cmake can still exercise the full e2e loop"
  — means routing the dev's local `bazel build` invocations at
  the buildbarn executor (the worker image already has cmake;
  see `deploy/buildbarn/runner/Dockerfile`). The
  `e2e-meta-buildbarn-re` gate already exercises exactly this
  shape; the missing piece is a documented `--config=remote`
  knob and CONTRIBUTING.md guidance so devs can opt in. Then
  the only hard local dep for the kind:cmake gates' build half
  becomes "bazel that can reach the executor", and cmake
  drops to optional even for the build half. The harder
  follow-on (wrapping `cmakerun.Configure` itself as a Bazel
  action so the converter doesn't need cmake at any layer) is
  a real architectural change; the open question is how the
  converter's in-process File API consumer reads the reply
  when the cmake-configure step runs on a remote node.

- **Two-species split: remotable, cacheable configure + convert.**
  The deeper architecture the item above leads to. `cmake configure`
  must run on the *target* platform P (its `try_compile`/`try_run`/
  `check_*`/`find_package` resolve against P), possibly a subset of
  platforms per element; the converter is a Linux/Go binary not built
  for every P. So split the welded `convert-element-cmake` (which execs
  cmake in-process via `cmakerun.Configure`) into two independently
  remotable+cacheable action species: `configure(element, P)` — native
  cmake on a P worker, **no Go**, emits a File API reply bundle — and
  `convert(element)` — Linux/Go, **no cmake**, consumes the per-platform
  bundles via the existing `--reply-dir` seam and folds them. The File
  API query is language-agnostic (five touch-files), so a configure
  action is just `cmake <argv>` with hooks staged as inputs; argv/hook
  construction stays a shared `cmakerun` function the planner (`write-a`)
  calls. The genex literal two-pass becomes a static
  `configure → analyze → litprobe → convert` graph whose `litprobe(P)`
  command branches on a 0-byte probe (no cmake when empty; the 0-byte
  probe on pass 1 makes the input roots byte-identical so the empty case
  is a warm no-op — only the scheduling round-trip remains, removable by
  making `litprobe` opt-in with `-unresolved` tail-baking). **Hard
  invariant: the standalone path keeps working** — `convert-element-cmake
  --source-root` stays a complete, infrastructure-free, full-fidelity
  composition of the same steps; the serialized reply bundle is a complete
  interface so `--reply-dir` is byte-identical to in-process (new gate
  guards it). Native-P configure also closes the `try_run` cross-compile
  fidelity gap (`docs/research/cmake_analysis.md` §7). Full design +
  cost/narrowing model + standalone-preservation disciplines in
  `docs/design/remotable-configure-convert.md` (delete that doc once this
  lands).

---

For how the codebase works *today* (not just what's planned here), see
`docs/architecture.md` (architecture + interop contract + build-time
flow, all in one place) and `docs/codebase-map.md` (the developer-facing
repo tour). `ROADMAP.md` tracks only what's *left*; git history is the
record of what shipped.


### protobuf finish — static-lib find_package header wiring (294/321)

protobuf converts 0-rej and builds **294/321** with the abseil manifest +
link_paths (78 @abseil deps on the LINKING targets: libprotobuf, libprotoc,
protoc — their `.a` link fragments path-attribute to @abseil-cpp). The remaining
failures are **static-archive libraries** (libprotobuf-lite) that `#include`
absl headers (absl_check.h, cord.h, …) but get NO absl wiring:
- a static lib has no link step → no `.a` link fragments → nothing for
  link_paths to attribute;
- find_package IMPORTED targets aren't in the codemodel's `t.Dependencies`
  (they're external), so the cmake_target name-match (lower.go ~2925) never sees
  them;
- abseil's host include dir (`/tmp/absl-install/include`) is ELIDED as a
  host-prefix include, so the headers aren't even on `-I`.
So libprotobuf-lite compiles with zero absl — `No such file`.

The hermetic fix: wire find_package imported deps onto STATIC libs that include
their headers — source the deps from the TRACE (`target_link_libraries(lib
${protobuf_ABSL_USED_TARGETS})` expands in trace-expand) since the codemodel
omits them for archives, and route through the imports manifest → @abseil-cpp
(header carriers). A non-hermetic shortcut (NOT "legitimate"): don't elide the
abseil host include so headers resolve from /tmp/absl-install — rejected, that's
a host dep on a hand-installed abseil, not reproducible.
