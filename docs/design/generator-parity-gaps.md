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

- **`CompileGroup.LanguageStandard`** — *codemodel-v2*. *Small slice*.
  cmake records the resolved `CMAKE_<LANG>_STANDARD` value per
  CompileGroup (e.g. `"17"` for `cxx_std_17`). The converter
  loads the struct but doesn't lower it. Bazel-idiomatic emit:
  `copts = ["-std=c++17"]` on cc_library/cc_binary, or surface
  via a cc_toolchain feature when the project standardizes across
  targets.

- **`CompileGroup.PrecompileHeaders`** — *codemodel-v2*. *Small slice*.
  Per-target `target_precompile_headers(...)` declarations.
  Bazel cc_library doesn't natively support PCH at the rule
  level; cc_toolchain has a `pch` feature shape. Lift: emit
  the PCH headers as `hdrs` and add a comment noting the
  cmake-side PCH intent (operators wire the toolchain feature
  for the actual PCH effect).

- **`CompileGroup.Frameworks` / `Link.Frameworks`** — *codemodel-v2*.
  *Small slice (macOS-only)*. Apple `-framework Foo` link
  directives. Bazel cc_library has `linkopts = ["-framework",
  "Foo"]`. Only meaningful on macOS targets; gate emission on
  the platform.

- **`CompileGroup.Sysroot` / `Link.Sysroot`** — *codemodel-v2*.
  *Small slice*. Per-target cross-compile sysroot. The converter
  loads but doesn't lower. Bazel handles sysroot via cc_toolchain;
  surfacing the per-target override is informational at best
  (the operator's cc_toolchain is the canonical home for
  cross-compile config).

- **`TargetArchive.LTO` / `TargetLink.LTO`** — *codemodel-v2*.
  *Tiny slice*. Per-target `INTERPROCEDURAL_OPTIMIZATION` flag.
  Lift: `--features=lto` per-target via the existing
  Phase 5 sanitizer-as-feature plumbing.

## Easy: trace data we already decode

- **`shadow.ExtractSourceFileProperties` properties** — *trace*.
  *Per-property slice each*. The decoder is wired; consumers
  for specific properties haven't all landed:

  - `HEADER_FILE_ONLY` → reclassify source from `srcs` to `hdrs`
    (file declared but not compiled).
  - `LANGUAGE` override → bypass compile-group's reported
    language (rare; usually `set_source_files_properties(...
    PROPERTIES LANGUAGE CXX)` on a `.c` file).
  - `COMPILE_FLAGS` (old form) → augment per-source copts via
    the CompileGroup-split mechanism (Phase 1 task 3
    extension).
  - `GENERATED` → already handled via `TargetSource.IsGenerated`
    in the codemodel; trace adds no new signal.
  - `OBJECT_DEPENDS` → declares manual header deps; could
    augment Bazel `cc_library.srcs` with the listed paths so
    incremental builds see them.

## Medium: data the File API exposes that we don't query

- **`configureLog-v1` event consumption** — *configureLog YAML*.
  *Medium slice*. The reader is wired (`fileapi.LoadConfigureLogYAML`)
  but no lifter consumes `try_compile-v1` /
  `find_package-v1` events. Use cases:

  - Cross-reference `try_compile` outcomes with refused
    `execute_process` probes — when the probe's
    `OUTPUT_VARIABLE` matches a `try_compile` result variable
    that the YAML records, lift the probe to a constant
    instead of the BucketProbe refusal.
  - Surface `find_package` resolutions as comments on the
    cc_import emission so operators see WHERE the dep came
    from (system / vendored / custom-CMAKE_PREFIX_PATH).

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

- **`add_dependencies(target dep)`** — *trace*. *Small slice*.
  Pure build-order edge (the dep doesn't propagate
  compile/link facts). Bazel-idiomatic shape:
  `data = [":dep"]` on the target (forces build but not link).
  Currently lost — only `Target.Dependencies` (the
  target_link_libraries-derived edges) survives.

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

## Where to start

Lowest-friction wins for a contributor wanting to extend coverage:

1. **`CompileGroup.LanguageStandard`** consumer (`-std=c++17`
   copts emission). Single field, direct lift, ~50 lines + test.
   *See [the LanguageStandard slice in this PR series](https://github.com/sstriker/buildstream-bazel/pull/N).*

2. **`HEADER_FILE_ONLY` source-file property** consumer.
   Reclassifies sources from srcs to hdrs. Small change to
   the source walk; benefits projects with header-only files
   declared via `set_source_files_properties`.

3. **`Link.Frameworks`** consumer (macOS `-framework Foo`).
   Tiny — emit linkopts entries directly from the codemodel
   slice we already load.

4. **`add_dependencies` → `data = [":dep"]`** wire from the
   trace decoder. Pure build-order edge that's currently
   dropped.

Each is independently shippable; the doc updates as each
lands.
