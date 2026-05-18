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
  `docs/design/cmake-execute-process-round2-fallback.md`)
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
## Later (research / open questions)

- **Platform-conditional source partitioning — Tier 2:
  recover skipped-branch sources by parsing CMakeLists.txt
  (#217 follow-on).** Tier 1 (shipped — see Done) handles
  the platform's own conditional sources from the trace
  alone. Tier 2 closes the cross-platform half: the other
  arms of an `if(CMAKE_SYSTEM_NAME ...)` block (the body
  cmake skipped on this configure) carry sources that
  exist in CMakeLists.txt but never appear in the trace
  (cmake only traces what runs). Recovering them needs an
  actual cmake-syntax parser keyed off the `file`/`line`
  the trace's `if()` event records. New capability —
  scope is "small targeted parser for the `target_sources`
  / `add_library` / `add_executable` shapes inside `if`
  bodies", not a full cmake interpreter. Demand: open
  until a real downstream surfaces a project where the
  cross-platform else-arm sources matter for the BUILD's
  correctness when bazel reconfigures for the other
  platform.

- **Cross-package TARGET_FILE resolution (PR 2).** The
  soundness-fix piece shipped (see Done — refusal stub for
  unresolved cross-package references); the resolved-lift
  piece remains. The file(GENERATE) lifter now sees the
  `*manifest.Resolver` but doesn't yet USE it to emit
  fully-qualified Bazel labels in the `--target-file` flag
  for resolvable cases. PR 2's wiring is sketched in
  `docs/design/cross-package-target-file.md` — branch the
  `--target-file=` flag emission per-target on resolution,
  populate `FileLocation` for imported targets from the
  codemodel's IMPORTED_LOCATION for the byte-equal check, add
  a render-gate fixture exercising two BuildStream elements
  end-to-end.

- **`$<TARGET_OBJECTS:t>` for OBJECT_LIBRARY targets.** The
  (a) evaluator now supports the other six on-disk-path
  variants (see Done). `TARGET_OBJECTS` is a distinct case —
  it resolves to a semicolon-separated list of .o paths
  per cmake's object-library convention, not a single path,
  so it needs a separate `Context.Targets[t].Objects []string`
  field and a list-valued wire (likely a repeatable
  `--target-objects=<name>=<path>` flag the lifter calls once
  per object). Convert-time byte-equal-check semantics work
  the same way; just more wire than the FILE_* variants.

- **TARGET_PROPERTY INTERFACE_* aggregation.** The v1
  evaluator (below) supports `$<TARGET_PROPERTY:t,p>` for
  properties cmake reports verbatim (NAME / TYPE / SOURCES /
  IMPORTED). The INTERFACE_* properties cmake aggregates from
  dependencies (INTERFACE_INCLUDE_DIRECTORIES,
  INTERFACE_COMPILE_OPTIONS, INTERFACE_COMPILE_DEFINITIONS,
  INTERFACE_LINK_LIBRARIES) need the lifter to mirror cmake's
  transitive-property resolution against the fileapi
  codemodel — accumulating across the target's dependency
  graph in cmake's documented order. Distinct work item from
  TARGET_FILE since it's all convert-time (no Bazel-label
  resolution needed).

- **Source-side AC narrowing for autotools.** Bazel's hermetic-action
  model says inputs in → outputs out; you can't have a byte be
  available to the action at exec time without it being in the AC
  key. So narrowing autotools is unavoidably a side-channel story.
  `docs/three-pass-flow.md` lays out three options (FUSE, host-fs
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
  is documented in `docs/fdsdk-coverage-status.md` (small
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

- **Platform-conditional source partitioning from a single-platform
  cmake trace (#217 Tier 1).** A new shadow extractor
  (`internal/shadow/platform_conditional.go`) walks the cmake
  `--trace-expand` stream maintaining a per-file `if()` stack,
  and reports each `target_sources` / `add_library` /
  `add_executable` source attached inside a recognized
  `if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")` block.
  `converter/internal/lower/lower.go` consumes the records and
  moves matching sources from the flat `irt.Srcs` to
  `irt.PerPlatform["srcs"][@platforms//os:*]`, so the emitter
  renders a `select()` arm even on single-platform runs. The
  innermost-recognized-key policy means nested ifs collapse to
  the most-specific OS constraint that was open when the
  source was added; `else` arms (where the constraint would be
  a NOT-of-something) stay unrecognized and fall through to
  flat srcs unchanged. Byte-stability preserved for projects
  without platform conditionals (TraceRaw nil → no partition;
  matching srcs missing → no partition). Tier 2 — recovering
  sources from skipped `if` branches by parsing CMakeLists.txt
  at trace-recorded line numbers — remains open (see Later).

- **FDSDK-glue placeholder handlers — kind catalog now
  fully covered.** Stub handlers for the four previously-
  missing FDSDK-specific kinds (`collect_initial_scripts`,
  `collect_integration`, `check_forbidden`, `flatpak_repo`),
  matching the shape of the pre-existing `collect_manifest`
  stub. Each emits an empty `install_tree.tar` so render of
  FDSDK fixtures reaches completion without these kinds
  breaking the graph; real plugin semantics deferred until
  an FDSDK fixture forces a bazel-build-time correctness
  need. Coverage table in `docs/fdsdk-coverage-status.md`
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
  `docs/design/cross-package-target-file.md`. Audit tag:
  `cmake-codegen-file-generate-genex-cross-package`. The
  `*manifest.Resolver` plumbing this set up is the foundation
  PR 2 (resolved cross-package lifts; see Later) builds on.

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
  payload stays small for the common case. Aggregated
  INTERFACE_* properties remain UnsupportedError until the
  matching lifter pipeline lands (queued under Later).

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
  assert the new shape; `docs/design/build-output-conventions.md`
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

- **Docs consolidation + architecture slide deck.** The
  cross-element configure-step bootstrap stack landed; the
  design-trail docs got folded.
  `docs/design/conversion-architecture.md` is the new end-state
  architecture doc — three diagrams (rendezvous channel, driver
  loop, per-element BUILD evolution through `finalize-b`), one
  section per rule pattern in `rules_buildstream_bazel/`, and
  cross-links into the focused mechanism docs.
  `docs/design/conversion-architecture-slides.md` is the 8-slide
  Marp companion (problem → two projects → Bazel
  anti-pattern → trace_load/trace_build pair → two cache layers →
  driver loop → finalize-b). `docs/design/staged-pipeline.md`
  deleted outright; `docs/three-pass-flow.md` trimmed to the
  per-pass cost model + scenario walks; the architectural framing
  in `docs/design/autotools-round2-rendezvous.md` trimmed to
  mechanism details. README / CONTRIBUTING / overview /
  visual-guide cross-links refreshed.

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
  `docs/design/cross-element-config-rendezvous.md`,
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
  PR sequence (`docs/design/orchestrator-absorption.md` has the
  full capability map):
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
  `defaultPlatform` / `Action.Platform` fallback — the last open
  question in `docs/design/orchestrator-absorption.md`. Render
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
  `docs/design/build-output-conventions.md`. Shipped across the
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
    `docs/design/operator-gazelle-step.md` workflow; `cmd/relax-keeps`
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
  kind:cmake's Phase B (`docs/design/cmake-execute-process-round2-fallback.md`)
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
  placeholder. Recipe: `docs/design/meson-round2-fallback.md`.

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
  Recipe: `docs/design/pyproject-native-render.md` "Phase B"
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
  available at convert-element-cmake action time (the
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
