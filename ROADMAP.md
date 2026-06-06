# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

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

- **Stage a genrule's invoked-script inputs.** A recovered/standalone genrule
  whose cmd runs an interpreter over a source-tree script (`perl scripts/foo`,
  `python tools/gen.py`, `sh scripts/x.sh`) needs that script — and the inputs
  it reads — in the rule's `srcs`, or Bazel's sandbox can't open it at action
  time (`Can't open perl script "…/cd2nroff": No such file`). curl surfaces
  this: its `docs/` manpage genrules (`cd2nroff`/`managen`/`mkhelp.pl` over
  ~300 `.md` files) and its perl `runtests.pl` **test** harness all fail this
  way, so the curl lens currently scopes docs+tests off (`curl.conf`).
  Implement: scan the genrule cmd for an interpreter+script invocation, resolve
  the script (and, where tractable, its read inputs — cf. the `--cmake-script-
  trace` read-path augmentation) against the source tree, and add them to
  `srcs`. Unblocks building docs/test surfaces faithfully instead of scoping
  them away. See docs/survey-corpus.md (curl row).

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

- **Agent-actionable prompts for no-mechanical-form constructs.**
  Some cmake constructs have a perfectly good *Bazel* form but no
  *mechanical translation* — the behavior lives in a script the
  converter can't faithfully rewrite, only an author can. The canonical
  case is `add_test(COMMAND cmake -P <runner> …)` integration tests
  (brotli's roundtrip/compatibility harness): the idiomatic target is an
  `sh_test` / `bazel_skylib` `diff_test` driving the built CLI, but
  reaching it means *re-authoring* the cmake-script harness, not
  translating an AST. Today the converter correctly **breadcrumbs** these
  to a warning (the #417 `add_test`-not-converted audit, the #412
  cmake-internal-drop audit) so the gap is visible rather than silent.
  Next: promote the breadcrumb from a human-readable warning to a
  **structured, agent-actionable prompt** a post-conversion AI pass can
  pick up. Shape: a `conversion-todos.json` sidecar alongside the
  survey's existing `rejections` / `bazel-idiom` / `coverage` reports,
  one entry per untranslatable construct carrying (a) a source anchor
  (`CMakeLists.txt:line` + the construct), (b) recovered *evidence*
  (the runner script path, the built exe target, the invocation args,
  the verification — e.g. "SHA512(input)==SHA512(roundtrip)"), (c) a
  *suggested Bazel shape*, and (d) a precise natural-language prompt.
  Design constraints: the converter stays **deterministic** (same
  prompts every run; the non-deterministic authoring is quarantined to
  the separate post-pass operating on the fixed worklist); detection
  **reuses** the existing breadcrumb logic (only the payload format is
  new); prompts are **grouped by the unit the breadcrumb already groups
  by** (brotli's 28 roundtrip tests share one runner → one
  "author a reusable macro" prompt, not 28 near-duplicates); a stable
  TODO marker (tag / comment ID) in the emitted BUILD makes pickup
  idempotent; and agent-authored output crosses the **same trust
  boundary** as mechanical output — it goes through the render gates,
  it is not trusted on faith. First producer: the `cmake -P` test
  breadcrumb; the same mechanism then fits any no-mechanical-form
  breadcrumb (`install(SCRIPT)` / `install(CODE)`, dropped command edges).
  Surfaced from the brotli test-form discussion.

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

---

For how the codebase works *today* (not just what's planned here), see
`docs/architecture.md` (architecture + interop contract + build-time
flow, all in one place) and `docs/codebase-map.md` (the developer-facing
repo tour). `ROADMAP.md` tracks only what's *left*; git history is the
record of what shipped.

