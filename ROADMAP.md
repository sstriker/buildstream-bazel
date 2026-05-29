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
  single-`-I`-root include semantics survive the split); intra-
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
  `cmake_config_bundle`, aliases, genrules, interface libs)
  stay in the root package. Wired end-to-end through the
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

  - **Phase 1 — read what we already loaded.** Consume the
    `backtraceGraph` indices on `Target.Dependencies[]` to
    recover PUBLIC/PRIVATE/INTERFACE keywords without
    re-parsing `--trace-expand` (trace stays as fallback
    for cmake < 3.21 where backtraces are incomplete).
    Plumb `DirectoryInstaller.Type == "file"` /
    `"directory"` from `directory-*.json` into `ir.Package`
    so install(FILES) / install(DIRECTORY) lower to
    `pkg_files` at convert time instead of falling into the
    round-2 install-tree.tar path. Add a
    `shadow.ExtractSourceFileProperties` decoder mirroring
    `ExtractFileGenerate` to recover per-file
    `COMPILE_DEFINITIONS` / `GENERATED` / `OBJECT_DEPENDS`
    from the trace. No new cmake hooks; pure consumer-side
    wins on data the converter already pulls in.

  - **Phase 2 — request `configureLog-v1`.** Add a fourth
    File API object kind alongside codemodel / cache /
    cmakeFiles / toolchains in `fileapi.Index.requestQuery`
    (cmake 3.26+; gracefully absent on older). Decode
    `try_compile` / `find_package` / `check_*` outcomes into
    `fileapi.ConfigureLog`. Retire the probe-bucket
    `unsupported-execute-process` Tier-1 refusals where the
    outcome is already a recorded try_compile result —
    emit a `select()` over `@platforms` config_settings with
    the resolved value baked in.

  - **Phase 3 — genex-probe TOP_LEVEL_INCLUDES extension.**
    Generalize `dump-vars.cmake` into a probe-staging pass.
    The lifter walks trace + codemodel for any `$<…>`
    literal in a non-File-API site (file(GENERATE) CONTENT,
    add_custom_command argv, install destinations,
    target-property aggregates), then emits a per-literal
    `file(GENERATE OUTPUT cmake-to-bazel.genex.${hash}.txt
    CONTENT "<literal>" [TARGET t])` deferred call and reads
    the resolved bytes back into the lift Context. Retires
    `internal/genexeval`'s `UnsupportedError` surface
    (`TARGET_OBJECTS`, `INTERFACE_*` aggregation,
    cross-package `TARGET_FILE` PR2, and the other
    target-evaluator-dependent ops queued under `Later`)
    by letting cmake's own evaluator answer. The existing
    (a) Go-side evaluator becomes the offline-replay fast
    path; the probe becomes the source of truth when a
    fresh configure is available. The audit tag set
    collapses from `cmake-codegen-file-generate-genex{,-
    evaluated,-lifted,-cross-package}` to a single
    `cmake-codegen-genex-resolved`.

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
    documents the role. Still queued for Phase 4: a
    sibling TOP_LEVEL_INCLUDES hook that
    `file(GENERATE)`s each `OUTPUT_VARIABLE`'s captured
    probe / stamp value — cmake already executed the
    probe, we just persist the result. Stamp values lift
    to `stamp = 1` genrule attrs (so they don't bake into
    srckey); probe values lift to `select()` over the
    `configureLog` keys Phase 2 surfaces.

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
    wrong output).

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
    `cmake --install` at convert time. The manifest's
    `*manifest.Export` carries new `omitempty`
    `cmake_config_bundle_label` + `cmake_import_labels`
    fields so cross-element `find_package` consumers can
    resolve directly to the synthesized bundle.
    **Hard constraint preserved: convert is metadata-only
    — no `cmake --build` / `cmake --install` runs at
    convert time.** Earlier WIP that wired convert-time
    build was backed out (it would have changed the
    project's runtime model from sandboxable-and-cheap to
    "build farms"). The non-declarative residue stays on
    the round-2 `_install_tree_extract` fallback. The
    `resolved-lift` manifest-synth piece queued under
    `Later` is the remaining slice (the orchestrator's
    M3 step that populates the new manifest fields from
    Phase 6 codemodel verdicts).

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

  Phase shape: overview / phase boundaries / acceptance criteria
  are tracked here. The hook protocols + fold semantics + classifier
  rules land as code comments on the implementation PRs rather than
  separate design docs.

  Acceptance: FDSDK kind:cmake coverage delta drops to
  near-zero (the structural residue is `try_compile`-keyed
  target-graph shape per `docs/research/cmake_analysis.md`
  §7, which the round-2 fallback covers by construction);
  the `cmake-codegen-*-genex*` audit tag set collapses to
  one `-resolved` tag; `internal/genexeval`'s
  `UnsupportedError` surface goes away;
  `cmake-conversion-deltas.md` "open deltas" closes the
  configurable items; render-gate output for known
  sanitizer configs uses `--features` rather than raw
  per-config selects. The genex / TARGET_FILE / TARGET_OBJECTS
  / INTERFACE_* aggregation items currently under `Later`
  retire as Phase 3 lands.

- **Promote the cmake-version matrix from soft to blocking.**
  The four-version `e2e-cmake-matrix` shakeout (3.22 / 3.28 /
  4.0 / 4.3) shipped — see Done. Promotion criteria + tracker
  live in `docs/cmake-version-matrix.md`: three consecutive
  green merges across all four entries, any "Known
  per-version notes" rows resolved or explicitly deferred,
  then flip `continue-on-error: true` → `false` on the
  `e2e-cmake-matrix` job header (one-line YAML change; the
  `strategy.fail-fast: false` flag stays — it controls
  intra-matrix isolation, not block / non-block). Whatever
  real converter bugs the matrix surfaces in the meantime
  get filed here (or as follow-up bullets) as they show up.
- **`EventFindPackageFound` polymorphic-decode for cmake
  4.3+** (surfaced by the matrix's 4.3.3 entry on PR #243's
  first CI run — see `docs/cmake-version-matrix.md`'s
  "Known per-version notes" row). **Shipped in PR #244.**
  Cmake 4.3 reshaped the configureLog: a sibling `find-v1`
  event records every `find_program` / `find_file` /
  `find_library` call, and the `found` field is polymorphic
  across both event kinds — string path / `false` bool /
  `null` / legacy struct. Custom `UnmarshalYAML` on
  `EventFindPackageFound` accepts all four shapes; a
  captured cmake-4.3.3 configureLog fixture pins the
  parser. With this landed, the matrix's 4.3.3 entry goes
  green and one of the three promotion criteria clears.
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

- **A-B-C fidelity harness — productionize the ad-hoc convert+rebuild
  survey.** Foundation shipped — `cmd/fidelity-compare` Go tool
  + `scripts/run-fidelity.sh` driver + `make e2e-fidelity-compare-{zlib,
  spdlog,fmt}` library-side gates + `make e2e-fidelity-compare-{zlib,fmt}-consumer`
  consumer-side gates + `testdata/fidelity/*.allowlist.txt` companions.
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
    - **spdlog v1.14.1** ✅ library shipped — 1404/1404, 5
      template-instantiation entries.
    - **fmt 11.0.2** ✅ library + consumer both shipped — 146/146
      lib-side, 1/1 consumer-side; 3 lib-side + 4 consumer-side
      template-instantiation allowlist entries.
    - **nlohmann-json 3.11.3** ⚠️ blocked on converter — consumer-side
      gate is the right shape for this header-only INTERFACE library,
      but the converter today emits only an `install_directory__include`
      filegroup for json (no `cc_library`), so a consumer's
      `cc_library(deps = [...])` can't depend on it. Unblocking work:
      teach the converter to lower INTERFACE library targets to
      `cc_library(hdrs = glob(...), strip_include_prefix = ...)` when
      `target_include_directories(... INTERFACE ...)` is the only
      surface — separate converter improvement, then this gate becomes
      a clean addition.
    - **Catch2 3.5.3** ⚠️ deferred — needs the converter invoked with
      `--lift-configure-file` (to recover `catch_user_config.hpp` from
      the configure_file template) AND `//tools:cmake-configure-file`
      staged in the test workspace as a Bazel-build-time tool. Harness
      extension: a `--convert-flags '...'` passthrough in
      `scripts/run-fidelity.sh` + a tool-staging step that builds the
      converter's existing `cmd/cmake-configure-file` and writes a
      `tools/BUILD.bazel` exposing it as a `sh_binary`.
    - **libpng 1.6.x** ⚠️ deferred — `find_package(ZLIB REQUIRED)` in
      libpng's CMakeLists means Bazel-side needs a `@zlib` external
      repo with cc_library labels libpng's converted BUILD can resolve.
      Harness extension: a `--bazel-external '...'` passthrough that
      writes `http_archive`/`new_local_repository` entries into the
      synthesized WORKSPACE.

  Remaining work:
    - Converter improvement to emit `cc_library` for INTERFACE-only
      targets — unblocks nlohmann-json's consumer-side gate.
    - Consumer-side gates for spdlog (the project's own static lib
      consumers also use header-side typedefs / templates; an extra
      signal beyond the lib-side 1404 match).
    - Harness extensions for Catch2 (`--convert-flags` + tool staging)
      and libpng (Bazel-side external repos). VTK / LLVM gates need
      the project's specific configure flags + tooling and may need
      larger allowlists.
    - CI wiring (`continue-on-error: true` initially, then promote
      to blocking after three consecutive green merges across all
      configured fixtures).

  Acceptance: a converter regression that drops a symbol from the
  output artifact (e.g. accidentally skipping a source file in
  some edge case — see post-#258/#253 interaction caught by hand
  in PR #261) fails CI with a precise per-symbol diagnostic
  instead of being caught only when a downstream consumer breaks.

- **Per-platform fold for round-2 trace-driven kinds —
  kind:meson Phase B promotion.** The render gate for the
  install fan-out shipped
  (`scripts/meta-meson-round2-fallback-multiplatform.sh`,
  `make e2e-meta-meson-round2-fallback-multiplatform`); the
  sibling-kind contract is now uniformly green across kind:make
  / autotools / cmake-fallback / meson-fallback. Production
  promotion is gated on an FDSDK fixture that actually exercises
  multi-platform meson at scale (today the gate uses the
  meson-greet smoke fixture). Promote once a real consumer
  surfaces the need.
- **Promote the narrowing-audit CI gate from soft to blocking.**
  Soft launch shipped (see Done — `make e2e-audit-narrowing`
  exits non-zero on drift; the CI step uses
  `continue-on-error: true` to keep the build green). The
  remaining work is policy plus fixture coverage: once a
  representative set of meta-projects has stabilized
  expected-drift allowlists (`srckey-expected-drift.txt` per
  element), flipping `continue-on-error` to false on the CI
  step promotes the gate to blocking. The flip is a real
  one-line YAML change because the script's exit code
  already differentiates clean vs drift — nothing else
  needs to move. Until then, promotion is gated on
  accumulated signal: operators need to see real drift hit
  the gate without affecting their builds, decide which
  entries deserve the allowlist vs which deserve a pattern
  fix, and let the allowlist set converge. Trace-side
  coverage (the build-tracer + trace.log oracle for round-2
  trace-driven kinds) also needs a CI fixture:
  `--trace-source-root` is wired but no e2e job exercises
  it yet, so the gate today only covers the cmake oracle.
- **Repo-rule install for kind:cmake round-2 fallback.**
  Phase B's round-2 fallback (per
  `docs/design/rendezvous.md`)
  transports the install tree as `install_tree.tar` between
  project B and project A's `BUILD.bazel.out` AND extracts a
  subset of its contents via a per-element `_install_tree_extract`
  genrule, costing CAS roughly tar_bytes + Σ(per-target
  artifact bytes the cc_import / sh_binary stubs reference).
  Storage duplication adds up across a fleet. Alternative: a
  Bazel repository rule whose `repository_ctx.execute()` either
  runs cmake at loading time directly OR untars
  `install_tree.tar` into a per-element repo, exposing
  per-target labels without the extract genrule + CAS
  duplication. Precedent: `rules/traces.bzl`'s `_trace_repo`
  (loading-time AC lookup) — but that one only does AC
  `GetActionResult`, not a full build. Trade-offs: loading-time
  work blocks Bazel startup; repo rules don't run on RBE
  (executor-pool advantages forfeited); hermeticity weaker
  (relies on host-side cmake/ninja). A render-time
  measurement gate shipped
  (`scripts/meta-cmake-round2-fallback-storage-cost.sh`,
  `make e2e-meta-cmake-round2-fallback-storage-cost`) that
  reports the extract-genrule outs count for a small fixture —
  enough to confirm the duplication is per-stub-artifact (not a
  flat 2× on the whole tar; legacy `install(FILES ...)`
  entries stay in tar only). FDSDK-scale numbers from this
  gate would drive the promotion decision.
- **Toolchain-feature parity vs. cmake's default Release
  hardening flags.** Surfaced by the convert-and-build
  artifact comparison of zlib (cmake `libz.a` vs. Bazel
  `libzlibstatic.a` from the converted BUILD.bazel):
  exported-symbol sets are identical (105/105), but cmake's
  archive references `__snprintf_chk` / `__vsnprintf_chk` /
  `__stack_chk_fail` (FORTIFY_SOURCE + stack-protector)
  while Bazel's does not. cmake's distro defaults add
  `-D_FORTIFY_SOURCE=2 -fstack-protector-strong` to Release
  copts; Bazel's hermetic toolchain doesn't. The audit-time
  feature-lift (raw flags → `features = ["pic", ...]`) only
  fires when the cmake CMakeLists explicitly sets these
  flags — distro-default copts arrive via CFLAGS-env and
  the codemodel never records them. Closure path: either
  detect distro-default hardening via a probe at toolchain-
  derive time and emit `features = ["fortify_source",
  "stack_protector"]` on the cc_toolchain (requires a real
  Bazel cc_toolchain feature definition), or document the
  delta as expected and surface it via the verify pass.
  Same shape applies to other distro CFLAGS additions
  (`-fasynchronous-unwind-tables`, `-grecord-gcc-switches`).
- **Bake refinement: positional output-name args for
  `cmake -P` scripts.** Surfaced by re-surveying libpng:
  `add_custom_command(... cmake -P gensrc.cmake <output-name>
  ...)` is the canonical "one script, many outputs" shape.
  `--cmake-script-bake` currently passes the script path and
  `-D var=val` args verbatim but drops post-script positional
  args (the `<output-name>`), so cmake's argv[1+] indexing
  inside the script sees nothing and the bake fails with
  "Unsupported output:". Closure: pull positional args off
  the recovered command (between the `-P <script>` and the
  next cmake / shell metachar) and pass them to the
  convert-time cmake invocation as `cmake -P <script>
  <arg1> <arg2> ...`. Outputs would still bake one
  invocation per declared OUTPUT; if the script's argv-
  switched single invocation produces multiple outputs the
  bake harness already loops over `b.Outputs` and re-runs
  cmake per output. Each bake invocation needs its own
  positional-arg selection — the first OUTPUT's positional
  arg index would need to come from a `bake-args = [...]`
  binding on the build statement, OR (simpler) we pass
  ALL the build statement's recovered command tail and let
  the script's own `if(${ARGV0} STREQUAL "x")` logic
  dispatch.
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
  Two slices landed (see Done): per-gate cmake prereq honesty +
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

## Done (high points)

- **Operator-facing mode dials for write-a (`--fidelity` /
  `--bake-in` / `--diagnostics` / `--deployment`) with pass-
  through architecture into the converters.** Today's CLI exposes
  ~24 flags on `write-a` alone, with the "what should happen on
  per-element refusal" question spread across
  `--cmake-round2-fallback` / `--meson-round2-fallback` /
  `--pyproject-fallback` and converter-internal `--unsupported-*-
  fallback` switches; the "how should conversion shape its
  emission" question spread across `--lift-configure-file` /
  `--cmake-script-bake` / per-converter bake toggles; the "where
  should it run" question across `--trace-round1` + implicit
  presence checks. Four operator-facing dials replace the
  forest:

  - `--fidelity={strict|best-effort}` (default strict; refusals
    exit vs. lower to install_tree.tar placeholders).
  - `--bake-in={warn|allow|reject}` (default warn; convert-time-
    baked bytes surfaced / silent / rejected).
  - `--diagnostics` (default off; collect every Tier-1 refusal
    rather than aborting on the first).
  - `--deployment={auto|local|production}` (default auto: round-2
    REAPI-AC split if publish+lookup wired, else round-1).

  Pass-through architecture: write-a threads the operator's dial
  value VERBATIM into each converter's matching `--fidelity` /
  `--bake-in` / `--diagnostics` flag (lives on convert-element-
  cmake / -meson / -pyproject; shared enum vocabulary in
  `internal/convmode`). Each converter interprets per-kind:
  cmake → execute_process placeholder + bake-in inventory +
  rejection collector; meson → install-plan stubs + dial
  validation (rejection collector future-ready); pyproject →
  write-a's pipeline-shape dispatch (dial pass-through validation
  only). Per-kind escape hatches (`--cmake-round2-fallback`,
  `--unsupported-execute-process-fallback`, ...) stay as
  overrides that pre-empt the dial-derived default. `--deployment`
  is the one dial that stays write-a-local — it controls
  workspace-rendering (Project B install genrule emission,
  Project A converter-genrule fan-out for `--platforms-json`,
  round-1-vs-round-2 trace shape) rather than per-converter
  behavior, so it doesn't pass through.

  Converter-side dial implications (cmake): `--fidelity=strict`
  implies `--strict-trace=true` (offline replay flows that lack
  trace data must opt out via explicit `--strict-trace=false`);
  `--diagnostics=true` implies `--ignore-rejections-for-
  diagnostics=true`, `--probe-distro-hardening=true`, and
  `--verify=true`. Each implication respects an explicit
  per-flag override. Write-a's `--diagnostics` additionally
  threads `--rejections-report="$(location rejections.json)"`
  into the cmake-converter genrule and declares the per-element
  rejections.json output, so the structured rejection list
  actually lands on disk under elements/<name>/ rather than
  being silently collected and dropped.

  The startup banner prints the resolved dials, the wired vs.
  not-provided tools, the kind summary, and any downgrade notes
  (e.g. "auto picked local because publish/lookup weren't set")
  — operators see the run context at a glance instead of
  grepping rendered BUILDs. `cmd/write-a/modes.go` keeps the
  derivation testable apart from `flag.Parse()`. README quick-
  start and `tools/bst` switched to the dials; per-kind flags +
  all `meta-*` render gates continue to work unmodified.

- **Convert-time output bake for the cmake -P lift
  (`--cmake-script-bake`).** Opt-in convert-time execution
  that runs `cmake -P <script>` in a fresh tmp dir,
  captures the declared output bytes, and emits genrules
  that materialize them via base64-decode. Closes the
  script-hardcoded-absolute-paths gap (paths resolve at
  convert time where they exist) but at the cost of
  convert-time-baked outputs that don't auto-refresh on
  upstream input change — same trade-off + warning shape
  as the legacy configure_file capture. The
  `cmake-codegen-cmake-script-bake` tag funnels into the
  existing `warnConvertTimeBaking` post-pass.

- **Trace-based dep discovery for the cmake -P lift
  (`--cmake-script-trace`).** Opt-in convert-time execution
  under `cmake --trace --trace-format=json-v1 -P <script>`
  classifies every read path the script touches as
  source / build / sysroot / unknown. Source-class paths
  beyond the ninja edge's declared `DEPENDS` auto-augment
  the genrule's `srcs` (closes the "script reads
  `${SRCDIR}/scripts/options.awk` but add_custom_command
  didn't list it" subset). Unknown / unresolvable
  build-class paths refuse with a structured diagnostic
  naming the offending paths so operators see exactly what's
  blocking. Sysroot-class paths warn but proceed (operator's
  runner-image responsibility). Convert-time-coupling and
  side-effect caveats documented in
  `docs/design/conversion-architecture.md`'s "Convert-time
  platform coupling" section.

- **`cmake -P <script>` lift via operator-staged runner
  (`--cmake-script-runner`).** The dominant non-audit refusal
  in the cross-project survey (libpng ×4, VTK ×1) becomes a
  genrule that invokes `<runner> -P <script> [preserved -D
  args]` at Bazel build time. Off by default — only operators
  who stage a runner target (a Bazel `sh_binary` / `alias` /
  `cc_binary` that behaves like cmake) pass
  `--cmake-script-runner=<label>` to opt in. Soundness
  caveats: parameter-driven scripts (VTK's vtkHashSource
  shape, which takes inputs via `-D` args) work cleanly;
  configure_file-derived scripts with hardcoded absolute
  paths under the build dir fail because the paths don't
  exist in Bazel's sandbox. The lift refuses cleanly when
  the script path doesn't anchor under `cmakeSrc`, falling
  through to the existing UnsupportedCustomCommandScript
  refusal (preserves pre-lift behaviour for the
  configure_file-shape case). Same operator-plumbing pattern
  as `--cmake-configure-file-bin`.

- **`cmake -E create_symlink` op support.** Adds the
  `create_symlink` op to the cmake -E lift's allowlist
  (alongside `copy` / `copy_if_different` / `touch` /
  `configure_file`). LLVM's AddLLVM.cmake uses this for
  tool-aliases (`clang` → `clang-18`); under Bazel's
  hermetic action model the link-vs-copy distinction is
  meaningless (consumers read bytes by path), so the lift
  reuses `liftCMakeECopy` with the original op preserved
  in the genrule tag for audit/triage. Refusal message
  for the harder cmake-P-script residue now mentions all
  four operator escape hatches (rewrite, override via
  --build-files-dir, round-2 fallback, diagnostic flag).

- **Strip cross-target hdrs duplication
  (`stripDepOwnedHdrs`).** Real-world cmake projects
  surface every public header in every target's `hdrs`
  attribute (cmake's per-target include-dir walk includes
  shared `include/` roots). At LLVM / VTK scale this
  inflated per-target hdrs counts by ~11× — LLVM 1808 →
  571 hdrs/lib, VTK 577 → 59. New post-pass
  `liftRawFeatureFlags` walks each cc_library /
  cc_interface in the package and drops `hdrs` entries
  owned by a sibling that's already in the consumer's
  `deps` / `implementation_deps`. Bazel propagates hdrs
  through deps so the re-listing was pure noise. The
  conservative dep-aware guard preserves compilability
  for any latent consumer-without-declared-dep cases.
  PR #247 survey impact: LLVM BUILD 19M → 11M, VTK BUILD
  5.2M → 1.6M.

- **Lift per-target raw toolchain-feature flags into
  `features = [...]`.** Closes the single biggest non-idiom in
  real-world converter output. New post-pass
  `liftRawFeatureFlags` walks each ir.Target's Copts/LinkOpts
  and rewrites `-fPIC` → `features = ["pic"]`,
  `-fvisibility=hidden` → `["visibility_hidden"]`,
  `-fvisibility-inlines-hidden` → `["visibility_inlines_hidden"]`,
  `-flto` → `["lto"]`, `-fsanitize=<x>` → `["<x>"]`. The
  9-project cross-project survey on PR #247 dropped from
  ~785 `raw-toolchain-feature-flag` audit findings to **0**
  (LLVM 390 → 0, VTK 346 → 0, fmt 46 → 0, others 1–10 → 0).
  Per-target dedup + sort keeps the Features list byte-stable
  when multiple sources (probe-genex, codemodel LTO flag,
  raw copts) compose. Mapping shared with the audit via
  `converter/internal/toolchainfeature.Feature` so detection
  and rewrite stay in lockstep. Operator concomitant: the
  cc_toolchain must declare the matching feature names
  (template in
  `examples/sanitizer-features/toolchain/features.bzl`);
  Bazel ignores unknown features in user-supplied lists, so
  toolchains that haven't opted in still get a clean build.
  Surfaced + prioritized by the survey on PR #247.

- **bazelidiom audit catches raw `-fvisibility=hidden` /
  `-fvisibility-inlines-hidden` copts.** Extends the
  `raw-toolchain-feature-flag` finding kind to also fire on the
  two visibility-related flags the converter emits today from
  the `CMAKE_<LANG>_VISIBILITY_PRESET` /
  `VISIBILITY_INLINES_HIDDEN` lifts. Surfaced by running the
  converter against VTK 9.3.0's StandAlone module set, where
  every cc_library carried both flags as per-rule copts (213
  rules, the largest single audit-able gap in that output
  alongside 133 `-fPIC` rules — same pattern, different
  feature name). Bazel-idiomatic form: a cc_toolchain feature
  named `visibility_hidden` / `visibility_inlines_hidden`
  that consumers enable via `--features=`. Default-visibility
  (`-fvisibility=default`) is not flagged — it's the toolchain
  default anyway. Audit-only for now: lowering the converter
  emit shape from copts to `features = [...]` is a separate
  follow-on (matches the queued `-fPIC` migration).

- **`--ignore-rejections-for-diagnostics` flag for cmake converter.**
  Diagnostic-survey mode for running the converter against large
  real-world cmake projects (VTK, LLVM, etc.) without aborting on
  the first Tier-1 refusal. When set, every refusal site in
  `converter/internal/lower/` (UnsupportedTargetType, UnresolvedLinkDep,
  UnsupportedSourcePath, UnsupportedCustomCommand / -Script,
  FileAPIMalformed dangling-target-ref, UnsupportedExecuteProcess)
  appends to a `*rejection.Collector` and falls through with a local
  skip (drop the bad source / dep / target) instead of returning the
  typed `failure.Error`. The execute_process arm implicitly enables
  the pre-existing `--unsupported-execute-process-fallback` placeholder
  emit. Output BUILD.bazel is best-effort and not guaranteed to build;
  the goal is enumerating the refusal surface in one pass, not
  producing usable output. `--rejections-report=<path>` captures the
  structured rejections JSON (one `{code, message, target, source}`
  record per refusal). Off by default — strict-mode callers (the
  M3 orchestrator + every render gate) see no behaviour change. New
  package: `converter/internal/rejection/`.

- **Phase 4 trace cross-reference + execute_process unknown-arm
  retirement.** Closes the two residue items the standalone-
  genrule graduation left behind. `internal/shadow/trace_commands.go`
  gains `AddCustomCommandCall` / `AddCustomTargetCall` /
  `AddDependenciesCall` decoders on the same single-pass walker
  that PR #238's INTERFACE_* extractors travel; the decoded slices
  thread through to `lowerStandaloneCustomCommands` via a new
  `standaloneTraceContext`. When the trace records an
  `add_custom_target` wrapping an OUTPUT, the emitted genrule
  takes that target's name instead of the synthetic
  `custom_command_<sanitized-output>` shape; when
  `add_dependencies` references the wrapping target (or the
  OUTPUT directly), visibility opens from `//visibility:private`
  to `:__pkg__`. Offline-replay-no-trace path passes a
  zero-valued ctx → legacy naming + private visibility
  preserved (byte-stable for trace-less runs).
  `converter/internal/lower/execute_process_classify.go` retires
  the catch-all `Reason: "no recognized lift pattern"` string in
  favour of `unliftableShapeReason`, threading the call shape
  (OutputVariable / ResultsVariable / ErrorVariable / InputFile /
  opaque side-effect) into a per-shape diagnosis. `BucketUnknown`
  is renamed `BucketRefuse` (with `BucketUnknown` retained as an
  alias and the on-disk `unknown` string preserved for
  failure.json triage continuity). Phase 4's remaining queued
  work is now just the probe / stamp TOP_LEVEL_INCLUDES hook.

- **Platform-conditional source partitioning — Tier 2:
  recover skipped-branch sources by parsing CMakeLists.txt
  (#217 follow-on).** Closes the cross-platform half Tier 1
  left open: the other arms of an `if(CMAKE_SYSTEM_NAME ...)`
  block carry sources cmake skipped on this configure and
  therefore never recorded in the trace. A new
  `internal/cmakeparse` package — a deliberately tiny cmake-
  syntax parser scoped to command-invocation shape +
  `if`/`elseif`/`else`/`endif` structural delimiters — re-reads
  CMakeLists.txt at every recognized-predicate `if()` event
  the trace records, parses the skipped arm's body, and
  attributes `target_sources` / `add_library` /
  `add_executable` sources to the OTHER platform's
  `@platforms//os:*` constraint. `internal/shadow/
  platform_conditional_tier2.go` drives the integration:
  Tier 1's existing if-stack walker still owns the entered
  arm, Tier 2 owns the unentered arms, and `converter/
  internal/lower/lower.go`'s `addPlatformConditionalSrcs`
  injects Tier-2 records directly into
  `irt.PerPlatform["srcs"][selectKey]` (they're not in the
  codemodel's flat list by construction). Conservative shape:
  `${...}` and `$<...>` in source paths refuse cleanly with
  an `UnsupportedTier2Reason`; cross-file `include()` from
  inside a skipped arm isn't followed; function/macro bodies
  in skipped arms aren't expanded. Render gate:
  `scripts/meta-cmake-platform-partition-tier2.sh` drives the
  `platform-partition-tier2` sample project (a Linux configure
  of an `if(LINUX) ... elseif(WIN32) ...` fixture) and
  asserts both arms surface in the emitted BUILD.bazel —
  `linux.c` under `@platforms//os:linux` (Tier 1) AND `win.c`
  under `@platforms//os:windows` (Tier 2, never seen by
  cmake).

- **Phase 4 standalone-genrule emission graduated to default-on.**
  The opt-in `--emit-standalone-custom-commands` flag PR #227
  landed flipped to default-on after fixture + render-gate
  coverage landed. New fixture
  (`converter/testdata/sample-projects/standalone-custom-command`)
  exercises a `add_custom_command` whose output is consumed only
  by an `add_custom_target` — the recoverGenrule path skips
  because no cmake target lists the output in its sources, so the
  Phase 4 walker is the only path that emits a genrule for it.
  Render gate
  (`scripts/meta-cmake-standalone-custom-command.sh`) drives
  convert-element-cmake against the fixture with default flags
  and asserts (1) the standalone genrule surfaces with the
  expected `cmake-codegen-standalone-custom-command` tag, (2) the
  companion `cc_library` survives unchanged, (3)
  `--emit-standalone-custom-commands=false` still honours the
  opt-out path for operators with edge-case projects. The
  library-side default (`lower.Options.EmitStandaloneCustom
  Commands`) stays at the Go zero value (`false`) so in-process
  goldens / unit-level callers that construct `lower.Options{...}`
  literals keep their existing emission shape — the two-tier
  default (CLI on, library off) graduates the operator-facing
  surface without rebaking the unit-level fixture matrix. The
  trace-side cross-reference + `execute_process_classify.go`
  unknown-arm retirement land as the follow-on residue bullet
  above.

- **TARGET_PROPERTY INTERFACE_* convert-time aggregation.**
  `$<TARGET_PROPERTY:t,INTERFACE_INCLUDE_DIRECTORIES>` /
  `INTERFACE_COMPILE_DEFINITIONS` /
  `INTERFACE_COMPILE_OPTIONS` / `INTERFACE_LINK_LIBRARIES`
  now resolve at convert time without relying on the
  probe-genex hook running. `lower/buildGenexTargets`
  consumes the cmake trace's per-target
  `target_include_directories` / `target_compile_definitions` /
  `target_compile_options` / `target_link_libraries` calls
  (PUBLIC + INTERFACE arms — the propagating visibility set),
  then walks the dep chain transitively (trace
  target_link_libraries shapes first, codemodel
  Dependencies[] as fallback) to assemble each target's
  effective INTERFACE_* value with cmake's documented
  first-listed-first ordering and first-occurrence-wins
  dedup. Probe-genex hook values still override the
  convert-time aggregate when both are populated — cmake's
  own evaluator output remains the source of truth when
  available. Pinned by `TestEmit_InterfacePropertyAggregation_Resolves`
  against a base (INTERFACE) → mid (INTERFACE) → leaf
  (STATIC) chain fixture: leaf's transitively-aggregated
  INTERFACE_INCLUDE_DIRECTORIES resolves to base's include
  path through mid, and the file(GENERATE) genrule carries
  `cmake-codegen-file-generate-genex-evaluated` (not the
  legacy fallback tag). Two new shadow extractors —
  `ExtractTargetCompile` for `target_compile_definitions` /
  `target_compile_options` — round out the per-visibility
  trace decoders the aggregation pipeline consults.
  Out of scope: cross-package INTERFACE_* (needs a
  manifest-side INTERFACE export surface; queued as
  separate work) and INTERFACE_LINK_OPTIONS (no clean
  trace-side decoder; rides via the probe-genex hook only).
- **`$<TARGET_OBJECTS:t>` for OBJECT_LIBRARY targets.** The (a)
  Go-side evaluator now resolves `$<TARGET_OBJECTS:t>` against
  `Context.Targets[t].Objects` — populated at convert time from
  the probe-genex hook's per-target `objects.txt` emission
  (gated on `_CMTB_TYPE STREQUAL "OBJECT_LIBRARY"`, since
  OBJECT_LIBRARY targets are the only type cmake's
  `$<TARGET_OBJECTS:>` resolves non-trivially for). The lifter's
  `file_generate.go` extracts TARGET_OBJECTS references and
  emits one `--target-objects=<name>="$(echo $(locations :<name>) | tr ' ' ':')"`
  flag per referenced target alongside the existing
  `--target-file=` flags — the colon-delimited wire shape
  sidesteps shell quoting hazards around cmake's native `;`
  separator. `cmake-configure-file` parses the flag, rewrites
  colons back to semicolons (cmake's native list shape), and
  populates `Context.Targets[name].Objects` so the genex
  evaluator at Bazel time sees the executor's actual on-disk
  paths. The cross-package soundness gate
  (`unresolvedCrossPackageTargetFiles`) extends to TARGET_OBJECTS
  for the same reason it covers TARGET_FILE — an unresolved
  cross-package reference would otherwise embed the recording-
  machine absolute path. Probe-genex.cmake fixed in the same
  change: OBJECT_LIBRARY was previously in the TARGET_FILE
  emission gate, which cmake rejects ("Target … is not an
  executable or library"); it now only emits `objects.txt`,
  not `file.txt` / `file_dir.txt` / `file_name.txt`. New
  render gate `scripts/meta-cmake-probe-genex-object-library.sh`
  exercises the end-to-end flow against the existing
  `object-library` sample project.

- **Multi-version cmake compatibility shakeout.** The single
  `e2e-latest-cmake` job (cmake `4.0.3` only) expanded into a
  four-version matrix: `3.22.6` (Ubuntu 22.04 LTS default;
  the operator floor), `3.28.6` (Ubuntu 24.04 LTS default;
  LLVM 23's floor), `4.0.7` (the major-bump that dropped
  pre-3.5 compat), `4.3.3` (latest stable as of May 2026).
  Runs in parallel via `strategy.matrix.cmake_version`;
  `strategy.fail-fast: false` isolates entries so one red
  version doesn't cancel the others. Job-level
  `continue-on-error: true` keeps the matrix non-blocking
  per the shakeout's design goal — surface drift, don't gate
  PRs on it. The composite `install-cmake-toolchain` action
  takes `cmake_version` as input and pulls the matching
  Kitware-released tarball, so the matrix entry list is the
  only knob to bump for a new cmake. cmake 4.x entries set
  `CMAKE_POLICY_VERSION_MINIMUM=3.5` defensively (every
  in-tree fixture floor is already ≥ 3.20, but try_compile
  sub-projects + future fixtures might not be). The matrix's
  promotion-to-blocking criteria + per-version known-notes
  table live in `docs/cmake-version-matrix.md`; promotion
  itself is a one-line YAML flip on the job header
  (`continue-on-error: true` → `false`) once the criteria
  are met. The previously-listed Now bullet ("Per-object
  schema-major validation now lives in `fileapi/reply.go`
  and a non-blocking `e2e-latest-cmake` CI job...") retires
  in favour of this Done entry; the schema-major validation
  it referenced already shipped pre-matrix and stays as is.

- **probe-genex composes with Ninja Multi-Config.**
  `probe-genex.cmake` now emits per-config OUTPUT paths
  (`file.$<CONFIG>.txt`) so cmake's generation step stops
  erroring on "Evaluation file to be written multiple times with
  different content" when `BuildTypes` carries more than one
  config. The reader (`cmakerun.ReadGenexProbe`) collapses
  config-invariant values back to single fields and silently
  drops fields that diverge across configs (the routine case
  for `TARGET_FILE` / `TARGET_FILE_DIR` under multi-config,
  where each config lives in a per-config subdir of
  `CMAKE_BINARY_DIR`). The `--probe-genex=false` workaround
  PR #229's sanitizer-features render gate was carrying is
  dropped.

- **Platform-conditional source partitioning from a single-platform
  cmake trace (#217 Tier 1).** A new shadow extractor
  (`internal/shadow/platform_conditional.go`) walks the cmake
  `--trace-expand` stream maintaining a global if-stack scoped
  to in-tree files (cmake-internal `if()`s under
  `/usr/share/cmake-*` are filtered out so they don't pollute
  user-target attribution), and reports each `target_sources` /
  `add_library` / `add_executable` source attached inside a
  recognized platform-conditional. Recognized predicates:
  `if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")` (canonical three-arg)
  plus the single-identifier shorthands `WIN32` / `LINUX` /
  `APPLE` / `MSVC` / `MINGW` / `CYGWIN`, mapping to the
  matching `@platforms//os:*` constraint. `UNIX` / `BSD` /
  `NOT <X>` / `MATCHES` shapes stay unrecognized (no clean
  single-positive-constraint mapping). `converter/internal/
  lower/lower.go` consumes the records and moves matching
  sources from the flat `irt.Srcs` to
  `irt.PerPlatform["srcs"][@platforms//os:*]`, so the emitter
  renders a `select()` arm even on single-platform runs. The
  innermost-recognized-key policy means nested ifs collapse to
  the most-specific OS constraint that was open when the
  source was added; `else` arms fall through to flat srcs
  unchanged. Byte-stability preserved for projects without
  platform conditionals (TraceRaw nil → no partition; matching
  srcs missing → no partition).

- **FDSDK-glue placeholder handlers — kind catalog now
  fully covered.** Stub handlers for the four previously-
  missing FDSDK-specific kinds (`collect_initial_scripts`,
  `collect_integration`, `check_forbidden`, `flatpak_repo`),
  matching the shape of the pre-existing `collect_manifest`
  stub. Each emits an empty `install_tree.tar` so render of
  FDSDK fixtures reaches completion without these kinds
  breaking the graph; real plugin semantics deferred until
  an FDSDK fixture forces a bazel-build-time correctness
  need. Coverage table in `docs/fdsdk-coverage.md`
  refreshed: 100 % of FDSDK's element-kind catalog now has
  a handler (~76 % deep + ~22 % structural/placeholder).
  Unit tests for the four new kinds + the pre-existing
  `collect_manifest` shape locked into a table-driven
  `TestWriter_FDSDKGlueHandlers`.

- **Strict-sandbox `.bazelrc` rendered into every project.**
  `cmd/write-a` now renders a `.bazelrc` in both project A and
  project B carrying
  `--spawn_strategy=sandboxed --genrule_strategy=sandboxed
  --sandbox_default_allow_network=false
  --incompatible_strict_action_env`. The hermeticity contract is
  now explicit at the rendered-output layer instead of relying
  on bazel's per-OS default (which is `linux-sandbox` on Linux
  but `local` on macOS — a silent loss of isolation otherwise).
  Operator escape valve: the rendered `.bazelrc` ends with
  `try-import %workspace%/.bazelrc.operator`, so operators who
  need persistent additions put them in `.bazelrc.operator` (a
  file write-a never touches); bazel loads it after the prelude
  so operator entries override the strict defaults on conflicting
  flags. The four buildbarn-RE gate scripts
  (`tools/e2e-meta-*-re.sh`) switched their `.bazelrc` writes
  from `cat >` to `cat >>` so the RBE flags append on top of the
  write-a-rendered prelude; per-rule `--strategy=Genrule=remote`
  continues to take precedence over `--genrule_strategy=sandboxed`
  for the converter genrule when RBE is wired up. Render
  assertions in `scripts/meta-hello.sh` + a unit test in
  `cmd/write-a/main_test.go` guard the contract.

- **Drop the bwrap dead-code branch.** Investigation triggered by
  a side-note ask about strict Bazel sandboxes revealed that
  bwrap has **never** been invoked from any Go code in this
  repo's history (`git log --all -S 'exec.Command("bwrap"'`
  returns empty). The dependency was an aspirational
  placeholder: `mesonrun.go`'s package doc said hermeticity
  comes from "a Bazel genrule sandbox or a bwrap envelope from
  the orchestrator," and the orchestrator (now absorbed away
  in step 7b) was the placeholder for the bwrap-using path that
  never materialized. The current converter at
  `cmakerun.Configure` invokes `cmake` directly via
  `exec.CommandContext` with controlled env (empty `HOME`,
  fixed locale, `SOURCE_DATE_EPOCH`) and relies on Bazel's
  per-action sandbox at the genrule layer for hermeticity.
  Drop the dead bwrap references: install from the runner
  image (`deploy/buildbarn/runner/Dockerfile`) and the CI
  install path; the `bwrap-version` constraint from the worker
  advertisement (`deploy/buildbarn/config/worker.jsonnet`) and
  every rendered platform that mirrored it (the four
  `tools/e2e-meta-*-re.sh` REAPI gate scripts); the prereq
  check from `check-cmake-toolchain` and the inline checks PR
  #185 added to the kind:cmake meta-* scripts; the stale
  comments claiming `cmakerun.Configure runs cmake under
  bwrap`; the `BWRAP_VERSION` Makefile var. Net effect: one
  fewer host-toolchain dep for every contributor + the runner
  image; the CI `chmod u+s "$(command -v bwrap)"` workaround
  for Ubuntu 24.04's restricted unprivileged-userns kernel
  becomes unnecessary and goes away too.

- **kind:cmake gates self-skip the bazel-build half on missing
  toolchain.** Following from the per-gate prereq honesty pass
  below, the remaining kind:cmake meta-* gates (`meta-hello`,
  `meta-stack`, `meta-cross-cmake`, `meta-compose`, `meta-filter`,
  `meta-regression`) now inline a cmake/ninja/bwrap availability
  check right after their existing bazel-availability gate, and
  drop the Makefile-level `check-cmake-toolchain` prereq. The
  render half always runs (the contract write-a owes its
  consumers); the bazel-build half self-skips cleanly with the
  same `render OK; <tool> not on PATH, skipping build phase`
  message the bazel-missing path uses, when any of cmake/ninja/
  bwrap is missing. Two render-only kind:cmake gates
  (`meta-cross-kind`, `meta-cmake-round2-fallback-multiplatform`)
  never had a bazel-build half and drop the prereq outright
  without a script edit. Net effect: a contributor with only
  Go installed can run **every** render gate locally, including
  kind:cmake gates' render half. Only the converter's own
  `-tags=e2e` Go tests, `e2e-audit-narrowing`,
  `e2e-meta-cmake-round2-fallback-storage-cost`, and
  `record-fixtures` still pin cmake — the targets that exec
  `cmakerun.Configure` or `convert-element-cmake` directly
  before any bazel-availability check could gate them.

- **Per-gate cmake/ninja prereq honesty.** The Makefile's
  monolithic `check-tools` target (cmake + ninja on PATH) was
  declared as a prerequisite of every `e2e-meta-*` gate, even
  ones whose fixtures don't exercise any `kind:cmake` element
  and never shell to cmake/ninja from either the script or the
  converter. Renamed to `check-cmake-toolchain` and re-routed:
  the ~18 cmake-needing gates (`e2e-meta-hello`, `e2e-meta-stack`,
  `e2e-meta-cross-cmake`, `e2e-meta-cmake-round2-fallback-*`,
  `e2e-meta-compose`, `e2e-meta-filter`, `e2e-meta-cross-kind`,
  `e2e-meta-regression`, `e2e-audit-narrowing`, the converter
  e2e tests, `record-fixtures`) keep the dep; the ~32 non-cmake
  gates (`e2e-meta-{bazel-passthrough, bazel-override, script,
  vars, manual, conditional, import, finalize-b, unify-toolchains,
  render-project-a, gazelle-roundtrip, pyproject*, meson*,
  autotools*, make*, trace-round2-fold, converge}`) drop it
  entirely. Net dev-loop win: a contributor with only Go
  installed can now run ~60% of the render gates locally without
  apt-installing cmake/ninja. Closing the rest of the gap
  ("dev only needs Bazel + bb_clientd") is queued under
  Later — see "Push the converter's cmake invocation onto the
  RBE executor".

- **Cross-package `$<TARGET_FILE*:t>` resolved lift (PR 2).**
  The PR 1 refusal stub catches the unresolvable case; PR 2
  closes the resolvable half. The file(GENERATE) lifter now
  uses the threaded `*manifest.Resolver` to translate
  `$<TARGET_FILE:Foo::bar>` references whose target isn't in
  the local cmake codemodel but IS in the imports.json
  manifest. The lifted (a)-shape genrule's cmd branches per-
  target on resolution: same-package targets emit
  `--target-file=<name>="$(location :<name>)"` (PR 1
  behaviour preserved); manifest-resolved targets emit the
  full cross-package label
  `--target-file=<name>="$(location //elements/foo:bar)"`
  and the genrule's `srcs` picks up the cross-package label
  so Bazel's `$(location)` substitution resolves at action
  time. `buildGenexTargets` now folds the imports manifest's
  exports into the evaluator's `Context.Targets` (keyed by
  the namespaced cmake name like `Foo::bar`) with
  `FileLocation` seeded from the export's first `LinkPaths`
  entry — cmake's `$<TARGET_FILE>` at recording time
  resolves to `IMPORTED_LOCATION_<CONFIG>`, which the
  orchestrator-side `internal/exports` package captures into
  LinkPaths. The byte-equal check at convert time now passes
  for manifest-resolved imports; the marshaled wire still
  omits `FileLocation` (json:"-") so the lifted cmd stays
  byte-stable across recording machines. Render gate:
  `scripts/meta-cmake-cross-package-target-file.sh` exercises
  two BuildStream elements end-to-end (producer + consumer
  with `file(GENERATE)` against `$<TARGET_FILE:producer::producer>`).
  The refusal-stub from PR 1 still fires when the manifest
  doesn't carry the target — the resolved-lift path
  supersedes only the resolvable case.

- **Cross-package `$<TARGET_FILE*:t>` soundness gate (PR 1).**
  The file(GENERATE) lifter previously refused unresolvable
  `$<TARGET_FILE*:t>` references via the (a) shape and fell
  through to (b)/legacy, both of which embed cmake's rendered
  bytes — the RECORDING MACHINE's absolute path. Shipping
  those paths into Bazel produced a genrule that builds
  against a path that doesn't exist on the executor. Now the
  lifter walks the template body for any of the seven
  TARGET_FILE-family op prefixes (FILE, FILE_DIR, FILE_NAME,
  LINKER_FILE, LINKER_FILE_DIR, LINKER_FILE_NAME, SONAME_FILE);
  any name that resolves to neither the local cmake codemodel
  NOR the threaded `*manifest.Resolver` (imports.json) emits
  a refusal-stub genrule whose cmd fails the bazel build with
  a clear diagnostic pointing at
  `ROADMAP.md`. Audit tag:
  `cmake-codegen-file-generate-genex-cross-package`. The
  `*manifest.Resolver` plumbing this set up is the foundation
  PR 2 (resolved cross-package lifts) builds on.

- **On-disk-path genex variants — TARGET_FILE_DIR /
  TARGET_FILE_NAME / TARGET_LINKER_FILE / TARGET_LINKER_FILE_DIR
  / TARGET_LINKER_FILE_NAME / TARGET_SONAME_FILE.** The (a)
  evaluator now supports the six FileLocation-derived ops
  alongside TARGET_FILE. All seven share the lifter's existing
  `--target-file=<name>=$(location :name)` wire; the evaluator
  computes the per-op derivation (Dir / Base / identity) at
  Bazel time against the same FileLocation. Linux v1 aliases
  LINKER_FILE / SONAME_FILE to TARGET_FILE (no Windows import-
  library / Mach-O distinction); the convert-time byte-equal
  check catches any cross-platform disagreement and routes to
  (b)/legacy. Lifter's `extractTargetFileRefs` now scans for
  all seven prefixes; a template referencing the same target
  via multiple op forms collapses to one wire flag (deduped
  by name, not by op). TARGET_OBJECTS remains UnsupportedError
  pending a list-valued wire (queued under Later).

- **`$<TARGET_FILE:t>` for same-package targets.** The (a)
  evaluator handles `$<TARGET_FILE:t>` for any target in the
  current Bazel package's codemodel. Architecture: the lifter
  populates `Context.Targets[t].FileLocation` with cmake's
  recorded artifact path (build-dir-relative path joined with
  recordedBuildDir) for the convert-time byte-equal check;
  the marshaled wire struct OMITS FileLocation so the lifted
  cmd stays srckey-stable across recording machines. At
  Bazel time the new `cmake-configure-file` `--target-file=<name>=<path>`
  repeatable flag carries the real value (`$(location :t)`
  expanded by Bazel at action time) and overrides the
  Context's FileLocation. Lifter scans the template for
  `$<TARGET_FILE:name>` and emits one `--target-file` flag
  per unique referenced name in sorted order. Cross-package
  TARGET_FILE references and the related on-disk genexes
  (TARGET_FILE_DIR / TARGET_LINKER_FILE / etc.) remain
  UnsupportedError and are queued under Later.

- **TARGET_PROPERTY for cmake-direct properties.** The (a)
  evaluator now supports `$<TARGET_PROPERTY:t,p>` for the
  subset of properties cmake reports verbatim from the fileapi
  codemodel: NAME / TYPE / SOURCES / IMPORTED. Implementation:
  new `genexeval.Context.Targets` field +
  `genexeval.TargetInfo` struct; lifter-side
  `buildGenexTargets(r)` projects fileapi targets into the
  Context (cmake-internal helper targets are skipped). The
  marshaled Context payload prunes the Targets dict for
  templates that don't reference `$<TARGET_PROPERTY:` —
  payload stays small for the common case. INTERFACE_*
  aggregation now lives in its own Done entry above (initially
  refused as UnsupportedError until the lifter's convert-time
  walker shipped).

- **file-generate fixture exercises (a) end-to-end.** The
  existing file-generate sample-project's `gen_config_tag_h`
  previously fell back to (b) at render-gate time because the
  fileapi reply didn't carry `cmake-to-bazel.vars.dump` — so
  the offline path's `cmakeVars` stayed nil and the (a)
  evaluator's Context was empty. Two-part fix: (1) new
  `cmakerun.ReadVarsDumpFromReplyDir` exported helper, wired
  into `convert-element-cmake`'s offline branch so any
  fixture or pre-recorded reply with a vars-dump
  opportunistically populates `cmakeVars`; (2) a minimal
  vars-dump committed under `converter/testdata/fileapi/
  file-generate/` carrying `CMAKE_BUILD_TYPE=Release`,
  `CMAKE_SYSTEM_NAME=Linux`, `CMAKE_C_COMPILER_ID=GNU`,
  `CMAKE_CXX_COMPILER_ID=GNU`. Result: `gen_config_tag_h` now
  lifts via the (a) shape end-to-end through the render gate,
  with the `cmake-codegen-file-generate-genex-evaluated` tag
  asserted by `scripts/meta-file-generate.sh`. The
  `TestEmit_FileGenerate_Golden` golden updates to match.

- **OUTPUT-side and INPUT-arg genex resolution at convert
  time.** The (a) evaluator (below) now also resolves `$<...>`
  in the file(GENERATE) `OUTPUT` and `INPUT` paths at convert
  time. Pre-fix the early-gate on each side dropped any call
  with a genex in the path entirely (the trace records the
  literal string and the lifter couldn't anchor against the
  resolved filename); the evaluator picks up the same
  `Context` the body lift consults, resolves the path, and
  the call continues down the normal lift pipeline — OUTPUT
  becomes the genrule's `outs`, INPUT becomes the on-disk
  template path (and the genrule's `srcs`). Refusal modes
  (`UnsupportedError` from target-dependent ops, empty
  Context) drop the call (OUTPUT) or fall back to legacy
  (INPUT) the same way the pre-evaluator gates did —
  soundness preserved.

- **file(GENERATE) genex evaluator via genexeval — (a) shape.**
  New `internal/genexeval` package: Go-side parser + evaluator
  for the configure-time-resolvable cmake `$<...>` subset
  (`$<CONFIG[:cfg,...]>`, `$<COMPILER_ID[:id,...]>`,
  `$<PLATFORM_ID[:id,...]>`, `$<COMPILER_LANGUAGE:lang,...>`,
  boolean combinators `$<AND:...>` / `$<OR:...>` / `$<NOT:b>` /
  `$<IF:cond,then,else>` / `$<BOOL:str>`, string ops
  `$<UPPER_CASE:>` / `$<LOWER_CASE:>` / `$<STREQUAL:s1,s2>`,
  conditional emit `$<0:str>` / `$<1:str>`). Target-evaluator-
  dependent ops (`$<TARGET_FILE:>`, `$<TARGET_OBJECTS:>`,
  `$<TARGET_PROPERTY:>`, `$<INSTALL_INTERFACE:>`, ...) surface
  as `UnsupportedError` so the lifter can pattern-match and
  fall back. Strict boolean interpretation (only `"0"` / `"1"`)
  avoids silent divergence with cmake's looser truthy set.
  The lifter now tries (a) first via
  `buildGenexContext(cmakeVars)` projecting CMAKE_BUILD_TYPE /
  CMAKE_SYSTEM_NAME / CMAKE_<LANG>_COMPILER_ID into
  `genexeval.Context`; on success ships the Context as a
  base64 sidecar in the genrule and tags the result
  `cmake-codegen-file-generate-genex-evaluated`. On
  `UnsupportedError` or byte-mismatch, falls through to (b)
  capture, then legacy. cmake-configure-file gains a
  `--genex-context=<path>` flag (mutex with `--genex-values`).
  The (a) lift handles template edits that add NEW genex
  literals against the same Context — they get evaluated at
  Bazel time without re-running convert-element-cmake. Unit
  tests in `converter/internal/lower/file_generate_test.go`
  cover the (a) success path, the (a)→(b) fallthrough on
  unsupported ops, and the (a) refusal when cmakeVars are
  empty.

- **gazelle_cc `# gazelle:cc_search` path-frame fix.** Phase 7d's
  cc_search emission was wrong on both axes the acceptance
  criterion called out. gazelle_cc's parser
  (`language/cc/config.go` in `EngFlow/gazelle_cc`) takes
  `<strip_include_prefix> <include_prefix>` — two arguments, both
  repo-root relative — and warns + skips directives with the
  wrong arity; we were emitting a single package-relative arg
  (`# gazelle:cc_search include`), which gazelle_cc interprets
  as "strip leading `include/` and look at the workspace root."
  `bazel.Options` gained `BazelPackagePath`; the converter
  (convert-element-cmake / -meson / -trace and fold-element)
  takes a matching `--bazel-package-path` flag that write-a's
  per-element genrule templates fill with `elements/<name>`. The
  emitter then writes the correct two-arg form
  `# gazelle:cc_search "" <pkgpath>/<include>` per include dir.
  Unit tests without a `BazelPackagePath` get no directive
  (zero-value Options preserves byte-stability and avoids wrong
  bytes that would silently mis-route gazelle_cc's resolver);
  the 16 affected goldens dropped their stale single-arg lines.
  Render gate `scripts/meta-gazelle-roundtrip.sh` updated to
  assert the new shape; `ROADMAP.md`
  documents the frame distinction.

- **file(GENERATE) genex lift via structured base64 (the (b)
  shape).** Phase 7d's file(GENERATE) lifter previously short-
  circuited every `$<...>`-bearing template to the legacy
  bytes-embedded shape — rendered output content-load-bearing
  in srckey, audit-tagged `cmake-codegen-file-generate-genex`.
  The (b) lift captures each top-level genex's resolved bytes
  at convert-element-cmake time by aligning the template's
  static chunks against cmake's rendered output, ships the
  literal-to-value map as a base64 sidecar JSON in the genrule's
  cmd, and replays the substitution at Bazel time via
  cmake-configure-file's new `--genex-values=<path>` flag. The
  audit signal splits cleanly: `cmake-codegen-file-generate-
  genex-lifted` (with `cmake-codegen-lifted`) means "(b) lift
  succeeded — rendered bytes no longer in srckey;"
  `cmake-codegen-file-generate-genex` alone keeps its existing
  meaning, "lift failed, legacy shape in play." Extractor
  failure modes (static chunks don't align, adjacent genexes
  with no separator, same literal resolving to different values)
  all fall back to legacy with the original tag, preserving
  soundness. Render gate: `scripts/meta-file-generate.sh`.
  Unit tests: `converter/internal/lower/genex_extract_test.go`
  + `file_generate_test.go`. Tool change:
  `cmd/cmake-configure-file/main.go` gains `--genex-values=`
  alongside the existing `--values=` (separate JSON files
  — `--values` drives @VAR@/${VAR}/#cmakedefine substitution,
  `--genex-values` drives literal `$<...>` → resolved-bytes
  replacement). Out of scope: OUTPUT-side genex (no anchor),
  same-literal-different-value cases, and template edits that
  introduce new genex literals — the queued (a) Go-side
  evaluator addresses those (Later).

- **`--build-files-dir` per-element BUILD overrides.** Operators
  can drop a directory of per-element override subtrees next to
  the meta-project and pass `--build-files-dir <dir>` to write-a;
  for every element with a matching `<dir>/<name>/BUILD.bazel`
  (or `BUILD`), write-a re-stamps the element to kind:bazel and
  copies the entire `<dir>/<name>/` subtree on top of project B's
  `elements/<name>/`. The subtree shape — rather than a flat
  `<name>.BUILD.bazel` file — lets one element ship multiple
  BUILDs (subpackages), `.bzl` helpers, defs files, or any other
  files the operator needs alongside. Source resolution still
  runs under the element's declared kind so kind:local sources
  stage underneath the override and its `srcs = [...]` references
  resolve; the override files shadow source files on collision.
  Escape hatch for elements whose declared kind (kind:cmake /
  kind:autotools / kind:manual / ...) doesn't yet convert cleanly
  — bypass the converter without forking the .bst. Render gate:
  `scripts/meta-bazel-override.sh`.

- **Docs consolidation (33 → 14 files).** Top-level
  `docs/architecture.md` is the single architecture story (prose +
  diagrams, absorbs the old overview / visual-guide / three-pass-
  flow / build-structure / trace-driven-autotools). `docs/codebase-
  map.md` gives the developer-facing repo tour. `docs/fdsdk-
  coverage.md` consolidates per-kind coverage. `docs/known-
  deltas.md` merges the conversion + fidelity delta catalogs.
  `docs/design/` slimmed to the load-bearing mechanism specs:
  `conversion-architecture.md` (end-state + rule patterns) +
  `conversion-architecture-slides.md` (Marp companion) +
  `rendezvous.md` (merged autotools-round2 + cross-element-config
  keyspaces) + `convergence-driver.md` + `finalize-b.md` +
  `narrowing-audit.md` + `sources.md` (merged sources-design +
  bazel9-cas-fs). Implementation-plan docs (generator-parity
  uplift/gaps, meson/pyproject native-render, sanitizer-as-feature,
  cmake/meson Phase B fallbacks, operator-gazelle-step, build-
  output-conventions, cross-package-target-file, orchestrator-
  absorption, fdsdk-element-survey) deleted — concepts live in
  comments + ROADMAP entries, status lives here.

- **Cross-element configure-step bootstrap.** Six-PR
  architectural shift from a load-time `_trace_repo` repository
  rule + per-project rendered rules to an action-time
  `trace_load` rule + `trace_build` genrule pair, with the rule
  implementations extracted into an in-repo
  `rules_buildstream_bazel/` Bazel module referenced via
  `bazel_dep + local_path_override`. Pass-3 install genrules
  synthesize a cmake-config bundle from the install tree and
  publish it alongside the trace to a separate AC keyspace
  partition (`SyntheticConfigDigest`, distinct argv0 from
  `SyntheticActionDigest`); pass-2 converter genrules consume
  both via the same trace_load action. `cmakeDepBundleLabels`
  retires its `kind == "cmake"` filter, so a kind:cmake
  element with `find_package(Dep CONFIG)` against a
  kind:autotools dep now resolves at pass-2 configure time
  instead of silently failing. `tools/converge.sh` implements
  the fixpoint driver — each round builds project A's
  trace_loads with a bumped `CONVERGE_GENERATION` (forces AC
  re-query), stages outputs into B, identifies the
  miss-marker frontier, builds the matching trace_build
  targets, retries. Termination guaranteed by the `.bst` DAG
  bound; offline mode (no `CAS_GRPC_ADDR`) terminates by
  `--max-rounds`. `cmd/finalize-b` is the deliverable-handover
  step: takes a converged project B and writes a stripped
  standalone Bazel project — converged elements' trace_load /
  trace_build / intermediate filegroups pruned, the
  `rules_buildstream_bazel` `bazel_dep` removed when no
  surviving target references it, idempotent and non-
  destructive. Design docs:
  `docs/design/rendezvous.md`,
  `docs/design/convergence-driver.md`,
  `docs/design/finalize-b.md`. The kind:meson-side bundle
  staging for consumers of trace-driven deps is queued as a
  small follow-up that lands when an FDSDK fixture forces it. Bazel-build-half end-to-end
  (driver against a live REAPI endpoint with bb_clientd) is
  covered by `tools/e2e-meta-autotools-round2-live.sh` once
  it grows convergence-driver wiring; render-half gates
  ship today (`meta-autotools-round2.sh`,
  `meta-make-round2.sh`,
  `meta-cmake-round2-fallback.sh`,
  `meta-meson-round2-fallback.sh`,
  `meta-trace-round2-fold.sh`,
  `meta-converge.sh`, `meta-finalize-b.sh`).

- **Folded `orchestrator/` into the write-a + Bazel path.** The
  repo had two multi-element drivers: the original
  `orchestrator/cmd/orchestrate` (one-pass — it *was* the
  scheduler, owned a REAPI/CAS/AC layer, fanned out to a remote
  Buildbarn cluster) and the write-a + Bazel two-pass shape
  (write-a renders, Bazel schedules). Only the write-a path can
  express the trace-driven 3 → 2′ loop non-cmake kinds need, so
  the orchestrator was absorbed into it and deleted. Shipped as a
  PR sequence:
  - **`tools/bst` → `--bst-root`** — write-a does leaf-rooted
    `.bst` discovery through the render's own parser; the shell
    awk graph-walker is gone.
  - **Parser consolidation** — the write-a parser rejects
    junction-crossing deps with a clear diagnostic, matching the
    rigor of the orchestrator's `element` package.
  - **Re-homed the libraries + tools** — `internal/element`,
    `regression`, `sourcecheckout`, `exports`, `allowlistreg`,
    `bsttranslate` moved under `internal/`; `bst-translate`,
    `orchestrate-diff`, `orchestrate-history` under `cmd/`; dead
    `internal/translate` deleted.
  - **RE/bwotb CI gate** — `make e2e-meta-buildbarn-re` drives a
    rendered project A's converter genrule against the real
    `deploy/buildbarn/` stack via Bazel-native `--remote_executor`,
    asserting it executes on a worker build-without-the-bytes —
    the production-path replacement for the orchestrator's
    Go-harness `e2e-buildbarn*` coverage. `BST_RE_GATE_REQUIRE`
    makes a green CI run mean the gate actually ran.
  - **Re-homed the converter-behaviour e2e gates** — `e2e-fidelity`
    / `-fmt`, `e2e-cmake-consumer`, `e2e-toolchain-skip` became
    converter e2e tests under `converter/cmd/convert-element-cmake/`;
    `e2e-bazel-build`'s coverage moved into `scripts/meta-cross-cmake.sh`
    (a project-B build phase). None had genuinely needed the
    orchestrator — it was just their test driver.
  - **Deleted the scheduler** — `orchestrator/cmd/orchestrate` +
    `orchestrator/internal/orchestrator` + the orchestrator-specific
    gates + `orchestrator/testdata/` are gone; the `orchestrator/`
    tree no longer exists.
  - **Follow-ups closed** — `cmd/run-manifest` snapshots a built
    project A into the run-manifest schema `internal/regression`
    consumes, so the run-vs-run regression e2e re-homes onto the
    write-a path (`scripts/meta-regression.sh`,
    `make e2e-meta-regression`: no-drift invariant + drift
    detection; newly-failed detection is out — the write-a path
    hard-fails rather than soft-recording Tier-1s). And
    `internal/reapi` was deleted outright — the whole package, not
    just `reapi.Executor`, was orchestrator REAPI-submission
    machinery with no other consumer (`trace-publish` /
    `trace-lookup` use `internal/cas` + `internal/tracenorm`).
- **Per-platform `exec_properties` routing for write-a + Bazel.**
  `write-a`'s `--platforms-json` `reapi_properties` field is no
  longer ignored. Each platform's `reapi_properties` — the REAPI
  Platform.properties wire shape, a list of `{name, value}` pairs —
  maps one-to-one onto a Bazel `exec_properties` dict, and write-a
  emits a `platform()` per declared platform into project A's
  `//platforms` package carrying `constraint_values` +
  `exec_properties`. The per-element converter genrules already
  carry `exec_compatible_with = <constraints>`; an operator who
  registers these platforms via `--extra_execution_platforms` gets
  each genrule routed to the matching Buildbarn worker pool, the
  action inheriting that platform's `exec_properties`. A repeated
  or empty `reapi_properties` name is rejected at load time (REAPI
  tolerates repeated names; `exec_properties` is a map). This was
  the live remainder of the deleted orchestrator's hardcoded
  `defaultPlatform` / `Action.Platform` fallback. Render
  gates: the three multi-platform gates
  (`scripts/meta-trace-round2-fold.sh`,
  `scripts/meta-autotools-round2-multiplatform.sh`,
  `scripts/meta-cmake-round2-fallback-multiplatform.sh`) assert
  the emitted `//platforms/BUILD.bazel` shape.
- **`bst` wrapper.** `tools/bst` is a POSIX-sh BuildStream-style
  CLI wrapper around write-a so `bst build <element.bst>` keeps
  working against a converted project. Supports
  `bst build / show / workspace open|close|reset`. The `build`
  subcommand hands the leaf .bst to write-a's `--bst-root` flag,
  which walks the `depends:` / `build-depends:` /
  `runtime-depends:` graph on disk via the same `loadElement`
  parser the render uses — so the wrapper no longer reimplements
  .bst graph walking in shell. It then runs write-a in the
  round-1 trace-driven shape (no REAPI AC / bb_clientd needed
  for local dev) and shells out to `bazel build` against the
  rendered project B. Bazel isn't required at render time —
  when it's absent the wrapper prints the target line and stops
  cleanly. `workspace open` copies the element's kind:local
  sources to a scratch dir and rewrites the .bst's
  `sources: - path:` so subsequent `bst build` picks up edits;
  `workspace close` restores from a deterministic
  `.bst-bazel-orig` backup. Render gate:
  `scripts/meta-bst-wrapper.sh` (`make e2e-meta-bst-wrapper`)
  covers kind:cmake + kind:autotools + multi-element graph +
  workspace round-trip. The wrapper is BuildStream-developer
  muscle-memory glue for the transition window; teams that
  prefer the Bazel CLI directly can ignore it.
- **Rename `convert-element` → `convert-element-cmake`** for
  consistency with the rest of the per-kind converter family
  (`convert-element-meson`, `convert-element-trace`,
  `convert-element-pyproject`). The bare name dated back to
  when cmake was the only converter and predated the per-kind
  suffix convention. Touched the binary path
  (`build/bin/convert-element` → `build/bin/convert-element-cmake`),
  the Go package directory, every call site in `cmd/write-a/`
  + orchestrator + `internal/reapi/`, the `--convert-element`
  CLI flag (now `--convert-element-cmake`), the Makefile, all
  render-gate scripts, the CI workflow, and the converter's
  own attribution header (`# Generated by convert-element` →
  `# Generated by convert-element-cmake`) along with the
  affected goldens. Pure mechanical rewrite, no semantic
  changes.
- **Rename Go module path `cmake-to-bazel` →
  `buildstream-bazel`** so the module + repo name agree (PR
  #129). `github.com/sstriker/cmake-to-bazel` predated the
  project's framing as a BuildStream-side conversion tool
  (cmake was just the first translator we built; the project
  is broader now — autotools/meson/pyproject all live here
  too). The repo on GitHub is `sstriker/buildstream-bazel`;
  only the Go module path still carried the old name. Pure
  mechanical sed against go.mod + every `import` statement
  (149 files); operator-visible state (cache paths,
  `.vars.dump` filename, AC-keyspace protocol IDs in
  `internal/tracenorm/synthkey.go`, docker image name, and
  byte-stable testdata fixture paths) deliberately preserved.
- **Cross-element index-file population from the imports manifest.**
  `build-cc-index` gained `--imports-manifest`: alongside the BUILD
  walk that already lands sibling-element headers / module names in
  `cc_index.json` / `python_modules.json`, it now folds the imports
  manifest's per-export `exported_headers` / `import_modules`
  entries — the external-repo cross-element edge, where a
  genuinely-external dep's header / module universe lives outside
  project B and only the manifest knows the resolving Bazel label.
  The fold runs after the walk with first-write-wins, so in-project
  entries always beat the manifest (it gap-fills the external edge
  only). The `manifest.Export` schema gained `exported_headers` /
  `import_modules` (append-only, `omitempty`) — the resolver-shaped
  keys gazelle indexes, distinct from the existing
  `interface_includes` (include *directories*) and `link_libraries`
  (flag fragments / distribution names); `Resolver.AllExports`
  enumerates them deterministically. Render gate:
  `scripts/meta-gazelle-roundtrip.sh` exercises the fold in its
  bazel-build half; the `build-cc-index` + `manifest` unit tests
  cover the bazel-free path.
- **Normalize emitted BUILD shape to Bazel/Gazelle conventions
  for post-conversion roundtrip.** Project B now looks like what
  a human using `EngFlow/gazelle_cc` + `rules_python/gazelle`
  would have written: `buildifier --mode=fix` is a no-op and
  `gazelle fix` preserves our emit. Architectural recipe:
  `ROADMAP.md`. Shipped across the
  PR #119–#130 stack:
  - **Phase 1** — internal renderer consistency: unified
    visibility under `package(default_visibility = ...)`, folded
    trace's inline `renderRules` into `bazel.Emit(toIR(...))`,
    sorted/trimmed `load()` lines, dropped dead load entries.
  - **Phase 2** — attribute completeness: `include_prefix` /
    `strip_include_prefix` plumbed through IR; `py_test` for
    test-pattern files; `pyi_srcs` discovery; `conftest.py`
    lifted into `py_library(testonly = True)`.
  - **Phase 3** — buildtools-AST migration: the three renderers
    (`text/template`, `fmt.Fprintf`, write-a format-strings) now
    route through a single `bazel.build/buildtools/build` AST
    primitive; `buildifier --mode=fix` no-op contract.
  - **Phase 4** — `implementation_deps` split: CMake
    `target_link_libraries(... PUBLIC|PRIVATE ...)` scope plumbed
    through to IR; PRIVATE → `implementation_deps` (cc_library
    only); meson + trace map everything to `deps` (documented
    lossy translation).
  - **Phase 5** — entry-shim strict mode + `__main__.py`
    detection: `[project.scripts]` entries with a self-invoke
    block emit `py_binary(main=...)` directly; `<pkg>/__main__.py`
    emits `py_binary(name="<pkg>_bin", ...)`;
    `--always-emit-entry-shim` for back-compat.
  - **Phase 6** — the conventions doc itself.
  - **Phases 7a–7d** — gazelle roundtrip: `# keep` markers on
    load-bearing attributes; `cc_index.json` /
    `python_modules.json` resolver files + MODULE.bazel
    directives; `scripts/meta-gazelle-roundtrip.sh` conformance
    gate; `# gazelle:cc_search` file-head directives mirroring
    each package's `includes`. (`# gazelle:resolve` is an
    operator escape hatch, not converter output; external-repo
    cross-element index population is the one remaining sliver
    — see `Next`.)
  - **Phase 8** — operator-owned `overlay.MODULE.bazel` seam +
    `ROADMAP.md` workflow; `cmd/relax-keeps`
    + `tools/gazelle-rewritable.json` for continuous-conversion
    auto-rewrite; `cmd/build-cc-index`.
  - **Phase 8b** — the write-a + Bazel driver's opt-in gazelle
    tail. `cmd/stage-b` stages project A's converted
    `BUILD.bazel.out`s into project B and emits the
    changed-element signal (a content diff — the write-a + Bazel
    path's replacement for the orchestrator's `res.Converted`,
    and more precise: a genrule that re-ran but emitted identical
    bytes is correctly reported unchanged). A driver feeds that
    `$changed` list into `relax-keeps` + a targeted
    `bazel run //:gazelle -- $changed`; `scripts/meta-gazelle-roundtrip.sh`
    is the reference driver and conformance gate. "Opt in" =
    the driver includes the tail once the operator has wired
    gazelle / gazelle_cc into `overlay.MODULE.bazel` (there is
    no orchestrator and no `--enable-gazelle` flag — the driver
    is a script). The one boundary: the actual
    `bazel run //:gazelle` needs `gazelle_cc` declared as a
    `bazel_dep`, which waits on a bcr-published gazelle_cc
    release; the gate runs the tail guarded on the `//:gazelle`
    target existing, exercising the changed-element plumbing
    unconditionally either way.
- **Narrowing-undercoverage audit CI gate (soft launch).**
  The audit (`cmd/audit-narrowing`) now runs in CI as
  `make e2e-audit-narrowing` via
  `scripts/meta-audit-narrowing.sh` (render the meta-project,
  invoke `convert-element-cmake` offline to populate
  `cmake-reads.json` per kind:cmake element, walk
  `scripts/audit-narrowing-walk.sh` to accumulate the combined
  report). The meta script exits non-zero on drift (the
  underlying primitives — `cmd/audit-narrowing` and the
  walker — stay policy-agnostic with "exit 0, report is the
  signal", but the meta script IS the policy layer); the CI
  step uses `continue-on-error: true` so the gate is
  non-blocking while operators accumulate signal about
  real-world drift. The two open conversations the previous
  Next bullet flagged resolved as:
  - **Allowlist** (`<elem>.expected-drift.txt` next to
    `<elem>.read-paths.txt`, staged as
    `srckey-expected-drift.txt` in project A): one path per
    line, no glob grammar — each entry is a deliberate
    per-path declaration. Format mirrors audit-narrowing's
    output so `cat audit-report.txt >> <elem>.expected-drift.txt`
    is a valid (manually-reviewed) silencing flow.
    `cmd/audit-narrowing --allowlist=<path>` subtracts entries
    before writing the report. The `cmake-codegen-lifted` tag
    is the inverse-audit query for spotting stale entries
    (an allowlisted `.h.in` whose corresponding genrule now
    carries `cmake-codegen-lifted` is safe to delete).
  - **Per-build trace-side capture**: write-a learns
    `--trace-source-root` which threads
    `--source-root="$$BUILD_ROOT"` into the round-2 install
    genrule's build-tracer invocation. Default off (preserves
    the legacy AC byte schema for trace-driven kinds);
    flipping the flag invalidates that build's AC entries for
    those kinds (one-shot rebake). CI / e2e fixtures opt in
    to populate the trace oracle; production deployments stay
    on the legacy byte schema until they choose to rebake.
  Promotion to blocking is queued in the Next section, gated
  on real-world signal accumulation. Recipe + policy in
  `docs/design/narrowing-audit.md`.

- **Same lift shape for `file(GENERATE)` and bytes-embedding
  `cmake -E configure_file`.** The bytes-into-genrule-cmd
  surface in `lower/configure_file.go`'s legacy
  base64-of-rendered fallback shape is now also reachable —
  but rarely taken — by the file(GENERATE) lifter
  (`lower/file_generate.go`) and the cmake -E configure_file
  branch of the cmake-E lifter
  (`lower/execute_process.go::liftCMakeEConfigureFile`).
  `shadow.ExtractFileGenerate` extracts the new call kind from
  cmake's trace; the trace-recording script
  (`tools/fixtures/record-fileapi.sh`) stashes rendered outputs
  alongside the configure_file stash so offline tests have the
  bytes the lifter expects. The Bazel-time tool
  (`cmd/cmake-configure-file`) gained a `--content-base64`
  mode so the CONTENT form of file(GENERATE) (no on-disk
  template) can ride the lifted shape without staging a fake
  srcs entry. Tags split lifted vs legacy via
  `cmake-codegen-lifted` and call out the genex fallback via
  `cmake-codegen-file-generate-genex` so the audit can find the
  templates the genex-evaluator follow-ups (since landed as
  the (a)/(b) shapes; see Done bullets above) addressed.
  Render gate:
  `scripts/meta-file-generate.sh`. Fixture:
  `converter/testdata/sample-projects/file-generate/`.

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

- **Per-platform fold for round-2 trace-driven kinds —
  kind:cmake Phase B fallback project B install fan-out.**
  kind:cmake's round-2 fallback (`--cmake-round2-fallback`)
  joins the per-platform install fan-out story.
  `cmakeRound2InstallBuild` gained `tracePlatform` parameters
  (NameSuffix / OutputPrefix / sorted ExecCompatibleWith /
  baked `--platform=`) mirroring the pipelineHandler
  `OutputPrefix` knob trio from #114. A new
  `renderCmakeRound2B` dispatcher hands the single-platform
  legacy path through unchanged (byte-stable) and the
  multi-platform path through `composeMultiPlatformInstallBuild`
  — same `:install_tree.tar` select()-filegroup with a
  `"//conditions:default": []` arm.
  Project A's side under cmake round-2 fallback is unchanged
  here — the orchestrator's existing multi-platform fan-out
  for kind:cmake (PR #112) runs convert-element-cmake per-platform
  at orchestrate time, and fold-element composes the
  per-platform IRs (placeholder or native, depending on
  whether the classifier refused) into the unified
  `BUILD.bazel`. Render gate:
  `scripts/meta-cmake-round2-fallback-multiplatform.sh`. Test:
  `TestWriter_CmakeRound2Fallback_MultiPlatform_ProjectB`.

- **Per-platform fold for round-2 trace-driven kinds —
  kind:autotools project B install fan-out.** kind:autotools
  joins the per-platform install fan-out story by reusing
  `pipelineHandler.renderPipelineRound2B`.
  `autotoolsHandler.RenderB`'s round-2 branch dispatches to it
  directly (was
  `h.RenderA` before, the legacy single-genrule path);
  `autotoolsPipelineHandlerForElement` already wired the
  pipelineHandler instance with `kindName: "autotools"`, so
  the fan-out's per-platform extension construction
  (`pipelineTraceExtensionRound2(elem, []string{"autotools"},
  plat)`) and `depKindAllow` agree with the pre-fan-out
  shape. Single-platform autotools renders the same legacy
  `<elem>_install` genrule as before (the function's
  empty-platforms branch); multi-platform mode produces N
  install genrules + the top-level
  `:install_tree.tar` select()-filegroup.
  Round-1 autotools is gated out — its
  `autotoolsTraceExtension` (which wraps the converter inline
  alongside the install action via the BUILD.bazel.out +
  install-mapping.json outs) is incompatible with the round-2
  trace-publish wrapper that `renderPipelineRound2B`
  constructs, so the round-1 path keeps the existing
  `h.RenderA` call. Render gate:
  `scripts/meta-autotools-round2-multiplatform.sh` (sibling
  of `meta-trace-round2-fold.sh`); test:
  `TestWriter_AutotoolsRound2_MultiPlatform_ProjectB`.

- **Per-platform fold for round-2 trace-driven kinds — project B
  install fan-out, pipelineHandler kinds.** Second half of the
  fold: when `--platforms-json` is set, project B's per-element
  install render fans out to N install genrules (one per
  platform) instead of one. Each genrule:
  - Names `<elem>_install_<platform>` so N coexist in one
    package.
  - Outputs land under `<platform>/install_tree.tar`,
    `<platform>/trace.log`, `<platform>/make-db.txt`,
    `<platform>/generated-headers.txt` so there are no path
    collisions.
  - `exec_compatible_with` carries the platform's constraint
    set — Bazel routes the install action to a matching
    executor pool so the linux build doesn't try to run on a
    darwin worker.
  - The inline `trace-publish` call bakes `--platform=<plat>`
    literally into the argv so each cell publishes under the
    matching AC partition; the env-var fallback
    (`--action_env=CMAKE_TO_BAZEL_PLATFORM=...`) can't differ
    across N parallel actions in one Bazel build.

  A top-level filegroup at `:install_tree.tar` `select()`s the
  matching per-platform tarball so downstream
  `//elements/<dep>:install_tree.tar` references resolve
  correctly at each consumer's build platform. The
  `pipelineExtension` struct gains three new knobs
  (`OutputPrefix`, `NameSuffix`, `ExecCompatibleWith`) so the
  rendering helpers stay one code path; empty values preserve
  the single-platform byte-stable shape exactly.
  `pipelineTracePublishStep` takes `platform` + `outputPrefix`
  parameters so the trace-publish argv and `$(location ...)`
  references resolve to the right per-platform paths.
  `converter/internal/elementfold` → `converter/elementfold`
  promotion (same precedent as the earlier `ir` promotion)
  so write-a can call `elementfold.PickSelectKeys` to derive
  the per-platform select() arm labels — both project A's
  fold and project B's install-tree filegroup pick the same
  labels for the same matrix.

  Together with the project A fan-out (also Done, below), this
  closes the runtime gap: a multi-platform render now publishes
  N AC entries with distinct platform tags AND the project A
  side resolves N per-platform `_trace_repo` lookups, so a
  single Bazel build sees the right trace for each platform's
  install. Render gate:
  `scripts/meta-trace-round2-fold.sh` covers both sides;
  `TestWriter_PipelineKindsRound2_MultiPlatform_ProjectB`
  asserts the rendered B-side shape end to end. Scope today
  is pipelineHandler-shaped kinds (kind:make / manual /
  script / makemaker / modulebuild); kind:autotools and
  kind:cmake Phase B fallback have the same shape ahead of
  them in Next.

- **Per-platform fold for round-2 trace-driven kinds — project A,
  pipelineHandler kinds.** First half of the per-platform fold
  for round-2: project A's per-element converter render fans out
  one genrule per (element, platform) cell plus a fold-element
  genrule composing the N `ir.Package` JSONs into one
  `BUILD.bazel.out`. `convert-element-trace` gained
  `--out-ir-json` and the trace converter's recovered rules now
  flow through the shared `converter/ir.Package` so
  `fold-element` + `converter/elementfold` compose them
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
  `convert-element-cmake` no longer bakes the host's viewpoint into each
  per-element `BUILD.bazel`. The orchestrator's
  `--platforms-json` flag (parallel to the toolchain unifier's
  manifest) drives one `convert-element-cmake` REAPI Action per
  (element, platform) cell; the resulting per-platform
  `ir.Package` JSONs (emitted via convert-element-cmake's
  `--out-ir-json`) feed `cmd/fold-element`, which composes them
  into a single unified `BUILD.bazel` whose attributes carry
  `select()` blocks for divergent srcs/hdrs/includes/defines/deps
  and per-platform-routed copts/linkopts. `internal/empfold`
  factors out the cardinality-partition primitive
  (`toolchain.Observe` now uses it too).
  `converter/elementfold` enforces per-target
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


- **kind:meson round-2 fallback (Phase B).** Stacked on Phase A.
  When write-a is given `--meson-round2-fallback` +
  `--build-tracer-bin` + `--trace-publish-bin` +
  `--trace-lookup-bin`, every kind:meson element renders with
  the same A-converter + B-install + round-2-rendezvous split
  kind:cmake's Phase B (`docs/design/rendezvous.md`)
  already established. Project A's converter genrule threads
  `--unsupported-target-fallback=true` into
  `convert-element-meson`, so native-lowering refusals
  (`unsupported-meson-subproject` /
  `unsupported-meson-custom-target` /
  `unsupported-meson-generated-sources` /
  `unsupported-meson-cross-compile` /
  `unresolved-meson-dependency` /
  `unsupported-meson-target-type`) produce an install-plan-driven
  placeholder shape (per-target `cc_import` / `sh_binary` stubs
  referencing `install_tree.tar` + an extract genrule untarring
  it) instead of Tier-1 exit. Project B emits a real install
  genrule wrapping `meson setup --prefix=/ --libdir=lib + ninja +
  meson install --destdir + tar` under `build-tracer` + inline
  `trace-publish` (when `CAS_GRPC_ADDR` is set in the action
  env). The placeholder enumerates per-target stubs from
  `intro-install_plan.json`'s `tag` field (richer signal than
  cmake's destination-path inference: `runtime`/`devel`/`man`/...
  partition the install set unambiguously) and resolves the
  install-path placeholders (`{libdir_static}`, `{bindir}`,
  `{includedir}`, ...) against `intro-buildoptions.json`'s
  `section: directory` rows. The `--prefix=/ --libdir=lib` pin
  on both the converter's meson invocation AND project B's
  install genrule keeps the placeholder paths the converter
  computes aligned with the actual install_tree.tar layout
  across multiarch hosts. The trace-driven convergence
  follow-on (teaching convert-element-meson to consume
  `@trace_<elem>//:trace` to refine refusals into fine cc
  rules) is staged today — kind-agnostic with kind:cmake's
  matching wiring — but the trace bytes aren't yet consumed.
  Render gate: `scripts/meta-meson-round2-fallback.sh`
  (`make e2e-meta-meson-round2-fallback`); also exercises the
  standalone converter against a custom-target-refusal fixture
  to confirm strict mode refuses while the fallback emits the
  placeholder. Recipe: `docs/design/rendezvous.md`.

- **kind:pyproject Phase B install-plan fallback (option A:
  per-element auto-detection).** Stacked on Phase A. New
  `--pyproject-fallback` write-a flag activates per-element
  dispatch: write-a probes each element's pyproject.toml at
  render time (running the converter binary with `--probe`,
  which runs the parse/discover/lower pipeline without writing
  output) and emits the pipeline-shape coarse install genrule
  for elements that would refuse, the native genrule for
  elements that would succeed. Operator flips the flag once
  and every kind:pyproject element renders correctly
  regardless of per-element backend / metadata shape.
  Refused-element diagnostics surface on write-a's stderr.
  Render gate: `scripts/meta-pyproject-fallback.sh` against a
  two-element fixture (one Phase-A-friendly setuptools
  element + one pdm-backend element refused by Phase A).
  Recipe: `docs/architecture.md` "Phase B"
  section. Coverage status: every kind:pyproject element in
  FDSDK now renders without operator intervention, taking
  pyproject's effective coverage to 100 %.
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
  with `--convert-element-pyproject` set those refusals fail
  the per-element genrule at bazel-build time. Routing refused
  elements to the pipeline-shape fallback automatically is the
  Phase B install-plan fallback follow-up (queued); today's
  operator escape is to re-render without
  `--convert-element-pyproject` to take the pipeline default
  for the whole graph. Activated by passing
  `--convert-element-pyproject <path>` to write-a; project B's
  MODULE.bazel auto-adds `rules_python` when at least one
  kind:pyproject element is present and the native path is on.
  Render gate: `scripts/meta-pyproject.sh` against
  `testdata/meta-project/pyproject-greet/` (representative
  setuptools fixture). Coverage status:
  `docs/fdsdk-coverage.md`.
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
  taxonomy follow-up: `docs/fdsdk-coverage.md` now
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
  `docs/architecture.md`. Coverage status:
  `docs/fdsdk-coverage.md`.
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
  available at convert-element-cmake action time (the
  trace-driven convergence path queued in `Later` will teach
  the converter to refine refusals from the trace; the
  wiring is in place today).

  Render gate: `scripts/meta-cmake-round2-fallback.sh`.
  The kind-agnostic live-AC gate
  (`tools/e2e-meta-autotools-round2-live.sh`) covers
  kind:cmake's wire contract through its publish/lookup
  round-trip half. Recipe:
  `docs/design/rendezvous.md`.
  Failure schema: `docs/failure-schema.md`
  `unsupported-execute-process`.
- **Element-signal consumption in the unifier.** `unify-toolchains`
  gained `--element-signal <dir>` (optional, repeatable): it loads
  the per-element toolchain-signal reply dirs that
  `convert-element-cmake --out-toolchain-signal-dir` captures and
  folds any builtin include / link search root a real element
  exposed — a sysroot leg a `find_package` added, a vendored-SDK
  include dir a project-side toolchain file injected — that the
  dedicated probe matrix missed into the matching platform's
  `ResolvedToolchain.Base`. The merge is strictly additive (a path
  the probe already recorded keeps its place; only languages
  already present in `Base` are touched) and lives in
  `toolchain.FoldElementSignal`. Platform association is heuristic:
  the signal's observed `TargetPlatform` is matched against each
  platform's probe-derived `Base.TargetPlatform`, with a
  single-platform fast path — a write-a render targets one platform
  per run, so the signal directory belongs to that one platform
  even when the recorded reply carries no `CMAKE_SYSTEM_NAME`.
  Signals that match zero platforms or are ambiguous across several
  are skipped with a stderr diagnostic; signal consumption is
  best-effort enrichment, not a hard input. Render gate:
  `scripts/meta-unify-toolchains.sh` (section 9).
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
    `convert-element-cmake --out-toolchain-signal-dir` + orchestrator
    `Options.CollectToolchainSignal` + `orchestrate
    --collect-toolchain-signal`. Sets the foundation for the
    unifier to fold per-element builtin-include / sysroot facts
    into each platform's `ResolvedToolchain.Base` (consumed by the
    unifier's `--element-signal` fold — see the **Element-signal
    consumption in the unifier** entry).
  - Render gates: `meta-render-project-a.sh` + `meta-unify-toolchains.sh`.
- **Configure_file lift.** Per-element `*.h.in` templates are
  no longer load-bearing inputs of convert-element-cmake's cache key
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
  passes `--lift-configure-file=true` to convert-element-cmake).
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
  drive convert-element-cmake to emit `cc_library` / `cc_binary` rules.
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
  `docs/design/rendezvous.md`.
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
  path. Recipe: `docs/design/sources.md`.
- Trace + make-db canonicalization (pids stripped, gcc temp paths
  placeholdered, action-time mktemp paths normalized). Foundation
  for round-2 cache reuse.
- Per-element srckey + per-kind narrowing patterns — defines what
  counts as graph-affecting vs name-only for the autotools build.

The "Done" list is in the rear-view; the doc that captures the
current state of the codebase is `docs/architecture.md`
(architecture + interop contract + build-time flow, all in one
place), plus `docs/codebase-map.md` for the developer-facing repo
tour.
