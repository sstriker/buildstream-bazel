# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **Multi-version cmake compatibility shakeout.** Per-object
  schema-major validation now lives in `fileapi/reply.go` and a
  non-blocking `e2e-latest-cmake` CI job runs the converter
  against the runner's stock cmake (3.31+) alongside the pinned
  3.28.3 path. The first surface this catches in practice is
  build.ninja drift in newer cmakes (C++20 module dyndep, custom
  command emission); fixes for whatever the matrix surfaces land
  here as they come up. Once the matrix has been green for a
  release cycle we can promote it to a blocking gate.
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

## Next

- **Per-platform fold for round-2 trace-driven kinds.** First
  half shipped (see Done — `convert-element-trace --out-ir-json`,
  `ir.Target.PerPlatformScalar` for cc_import path attrs,
  elementfold's scalar-attr per-platform delta, `_trace_repo`
  `platform` attr, write-a `--platforms-json` driving project A
  fan-out for pipelineHandler kinds — kind:make / manual /
  script / makemaker / modulebuild). What's left:
  - **Project B per-platform install genrules.** Project A's
    fan-out is in; project B still emits one install genrule
    per element. End-to-end correctness needs N install
    genrules so `trace-publish` lands N AC entries with
    distinct platform tags. Mechanical wiring — the inline
    publish step already reads `CMAKE_TO_BAZEL_PLATFORM` at
    action time.
  - **kind:autotools per-platform render.** Same shape as
    pipelineHandler's, just at the `autotoolsHandler`
    dispatch site (`handler_autotools_native.go`).
  - **kind:cmake Phase B fallback per-platform render.** The
    converter genrule already exists
    (`handler_cmake_round2.go`); needs the fan-out wired.
  - **kind:meson Phase B per-platform render** when Phase B
    lands.

  The cc_import scalar-select() rendering already handles the
  diverging path-attr case (`.so` vs `.dylib`, multiarch lib
  dirs, arch-tagged binary names) for the install_tree.tar
  stub shape. Render gate: `scripts/meta-trace-round2-fold.sh`.
- **Element-signal consumption in the unifier.** Stage 6 capture
  is in (`--collect-toolchain-signal` flows fileapi replies into
  `<out>/elements/<name>/toolchain-signal/`). Pending: wire
  `unify-toolchains --element-signal <dir>` to fold any
  builtin-include / sysroot fact a real element exposes that
  the dedicated probe missed into the platform's
  `ResolvedToolchain.Base`. Needs platform-association heuristic
  (orchestrator runs single-platform today; the signal directory
  belongs to that one platform).
- **Drop the hardcoded `defaultPlatform`.** The orchestrator's
  REAPI Action.Platform fallback (linux/x86_64 + cmake/ninja/bwrap
  pins) is transitional: once operators have
  `//platforms:*` declared by the unifier, the orchestrator
  should derive Action.Platform properties from the chosen
  Bazel platform's constraint_values via a constraint→property
  mapping. The CLI gets a `--target-platform=//platforms:linux_aarch64`
  flag with no default; CI / e2e tests update to pass it
  explicitly. Blocked on the unifier seeing real probe data
  in CI so the platforms package exists at orchestrate time.
- **Wire the narrowing-undercoverage audit into a CI gate.**
  All the plumbing is in:
  - cmake oracle via convert-element's
    `--out-cmake-configure-reads`.
  - trace oracle via build-tracer's `--source-root` flag +
    `tracenorm.ExtractReads`.
  - per-element pattern surface emitted by write-a as
    `srckey-patterns.txt`.
  - `cmd/audit-narrowing` consumes patterns + oracle(s) and
    emits a sorted undercoverage report.
  - configure_file lift (`--cmake-configure-file-bin` on
    write-a) makes `*.h.in` safe to mark
    `exclude **/*.h.in` in patterns for elements whose
    templates lifted: with the full cmake-variable namespace
    captured into the values JSON
    (`cmakerun.filterVolatilePaths`), template edits that add
    `@VAR@` markers resolve correctly through the Bazel-time
    tool without convert-element rerunning. The
    `cmake-codegen-lifted` tag distinguishes lifted vs legacy
    genrules; legacy ones still have `.h.in` content-load-bearing
    and shouldn't be excluded.
  
  Missing piece: actually run the audit somewhere. Two
  conversations to have before flipping the gate on:
  - **Policy**: hard-fail on any drift, or accept a per-element
    allowlist of expected misses? Templates that didn't lift
    (the v1 Extract can't recover values from every shape)
    keep flagging their `.h.in` correctly. A whitelist mechanism
    plus an "expected drift" file alongside
    `srckey-patterns.txt` is the realistic landing.
  - **Per-element opt-in to capture** for trace-driven kinds:
    pass `--source-root` into the round-2 install genrule's
    build-tracer + trace-publish invocations (today
    `pipelineTracePublishStep` doesn't). Without this the
    trace oracle is empty and only the cmake side carries
    signal.
- **Same lift shape for `file(GENERATE)` and cmake-builtin
  `add_custom_command`s.** Wherever `lower/` currently reads
  bytes from the build dir to embed in a genrule's `cmd`, the
  same cache-key issue applies. The configure_file lift's
  pattern (template + values + Bazel-time tool) is reusable
  for these other configure-time codegen surfaces; sweep
  through `lower/codegen.go` and `lower/configure_file.go`'s
  callers, classify each by what cmake feature they recover,
  and lift the cleanly-tractable cases.
- **kind:cmake round-2 fallback for unliftable
  `execute_process`.** Phase B follow-on to the Now-bullet
  native lift. When `convert-element` exits with
  `unsupported-execute-process`, the round-2-style coarse
  "cmake configure + ninja + install" genrule takes over for
  that element — same destination as kind:autotools / make /
  makemaker / modulebuild / manual / script, but reached
  differently. kind:cmake
  is **not** a `pipelineHandler` variant (no
  `traceDrivenSrckeyPatterns` field to flip; no
  `shouldUseRound2()` branch), and it doesn't have an
  autotools-style `round2Enabled` build-wide flag either —
  flipping either would force every kind:cmake element through
  round-2, sacrificing the fine-grained native render. Instead
  the dispatch is **per-element**: `cmakeHandler` keeps the
  native render as the primary path, and the round-2 install
  genrule + placeholder BUILD.bazel.out are extra wiring
  emitted alongside, activated only when convert-element
  refuses the call. Reuses `cmd/build-tracer`,
  `@trace_<elem>//:trace`, and the inline `trace-publish`
  rendezvous machinery — all of which are kind-agnostic
  already. Render gate: `scripts/meta-cmake-round2-fallback.sh`. Live-AC gate:
  the publish/lookup wire half of
  `tools/e2e-meta-autotools-round2-live.sh` is kind-agnostic
  but its bazel-build half is autotools-fixture-specific; a
  cmake sibling gate is the v1 plan. Architectural recipe:
  `docs/design/cmake-execute-process-round2-fallback.md`.
- **Repo-rule install for kind:cmake round-2 fallback.**
  Phase B's round-2 fallback (per
  `docs/design/cmake-execute-process-round2-fallback.md`)
  transports the install tree as `install_tree.tar` between
  project B and project A's `BUILD.bazel.out`, costing roughly
  2× bytes in CAS (tar blob + extracted files via the
  in-`BUILD.bazel.out` extract genrule) and one extra Bazel
  action per consumer. Storage duplication adds up across a
  fleet. Alternative: a Bazel repository rule whose
  `repository_ctx.execute()` either runs cmake at loading
  time directly OR untars `install_tree.tar` into a
  per-element repo, exposing per-target labels without the
  extract genrule + CAS duplication. Precedent:
  `rules/traces.bzl`'s `_trace_repo` (loading-time AC
  lookup) — but that one only does AC `GetActionResult`, not
  a full build. Trade-offs: loading-time work blocks Bazel
  startup; repo rules don't run on RBE (executor-pool
  advantages forfeited); hermeticity weaker (relies on
  host-side cmake/ninja). Worth re-evaluating once fixtures
  reveal the storage-duplication cost in practice.
- **kind:meson round-2 fallback.** Phase A (deep introspection-driven
  render) shipped — see Done. The Phase B sibling for elements whose
  configure refuses (subprojects, generated_sources from custom
  targets the v1 lift can't recover, unresolved external deps) is the
  same shape as kind:cmake's round-2 fallback: project B hosts an
  install genrule wrapping `meson setup + ninja + meson install
  --destdir + tar` under build-tracer + inline trace-publish; project
  A's converter genrule reads `intro-install_plan.json` to emit per-
  target stubs (`runtime` → `sh_binary` / `cc_binary`, `devel` +
  `libdir_static` → `cc_import(static_library=…)`, `devel` +
  `libdir_shared` → `cc_import(shared_library=…)`) referencing the
  install_tree.tar. The install-plan's `tag` field
  (`runtime`/`devel`/`man`) gives a richer signal than cmake's
  destination-path inference; structural recipe parallels
  `docs/design/cmake-execute-process-round2-fallback.md`.
- **`bst` wrapper** so `bst build` works against a converted project
  (and against `bst workspace open`-modified element source trees).
  Goal: BuildStream developers' muscle memory keeps working through
  the transition.
## Later (research / open questions)

- **Source-side AC narrowing for autotools.** Bazel's hermetic-action
  model says inputs in → outputs out; you can't have a byte be
  available to the action at exec time without it being in the AC
  key. So narrowing autotools is unavoidably a side-channel story.
  `docs/three-pass-flow.md` lays out three options (FUSE, host-fs
  source cache via `--repo_env`, write-a-time registry) and rules
  out two; the third is the path forward but the value-vs-complexity
  trade-off is open.
- **kind coverage breadth.** `kind:script` / `kind:pyproject` /
  `kind:flatpak_image` / `kind:snap_image` / `kind:collect_*` all
  have placeholder handlers today. Each kind is bounded work; what's
  not bounded is the question of which kinds are graph-recoverable
  vs need-to-stay-coarse.
- **Drop the host-toolchain assumption from CI / e2e gates.**
  Several gates expect cmake / ninja / bwrap / fuse3 installed on
  the host machine (CI runner or developer workstation). In a
  full remote-execution setup these belong on the executor, not
  on the dev's box — the dev only needs Bazel + bb_clientd. Walk
  the gate scripts and the `make check-tools` surface, identify
  which host-tool dependencies are CI-runner artifacts (vs
  hard build-tool needs we can't push remote), and migrate the
  former onto the executor toolchain.

## Done (high points)

- **Target-presence delta in `elementfold` — phantom-target select.**
  A target declared by some cells but absent from others no longer
  errors out the fold; it lands in the merged Package with its
  attrs routed through `PerPlatform` / `PerPlatformScalar` arms
  keyed only on the present cells. `Fold`'s target enumeration is
  now the union of every cell's `(Name, Kind)` set (taking
  cells[0]'s order first then any not-yet-seen keys), so single-
  platform goldens stay byte-stable when every cell declares the
  same set. `foldTarget` takes a `(presentCells, allCellNames)`
  pair: scalar/boolean agreement runs across present cells only,
  while `empfold.Partition` sees the full matrix so phantom
  targets collapse the flat baseline to empty (absent cells
  contribute no facts, no fact is "in every cell," nothing lands
  in baseline) — every present-cell observation routes to a
  delta arm. `foldOrderSensitiveAttr` / `foldScalarAttr` take a
  `phantom` flag and force the per-platform shape even when
  present cells agree, so absent platforms don't inherit a flat
  baseline that promises content for a target they don't have.
  Bazel consumers depending on a phantom target on an absent
  platform see attrs resolve to `[]` (a list attr's
  `//conditions:default`) or to `None` (a scalar attr's
  `//conditions:default`, treated by Bazel as "attribute unset"
  per cc_import's optional-path-attr semantics) — the right
  outcome for a target that genuinely doesn't exist on that
  platform. Picked the **phantom-target select** shape over the
  alias-driven gate variant: lowest-touch change to `elementfold`,
  no `//:no-op` filegroup overhead in every project A, no two-
  rules-per-target multiplication.

- **Per-platform fold for round-2 trace-driven kinds — project A,
  pipelineHandler kinds.** First half of the per-platform fold
  for round-2: project A's per-element converter render fans out
  one genrule per (element, platform) cell plus a fold-element
  genrule composing the N `ir.Package` JSONs into one
  `BUILD.bazel.out`. `convert-element-trace` gained
  `--out-ir-json` and the trace converter's recovered rules now
  flow through the shared `converter/ir.Package` so
  `fold-element` + `converter/internal/elementfold` compose them
  the same way they compose kind:cmake Phase A IRs. The IR also
  gained `PerPlatformScalar` for cc_import path attrs (the
  round-2 stub shape's main divergence axis: `.so` vs `.dylib`,
  multiarch lib dirs); `emit/bazel` renders `static_library =
  select({...: "lib/x86_64-linux-gnu/libfoo.a", ...})` for it.
  `rules/traces.bzl`'s `_trace_repo` gained a `platform` attr
  so a single Bazel build resolves N per-platform
  `@trace_<elem>__<platform>//:trace` repos without env-var
  conflict; legacy single-platform path stays byte-stable
  (empty attr → env-var fallback). `cmd/write-a` opt-in:
  `--platforms-json` + `--fold-element-bin`. Scope today is
  pipelineHandler-shaped kinds (kind:make / manual / script /
  makemaker / modulebuild); kind:autotools and kind:cmake
  Phase B fallback share the same shape and ship in follow-up
  commits on the same branch. Project B's per-platform install
  fan-out is queued under Next ("Per-platform fold for round-2
  trace-driven kinds"); without it, multi-platform end-to-end
  publishes only one platform's trace at runtime, so the
  feature is render-shape complete but not yet runtime-
  complete. Render gate:
  `scripts/meta-trace-round2-fold.sh`.

- **Platform-tagged synthetic AC key for trace publish/lookup.**
  `tracenorm.SyntheticActionDigest` takes a platform tag in
  addition to srckey; non-empty tags partition the synthetic
  AC keyspace so two platforms' traces against the same source
  content land at distinct AC keys instead of one shadowing
  the other. Empty platform preserves the historical
  2-argument shape exactly — single-platform operators
  upgrading past this revision keep their previously published
  AC entries valid. `trace-publish` / `trace-lookup` gain a
  `--platform` flag; `rules/traces.bzl`'s `_trace_repo` reads
  `CMAKE_TO_BAZEL_PLATFORM` from the operator's `--repo_env`
  and passes it to `trace-lookup`; project B's install
  genrules (cmake round-2 + the autotools-family pipeline)
  read the same env var via `--action_env` and pass it to
  `trace-publish`. The publish/lookup rendezvous now hits
  only when both sides agree on the platform — a darwin
  trace and a linux trace coexist in the AC without
  collision. The matching converter-side fold of per-platform
  install plans is still queued under Next as
  "Per-platform fold for round-2 trace-driven kinds".

- **Per-element multi-platform BUILD generation (kind:cmake Phase A).**
  `convert-element` no longer bakes the host's viewpoint into each
  per-element `BUILD.bazel`. The orchestrator's
  `--platforms-json` flag (parallel to the toolchain unifier's
  manifest) drives one `convert-element` REAPI Action per
  (element, platform) cell; the resulting per-platform
  `ir.Package` JSONs (emitted via convert-element's
  `--out-ir-json`) feed `cmd/fold-element`, which composes them
  into a single unified `BUILD.bazel` whose attributes carry
  `select()` blocks for divergent srcs/hdrs/includes/defines/deps
  and per-platform-routed copts/linkopts. `internal/empfold`
  factors out the cardinality-partition primitive
  (`toolchain.Observe` now uses it too).
  `converter/internal/elementfold` enforces per-target
  cross-cell agreement on scalar fields (Linkstatic, Alwayslink,
  Genrule*, Test*, …) and folds the order-sensitive
  copts/linkopts conservatively (identical sequences → flat
  baseline; any divergence → empty baseline + each cell's full
  sequence under its `SelectKey` so per-platform flag order
  survives to the compiler). `PickSelectKeys` auto-detects
  single-axis matrices ({linux, darwin} or {x86_64, arm64})
  and honours an operator-supplied `select_label` per platform
  for matrices where no constraint axis uniquely identifies
  each cell ({linux_x86_64, linux_aarch64, darwin_arm64}) — the
  operator declares a `config_setting` per platform in their
  `//platforms` package and supplies its label, escaping the
  auto-detect ambiguity error with an actionable contract. N=1
  manifests render flat lists byte-identical to today's content
  (the on-disk layout / Action digests differ because the
  multi-platform path always emits `ir.json` and lands outputs
  under per-platform subdirs; leave `--platforms-json` unset
  for the byte-stable legacy route). Render gate:
  `scripts/meta-element-fold.sh`. Scope is kind:cmake Phase A
  only; trace-driven kinds and round-2 fallbacks have a
  separate per-platform fold story queued in Next.


- **kind:pyproject native render (Phase A).** New
  `converter/cmd/convert-element-pyproject` statically analyzes
  `pyproject.toml` + the source tree and emits native
  `py_library` / `py_binary` rules. Per-backend dispatch (flit /
  hatchling / setuptools / poetry-core) drives package-
  discovery; `[project.scripts]` entries become py_binary with a
  generated entry shim. Typed Tier-1 refusals (`unsupported-
  pyproject-{backend,c-extension,dynamic-metadata,package-
  discovery,entry-point}`, `unresolved-pyproject-dependency`,
  `pyproject-parse-failed`) cover the patterns v1 doesn't lift;
  the pipeline-shape fallback (existing handler unchanged)
  catches the rest. Activated by passing
  `--convert-element-pyproject <path>` to write-a; project B's
  MODULE.bazel auto-adds `rules_python` when at least one
  kind:pyproject element is present and the native path is on.
  Render gate: `scripts/meta-pyproject.sh` against
  `testdata/meta-project/pyproject-greet/` (representative
  setuptools fixture). Recipe: `docs/design/pyproject-native-
  render.md`. Coverage status:
  `docs/fdsdk-coverage-status.md`.
- **`convert-element-autotools` → `convert-element-trace` rename.**
  The trace-driven converter has served kind:make / kind:manual /
  kind:script / kind:makemaker / kind:modulebuild as well as
  kind:autotools for several PRs (each kind opted in via its
  `pipelineHandler.traceDrivenSrckeyPatterns` field), but the
  binary kept the autotools-only name. The rename clarifies what
  the code actually does: it operates on cc/ar execve events,
  not on autotools-specific patterns. Renames in this PR:
  `cmd/convert-element-autotools/` →
  `cmd/convert-element-trace/`; the `--convert-element-autotools`
  write-a flag → `--convert-element-trace`; the
  `--autotools-round1` write-a flag → `--trace-round1`; the
  `autotoolsConfig` global in write-a → `traceConfig`; and the
  `//tools:convert-element-autotools` Bazel label →
  `//tools:convert-element-trace`. Clean break — no aliases. Doc
  taxonomy follow-up: `docs/fdsdk-coverage-status.md` now
  reclassifies the five formerly-coarse trace-driven kinds as
  deep, lifting the FDSDK deep-conversion figure from 44 % to
  ~65 %.
- **kind:meson native render (Phase A).** New
  `converter/cmd/convert-element-meson` runs `meson setup` against a
  source tree, parses `<build>/meson-info/intro-targets.json` +
  siblings, and lowers into the same IR the kind:cmake converter
  emits — yielding native `cc_library` / `cc_binary` rules in
  `BUILD.bazel.out`. Per-target `target_sources.parameters` are split
  into `Includes` (`-I`), `Defines` (`-D`), and `Copts` (everything
  else, with toolchain-handled flags like `-fPIC` /
  `-fdiagnostics-color=always` filtered). `link_with:` propagates as
  a `libfoo.a` linker argument, which the converter matches against
  in-project archive output basenames to populate `Deps`. External
  `dependency('foo')` entries resolve via the imports manifest; deps
  meson can fold inline (e.g. `threads → -pthread`) flow into Copts /
  LinkOpts directly. Typed Tier-1 refusals (`unsupported-meson-
  subproject`, `unsupported-meson-target-type`, `unsupported-meson-
  custom-target`, `unsupported-meson-generated-sources`,
  `unsupported-meson-cross-compile`, `unresolved-meson-dependency`)
  cover the patterns v1 doesn't lift; the Phase B install-plan
  fallback (queued in Next) catches the rest. `cmd/write-a`'s kind:
  meson handler is opt-in via `--convert-element-meson <path>` —
  unset preserves the historical pipeline-shape coarse install
  genrule. Render gate: `scripts/meta-meson.sh`. Recipe:
  `docs/design/meson-native-render.md`. Coverage status:
  `docs/fdsdk-coverage-status.md`.
- **`execute_process` recovery for kind:cmake.** Phase A
  (native lift): the deterministic buckets — `cmake -E touch
  / copy / copy_if_different` and file-producing tools with
  declared `OUTPUT_FILE` — translate to native Bazel genrules.
  Unliftable buckets (version stamps via `git rev-parse`,
  host probes like `uname -m / gcc --version`, multi-COMMAND
  pipelines, opaque shell scripts) emit a typed
  `unsupported-execute-process` Tier-1 failure with a per-call
  triage report. File-producing hoists carry a
  `cmake-codegen-execute-process-hoisted` audit tag.

  Phase B (round-2 fallback): opt-in
  `--cmake-round2-fallback` mode wires the kind-agnostic
  round-2 plumbing for kind:cmake. A's converter genrule
  threads `--unsupported-execute-process-fallback=true` so
  classifier refusals produce the placeholder shape
  (per-target `cc_import` / `sh_binary` stubs from codemodel
  install destinations + `cc_import.hdrs` from
  `Target.FileSets HEADERS` + extract genrule referencing
  `install_tree.tar`) instead of exiting Tier-1; Project B
  emits a real install genrule wrapping cmake configure +
  ninja + install + tar under build-tracer + inline
  trace-publish. A's converter genrule consumes
  `@trace_<elem>//:trace` via load-time `_trace_repo` lookup,
  so a published trace from a previous Project B run is
  available at convert-element action time (the
  trace-driven convergence path queued in `Later` will teach
  the converter to refine refusals from the trace; the
  wiring is in place today).

  Render gate: `scripts/meta-cmake-round2-fallback.sh`.
  The kind-agnostic live-AC gate
  (`tools/e2e-meta-autotools-round2-live.sh`) covers
  kind:cmake's wire contract through its publish/lookup
  round-trip half. Recipe:
  `docs/design/cmake-execute-process-round2-fallback.md`.
  Failure schema: `docs/failure-schema.md`
  `unsupported-execute-process`.
- **Unified multi-platform Bazel toolchain layout from CMake.**
  Operators with cmake projects can now generate a normal-shaped
  multi-platform Bazel toolchain layout — `//platforms`,
  `//toolchains`, `cc_toolchain_config.bzl`, `.bazelrc` — driven
  by per-cell cmake probes rendered as a Bazel project A:
  - `cmakerun.Options.ExtraCacheVars` (Stage 1) plumbs arbitrary
    `-D<k>=<v>` flags through, with sorted-key rendering for
    determinism. `toolchain.Probe` now forwards every Variant
    cache var (not just CMAKE_BUILD_TYPE).
  - `toolchain.BazelFeature` (Stage 2) gains `Asan`, `Tsan`, `Msan`,
    `Ubsan`, `Coverage`, `Lto`. `SanitizerVariants` is the canonical
    catalog. `DefaultVariantMapping` classifies by
    CMAKE_C_FLAGS / CMAKE_CXX_FLAGS content first, build-type second.
  - `bazeltoolchain.emitConfigBzl` (Stage 2) now emits a hand-rolled
    `cc_toolchain_config` rule built on `cc_toolchain_config_lib.bzl`
    primitives — unix's feature list is sealed, hand-rolling lets us
    add `feature("asan")`/`feature("tsan")`/etc. blocks fed by the
    cmake-derived flag bundles.
  - `internal/toolchain/presets` and `internal/toolchain/kits`
    (Stage 3) parse `CMakePresets.json` and VSCode `cmake-kits.json`
    into `[]Variant` for `VariantMatrix` consumption.
    `converter/testdata/toolchain-probe/CMakePresets.json` is the
    canonical catalog cross-checked against `SanitizerVariants` by
    a unit test.
  - `cmd/render-project-a` + `internal/toolchain/projecta` (Stage 4)
    render a BUILD.bazel that drives the per-cell probe matrix:
    one genrule per (variant, platform) cell with
    `exec_compatible_with` carrying the platform's constraint set,
    invoking `cmd/probe-cell` with the variant's `--cache-var` flags.
    Cell artifacts serialize via `internal/toolchain/probejson`.
  - `cmd/unify-toolchains` (Stage 5) reads probe.json artifacts
    grouped by platform, folds each platform's cells through
    `Observe`, and writes `platforms/BUILD.bazel`,
    `toolchains/BUILD.bazel`, `toolchains/cc_toolchain_config.bzl`,
    and `.bazelrc` into the operator's repo. `cc_toolchain_config.bzl`
    is one attr-driven rule shared across all platforms (per-platform
    data flows in via attrs). `.bazelrc` includes
    `try-import %workspace%/user.bazelrc` so operator overrides
    later-win. MODULE.bazel is intentionally untouched; a one-time
    setup banner instructs the operator to add
    `register_toolchains("//toolchains:all")`.
  - Per-element toolchain signal capture (Stage 6) lands via
    `convert-element --out-toolchain-signal-dir` + orchestrator
    `Options.CollectToolchainSignal` + `orchestrate
    --collect-toolchain-signal`. Sets the foundation for the
    unifier to fold per-element builtin-include / sysroot facts
    into each platform's `ResolvedToolchain.Base` (--element-signal
    consumption is queued under Next).
  - Render gates: `meta-render-project-a.sh` + `meta-unify-toolchains.sh`.
- **Configure_file lift.** Per-element `*.h.in` templates are
  no longer load-bearing inputs of convert-element's cache key
  for elements whose templates lift. Convert-element captures
  the FULL cmake variable namespace at end-of-configure
  (`cmakerun/dump-vars.cmake` registers a deferred callback
  that dumps every variable; `cmakerun.filterVolatilePaths`
  drops path-bearing vars so the dump is byte-stable across
  cmake invocations). The recovered genrule emits with the
  .h.in as a real Bazel `srcs` input plus a
  `//tools:cmake-configure-file` invocation that re-runs
  cmake's substitution at Bazel build time. Edits to .h.in
  — including ones that introduce new `@VAR@` markers —
  invalidate the genrule directly through Bazel's source graph
  and resolve correctly via the namespace dump; convert-
  element doesn't have to rerun. Opt-in via `write-a
  --cmake-configure-file-bin=<path>` (the binary gets staged
  into both projects' `tools/` and the per-element genrule
  passes `--lift-configure-file=true` to convert-element).
  Templates the verify-pass can't reproduce (Substitute hasn't
  modeled an option, or the template references a filtered
  volatile variable) fall back to the legacy base64-cmd shape;
  for those, .h.in stays content-load-bearing in srckey. The
  `cmake-codegen-lifted` tag distinguishes lifted vs legacy
  genrules at query time. Recipe:
  `docs/design/narrowing-audit.md`.
- **Narrowing-undercoverage audit.** cmake oracle from
  build.ninja's `RERUN_CMAKE` deps + trace oracle from
  build-tracer's openat capture (opt-in via `--source-root`)
  + per-element patterns surface emitted by write-a +
  `cmd/audit-narrowing` consumer that diffs the two and
  emits an undercoverage report. Recipe:
  `docs/design/narrowing-audit.md`.
- Two-pass meta-project shape: `cmd/write-a` renders project A and
  project B, Bazel owns cross-element scheduling and caching.
- `kind:cmake` native render: cmake's File API + `--trace-expand`
  drive convert-element to emit `cc_library` / `cc_binary` rules.
  Zero-stub narrowing means `.c`-only edits cache-hit at the
  convert action.
- `kind:autotools` round-1 native render: build-tracer wraps
  `configure && make && make install`; the trace + `make -np`
  feed `convert-element-trace`; install genrule lives in
  project B with deps as proper Bazel targets.
- `kind:autotools` round-2 graph derivation. Project A's
  per-element converter genrule consumes `@trace_<elem>//:trace`,
  a load-time `_trace_repo` lookup against the REAPI
  ActionCache keyed by `SyntheticActionDigest(srckey)`. Project
  B's install genrule ends with an inline `trace-publish` call
  that lands the AC entry. Round-2 is the default; pass
  `--trace-round1` to opt back into the legacy single-
  genrule shape. Render-half gate: `meta-autotools-round2.sh`.
  Live-AC gate (buildbarn + optionally bb_clientd):
  `tools/e2e-meta-autotools-round2-live.sh`. Recipe:
  `docs/design/autotools-round2-rendezvous.md`.
- **kind:make joins the round-2 trace-driven path.** Same
  architecture as kind:autotools — opt-in via the
  `traceDrivenSrckeyPatterns` field on the kind's
  `pipelineHandler` (`handler_make.go:makeSrckeyPatterns`).
  When the trace-driven binaries are supplied to write-a, kind:make
  elements render with the converter genrule in project A and the
  build-tracer-wrapped install genrule + inline trace-publish in
  project B. Render-half gate: `meta-make-round2.sh`. The
  kind-agnostic live-AC gate
  (`tools/e2e-meta-autotools-round2-live.sh`) covers the
  bazel-build-half wire contract end-to-end against bb_clientd,
  applying to any opted-in kind.
- **kind:makemaker, kind:modulebuild, kind:manual, kind:script
  join the trace-driven path.** Same one-line opt-in pattern
  kind:make established. Per-kind srckey narrowing:
  kind:makemaker tracks Makefile.PL + *.xs + *.h family;
  kind:modulebuild tracks Build.PL + *.xs + *.h family;
  kind:manual + kind:script use the conservative
  content-everything default (no kind-level signal for which
  files drive build commands — per-element narrowing comes
  via the existing read-paths.txt sibling). Coverage:
  `cmd/write-a/handler_pipeline_round2_test.go` table-driven
  test asserts the round-2 shape end-to-end for each kind.
- **Bazel 9 CAS-aware filesystem.** Bazel 9 dropped
  `--unix_digest_hash_attribute_name` (the xattr fast-path that
  let cas-fuse tell Bazel "trust this pre-computed digest, don't
  re-hash") without a direct replacement. Replacement: adopt
  `bb_clientd` as a Bazel-9 companion daemon — paired with Bazel
  via the surviving `--experimental_remote_output_service=` flag,
  serving a FUSE mount + speaking RemoteOutputService so Bazel
  trusts daemon-reported digests. **Not** an adoption of buildbarn
  end-to-end; bb_clientd talks plain REAPI to whatever CAS endpoint
  it's pointed at (the same way `bazelisk` pairs with `bazel`).
  Output-side BwoB (lazy materialisation of build artifacts) is a
  free side effect. Wiring: `make bb-clientd-up`/`down`,
  `deploy/buildbarn/config/bb_clientd.jsonnet`. Local end-to-end
  exercise: `tools/e2e-hello-bbclientd.sh` (also `make
  e2e-hello-bbclientd`); not yet wired into GitHub Actions CI
  because the runners don't ship bb_clientd by default — adding
  it as a CI step would self-skip until that changes.
  `rules/sources.bzl` + `rules/traces.bzl` parameterise the
  mount-side path layout via `CAS_DIRECTORY_PREFIX` (default
  `blobs` historically — the cmd/cas-fuse layout; bb_clientd
  users pass `cas/<instance>/blobs/<digest_function>`).
  `cmd/cas-fuse` and `internal/casfuse` were retired in a
  follow-up after bb_clientd proved itself the production
  path. Recipe: `docs/design/bazel9-cas-fs.md`.
- Trace + make-db canonicalization (pids stripped, gcc temp paths
  placeholdered, action-time mktemp paths normalized). Foundation
  for round-2 cache reuse.
- Per-element srckey + per-kind narrowing patterns — defines what
  counts as graph-affecting vs name-only for the autotools build.

The "Done" list is in the rear-view; the doc that captures the
current state of the codebase is `docs/architecture.md` (top-down)
plus `docs/build-structure.md` (interop contract) plus
`docs/three-pass-flow.md` (build-time flow).
