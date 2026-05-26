# Generator-parity uplift for the cmake converter

The cmake converter reads cmake's File API codemodel-v2 plus
`--trace-expand` JSON and emits BUILD files. That recovers ~67%
of FDSDK with fidelity gaps in
[`docs/cmake-conversion-deltas.md`](../cmake-conversion-deltas.md)
and the `Later`-section items in [`ROADMAP.md`](../../ROADMAP.md)
(genex coverage, custom-command shape, install(EXPORT) bundles).
A hypothetical `cmake -G Bazel` generator running inside cmake's
generation pass would resolve most of those gaps by virtue of
being cmake.

The uplift takes the same fidelity by extending hooks already in
tree so that **cmake itself is the oracle** for the residue the
File API doesn't expose. The seven phases below — each landing
as its own PR stack — move the converter toward generator-class
output without leaving the codemodel-driven path.

**Bazel-idiom shaping** is a first-class goal at every phase:

- Sanitizer / debug-info / LTO config variants lower to
  `--features` on the cc_toolchain, not raw per-config `select()`s.
- `install(EXPORT)` bundles become `cc_import` + `pkg_files`, not
  tar-and-extract.
- Custom commands become `genrule`s with depfile-derived `srcs`,
  not opaque copies of cmake's `RERUN_CMAKE` driver.
- `buildifier --mode=fix` + `gazelle fix` stay no-ops over the
  output (the Phase 8 contract in
  [`build-output-conventions.md`](build-output-conventions.md)).

## Phase 1 — Read what we already loaded

Three slices, all decoder-side. No new cmake hooks.

- **`backtraceGraph` consumption** (✓ landed end-to-end).
  Two complementary uses of the codemodel's per-target
  BacktraceGraph:

  - **Provenance annotation** — `ir.Target.Provenance{File, Line,
    Command}` populated from `Target.Backtrace`; emit-side
    `# Source: <file>:<line> (<command>)` comment gated by
    `--emit-provenance`. Operator-facing "why does this rule
    exist?" navigation aid; no source-side parsing needed.

  - **Keyword recovery** — `backtraceRecoverLinkScope` walks
    every `TargetDependency.Backtrace` to the outermost
    user-source frame, reads the cmake call via the new
    `converter/internal/cmakeargv` lexer, and recovers the
    PUBLIC / PRIVATE / INTERFACE keyword. **Backtrace runs
    first** (it's strictly more authoritative); the trace-based
    recovery fills only the gaps backtrace can't address.

    Backtrace is more authoritative because:

    1. **Always available** — codemodel BacktraceGraph is part
       of fileapi; no `--trace-expand` dependency.
    2. **Recovers user intent through macros** — walks to the
       outermost user-source frame. A `my_link_helper(foo PUBLIC
       zlib)` macro that internally calls
       `target_link_libraries(foo INTERFACE zlib)` recovers
       PUBLIC (user's intent), not INTERFACE (macro author's
       choice). Trace alone would record only the inner call.
    3. **Genex-wrapped deps** unwrap via `stripGenexWrapper`
       (`$<BUILD_INTERFACE:zlib>` → `zlib`) so genex-gated deps
       still match.

    The one case where trace is needed:
    `target_link_libraries(foo PUBLIC ${SOME_DEP_VAR})` — the
    source argv carries `${SOME_DEP_VAR}` literally, so
    cmakeargv's literal match against the codemodel's dep name
    misses. Trace's post-expansion argv has the resolved
    literal; trace fills the gap (the trace block's existing
    first-write-wins guard means backtrace data is preserved
    elsewhere).

    cmake's BacktraceGraph requires cmake 3.21+ for complete
    per-property backtraces; older cmakes leave incomplete data
    and trace stays the primary recovery there by virtue of
    backtrace returning no entries.

- **Directory installers → `filegroup()`** (✓ landed for both
  install(FILES) and install(DIRECTORY)).
  `lower/directory_installers.go` walks
  `DirectoryInstaller.Type` in {`"file"`, `"directory"`} and
  emits one `filegroup()` per (type, destination):
  `install_files__<sanitized-dest>` for FILES,
  `install_directory__<sanitized-dest>` for DIRECTORY.
  `decodeInstallerPath` handles the type-specific path schemas
  (plain string vs `{"from": ..., "to": ...}` object). Uses
  Bazel-native `KindFilegroup`; the pkg_files variant for
  per-file destination renames slots in alongside as a future
  kind when richer attribute support surfaces.

- **Per-source `COMPILE_DEFINITIONS` via CompileGroup-split**
  (✓ landed end-to-end). cmake's codemodel already partitions
  sources by CompileGroupIndex when
  `set_source_files_properties(... PROPERTIES COMPILE_DEFINITIONS
  ...)` (or `target_sources(PRIVATE FILE_SET ...)`) gives them
  differing compile contexts. The new `shouldSplitCompileGroups`
  gate fires when two CGs differ in `Defines` /
  `CompileCommandFragments` within the same language, not just
  across languages. `splitMultiLanguage`'s emit loop
  disambiguates names (`<target>_C_0`, `<target>_C_1` for
  multi-CG-per-language; single-CG-per-language keeps the
  legacy `<target>_C` shape for byte-stable goldens).

  `shadow.ExtractSourceFileProperties` decodes the trace-level
  `set_source_files_properties` calls and remains useful for
  properties the codemodel doesn't surface (`HEADER_FILE_ONLY`,
  `LANGUAGE` override) — consumer wiring for those properties
  lands separately as each gets its own Bazel-side lowering
  recipe.

## Phase 2 — Request `configureLog-v1`

✓ Landed end-to-end. cmake 3.26+ object kind that records
`try_compile` / `try_run` / `find_package` / `message` events
during configure. Two-level: sidecar JSON in the reply dir
points at `CMakeConfigureLog.yaml` outside it.

- `cmakerun.Configure` stages the query alongside the four
  existing kinds — cmake < 3.26 ignores silently.
- `fileapi.SupportedObjectMajors` knows the schema.
- `Reply.ConfigureLog` carries the sidecar (`Path`,
  `EventKindNames`).
- `fileapi.LoadConfigureLogYAML(path)` reads the YAML; called by
  the convert binary, threaded into `lower.Options.ConfigureLog`.

The YAML decoder covers four event kinds: `try_compile-v1`,
`try_run-v1`, `find_package-v1`, `message-v1`. Each event carries
the cmake backtrace, the human-readable check chain, and
kind-specific payload (build/run results, found-package metadata,
message body).

Phase 4 reads these events to retire probe-bucket
`execute_process` refusals: when a refused `execute_process(...
OUTPUT_VARIABLE X)` resolves to the same answer as a recorded
`try_compile` / `check_*`, the lifter emits a `select()` arm with
the resolved value instead of Tier-1-failing.

## Phase 3 — Genex-probe TOP_LEVEL_INCLUDES extension

✓ Landed end-to-end. New `probe-genex.cmake` hook layered onto
`CMAKE_PROJECT_TOP_LEVEL_INCLUDES` (cmake 3.24+). On
`--probe-genex`, the hook DEFERs to end-of-top-level-directory,
walks BUILDSYSTEM_TARGETS recursively, and emits per-target
`file(GENERATE)` declarations capturing common genex shapes:

- `$<TARGET_PROPERTY:t,TYPE>` → `type.txt`
- `$<TARGET_FILE:t>` / FILE_DIR / FILE_NAME → `file{,_dir,_name}.txt`
  (skipped for INTERFACE_LIBRARY)
- `$<TARGET_OBJECTS:t>` → `objects.txt` (OBJECT_LIBRARY only)
- `$<TARGET_PROPERTY:t,INTERFACE_<P>>` for the five common
  `<P>` (INCLUDE_DIRECTORIES, COMPILE_DEFINITIONS,
  COMPILE_OPTIONS, LINK_LIBRARIES, LINK_OPTIONS)
  → `interface_<P>.txt`

cmake's own generator-phase evaluator resolves each `$<…>` at
generation time, so the post-walk INTERFACE_* aggregates and the
on-disk TARGET_FILE_* paths land on disk byte-equal to what
cmake would have shipped to a downstream build system.

End-to-end flow:

1. `convert-element-cmake --probe-genex` flips
   `Options.ProbeGenex`.
2. `cmakerun.Configure` stages `probe-genex.cmake` and layers it
   onto `CMAKE_PROJECT_TOP_LEVEL_INCLUDES` (after cmp0026-shim
   and dump-vars in the documented order).
3. cmake's generation pass writes per-target files under
   `<buildDir>/cmake-to-bazel.genex/`.
4. The convert binary calls `cmakerun.ReadGenexProbe(buildDir)`;
   probes get threaded as `lower.Options.GenexProbes`.
5. `buildGenexTargets` folds each probe into the matching
   codemodel-derived `genexeval.TargetInfo` entry's
   `Objects` + `InterfaceXxx` fields.
6. `genexeval`'s `evalTargetObjects` and `evalTargetProperty`
   resolve from the new fields; `UnsupportedError` paths for the
   six probed shapes retire.

Probes for targets not in the codemodel are dropped silently —
the codemodel is ground truth for "what targets exist". Probes
empty (hook didn't stage, cmake < 3.24, or operator didn't pass
`--probe-genex`) leave the new `TargetInfo` fields zero-valued
and the evaluator falls back as before — back-compat preserved.

## Phase 4 — build.ninja custom-command walk

Partial. Walker helpers landed
(`ninja.CustomCommandEdges`, `ninja.DepfileFor`,
`ninja.DescriptionFor`); probe / stamp execute_process rescue
via dump-vars ✓ landed; standalone genrule emission from
`CustomCommandEdges` remains queued.

Two halves:

- **Custom command edges → genrules** (✓ landed, opt-in).
  `lower/standalone_genrules.go` walks every `CUSTOM_COMMAND`
  edge in build.ninja and emits a genrule for each whose outputs
  aren't already covered by an existing recoverGenrule emission.
  Dedup is via `existing[].GenruleOuts` lookup — any single-
  output overlap skips the standalone emit. Naming:
  `custom_command_<sanitized-first-output>` with `_N` suffix on
  collision. Tag: `cmake-codegen-standalone-custom-command` so
  the Phase 7 audit can inventory the new emissions. CLI surface:
  `--emit-standalone-custom-commands` (off by default to keep
  existing goldens byte-stable; opt-in covers add_custom_target
  bookkeeping rules and version-stamp edges).

- **Probe / stamp execute_process rescue** (✓ landed).
  `recoverExecuteProcess`'s default arm now skips refusal for
  BucketProbe / BucketStamp calls whose `OUTPUT_VARIABLE` is
  already in `cmakeVars` (captured by the dump-vars hook at
  end-of-configure). Downstream `configure_file` and
  `file(GENERATE)` lifts consume the value through `cmakeVars`,
  so no Bazel-side emission for the probe call itself is needed
  — the rescue collapses the probe's effect into the existing
  variable-substitution path. Back-compat preserved: when
  `--lift-configure-file` is off the dump hook doesn't fire,
  `cmakeVars` is empty, and the refusal still surfaces.

  The configureLog-driven rescue (cross-referencing
  `try_compile-v1` / `find_package-v1` events with refused
  probes) is a strict extension: it covers probes whose
  OUTPUT_VARIABLE landed in cmake's cache via a Check / probe
  module rather than directly in the project's variables.
  Lands once a fixture forces the distinction.

## Phase 5 — Ninja Multi-Config + sanitizer-as-feature

Plumbing + cross-config Partition landed:
- `cmakerun.Options.BuildTypes []string` switches the generator
  to `Ninja Multi-Config` with the entries joined into
  `-DCMAKE_CONFIGURATION_TYPES=<a;b;c>`. CLI surface via
  `convert-element-cmake --build-types=A,B,C`.
- `fileapi.Reply.TargetsByConfig map[id]map[config]Target`
  carries per-config target data; `Reply.Targets` retains the
  primary config (first declared in `Configurations`) so existing
  single-config consumers keep working.
- `converter/internal/configfold` projects `TargetsByConfig` into
  per-target `TargetFold` partitions over `empfold.Partition`:
  Defines / Includes / LinkFragments / CompileFragments each
  expose `{Baseline, Deltas[cell]}` for the downstream emit.
- `configfold.SanitizerFeature(config)` maps cmake config names
  matching known sanitizer / instrumentation patterns (ASan /
  TSan / MSan / UBSan / LSan / Coverage / LTO + suffix variants)
  onto the cc_toolchain feature name a Bazel build would use.

Lower-side consumer landed: `lower/multiconfig.go` calls
`configfold.Project` when `Reply.TargetsByConfig` is populated
and routes per-config Defines / CompileFragments / LinkFragments /
Includes deltas into the target's `PerPlatform` map keyed by
`//config:<name>` config_setting labels. The emitter renders the
same select() shape it already produces for per-platform deltas
— no emit-side changes needed; per-platform and per-config arms
merge cleanly when both axes populate.

Sanitizer-shaped configs are filtered out before the fold runs;
the `--features` routing for them is operator-controlled cc_toolchain
wiring rather than per-rule emit. The convention + an
auto-generation slice are documented in
[`sanitizer-as-feature.md`](sanitizer-as-feature.md) with a
runnable starting point in
[`examples/sanitizer-features/`](../../examples/sanitizer-features/):

  - cmake-side `CMAKE_<LANG>_FLAGS_<NAME>` defines the per-config
    flag set (unchanged from the typical cmake idiom).
  - **Auto-generation**: `convert-element-cmake
    --out-sanitizer-features <path>` extracts each
    sanitizer-shaped config's cmake cache entries and renders
    a matching cc_toolchain `features.bzl` (one `feature {}` per
    recognized sanitizer, per-language compile + merged link
    flag_sets, `enabled = False` opt-in default).
  - Operator's cc_toolchain threads `SANITIZER_FEATURES` from
    the generated file (or the hand-curated reference) into
    `cc_common.create_cc_toolchain_config_info(features = …)`.
  - Operators opt in via `bazel build --features=asan` /
    per-package `package(features = ["asan"])` / per-rule
    `cc_library(features = ["asan"])`.

The Phase 7 audit's `sanitizer-select-not-feature` finding catches
any hand-rolled per-rule sanitizer select that bypassed the
convention.

Fold semantics (mirrors the existing per-platform fold):

- Per-config compile/link fragments fold via `select()` over
  `//config:<name>` config_settings.
- Phantom-target shape collapses cleanly: a target present in
  Debug but absent in MinSizeRel becomes
  `select({"//config:debug": [...], "//conditions:default": []})`.
- Refuse projects where the trace records
  `if(CMAKE_BUILD_TYPE STREQUAL "…")` branches affecting
  target-graph shape (silently no-op under multi-config; would
  produce wrong output).

**Bazel-idiom shaping** kicks in for config names matching
sanitizer / instrumentation patterns:

- `ASan`, `TSan`, `MSan`, `UBSan` → `--features=asan` /
  `--features=tsan` / etc.
- `RelWithDebInfoLTO`, `MinSizeRelLTO` → `--features=lto`
- The flag deltas (`-fsanitize=address`, `-flto`, …) move from
  per-config selects into the operator's cc_toolchain
  feature definition, which is where Bazel users expect them.
- The cmake-config names are mapped via a documented dictionary
  in the converter; unknown names stay on raw per-config
  selects.

## Phase 6 — install(EXPORT) declarative IR projection

Classifier + IR projection + codemodel-only EmitInputs landed:
- `converter/internal/exportshape.Classify` decides declarative
  vs imperative per install(EXPORT) installer; Verdict carries
  Reasons[] for the audit gate.
- `exportshape.EmitDeclarative` projects the declarative bundle
  into IR: one `cc_import` per STATIC/SHARED/MODULE target, one
  `cc_library` (header-only) per INTERFACE_LIBRARY, one
  filegroup per target's public headers, one bundle-wide
  `cmake_config_bundle` filegroup for the generated
  `<Pkg>{Config,ConfigVersion,Targets}.cmake` files.
- `exportshape.BuildInputs` synthesizes `EmitInputs` from
  codemodel-only sources — `Target.NameOnDisk`,
  `Target.Install.Destinations`, `Target.FileSets` HEADERS,
  plus the installer's own `Destination` for the bundle
  script path. No `cmake --install` runs at convert time;
  the artifact paths come from what cmake records cmake
  WILL produce when the producer's converted BUILD rule
  builds under Bazel.
- `lower/directory_installers.go::lowerExportInstallers` wires
  the verdict + inputs + emit into the per-package IR.
  Declarative export-derived cc_import targets carry an
  `_import` name suffix to avoid colliding with the
  producer's own cc_library (`install(TARGETS foo EXPORT ...)`
  declares the same `foo` target that the project authored;
  both shapes coexist in the producer's BUILD package).
  They also carry the `cmake-codegen-install-export-import`
  tag so `internal/emit/cmakecfg`'s bundle-synth pass can
  de-duplicate when emitting `add_library(<Pkg>::foo IMPORTED)`.

`internal/manifest.Export` gained two `omitempty` fields —
`cmake_config_bundle_label` + `cmake_import_labels` — so the
orchestrator's manifest synth (queued under `resolved-lift`)
can point cross-element `find_package(<Pkg>)` consumers at the
producer's synthesized bundle filegroup + per-target cc_import
facades without re-deriving them from the codemodel.

**Architectural constraint: the convert action does not build.**
Earlier work-in-progress on this phase wired
`cmakerun.BuildAndInstall` + `cmakerun.WalkInstallPrefix` to run
`cmake --build` + `cmake --install` at convert time, then walked
the materialized install prefix to populate `EmitInputs.InstallFiles`
+ `CMakeConfigBundleFiles` + `PublicHeaders`. That wiring was
backed out: convert is a metadata extraction step (configure +
codemodel + trace + dump-vars + configureLog read; emit a BUILD).
Compiling, linking, and install-tree materialization stay in
Bazel's action graph under its own caching and sandboxing —
running them inside the convert action would change the
project's whole runtime model (sandboxable, cheap to re-run,
parallelizable across the orchestrator's fleet).

Verdict shape (`exportshape.Classify`):

- **Declarative** — the install(EXPORT) call shape matches the
  CMakePackageConfigHelpers-style canonical layout:
  - Destination matches `lib/cmake/`, `lib64/cmake/`,
    `share/cmake/`, or `share/` prefix.
  - All exported targets have TYPE in {STATIC_LIBRARY,
    SHARED_LIBRARY, INTERFACE_LIBRARY, MODULE_LIBRARY} —
    EXECUTABLE is rejected because cc_import doesn't model it.
  - ExportName is non-empty; ExportTargets is non-empty.
  - `!IsExcludeFromAll`, `!IsOptional` — the bundle must be
    part of the default install.

- **Imperative** — anything else, with `Verdict.Reasons` listing
  the failed preconditions. Stays on the existing round-2
  `_install_tree_extract` fallback.

## Phase 7 — Bazel-idiom shaping audit

✓ Audit framework + first checks landed.
`converter/internal/bazelidiom.Audit` parses emitted BUILD bytes
and surfaces findings for known anti-patterns:

- `empty-cc-library` — cc_library with no srcs AND no hdrs
  (placeholder; typically signals upstream lowerer refused).
- `empty-cc-import` — cc_import with neither static nor shared
  library (unusable; consumers can't link).
- `empty-srcs` — cc_binary / cc_test with no srcs (Bazel
  rejects at build time).
- `sanitizer-select-not-feature` — copts / linkopts / defines
  is a select() on sanitizer-shaped config_setting labels
  (//config:asan, //config:tsan_enabled, …); the Bazel-idiomatic
  form is a cc_toolchain feature.

Wiring: `convert-element-cmake --audit-bazel-idiom` runs the pass
after emission and prints findings to stderr;
`--audit-bazel-idiom-report <path>` writes them as JSON.
Observational, not prescriptive — findings inform upstream
lowerer prioritization rather than rewriting emit.

Queued for future audit extensions: header-fileset-derived
strip_include_prefix checks, `# keep` placement on
gazelle-vulnerable attrs, IMPORTED targets emitted as
`cc_library(srcs=[…lib…])` instead of `cc_import`.

## Acceptance criteria

The uplift is "complete" when:

- FDSDK kind:cmake coverage delta drops to near-zero; the
  structural residue is `try_compile`-keyed target-graph shape
  per [`docs/research/cmake_analysis.md`](../research/cmake_analysis.md)
  §7, which the round-2 fallback covers by construction.

- The `cmake-codegen-*-genex*` audit tag family collapses to a
  single `-resolved` tag.

- `internal/genexeval`'s `UnsupportedError` surface goes away
  for the shapes the probe-genex hook covers (TARGET_OBJECTS,
  INTERFACE_* aggregates have already retired in Phase 3 —
  more retire as the hook's output gains coverage).

- [`cmake-conversion-deltas.md`](../cmake-conversion-deltas.md)
  "open deltas" closes the configurable items.

- Render-gate output for known sanitizer configs uses
  `--features` rather than raw per-config selects.

- The genex / TARGET_FILE / TARGET_OBJECTS / INTERFACE_*
  aggregation items currently under `Later` in
  [ROADMAP.md](../../ROADMAP.md) retire (most lapsed as Phase 3
  landed; the rest fall as the consumers in Phase 4/5/6 grow).
