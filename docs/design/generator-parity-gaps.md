# Remaining gaps vs `cmake -G Bazel`

Companion to
[`generator-parity-uplift.md`](generator-parity-uplift.md).
The uplift's seven phases cover the structural shape:
configureLog, probe-genex, multi-config fold, install(EXPORT)
pre-resolution, BUILD-output audit, etc. This doc enumerates
what's STILL not covered relative to a hypothetical `cmake -G
Bazel` generator running inside cmake's `cmGlobalGenerator::
Generate`, along with the data we'd need to lift each gap.

Each entry follows the format:

> **<Feature>** — *<data source>*. *<lift difficulty>*. *<gap if not lifted>*.

Sorted by lift difficulty (easy first) so operators / contributors
can pick off the cheap wins without scanning everything.

## Easy: data we already load but don't consume

These are gaps where the codemodel already carries the data —
the lift is wiring it into IR + emit.

- ✓ **`CompileGroup.LanguageStandard`** — *codemodel-v2*. **Landed.**
  cmake records the resolved `CMAKE_<LANG>_STANDARD` value per
  CompileGroup (e.g. `"17"` for `cxx_std_17`). Lifted to
  `copts = ["-std=c++17"]` with an idempotency guard for
  projects where cmake's generator already inlined the flag.

- ✓ **`TargetArchive.LTO` / `TargetLink.LTO`** — *codemodel-v2*.
  **Landed.** Per-target `INTERPROCEDURAL_OPTIMIZATION` flag
  surfaces as `features = ["lto"]` via the new
  `ir.Target.Features` slot. Operator's cc_toolchain owns the
  actual `-flto` flag set (see `examples/sanitizer-features/`
  where lto is already in SANITIZER_FEATURES).

- ✓ **`CompileGroup.PrecompileHeaders`** — *codemodel-v2*.
  **Landed (tag).** Targets using `target_precompile_headers`
  get the `cmake-codegen-pch` tag so operators can grep for the
  gap and route via a cc_toolchain pch feature. Bazel cc_library
  has no native PCH attribute; the PCH headers themselves are
  already in the srcs/hdrs walk.

- ✓ **`CompileGroup.Frameworks`** — *codemodel-v2*. **Landed
  (macOS).** Per-framework search paths surface as `-F<path>`
  copts entries. Empty / no-op on non-Apple targets. Link-time
  `-framework Foo` directives already flow via
  Link.CommandFragments → linkopts.

- **`CompileGroup.Sysroot` / `Link.Sysroot`** — *codemodel-v2*.
  *Small slice (informational)*. Per-target cross-compile sysroot.
  The operator's cc_toolchain is the canonical home for sysroot;
  per-target overrides risk conflict. Lift as a Provenance
  comment rather than a copt to avoid the conflict; surface
  via `--emit-provenance`.

## Easy: trace data we already decode

- **`shadow.ExtractSourceFileProperties` properties** — *trace*.
  *Per-property slice each*. The decoder is wired; consumers
  for specific properties:

  - ✓ `HEADER_FILE_ONLY` → reclassify source from `srcs` to
    `hdrs` (file declared but not compiled). **Landed via
    `reclassifyHeaderOnlySources` post-pass.**
  - `LANGUAGE` override → bypass compile-group's reported
    language (rare; usually `set_source_files_properties(...
    PROPERTIES LANGUAGE CXX)` on a `.c` file).
  - `COMPILE_FLAGS` (old form) → augment per-source copts via
    the CompileGroup-split mechanism (the
    `shouldSplitCompileGroups` gate covers this when the
    different copts produce distinct CompileGroups in cmake's
    own partitioning).
  - `GENERATED` → already handled via `TargetSource.IsGenerated`
    in the codemodel; trace adds no new signal.
  - `OBJECT_DEPENDS` → declares manual header deps; could
    augment Bazel `cc_library.srcs` with the listed paths so
    incremental builds see them.

## Medium: data the File API exposes that we don't query

- ✓ **`configureLog-v1` event consumption** — *configureLog YAML*.
  **Probe-rescue half landed.** `configureLogVars` projects
  try_compile-v1 / try_run-v1 events into the rescue's
  var → value map alongside dump-vars' cmakeVars. Covers
  probes whose results landed in cmake's cache via Check_*
  modules rather than directly in user variables.

  Queued: `find_package-v1` event attribution as Provenance
  comments on cc_import targets so operators see WHERE the
  dep came from (system / vendored / custom-CMAKE_PREFIX_PATH).

- **`cache-v2` entries beyond sanitizer flags** — *cache-v2*.
  *Per-pattern slice*. We extract `CMAKE_<LANG>_FLAGS_<CONFIG>`
  for sanitizer-features auto-emit; other cache entries with
  Bazel relevance:

  - `BUILD_SHARED_LIBS` → cc_library `linkstatic` default flip.
  - `CMAKE_BUILD_RPATH` / `CMAKE_INSTALL_RPATH` → cc_binary
    `linkopts = ["-Wl,-rpath,..."]`.
  - `CMAKE_<LANG>_VISIBILITY_PRESET` →
    `copts = ["-fvisibility=hidden"]`.
  - `CMAKE_<LANG>_FLAGS_INIT` / `CMAKE_<LANG>_FLAGS` →
    project-wide flags; usually surface in CompileGroup
    fragments already.

- **`cmakeFiles-v1.Inputs[].IsCMake`** — *cmakeFiles-v1*. *Tiny*.
  Distinguishes cmake-bundled .cmake files from project-owned
  ones. Currently surfaces as opaque inputs; the audit gate
  could use the IsCMake flag to attribute reads correctly.

## Medium: trace patterns we could pattern-match

- ✓ **`add_dependencies(target dep)`** — *codemodel-v2 backtrace*.
  **Landed.** Detected via `TargetDependency.Backtrace`'s
  command-name (`isAddDependenciesEdge`); routes to the new
  `ir.Target.Data` slot rather than `Deps` /
  `ImplementationDeps`. Bazel `data = [":dep"]` for the
  build-order semantics. Conservative — fires when the
  backtrace records the call directly; macro-wrapped
  add_dependencies fall back to the link path.

- **`add_custom_target(name ALL DEPENDS file)`** — *trace +
  ninja*. *Medium slice*. cmake's "phony target with
  dependencies." We have CustomCommandEdges; the standalone
  emission picks them up as genrules but doesn't attribute the
  ALL keyword (would mean Bazel `--build_runfile_links`-style
  always-build).

- **`set_target_properties(... POSITION_INDEPENDENT_CODE)`** —
  *trace*. *Small slice*. Per-target PIC override.
  Bazel: cc_library has no direct PIC attribute; the
  toolchain feature `pic` handles it. Lift: emit
  `features = ["pic"]` or `features = ["-pic"]` based on the
  property value.

- **`set_target_properties(... VERSION SOVERSION)`** — *trace*.
  *Small slice (shared library only)*. cmake adds version
  suffixes to shared library names. Bazel cc_library doesn't
  expose this directly; would need a custom emit shape.

## Medium: per-target properties cmake doesn't surface in codemodel

The codemodel exposes a curated subset of target properties.
Many useful ones don't surface:

- **`POSITION_INDEPENDENT_CODE`** (see above).
- **`MSVC_RUNTIME_LIBRARY`** — Windows-only runtime selection.
- **`JOB_POOL_COMPILE` / `JOB_POOL_LINK`** — ninja job-pool
  routing. Bazel: `exec_properties = {"pool": "..."}` if
  using Remote Execution.
- **`<LANG>_VISIBILITY_PRESET`** — symbol visibility default.
- **`LINK_FLAGS_<CONFIG>`** — per-config link flags
  (the multi-config fold handles general flags; this is the
  per-config override).

The probe-genex hook from Phase 3 can extract these via
`file(GENERATE)` against `$<TARGET_PROPERTY:t,<prop>>` — the
existing infrastructure already handles unbounded property
queries. Cost: per-target file count grows linearly with the
property set we probe.

## Hard: behaviors a generator embeds but we'd need to reimplement

- **`$<COMPILE_LANGUAGE:CXX>` / `$<LINK_LANGUAGE:C>`** — *target
  evaluator only*. cmake's per-target-and-action evaluator
  knows the language being compiled when this genex fires;
  outside cmake we can't replicate the late-binding. The
  probe-genex hook captures only literal genex shapes the
  evaluator can resolve at generation time.

- **`cmake_policy(PUSH/POP)`** per-call-site state — *internal
  to cmake interpreter*. The codemodel reports the resolved
  end-state; per-call-site policy differences (CMP0054 NEW
  vs OLD in different parts of the same project) are lost.
  Mitigation: refuse projects without
  `cmake_minimum_required(VERSION 3.20+)` (we already gate on
  this for the codemodel floor).

- **`CMAKE_AUTOMOC` / `AUTOUIC` / `AUTORCC` (Qt)** —
  *generator-side codegen*. cmake's Qt support runs `moc` /
  `uic` / `rcc` as part of generation. Without that
  generator-time codegen, downstream cmake projects relying
  on AUTOMOC don't have the generated sources available at
  Bazel build time. Lift options: (1) refuse via Tier-1
  failure with a clear "use kind:bazel override" message;
  (2) emit a genrule that runs `moc` etc. at Bazel build
  time (operator must stage moc as a host-tool).

- **`add_test(NAME … COMMAND $<TARGET_FILE:foo>)`** with
  late-binding genexes — *target evaluator*. The
  probe-genex hook should cover TARGET_FILE-shaped cases;
  worth verifying with a fixture.

- **`install(SCRIPT ...)` / `install(CODE ...)`** —
  *arbitrary shell at install time*. A generator runs these
  via cmake; outside cmake we'd need to execute the script
  ourselves (out of scope for the converter; refuse cleanly
  in the Phase 6 classifier).

- **`ExternalProject_Add` + `FetchContent`** — *configure-time
  network I/O*. Bazel forbids network in actions; lift maps
  to `http_archive` repository rules in MODULE.bazel
  extensions only when URLs + hashes are literal. The cmake
  semantic of "fetch as part of configure, then participate
  in the target graph" can't be reproduced under Bazel's
  hermetic-action model.

## Architectural: cmake-side state we deliberately don't model

- **`set(CACHE)` / `option()`** — operator-tuned per-build
  config knobs. Bazel's equivalent is `--define` / bzlmod
  flags + select(). Currently we resolve cmake's cache
  one-shot at convert time; future slice could emit
  `config_setting`s for cache vars that genuinely drive
  per-build behavior (`select_to_setting` shape).

- **`if(CMAKE_BUILD_TYPE STREQUAL "Debug")` source-graph
  conditionals** — runtime cmake interpretation. The
  multi-config fold handles flag-level deltas; structural
  conditionals (adding/dropping targets per config) need the
  phantom-target select mechanism in `internal/empfold`
  (already in tree for per-platform; the per-config case
  inherits).

## Where to start (remaining wins)

Items still queued after the gap-fill pass:

1. ✓ **`Link.CommandFragments` role attribution**. **Landed.**
   The lower-side switch now routes role=flags / libraryPath /
   frameworkPath / frameworks each to the right linkopts form
   (previously every non-libraries fragment was silently
   dropped). Three goldens regenerated with the new flag entries.

2. ✓ **`OBJECT_DEPENDS` source property**. **Landed.**
   Headers declared via set_source_files_properties(...
   OBJECT_DEPENDS ...) now append to the consuming target's
   hdrs for incremental-rebuild correctness.

3. ✓ **Per-target `BUILD_RPATH` / `INSTALL_RPATH`** via
   probe-genex. **Landed.** probe-genex.cmake now also captures
   BUILD_RPATH / INSTALL_RPATH / POSITION_INDEPENDENT_CODE /
   {CXX,C}_VISIBILITY_PRESET as `property_<NAME>.txt` files.
   Lower routes BUILD_RPATH to `-Wl,-rpath,<path>` linkopts,
   PIC to `features=["pic"]` / `["-pic"]`, visibility presets
   to `-fvisibility=<value>` copts.

4. **`find_package-v1` event attribution** as Provenance
   tagging on emitted cc_import targets. Consumes Phase 2's
   configureLog YAML data threaded through lower.Options.
   Matching event → target needs the imports.Resolver walk.

5. **`SOURCE_FILE_PROPERTIES` LANGUAGE override**. Force a
   `.c` file to compile as C++. Bazel cc_library can't do this
   without per-source library splits (source extension drives
   language; renaming the file is intrusive). The
   shouldSplitCompileGroups gate covers cases where cmake
   already produced distinct CompileGroups for the override.

6. **`add_test` with TARGET_FILE genex**
   (`add_test(NAME foo COMMAND $<TARGET_FILE:my_test>)`).
   Should resolve via probe-genex's TARGET_FILE capture but
   lacks an end-to-end fixture exercising it.

7. **`option()` declarations** as Bazel
   `bool_flag` / `config_setting` so operators can tune at
   build time via `--//foo:opt=true`. Requires
   `@bazel_skylib` dep + the gen of the constants block.

Each is independently shippable; the doc updates as each
lands.
