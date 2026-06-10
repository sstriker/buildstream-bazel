# Survey corpus & how to run a faithful survey

The **survey** runs the cmake converter over real-world CMake projects in
diagnostic mode and counts what it can't yet lift natively. It's the
instrument we use to answer "is the converter getting better, and where is
intent still being lost?" — see `docs/codemodel-consumption-audit.md` for
the analysis framing and `scripts/run-survey.sh` for the driver.

> **Survey is not fidelity.** The survey runs the *faithful* shape
> (multi-config + split-packages, see below) to catch **intent loss**; the
> fidelity harness (`scripts/run-fidelity.sh`) runs the *opposite* shape
> (single-config + single monolithic BUILD) to catch **symbol divergence**
> in a built artifact. They're complementary oracles — neither subsumes the
> other. The full framing lives in
> [`docs/fidelity-deltas.md`](fidelity-deltas.md) under "Fidelity vs.
> survey: two complementary oracles".

This document is the single source of truth for **which projects are in the
corpus** and **how to survey them faithfully** (so two runs are comparable).

## Status at a glance

State per project across the six lenses (each lens is defined in the
sections below). The three **convertibility** lenses report finding
counts — `0` is healthy; the **build** lens reports the survey's
`build` token. The two opt-in lenses report the **2026-06-08 full-corpus
run** (detail in *Full-corpus lens snapshot* below, raw output in
[`survey-artifacts/`](survey-artifacts/)): **Fidelity** = matched
`CppCompile` TUs from the compile-db lens (`n/a` = header-only / no TUs);
**Intent** = the judge's net-new `missed` count — **non-deterministic, a
triage pointer not a comparable metric**. Per-project build-lens detail is
in *Build-lens status* below; the full corpus roster + rationale is under
*The corpus*.

| Project | Rejections | Idiom | Coverage | Build lens | Fidelity | Intent |
| --- | --- | --- | --- | --- | --- | --- |
| **fmt** | 0 | 0 | 0 | `ok` | 29 | 7 |
| **libxml2** | 0 | 0 | 0 | `ok` | 52 | 10 |
| **brotli** | 0 | 0 | 0 | `ok` | 36 | 8 |
| **glm** | 0 | 0 | 0 | `ok` | 1 | 8 |
| **googletest** | 0 | 0 | 0 | `ok` | 4 | 5 |
| **abseil** | 0 | 0 | 0 | `ok` | 156 | 7 |
| **zstd** | 0 | 0 | 0 | `ok` (repo-root overlay) | n/a‖ | 8 |
| **glog** | 0† | 30 | 0 | `ok` | 20 | 0 |
| **eigen** | 0 | 16‡ | 0 | `ok` | n/a | 8 |
| **cutlass** | 1§ | 0 | 0 | `ok` (header lib; CUDA tier) | n/a | 12 |
| **OpenBLAS** | 0¶ | 0 | 0 | `ok` (C-LAPACK; ~2460 targets) | 6277 | 9 |
| **curl** | 0 | 1 | 0 | `ok` (library + CLI + test surface; ~238 actions) | 213 | 9 |
| **zlib** | 0 | 0 | 0 | `ok` (lib + examples; shared-only linkopt drop) | 17 | 5 |
| **boost-core** | 0 | 0 | 0 | `ok` (header lib) | n/a | 6 |
| **spdlog** | 0 | 0 | 0 | `ok` | 8 | 10 |
| **catch2** | 1 | 0 | 0 | `ok` | 107 | 11 |
| **nlohmann-json** | 0 | 0 | 0 | `ok` (header lib; tests off) | n/a | 6 |
| **libpng** | 0 | 0 | 0 | `ok` (cmake -P script-bake; host zlib) | 24 | 9 |
| **libevent** | 0 | 5 | 0 | `ok` (libs; regress tests off) | 40 | 7 |
| **mbedtls** | 0 | 0 | 0 | `ok` (crypto libs; tests+programs off, link_to_source in==out drop) | 113 | 14 |
| **protobuf** | 0 | 8 | 0 | `ok` (libs + protoc + upb generators; find_package(absl) via @abseil-cpp + //absl_umbrella, host zlib) | 286 | 11 |
| **sdl** | 2 | 5 | 0 | `ok` (multi-config × per-platform; file(GENERATE) $<CONFIG> headers, PCH forced-include lift, select-arm relabel; host X11 + GL/GLES/EGL dev headers) | 259 | 9 |

The four large members driven for convertibility but not the build lens
(`grpc`, `llvm`, `vtk`, `cuda-samples`) aren't in this table but DO have
fidelity + intent rows in *Full-corpus lens snapshot* below.
‖ zstd's fidelity is **blocked by a converter regression** (split-emit
emits an invalid subpackage label), not the lens — see the snapshot's
footnote and `ROADMAP.md`.

`rej` = surveys with rejections, so the build lens skips it; for
protobuf these are honest external `find_package` deps
that resolve in a real `.bst` element graph (not converter debt).
† glog's 2 rejections are benign forward-declared-include "treated as
empty" notices (0 blocking — a no-op in strict mode), so it builds; see
*Build-lens status* for the static-link detail.
‡ eigen's 16 idioms are all `fortran-target-needs-ruleset` — the bundled
Fortran reference BLAS/LAPACK, which the diagnostic survey (full tree)
flags as needing a Bazel Fortran ruleset (deferred — none exists). They
don't block the **header-library** build lens, which disables the
Fortran BLAS/LAPACK + the test/doc/demo dev surfaces (see *Build-lens
status*).
§ cutlass's 1 rejection is an `unsupported-execute-process` (a `cmake -E env`
in `tools/library`) that the full-tree diagnostic survey flags but the
**header-library** build lens disables (`CUTLASS_ENABLE_TOOLS=OFF`), so it
doesn't block the build — `cutlass.conf` sets `BUILD_LENS_IGNORE_REJ=1` to
bypass the skip(rej) gate (see *Build-lens status*). Needs nvcc on `PATH`
to configure (`BSB_PROVISION_CUDA=1`).
¶ OpenBLAS is green on the **C-LAPACK** shape (`NOFORTRAN=1 C_LAPACK=1`,
deterministic `TARGET=HASWELL`): the whole ~2460-target kernel + LAPACK +
BLAS library `bazel build //...`s with no cmake. Getting there took four
converter features (generated-wrapper absolute-`#include` rewrite +
kernel staging, transitive micro-kernel closure, `file(RENAME)` config.h
recovery) and two build-lens flags (`-mfma`, `NO_CBLAS=1`) — see
*Build-lens status*. The **real reference-Fortran** LAPACK (no `C_LAPACK`)
remains future work: it needs a Bazel Fortran ruleset (`edbaunton/rules_fortran`
is an empty stub; the path is a hand-rolled `fortran_library` in
`rules_buildstream_bazel`).
Large members surveyed for convertibility but not yet driven through
the build lens (SDL, grpc, llvm, VTK, …) live under *The
corpus* / *Regression corpus* below.

## What a survey is checking for (the three lenses)

A survey isn't just "did it crash" — it's three distinct questions, and
they have very different tooling support:

1. **Are there rejections we could lift?** — the `rejections.json`
   report. The converter self-reports every Tier-1 refusal with a typed
   `code`. That captures the *surface*; the missing piece is a
   *liftability* verdict, which is still a manual triage. Classify each
   rejection into:
   - **lift-able** — a tractable converter improvement (e.g. Eigen's
     `unsupported-execute-process` for a `c++ --version | head` pipeline:
     teach the execute_process classifier the multi-COMMAND shape);
   - **external / resolves-in-graph** — *not* a converter bug, an artefact
     of surveying a project standalone (e.g. protobuf's
     `find-package-dep-unresolved` for `find_package(ZLIB)` — resolves
     through the producer→consumer export channel in a real `.bst`
     element graph);
   - **genuinely-unsupportable** — no Bazel analogue (e.g. a host
     toolchain probe writing a `RESULT_VARIABLE`).

   When comparing runs, always break `rejections.json` down by `code` and
   subtract the *external* class before reading a number as "converter
   debt" (see pitfall 3 below).

2. **Are we emitting non-idiomatic Bazel?** — three complementary
   oracles, weakest-to-strongest:

   - **`bazel-idiom.json`** (`bazelidiom.Audit`, runs on every convert):
     the *semantic* idiom check — `empty-srcs`, `empty-cc-library`,
     `empty-cc-import`, `test-with-no-entry`, `raw-toolchain-feature-flag`,
     `non-cc-language-source` (Fortran `.f` srcs that slipped into a cc_*
     rule — Bazel can't compile them; the converter normally partitions
     these out into a `<target>_fortran_srcs` filegroup and emits the
     operator-action `fortran-target-needs-ruleset` finding instead, so a
     raw `non-cc-language-source` now means the partition missed one),
     etc. Per-survey and fully automated; drive
     any non-zero count to zero (this surfaced the eigen empty-srcs and
     VTK empty-cc-library work).
   - **`buildifier -mode=diff`**: the *syntactic* canonical-form check.
     The emitter renders through buildifier's own formatter
     (`build.Format`), so output is canonical by construction — the
     `scripts/meta-cmake-split-packages.sh` gate asserts `-mode=diff` is a
     no-op. (Not re-run per-survey: it would be redundant with the
     emit-time formatting, and is already guarded by the gate.)
   - **gazelle round-trip**: the *structural* idiom check, and the
     strongest — "is the output already what gazelle would generate?"
     Two flavours:
       - **Fixture gates** (`scripts/meta-gazelle-roundtrip.sh`,
         `scripts/meta-cmake-split-gazelle.sh`): render the project-B
         fixture with `--gazelle-cc`, `bazel run //:gazelle`, assert the
         layout is maintained and a second pass is a fixpoint (whole-rule
         `# keep` survives). These are part of the e2e suite.
       - **Wild corpus** (`scripts/survey-gazelle-roundtrip.sh
         <name>=<src>`, also `make survey-gazelle`): same idea pointed at
         arbitrary corpus projects, without the project-A/B orchestration.
         It converts with `--split-packages`, overlays the emitted BUILDs
         onto a scratch Bazel module wired with `gazelle_cc` (rules_cc +
         rules_pkg + the gazelle stack, versions matching write-a's
         `--gazelle-cc` emission), and runs `bazel run //:gazelle` twice.
         Verdicts:
           - **load error → hard FAIL**: the converted BUILDs don't even
             load under gazelle/bazel — a non-idiomatic emission the
             converter produced. (This is how the harness caught the
             split-mode `pkg_files` glob-labelize bug —
             `glob(["//c/include:brotli/**"])`, an invalid absolute glob
             pattern — on brotli.)
           - **drift (converges) → reported datapoint**: gazelle_cc
             rewrote some `cc_*` rules on the first pass but a second pass
             is a fixpoint. Measures how far the split output is from
             gazelle's canonical per-source-dir layout; not a hard
             failure, since the converter+gazelle_cc design has gazelle
             own the layout going forward.
           - **non-convergence → reported datapoint** (hard fail under
             `SURVEY_GAZELLE_STRICT=1`): a second pass still changes the
             BUILDs. On a wild *standalone* project this often reflects
             gazelle_cc's own resolver limits (e.g. internal-header
             includes it can't attribute without the project's real Bazel
             config), so it's informational by default.

     Each wild run is a real bazel build (heavier than the per-convert
     survey loop), so run it on the small members routinely (`googletest`,
     `brotli` — the `make survey-gazelle` default) and the large ones
     (`vtk`, `llvm`) on demand via `SURVEY_GAZELLE_PROJECTS`. Needs
     bazel ≥ 9 + cmake + ninja and a reachable Bazel Central Registry;
     `META_GAZELLE_USE_HOST_GO=1` uses the host Go toolchain when go.dev
     egress is blocked, and the Bazel cache is persistent
     (`SURVEY_GAZELLE_BZL_CACHE`, default `~/.cache/survey-gazelle-bazel`)
     so repeat runs reuse the fetched `gazelle_cc` toolchain.

3. **Did we lose intent vs. the CMakeLists?** — **the adversarial lens**,
   now *partially* automated (dependency-coverage, below). Lenses 1 and 2
   are *self-reported*: the
   converter flags its own refusals and its own known-bad shapes. Lens 3
   is the one thing it structurally *cannot* self-report, because silent
   intent loss is exactly the stuff it dropped without noticing. Measuring
   it needs an independent oracle (the codemodel / the CMakeLists) and a
   diff against the emitted BUILD.

   What we learned about how to spend effort here:

   - **Target-level coverage is reliably complete** — don't over-invest in
     a "did every target emit" census. Empirically (VTK: 289 codemodel
     targets = 155 SHARED + 4 INTERFACE + 9 EXECUTABLE + 121 UTILITY) every
     buildable target emits a rule (9 EXECUTABLE → 9 `cc_binary`; the 159
     libraries → `cc_library` incl. object-lib splits; UTILITY correctly
     skipped). A whole target rarely vanishes.
   - **Intent loss is intra-target**, and that's where every past corpus
     bug lived: dropped dependency edges (#302, abseil INTERFACE deps),
     install-path prefixing (#301), alias-lift ordering (#300), refused
     pre-committed generated sources (#304). The `empty-srcs` /
     `empty-cc-library` idiom checks are *partial* proxies — they only
     fire when the result is structurally empty, and miss partial loss (a
     target that kept 3 of 4 sources, or dropped one of several deps).
   - **Much of the deterministically-detectable loss is already
     accounted** by existing self-reporting: `cmake-elided-*` source tags,
     `unresolved-link-dep` rejections, and the idiom findings. A naive
     codemodel→BUILD differ would mostly re-derive those *and* throw false
     positives on legitimate relocation (object-library inlining,
     generated-source re-wiring to a genrule edge, split-package header
     libraries, the EXECUTABLE→`cc_test` rewrite).

   So lens 3 is, for now, a **manual** comparison. A faithful pass:
   pick a target in the emitted BUILD, open its declaring CMakeLists
   stanza, and check each of — sources, PUBLIC/PRIVATE includes, defines,
   compile options, `target_link_libraries` deps (incl. INTERFACE),
   install destination — landed somewhere (a rule attr, a `# keep` tag, or
   an honest rejection). Anything that vanished with no tag/rejection is a
   lens-3 bug.

   **Dependency-coverage (implemented).** The one deterministic,
   low-false-positive lens-3 check runs on every convert
   (`converter/internal/coverage`): a trace `target_link_libraries` arm
   naming an **in-codebase** target (matching an emitted
   cc_library/cc_binary/cc_library-interface) that lands in none of
   `deps` / `implementation_deps` / `data` is a silent dropped edge —
   the exact class PR #302 fixed (INTERFACE-library arms). It surfaces on
   stderr, as the `coverage` column in the survey summary, and as
   `coverage.json` via `--audit-coverage-report`. Conservative by
   construction (skips `::`-namespaced alias/imported arms and any name
   that isn't an emitted target, so system libs and out-of-tree imports
   don't false-positive); **0 is the healthy state** (the whole corpus
   reads 0 — the fix holds). The broader codemodel→BUILD differ is
   deliberately *not* pursued (false-positive cost exceeds signal, per the
   relocation cases above).

## The build lens (4th lens, opt-in)

The three lenses above measure **convertibility** — did intent survive the
convert. They deliberately do *not* answer the end-to-end question: **does the
Bazel-native output actually `bazel build` green, with no cmake?** A project
can survey `0 / 0 / 0` (no rejections, no idiom gaps, no dropped edges) and
still not build — a missing `-I` include dir, an unfiltered `ctest` dashboard
`custom_command`, a header that wasn't wired as a declared input. The build
lens closes that gap.

Turn it on with `SURVEY_BAZEL_BUILD`:

- unset / `off` — (default) no build attempt; the `build` column shows `-`.
- `auto` / `on` — build only the curated near-clean starter set
  (`fmt libxml2 brotli` — projects that already survey clean, so a `FAIL` is a
  real regression, not expected external-dep noise).
- `all` — attempt every surveyed project (most will `FAIL` on unresolved
  standalone `find_package` deps — honest, but noisy).
- a name list (`"fmt,brotli"`) — exactly those.

How it builds (it mirrors **project B's wiring**, not a degraded shape): for
each selected project it does its own *clean* (non-`--diagnostics`) convert in
the **same faithful shape the survey diagnoses** (multi-config + split) plus
`--out-config-settings`, which emits the `//config` package the
`//config:build_type` `select()` arms resolve against — the one piece write-a
renders into project B that the bare converter leaves to the orchestrator. It
overlays the converted BUILD tree onto a copy of the source (stripping any
Bazel files the project *ships*, so the lens tests our output, not theirs),
synthesizes a minimal `MODULE.bazel` (rules_cc / rules_pkg / bazel_skylib /
local `rules_buildstream_bazel`), and runs `bazel build //...`. Needs
bazel/bazelisk on `$PATH`; absent → `skip(no-bazel)`.
`SURVEY_BAZEL_BUILD_TIMEOUT` (default 900s) bounds each build.

**The lens configures cmake STATIC** (`--cmake-define BUILD_SHARED_LIBS=OFF`,
applied globally before any per-project `CONVERT_FLAGS`). Bazel's `cc_library`
is always static-linked into a `cc_binary`, and the converter currently lowers
`SHARED_LIBRARY` → `cc_library` (no `cc_shared_library` yet — see ROADMAP), so
the lens must configure static for cmake's model to match what Bazel actually
links. A SHARED configure silently diverges — the converter collapses the `.so`
to static, and any project that compiles differently for shared vs static
builds wrong: curl's `tests/libtest` recompiles the curlx utility sources *only*
under SHARED ("part of the libcurl static lib — do not compile/link them again"
when static), so under a SHARED configure Bazel ends up with two copies of each
(the test's + the static-linked libcurl's) and the test binary SIGSEGVs at
`base+0` before `main`. Static configure makes cmake's own static/shared
conditionals fire the way Bazel links. The corpus has several SHARED-defaulting
members (curl / brotli / libxml2 set `option(BUILD_SHARED_LIBS ON)`; OpenBLAS
declares an explicit `_shared` target) that were silently static-collapsed
before this default; faithfully building the *shared* variant is the tracked
`cc_shared_library` ROADMAP item. A project that genuinely needs the shared
configure can re-enable it in its `.conf` `CONVERT_FLAGS` (a later cmake `-D`
wins).

The `build` column tokens: `ok` / `FAIL` (built or not), or a `skip(<why>)`
that didn't attempt the build — `skip(no-bazel)` (no bazel on `$PATH`),
`skip(rej)` (the project surveys with rejections; the lens contract is to skip
refusals rather than convert a tree that won't build), `skip(convert)` (the
clean convert itself failed), and `skip(copy)` (couldn't copy the source tree
into the build workspace). The rejection short-circuit also avoids paying for a
second convert on a project the diagnostic pass already flagged.

A `FAIL` is the start of a triage, exactly like a rejection: read
`<out>/<project>/build.log` for the first compile/load error. The lens is
strict on purpose — `//...` includes test targets, which for some projects
pull external test deps (e.g. fmt's tests need GoogleTest) that standalone
conversion can't resolve; scoping to the library targets is a possible
refinement if that noise outweighs the signal.

### Build-lens status (per project)

What the build lens has surfaced as the corpus is driven through it — recorded
the way the regression table records findings + the fixing PR. The `Build
lens` token is the survey's own `build` column (`ok` / `FAIL` / `skip(<why>)`);
queued follow-ups live in `ROADMAP.md`, not here.

| Project | Build lens | What it surfaced / fix |
| --- | --- | --- |
| **fmt** | `ok` | Green clean control (textual-source-include support + cross-package header / PRIVATE-include wiring). |
| **libxml2** | `ok` | Green (subdir `configure_file` output on the package-root include path; surveyed in project-B shape). |
| **brotli** | `ok` | Green. |
| **glm** | `ok` | Two parts. `include_prefix` for element-root-included headers under split re-homes the compiled `glm` lib to `//…/glm` (`include_prefix = "glm"`), so its whole `<glm/…>` surface resolves. The `test-*` targets build once the lens passes `--cmake-define CMAKE_CXX_FLAGS=-w` — glm's tests set `-Werror`, which Bazel's default-toolchain `-Wall` turns into a `-Wclass-memaccess` error inside glm's own `gtc/packing.inl` (`memcpy` over its packed vector types). `-w` inhibits the warning while keeping glm's C++ auto-detection intact; glm's own `GLM_DISABLE_AUTO_DETECTION` knob drops `-Werror` but also forces `GLM_FORCE_CXX_UNKNOWN`, zeroing `GLM_LANG` and breaking the `std::hash` tests, so it isn't usable here. See the `-Werror` note below. |
| **abseil** | `ok` | **Green — 639/639 targets build.** abseil's `target_include_directories(${PROJECT_SOURCE_DIR})` PRIVATE root grant made the `rel==""` root-walk pull **all ~397 element headers into every such target's `hdrs`** (`spinlock_wait` and `time_zone` had byte-identical 397-hdr IR); under split the cross-package ones became `//…` labels → fail `allPackageLocalHdrs` → block `include_prefix` → the target's own `absl/…` includes didn't resolve (~363 failures across ~10 packages — the broad blocker, not cctz-specific). The split emitter now detects when a `RootInclude` target's walked surface spans **more than one package** and re-homes it into per-package header libs — each carrying that package's headers with `include_prefix=<pkg>`, or `includes=["."]` for the root-owned headers in non-package dirs (`absl/meta`, …) — behind one aggregate `cc_library` every such target depends on. That mirrors the cmake grant (any such target may include any header under the root), so it needs **no per-TU include scan** (robust to quote-vs-angle include form). glm's single-package shape is untouched (keeps the `include_prefix`-on-target path). The last 2 residuals were both edges of one orphan INTERFACE library — `absl_heterogeneous_lookup_testing`, which abseil declares WITHOUT `TESTONLY` so it is always emitted, yet links two deps absent from a testing-off build: (1) `GTest::gmock`, a `find_package(GTest)` IMPORTED target — now routed through the imports manifest (the trace-synth INTERFACE path gained the same `imports.LookupCMakeTarget` step the codemodel path has) to the BCR googletest module's `@googletest//:gtest` (gmock is fused there); (2) `absl::test_instance_tracker`, abseil's own `TESTONLY` helper the macro never creates without `ABSL_BUILD_TESTING` (which would also emit every cc_test) — its dangling edge is now dropped by `pruneDanglingTraceInterfaceDeps`, a post-pass that prunes `:`-local deps on trace-synth INTERFACE libs naming no emitted target. Build-lens recipe in `scripts/build-lens/abseil.conf` (imports manifest + `googletest` bazel_dep + `--incompatible_autoload_externally` so the BCR googletest module's load-less BUILD survives bazel 9). |
| **googletest** | `ok` | Green. gtest/gmock libs build via the fused-source `textual_hdrs` fix (`gtest-all.cc` textually `#include`s its sibling `src/*.cc`); the `cmake_config_bundle` builds via the **opt-in** install(EXPORT) config-mode generation (`--emit-install-export-config`, which the build lens passes) — a `write_file` generates the real `GTestTargets.cmake` (`GTest::gtest`/`gmock`/… imported targets + `IMPORTED_LOCATION` under `${_IMPORT_PREFIX}`). Default converts omit the bundle (the orchestrated graph wires its own synthprefix-synthesized one). |
| **zstd** | `ok` | **Green via a repo-root overlay** (`scripts/build-lens/zstd.conf` → `ELEMENT_SOURCE_ROOT`). zstd is a **subdir-cmake** project: its buildable CMake root is the `build/cmake` subdir (the surveyed dir), but its actual library + program sources live at `repo/lib` + `repo/programs` — siblings of `build/`, *outside* the cmake root (`build/cmake/lib/CMakeLists.txt` does `file(GLOB ${LIBRARY_DIR}/common/*.c)` where `LIBRARY_DIR=${CMAKE_CURRENT_SOURCE_DIR}/../../lib`). The converter already lifts this correctly (the #303 umbrella/workspace-root path): pointed at `build/cmake` it walks up, finds the repo root via `.git`, and anchors every source label to it — emitting the cmake-glue library target in `elements/zstd/build/cmake/lib/BUILD.bazel` with `srcs = ["//elements/zstd/lib:common/debug.c", …]`. The **only** gap was the build lens overlaying just the `build/cmake` subtree, so the real `lib/` + `programs/` source packages weren't on disk and those labels dangled (`missing input file '//elements/zstd/lib:common/debug.c'`). The new `ELEMENT_SOURCE_ROOT` knob redirects the overlay to the repo root while cmake still configures at `build/cmake` (the `--source-root`); the whole repo then stages under `elements/zstd/` and `bazel build //...` compiles the real zstd library + CLI green (108 actions, 12 targets — `libzstd_shared`/`libzstd_static` + the `zstd` program). The earlier non-modelable command-edge filters still apply: `create_symlink` tool aliases (`unzstd`/`zstdcat`/`zstdmt` → `zstd`), the `ninja clean` target, and source-less `cmake -E copy` manpage edges. The 2 `empty-cc-library` idioms are the synthesized header libs for the (header-free) `build/cmake/lib` + `build/cmake/programs` glue dirs — non-build-blocking. (zstd also earns its corpus place on the *convertibility* lenses + as the subdir-under-umbrella regression guard, #303.) |
| **glog** | `ok` | **Green (three parts).** Its only 2 rejections are benign forward-declared-include notices (`$<TARGET_PROPERTY:…>` / `glog/`), a no-op in strict mode; the survey's `skip(rej)` gate now subtracts that benign class so the lens actually exercises glog. The empty-placeholder source `CMakeFiles/glog.cc` (CMake's `_glog_EMPTY_SOURCE`, a `cmake -E touch` recovered from ninja) builds via the recovered-genrule subdir-output `$(RULEDIR)` anchor. The 9 `cc_test`s reference glog's internal `-fvisibility=hidden` symbols (`GetExistingTempDirectories`, `g_logging_fail_func`, `SafeFNMatch_`, …); Bazel's default dynamic linking builds glog as a `.so` that doesn't export them, so the lens passes `--dynamic_mode=off` to link them statically (matching glog's own static test build + the converter's `linkstatic=True`). Remaining: 30 `raw-toolchain-feature-flag` idiom findings (non-build-blocking — a separate idiom-lens follow-up). |
| **eigen** | `ok` | **Green as the header-only library.** eigen is HEADER-ONLY with no `find_package`, so the header library converts to a single `cc_library` and builds. The build lens (`scripts/build-lens/eigen.conf`) disables the parts of eigen's tree that aren't the library: `EIGEN_BUILD_BLAS`/`EIGEN_BUILD_LAPACK` (eigen's bundled **Fortran reference BLAS/LAPACK** — no Bazel Fortran ruleset exists, genuinely unsupported, **deferred**), `EIGEN_BUILD_TESTING` (its ~900-target `-Werror` SIMD test suite — a separate dev surface), and `EIGEN_BUILD_DOC`/`EIGEN_BUILD_DEMOS` (the `doc/examples` + `doc/snippets` + `unsupported/doc/examples` + `demos/` programs — documentation/demo `cc_binary`s that fail to resolve `<Eigen/Dense>` in the converted shape, again dev surface not the library). One general converter fix was needed: eigen's `uninstall` maintenance target runs `cmake -P cmake/EigenUninstall.cmake`, and the cmake-internal-command filter keyed only on the conventional `cmake_uninstall.cmake` script name — it now matches any `-P` script whose basename contains "uninstall" (case-insensitive), catching project-specific names like eigen's. The diagnostic survey (full tree, no conf) still reports 16 `fortran-target-needs-ruleset` idioms for the Fortran BLAS/LAPACK — non-build-blocking; the deferred Fortran surface. |
| **cutlass** | `ok` | **Green as the header-only library** (CUDA tier), like eigen. cutlass's core is a HEADER-ONLY C++ template library (`project(CUTLASS … LANGUAGES CXX)`; `include/` is all `.hpp`/`.inl`) — every `.cu` device source lives in the unit tests / examples / `tools/` library / profiler, which are dev surfaces. The build lens (`scripts/build-lens/cutlass.conf`) disables them (`CUTLASS_ENABLE_TESTS/EXAMPLES/TOOLS/PROFILER/LIBRARY=OFF`) and converts to the single header `cc_library(name = "CUTLASS")` (822 hdrs, `strip_include_prefix = "include"`), which builds with the plain cc toolchain — **no nvcc needed to COMPILE the header library, only to CONFIGURE** (cutlass's `CUDA.cmake` does `enable_language(CUDA)` + `find_package(CUDAToolkit REQUIRED)` unconditionally, so cmake configure — which the converter runs for the codemodel — needs a CUDA toolkit; provision it with the hook's `BSB_PROVISION_CUDA=1`). The full-tree diagnostic survey reports 1 rejection (a `cmake -E env` execute_process in `tools/library` — disabled in the lens build), so the conf sets `BUILD_LENS_IGNORE_REJ=1` to bypass the cost-optimization skip(rej) gate (the build lens's own clean convert is the real test; it returns skip(convert) if it fails). The CUDA-compile path (rules_cuda + the converter's cuda_library lowering, below) is built and proven but not exercised by the header-library build — it's what cuda-samples needs. |
| **OpenBLAS** | `ok` (two variants) | **Surveyed in TWO variants off the same checkout** — `openblas` (the real **Fortran** reference LAPACK) and `openblas-clapack` (the f2c'd **C** LAPACK). Each exercises distinct converter machinery, so a regression in one shows as a build-status flip on that member while the other stays green. **C path** (`openblas-clapack.conf`, `NOFORTRAN=1 C_LAPACK=1`): the whole ~2460-target kernel + LAPACK + BLAS library `bazel build //...`s with no cmake, via (1) `stageGeneratedSourceRootIncludes` rewriting the ~1951 GenerateNamedObjects wrapper `#include "<srcroot>/…kernel.c"` to workspace-relative + staging the kernel (.c/.S) as a `textual_hdr`; (2) `textualIncludeClosure` staging the transitive micro-kernel chain (`caxpy.c`→`caxpy_microk_haswell-2.c`); (3) `classifyFileRename` recovering `config.h` (written via `file(RENAME)`, not `configure_file`) as a COPYONLY bake; (4) the compile-group split (`splitCompileGroups`) partitioning generated sources back into per-compile-group sub-cc_libraries by the codemodel's `SourceIndexes` — so the per-source `COMPILE_OPTIONS "-mfma"` FMA kernels (*rot_k*, dgemv_t_k) compile in their own sub-library WITH `-mfma` and the 65 generated `.S` ASM TUs compile WITHOUT the C-only `-mfma`/`BUILD_COMPLEX*` flags, matching cmake exactly (this retired the former build-wide `CMAKE_C_FLAGS=-mfma`; compile-db `copt_mismatch`/`define_mismatch` both 0, each was 65 before). **Fortran path** (`openblas.conf`, default): the real netlib reference LAPACK (`LAPACK_OVERRIDES`, 1,997 `.f`) compiles via the new `fortran_library` rule (gfortran through the cc toolchain's own driver → CcInfo; see `retagFortranTargets`). Fortran **module ordering** is handled like cmake's own `Fortran.dd` scanner — `splitFortranModuleSrcs` detects the 2 module-defining sources (`la_constants.f90`, `la_xisnan.F90`), topologically orders them into a `module_srcs` attr the rule compiles first into a shared module dir (`-J`/`-I`). Validated end-to-end: a C program links `:openblas_static` and `dgesv` solves a linear system correctly. Both variants set `NO_CBLAS=1` (the CBLAS reference-test `cblas_xerbla` collides under `alwayslink`); neither needs a build-wide `-mfma` (the compile-group split supplies it per-source). `scripts/build-lens/openblas.conf` + `openblas-clapack.conf`. |
| **llvm** | `ok` (libraries; `LLVM_INCLUDE_TOOLS=OFF` — tools are the next in-scope step) | First whole-tree build-lens drive (survey `$(LLVM_DIR)/llvm`, the subproject). **Strict convert now clean** — the 4 `AddLLVM.cmake` `execute_process` blockers were each fixed *in the converter* (preferred over conf flags): (a) the `cc -Wl,--version` linker probe was mis-parsed (argv `/usr/bin/cc;-Wl,--version;-o;/dev/null` arrived as one `;`-joined token → driver `null`); `splitCMakeListArg` re-splits cmake list-valued COMMAND args so argv[0] is `cc` → probe → skip; (b)/(c) `cmake -E copy_if_different .../Extension.def.tmp → Extension.def` (and `ExtensionDependencies.inc`) bake the build-dir output bytes (`bakeBuildDirCopyOutput`, config.h-class — the cmake-E copy lifter no longer refuses a build-dir intermediate src); (d) `git rev-parse --git-dir` is a repo-*location* probe, not a value stamp — `gitRepoLocationQuery` classifies it `BucketProbe` → skip. With those, `bazel build //...` runs (rejections 390→194). **Umbrella/split desync — FIXED** (the root blocker): the converter spuriously promoted labelRoot to the monorepo root (on the `.git` above `llvm/`) even though LLVM is self-contained under `llvm/`, then applied the `llvm/` umbrella prefix INCONSISTENTLY across emitters (genrule srcs got it, install(FILES)/root refs didn't) → a self-inconsistent single/double package tree no overlay could satisfy. `sourcesEscapeCmakeSrc` now gates promotion on a real signal (a File-API-absolute source outside cmakeSrc, zstd's case), so LLVM stays cmakeSrc-rooted (consistent single `llvm`) and zstd still promotes. **With that the survey reads 0 rejections** (the spurious promotion had inflated them) and the `.exports` labels resolve. **Temporarily scoped to the LIBRARIES** (`LLVM_INCLUDE_TOOLS=OFF` +
`LLVM_ENABLE_BINDINGS=OFF`) to get the library graph green first — but **the
tools are IN-SCOPE**: LLVM is itself a likely Bazel toolchain (clang/llvm in
bootstraps), so `llvm-tblgen`/`llc`/`opt`/the `.exports` shared libs must build.
`LLVM_INCLUDE_TOOLS=OFF` is a stepping-stone, not the end state (bindings can
stay off — OCaml/Go are language bindings, not the toolchain). The tools surface
needs a **REQUIRED converter fix**: the **`.exports` in-place-rewrite** genrules
(`Remarks`/`LTO`/`bugpoint` version scripts) — redirect-aware in-place cmd
rewrite, since the cmd's input+output share one build-dir-relative token that
`renameRawCmdBuildOutputs` can't disambiguate and `anchorGenruleOutputsToRuledir`
drags both to `$(RULEDIR)` (root-caused MONOLITHIC). Sequence: tablegen `.td`
anchoring → libraries green → `.exports` fix → drop `LLVM_INCLUDE_TOOLS=OFF` →
tools green. Then a sequence of converter/conf fixes took the build from analysis-only deep into the **library compile graph (533 / 2,401 actions, 466 real compiles)**, each its own landed fix: **make_directory standalone no-op** (`isMakeDirOnlyCmd` drops a pure `cmake -E make_directory`/`mkdir` custom command — it declared a stamp output the mkdir never wrote, strip the `cd … &&` preamble first); **VCSRevision.h** (an unconditional `cmake -P GenerateVersionFromVCS.cmake` reading git `HEAD` — the standalone path now routes `cmake -P` through `bakeCmakeScriptGenrule` like the ninja-genrule path, baked at convert time via `--cmake-script-bake`); **BLAKE3** three fixes — co-locate per-language split sub-libraries in the parent's sub-package (`cc.SubParent`, was a cross-package private-visibility error), make C/C++ subs dep on the same-target asm/fortran subs (the C dispatcher calls hidden-visibility asm), and force `Linkstatic` on split subs (so no standalone `.so` whose link can't resolve the sibling subs' hidden symbols); **data-attr relabeling** (`rewriteDeps` now also rewrites the `data` attr's intra-element `:x` edges to cross-package labels — add_dependencies edges were dangling); plus the `--dynamic_mode=off` conf flag (LLVM is static-by-default). **tablegen `.td` input/`-I` path anchoring — FIXED in the converter.** The tablegen genrules (`GenVT.inc`, `Intrinsics*.h`) referenced their SOURCE `.td` input AND source `-I` roots as `$(RULEDIR)/…` (the genfiles/bin tree) — `llvm-min-tblgen` "Could not open input file". Two root causes, both generic: (1) `rewriteGenruleCmd` stripped a source-tree path to its bare *labelRoot-relative* form (`include/…`), but a genrule cmd runs at the Bazel **exec root**, so a source input/`-I` root needs its **exec-root** form (`<bazelPackagePath>/…` = `elements/llvm/include/…`) — the strip now anchors source paths there (was umbrella-only, which only covered the fidelity harness's convert-at-root case); (2) `anchorGenruleOutputsToRuledir` substring-matched output **parent-dir** tokens (`llvm/CodeGen`) anywhere, so it injected `$(RULEDIR)/` into the *middle* of the now-exec-root source path that shares the output's subdir — a left-`/` boundary guard now skips mid-path occurrences (real token boundaries, joined `-Iflags`, and `<o>.d` depfiles still anchor). `recordCodegenIncludeClosure` strips the package prefix off `-I` roots before resolving the `.td` closure against the on-disk (labelRoot-relative) source tree. Generic — keys on cmake/Bazel path shapes, not LLVM. Verified: the `GenVT.inc` genrule now reads `elements/llvm/include/llvm/CodeGen/ValueTypes.td` + `-Ielements/llvm/include`. **With it the LLVM library graph is GREEN**: `bazel build //...` completes successfully — **2,401 / 2,401 actions, no cmake** — building 96 archives including `libLLVMCore.a` (IR core) and `libLLVMX86CodeGen.a` (the X86 backend), the full tablegen tier + every `Intrinsics*.h`/`.inc` consumer, and the whole `Transforms`/`CodeGen`/`ExecutionEngine` tree. The lens build is fast-build (`-O0`, the cc_library `-O3`/`-g` copts sit behind `//config:release`/`debug` selects that default empty). **The tools build + RUN too** (`LLVM_INCLUDE_TOOLS=ON` converts clean — **0 rejections** — and analyzes green, 527 targets). Two more generic converter fixes were needed and landed: (a) **`.exports` in-place-output rename** — cmake generates a linker version script whose output basename equals its source symbol file (`Remarks.exports` from `Remarks.exports`), a Bazel src/out label collision; the output is renamed to a `.gen` sibling, and the rename is now applied AFTER anchoring on the canonical `$(RULEDIR)/<out>` form (cmake emits the output as a bare-basename `> ${native_export_file}` redirect that the old raw-cmd rename never matched). (b) **host system-library link** — `find_package(ZLIB)` resolved to `/usr/lib/.../libz.so` but, with no imports-manifest entry, was tag-only; it now lifts to a `-lz` linkopt (a HOST dependency — see the ROADMAP item on making that explicit), without which every tool executable that pulls zlib's compression code (`opt`/`llc` → `compress2`/`crc32`/…) failed the final link (a static `.a` tolerates the undefined symbols; an executable doesn't). A third generic fix completed the tool surface: (c) **generated-header (`.inc`) consumers for `cc_binary`/`cc_test`** — LLVM's tools are `cc_binary` that `#include "Opts.inc"` (the `-gen-opt-parser-defs` tablegen output from each tool's `Opts.td`, wired in cmake via `add_public_tablegen_target`); the codegen-consumer pass that resolves a target's tablegen UTILITY deps to the generated `.inc` and has split synthesize a `generated_includes` wrapper lib + genfiles include was gated to `cc_library`, orphaning the tools' `Opts.inc` (`Opts.inc: No such file`). The split wrapper synthesis keys on consumer NAME not kind, so extending the gate wires the binaries unchanged. **Verified by EXECUTION on the full tool tree** via a disk-bounded cycle (build each tool → run `--version` → delete its outputs before the next, so all 83 never coexist on disk): **78 / 83 tools build, 68 run** (`llc`/`opt`/`llvm-as`/`llvm-dis`/`llvm-link`/`llvm-mc`/`llvm-nm`/`llvm-objdump`/`llvm-readobj`/`llvm-objcopy`/`llvm-dwarfdump`/`llvm-symbolizer`/`dsymutil`/`llvm-ar`/…). The full IR pipeline runs on converter-built binaries — `llvm-as` (`.ll`→`.bc`) → `opt -O2` → `llc` emits real X86 assembly (`add(i32,i32)` → `leal (%rdi,%rsi), %eax; retq`); the `.exports` genrule emits a valid `LLVM_20.1 { global: … }` version script. The **5 residual fails are niche/genuine**, not converter-core gaps: `llvm-c-test` (×3 — the LLVM-C *API test harness*, needs C-API symbol linkage), `llvm-config` (its special `LibraryDependencies.inc` component-map codegen), and `llvm-rc` (the Windows resource compiler, missing `ResourceScriptTokenList.def`). **Why `LLVM_INCLUDE_TOOLS` is still OFF in the committed conf: DISK, not converter capability.** A full `bazel build //...` with all of LLVM's tools links dozens of statically-linked executables on top of the ~14G library tree — more than the web sandbox's ~38G writable layer holds. The conf keeps tools off to stay green on the sandbox; the converter handles them (proven above) and `TOOLS=ON` is the end state on a larger disk. `scripts/build-lens/llvm.conf`. (Build-lens disk note: LLVM's tree is ~14G; on the ~38G layer, do NOT add a `--disk_cache` — it stores a second copy of every output and fills the disk mid-link. The out-dir's own `.bzcache` under `/home` is enough for resume-after-reclaim without the 2× duplication.) |
| **curl** | `ok` (library + CLI + test surface build green, ~238 actions, no cmake) | Converts **clean — 0 rejections**; **`bazel build //...` is GREEN with `BUILD_TESTING=ON`** — libcurl, the full curl CLI, and the test codegen, with all ~298 `EXCLUDE_FROM_ALL` test executables emitted + buildable on request. Five generic converter fixes drove it: **(1) cross-package generated-output visibility** — curl's `tool_hugehelp.c` genrule in `//src` consumes the `curl.txt` output of a root-package genrule; a genrule's outputs inherit its visibility, and the producer was private. `emit/split` now publicizes any producer whose generated output is referenced directly (in srcs) by a consumer in a different package (`splitPlan.publicize`, off `genOutProducer`; the `.inc`-via-wrapper path is unaffected). **(2) `add_definitions()` → `local_defines`** — cmake's directory-scoped `add_definitions(-DBUILDING_LIBCURL)` is PRIVATE (never INTERFACE-exported), but the codemodel folds it into every in-dir target's effective `Defines` untagged, so it was emitted as a transitive Bazel `defines` that **leaked onto the curl tool** linking libcurl. The tool compiles the same sources WITHOUT `BUILDING_LIBCURL` so `lib/curl_base64.h` aliases `Curl_*`→`curlx_*`; inheriting the macro left `var.c`'s `curlx_base64_encode` undeclared. The shadow trace now captures `add_definitions` and `lower` routes those defines to `local_defines` (a PUBLIC/INTERFACE `target_compile_definitions` of the same string still wins). **(3) non-`ALL` `add_custom_target` → `manual`** — curl's `test-ci`/`test-am` `runtests.pl` wrappers (no `ALL`, shell `$TFLAGS`, no real output) aren't in cmake's default build; the converter tags their stamps `manual` (out of `//...`). **(4) ninja-genrule exec-root + output anchoring** — `recoverGenrule` predated the exec-root anchoring, so test codegen like `perl mk-lib1521.pl < include/curl/curl.h lib1521.c` left the source input project-relative (unresolvable at exec root) and the output un-`$(RULEDIR)`-anchored; it now threads `BazelPackagePath` and anchors all outputs. **(5) cd-stripped output anchoring** — that output is recorded in cmake's `WORKING_DIRECTORY`-relative form (the `cd` preamble stripped), so `anchorGenruleOutputsToRuledir` now falls back to the longest path-suffix present in the cmd. `find_package(ZLIB)` lifts to the host `-lz`. **The lens configures STATIC (`BUILD_SHARED_LIBS=OFF`) to match Bazel's link model (cc_library is always static-linked).** This isn't cosmetic: curl's `tests/libtest/CMakeLists.txt` drops the curlx utility sources (`warnless.c`/`curl_multibyte.c`/`timediff.c`/threads) from each test when `LIB_SELECTED STREQUAL LIB_STATIC` ("part of the libcurl static lib — do not compile/link them again"). Configured SHARED, the tests RECOMPILE those sources, and since Bazel then ALSO static-links libcurl, each test binary ends up with two copies — which **corrupts startup: the test binary SIGSEGVs at base+0 before `main`** (a NULL/duplicate-resolved function pointer). Static configure makes the cmake model match Bazel's link model so the dedup fires; verified by EXECUTION — `unit1300`/`unit1302` pass (`Test ended with result 0`), and `lib1156` fails-to-connect gracefully (exit 1, "bad error code (7)") byte-for-byte like the cmake reference build instead of crashing. The lens keeps `BUILD_LIBCURL_DOCS=OFF`+`ENABLE_CURL_MANUAL=OFF` (the `docs/` tree is manpage generation — documentation, not library/test code). |
| protobuf, … | `skip(rej)` | Honest external `find_package` deps (resolved in a real `.bst` element graph, not standalone). |

**`-Werror` projects vs. the toolchain's `-Wall`.** A project that builds clean
under its *own* cmake can still `FAIL` the build lens when it sets `-Werror`
and Bazel's default C/C++ toolchain adds `-Wall` that the project's cmake build
omits — the union promotes a latent warning to an error (glm's
`-Wclass-memaccess` on its own headers; glm's `test/CMakeLists.txt` sets
`-Werror` but deliberately *not* `-Wall`). The converter faithfully reproduces
the project's flags — this is toolchain-flag strictness, not a conversion
fault. The **build lens** neutralizes it at configure time, the same way it
opts into install-export generation: `--cmake-define CMAKE_CXX_FLAGS=-w`
inhibits the warnings so `-Werror` has nothing to promote, scoped to the
"does it build" check (production converts don't pass it). Prefer `-w` over a
project's own "no -Werror" knob when that knob has side effects — glm's
`GLM_DISABLE_AUTO_DETECTION` also forces `GLM_FORCE_CXX_UNKNOWN`, which breaks
its `std::hash` tests.

**Iterating fast on one project.** The build half of `run-survey.sh` does a
fresh source copy + a cold bazel output root every run. When iterating on a
converter fix against a single project, keep a *persistent* workspace + bazel
`--output_user_root` and only re-convert + incrementally rebuild: after each
`make converter` the loop is a few seconds (warm cache) instead of a cold
fetch + full analysis. Drop the workspace when done (the bazel cache is large).

### Large-project (LLVM-scale) build-lens playbook

LLVM (~2,400 actions, a ~14G build tree, ~83 tool executables) doesn't fit
the naive "`bazel build //...` and wait" loop on the web sandbox. The recipe
that drove it green:

- **Disk is the binding constraint, not CPU.** The sandbox's writable layer is
  ~38G; LLVM's library tree alone is ~14G. **Do NOT pass `--disk_cache`** for a
  project this size — it stores a *second* copy of every action output and fills
  the disk mid-link (the build dies with a misleading `as: … .o: No such file
  or directory`, which is ENOSPC, not a compile error). The out-dir's own
  `--output_user_root` action cache is enough; put `SURVEY_OUT_DIR` under
  **`/home`** (a `/tmp` reclaim spares `/home`) so a reclaim mid-build resumes
  from cache rather than recompiling.
- **A disk-full failure poisons resume.** When the disk hits 100%, bazel can't
  write its action cache either, so a "resume" rebuilds from scratch. Keep
  enough headroom that the build never fills the disk and the warm server +
  `.bzcache` resume near-instantly.
- **Many large executables → cycle, don't accumulate.** A full `//...` with all
  of LLVM's tools links dozens of static executables that won't coexist on
  ~38G. Build them **one batch at a time, deleting each tool's outputs
  (`bazel-bin/<pkg>/<tool>` + `_objs/<name>`) before the next batch**, so disk
  stays flat. This proves every tool builds/runs without ever holding them all
  at once. (That's why `llvm.conf` keeps `LLVM_INCLUDE_TOOLS=OFF` for the
  committed lens — a disk scoping, not a converter gap; see the llvm row.)
- **Bump the lens timeout.** `SURVEY_BAZEL_BUILD_TIMEOUT` defaults low; a cold
  LLVM build needs ~40-50 min on 4 cores (`-c fastbuild`, already `-O0`).
- **Always rebuild the converter from source.** `run-survey.sh` now `go build`s
  it every run; never trust a `build/bin` binary left over from an earlier
  checkout (a stale one silently drops fixes the source already has).
- **Watch progress with a time-based poll of the log**, not a `grep` for exact
  `[N / total]` milestones (bazel's counter jumps and skips round numbers) and
  not a `pgrep` whose pattern matches the monitor's own command line.

## The corpus

The corpus is curated for **complementary high-signal coverage**, not
breadth for its own sake. Each project earns its place by exercising
patterns the others *don't* — a new member should expose a converter gap
(or guard a fixed one) that the existing set misses, so the "Why it's in
the corpus" column reads as a distinct capability, not a duplicate. The
full list is meant to be read together: between them they should cover
the idiom + intent surface the converter has to handle. When expanding,
prefer a codebase that stresses an unrepresented shape (a new codegen
style, dependency idiom, install/export pattern, generator quirk) over
one more project that looks like abseil.

| Project | Why it's in the corpus | Source | Fetch |
| --- | --- | --- | --- |
All versions/dirs are pinned as overridable `*_VERSION` / `*_DIR` vars in
the Makefile (`make fetch-*` clones each at its pinned tag).

| Project | Why it's in the corpus | Source (pinned tag) | Fetch |
| --- | --- | --- | --- |
| **abseil** | Deeply modular; many INTERFACE deps-only wrapper libraries (exercises interface-library synthesis); doubles as the feature-flag idiom oracle. | github.com/abseil/abseil-cpp (`ABSEIL_VERSION`) | `make fetch-abseil` |
| **protobuf** | `find_package` deps (ZLIB), protoc custom-command codegen, install(EXPORT) config-mode producer. | github.com/protocolbuffers/protobuf (`PROTOBUF_VERSION`) | `make fetch-protobuf` |
| **googletest** | `enable_testing()` + add_test / gtest_discover_tests; INTERFACE genex defines (`$<BUILD_INTERFACE:...>`). | github.com/google/googletest (`GTEST_VERSION`) | `make fetch-googletest` |
| **eigen** | Header-only INTERFACE library; config-mode export/components. Also bundles reference BLAS/LAPACK `.f` Fortran (surfaces the `non-cc-language-source` idiom). | gitlab.com/libeigen/eigen (`EIGEN_VERSION`) | `make fetch-eigen` |
| **fmt** | Small, clean modern lib; `target_compile_features` C++-standard propagation. The high-signal clean control — converts 0/0/0. | github.com/fmtlib/fmt (`FMT_VERSION`) | `make fetch-fmt` |
| **SDL** | Heavy platform-conditional source selection (37 `if(WIN32/APPLE/LINUX/...)` blocks) + Objective-C (`.m`) sources + `target_precompile_headers`. Stresses the platform-source-partition path + the objc language surface. | github.com/libsdl-org/SDL (`SDL_VERSION`) | `make fetch-sdl` |
| **curl** | Heavy `find_package` consumer — OpenSSL + ZLIB linked across hundreds of targets. ~1248 `find-package-dep-unresolved` findings, all the external/resolves-in-graph class (count inflated by the same libs re-linked everywhere); 0 rejections/coverage. Stresses the find_package path. | github.com/curl/curl (`CURL_VERSION`) | `make fetch-curl` |
| **grpc** | Deep transitive deps + many `install(FILES)` directives + bundled `third_party` (zlib submodule). Surfaced the install_files name-collision bug (`include/grpc` vs `include/grpc++`). Needs `--recurse-submodules` to configure. | github.com/grpc/grpc (`GRPC_VERSION`) | `make fetch-grpc` |
| **llvm** | Large stress test; `ENABLE_EXPORTS`, PCH, TableGen generated sources, forward-declared include dirs. | github.com/llvm/llvm-project (`LLVM_VERSION`) — **survey the `llvm/` subdir** | `make fetch-llvm` |
| **VTK** | Large; heavy `cmake -P` codegen (`vtkEncodeString`), `target_precompile_headers`, version-stamp probes. | github.com/Kitware/VTK mirror (`VTK_VERSION`) | `make fetch-vtk` |

`fetch-survey` fetches the cheap four (abseil, protobuf, googletest,
eigen). llvm and vtk are large, so fetch them explicitly with
`make fetch-llvm` / `make fetch-vtk`.

## Regression corpus

These projects were each surveyed in past sessions; most surfaced a
concrete converter bug (since fixed) and the rest are clean controls.
They're kept fetchable (`make fetch-survey-regression`, or one
`fetch-<name>` at a time) so a survey re-run guards against
regressing the fix — that's the whole point of keeping them around.

| Project | What it surfaced | Fixing PR | Source (pinned tag) | Fetch |
| --- | --- | --- | --- | --- |
| **Boost.Core** | Alias-target lift ordering | #300 (merged) | github.com/boostorg/core (`BOOSTCORE_VERSION`) | `make fetch-boost-core` |
| **googletest** | install-path double-prefix | #301 (merged) | already in the default corpus (`GTEST_VERSION`) | `make fetch-googletest` |
| **abseil-cpp** | INTERFACE deps not routed | #302 (merged) | already in the default corpus (`ABSEIL_VERSION`) | `make fetch-abseil` |
| **zstd** | workspace-root umbrella picks `build/` | #303 | github.com/facebook/zstd (`ZSTD_VERSION`) | `make fetch-zstd` |
| **libevent** | pre-committed generated sources refused | #304 | github.com/libevent/libevent (`LIBEVENT_VERSION`) | `make fetch-libevent` |
| **mbedtls** | wrapped `ctest -D Experimental` dashboard target lifted instead of filtered | fixed (`isCMakeInternalCmd` dashboard filter) | github.com/Mbed-TLS/mbedtls (`MBEDTLS_VERSION`) | `make fetch-mbedtls` |
| **libxml2** | clean — no converter bugs found | n/a | github.com/GNOME/libxml2 (`LIBXML2_VERSION`) | `make fetch-libxml2` |
| **brotli** | clean — no converter bugs found | n/a | github.com/google/brotli (`BROTLI_VERSION`) | `make fetch-brotli` |
| **cutlass** | NVIDIA CUDA project (header-heavy) | (prior session) | github.com/NVIDIA/cutlass (`CUTLASS_VERSION`) | `make fetch-cutlass` |
| **cuda-samples** | NVIDIA CUDA sample suite | (prior session) | github.com/NVIDIA/cuda-samples (`CUDASAMPLES_VERSION`) | `make fetch-cuda-samples` |
| **OpenBLAS** | assembly kernels + Fortran/LAPACK + arch-conditional source selection + ~2460 targets — scale + shapes nothing else has | name-collision robustness (add_test test-name == a different target's name) | github.com/OpenMathLib/OpenBLAS (`OPENBLAS_VERSION`) | `make fetch-openblas` |
| **glog** | unresolved-genex include dir (`$<TARGET_PROPERTY:glog,INCLUDE_DIRECTORIES>` on the trace-synthesized `glog_test` INTERFACE library) aborted `--split-packages` — it became a header-lib root whose name `$<…>_headers` is not a valid Bazel identifier | fixed (`dropGenexIncludeDirs` in ToIR + a `planSplit` genex-root backstop) | github.com/google/glog (`GLOG_VERSION`) | `make fetch-glog` |
| **glm** | a header-only INTERFACE lib (`glm-header-only`) whose include path is the source root (`$<BUILD_INTERFACE:${SOURCE_DIR}>`) emitted **empty** (the root include was skipped) and the `glm → glm-header-only` PUBLIC link edge was dropped (the codemodel omits it; cmake bakes the usage requirements in) | fixed (`lowerInterfaceLibraries` root-walk so the INTERFACE lib owns its headers + `routeTraceInterfaceLibDeps` routes the trace edge) | github.com/g-truc/glm (`GLM_VERSION`) | `make fetch-glm` |

Per-project survey caveats (faithful-survey rules, same spirit as the
llvm-subdir note below):

- **abseil (cmake 4 + probe-genex) — fixed:** abseil
  (`ABSEIL_VERSION` 20260107.1) used to fatal-error at configure under
  cmake 4.x with the genex-probe hook on, because its non-TESTONLY
  `heterogeneous_lookup_testing` INTERFACE library DEPS the TESTONLY
  `absl::test_instance_tracker` (which abseil skips creating when testing
  is off), and the probe's `INTERFACE_LINK_LIBRARIES` `file(GENERATE)`
  forced evaluation of that dangling `::` reference. `probe-genex.cmake`
  now skips probing any target with an unresolvable `::` link-interface
  dep (see `TestProbeGenex_DanglingLinkInterface_LiveCMake`), so abseil converts
  with the probe on instead of crashing — no `--probe-genex=false` needed.
  Surveys clean (`0/0/0`) under the default `auto`+split.
- **zstd:** the buildable CMake root is the **`build/cmake` subdir**, not
  the repo root — survey `$(ZSTD_DIR)/build/cmake`. (This subdir-under-an-
  umbrella layout is exactly what #303 fixed.) The **build lens** still
  surveys at `build/cmake` but overlays the **repo root** into the element
  (zstd's sources are siblings of `build/`, outside the cmake root) via
  `ELEMENT_SOURCE_ROOT` in `scripts/build-lens/zstd.conf` — see the
  *Build-lens status* zstd row.
- **mbedtls:** 3.6.x pulls its test/build helpers from a `framework` git
  **submodule**; `make fetch-mbedtls` recurses submodules, otherwise
  configure fails with `framework/CMakeLists.txt not found`.
- **cutlass / cuda-samples:** both need a **CUDA toolkit (`nvcc`) on
  `PATH`** to configure (they `enable_language(CUDA)` /
  `find_package(CUDAToolkit REQUIRED)`). Provision it with the SessionStart
  hook's `BSB_PROVISION_CUDA=1` path (apt `nvidia-cuda-toolkit` + `gcc-12`).
  **cutlass is GREEN on the build lens** as its header-only library (see
  *Build-lens status* / `scripts/build-lens/cutlass.conf`) — the header
  library needs nvcc only to *configure*, not to compile. **cuda-samples**
  needs the `.cu` compile path: the converter now lowers `.cu` targets to
  rules_cuda's `cuda_library` / `cuda_binary` / `cuda_test` (the
  KindCudaLibrary path), the build lens injects rules_cuda + a CUDA toolchain
  via `scripts/build-lens/cuda-samples.conf`'s `EXTRA_BAZEL_DEPS`, and
  `scripts/provision-cuda-root.sh` assembles the self-contained CUDA root
  rules_cuda's local toolchain needs from Debian's scattered packaging. That
  pipeline is proven (a real sample TU compiles via nvcc); the remaining
  cuda-samples greening work (its force-set Blackwell-class arches that CUDA
  12.0 rejects, the shared `Common/` headers, and the `9_CUDA_Tile` group's
  `find_program(tileiras REQUIRED)` configure blocker) is enumerated in
  `cuda-samples.conf`.
- **Boost.Core:** a modular Boost library — its `CMakeLists.txt`
  configures for the standalone modular build; sibling-library
  `find_package` deps surface as honest `find-package-dep-unresolved`
  findings (resolved in a real element graph), like protobuf's ZLIB.
- **OpenBLAS:** survey with `-DNOFORTRAN=1 -DC_LAPACK=1` on hosts without
  a Fortran compiler (the C_LAPACK route replaces the Fortran reference
  LAPACK). The assembly + Fortran kernels aren't Bazel-modelable, so the
  surveyed value is the large C surface, the codegen, and the scale
  (~2460 targets). It surfaced an `add_test` whose test name equals a
  *different* executable target's name (an upstream copy-paste:
  `add_test(openblas_utest_ext <openblas_utest binary>)`), which made
  the converter synthesize a cc_test colliding with the real
  `openblas_utest_ext` executable; `disambiguateTestNameCollisions`
  renames the cc_test so the convert no longer hard-fails. Its remaining
  rejection is the `getarch` arch-detection `execute_process` (genuinely
  not Bazel-modelable). OpenBLAS's top-level
  `cmake_minimum_required(VERSION 2.8)` is below the floor cmake 4 dropped;
  the May-2026 cmake-4-pin resurvey confirmed `cmakerun.Configure`'s
  policy-floor retry lifts it (the survey log shows the
  `CMAKE_POLICY_VERSION_MINIMUM=3.5` re-run firing), so OpenBLAS surveys
  `ok` under cmake 4.3.3 with gfortran: 1 rejection (the getarch probe) +
  51 `fortran-target-needs-ruleset` idioms (the Fortran partition). libevent
  (floor 3.1) is lifted the same way.
- **OpenBLAS on the build lens (not yet `ok` — one converter feature short):**
  the build lens (`SURVEY_BAZEL_BUILD=openblas`) needs OpenBLAS surveyed
  0-rejection so the gate doesn't `skip(rej)`. `scripts/build-lens/openblas.conf`
  selects OpenBLAS's deterministic-arch (cross-compile) configure branch
  (`CMAKE_SYSTEM_NAME=Linux` + `CORE/TARGET=HASWELL`, plus `NOFORTRAN=1
  C_LAPACK=1`): that branch (cmake/prebuild.cmake:98) writes the arch config
  from a static per-core table, dead-branching the four getarch
  `execute_process(OUTPUT_VARIABLE)` probes — the rejection drops to 0 and the
  Fortran idioms to 0. Because the build-lens skip(rej) gate runs the
  *diagnostics* convert flag-less, a `.conf`'s build-time `CONVERT_FLAGS` alone
  can't lift it; `run-survey.sh` gained an opt-in per-project
  `DIAG_CONVERT_FLAGS` knob (default empty → no change to any other member)
  applied to that gate convert. Three split-package converter fixes then take
  the ~2460-target graph cleanly through Bazel load + analysis (each was a real
  general bug OpenBLAS is the first corpus member large enough to trip):
  (1) a synthesized include-root header lib (`root_headers`, `includes=["."]`)
  listed headers physically owned by SUBPACKAGES as bare same-package strings —
  invalid labels; now relabeled to cross-package file labels like the
  real-target path (`headerLibTarget`); (2) `exports_files()` must not be raised
  for a cross-package reference to a GENERATED file (write_file/genrule out) —
  it errors "source file conflicts with existing generated file" (a new
  `splitPlan.genOuts` index guards all four export sites); (3) a cross-package
  GENERATED compiled source (.c/.S) swept into a header aggregation is dropped
  entirely — it's a translation unit its package compiles, never a header, and
  relabeling it would only force the private write_file rule public for
  visibility. **GenerateNamedObjects absolute-include — fixed.** OpenBLAS's
  `GenerateNamedObjects` codegen (cmake/utils.cmake:421) `file(WRITE ...)`s
  ~1951 per-routine wrappers each `#include`-ing the real kernel by ABSOLUTE
  configure-time path (`#include "<source-root>/lapack/getf2/zgetf2_k.c"`);
  the converter baked that path verbatim, so the Bazel compile failed "No such
  file" (the convert-host path isn't in the sandbox). The `lower`
  `stageGeneratedSourceRootIncludes` pass now rewrites every source-root-
  absolute quote-`#include` baked into a generated wrapper (write_file) to a
  WORKSPACE-relative path (resolving through Bazel's default `-iquote`
  exec-root) and stages the included in-tree source (.c **and** assembly .S/.s)
  as a `textual_hdr` on the target that compiles the wrapper — a declared input
  that isn't compiled standalone, mirroring the on-disk fused-source idiom
  (`synthesizeTextualSourceIncludeLibs`, with which it now shares the
  carrier-lib / textual_hdrs wiring). All 1951 includes rewrite + stage; the
  build advances from analysis-clean to a real C compile of the kernels.
  **Transitive micro-kernel closure — fixed.** An OpenBLAS kernel a wrapper
  stages itself `#include`s sibling micro-kernel sources by relative path
  (`caxpy.c` → `caxpy_microk_haswell-2.c`, `#ifdef`-guarded per arch), so the
  staging is now the full transitive textual-include closure
  (`textualIncludeClosure`), not just the first hop — relative sibling includes
  need no rewrite (they resolve against the includer's own dir once staged).
  **config.h — fixed.** With the kernels resolving, the C compile next failed
  `common.h:62: fatal error: config.h: No such file`. In the deterministic-arch
  branch the `.conf` selects, OpenBLAS writes `config.h` via
  `file(RENAME ${tmp} config.h)` at cmake/prebuild.cmake:1374 — NOT
  `configure_file` (that's the non-cross branch, which needs the getarch probe
  the `.conf` dead-branches), so the converter's configure_file recovery never
  saw it. `shadow.classifyFileRename` now models an in-source-tree
  `file(RENAME <src> <dest>)` as a synthetic COPYONLY configure_file: the
  existing recovery bakes `config.h` from the build-dir bytes and the split
  emitter folds the recovered root-package header into `root_headers`, so it
  resolves through the element-root header lib exactly like `common.h`.
  **FMA per-source flags — build-lens flag.** Past config.h, the FMA kernels
  (`*rot_k*`, `dgemv_t_k`) fail `inlining failed … '_mm256_fmadd_pd': target
  specific option mismatch`: OpenBLAS tags those individual generated wrappers
  with `set_source_files_properties(... COMPILE_OPTIONS "-mfma")`
  (cmake/utils.cmake, gated on HAVE_FMA3), and the converter doesn't yet split
  per-source `COMPILE_OPTIONS` into a per-flag sub-library (Bazel has no
  per-source copts). The build lens pins a single `TARGET=HASWELL`
  (DYNAMIC_ARCH off — no runtime CPU dispatch), so `openblas.conf` passes
  `--cmake-define CMAKE_C_FLAGS=-mfma` to enable FMA build-wide (correct for the
  arch, the same build-lens-flag move as glm's `-w`). **Follow-up (converter):**
  per-source `COMPILE_OPTIONS` recovery (split the divergent sources into a
  sub-`cc_library` carrying the extra copts) would drop the conf flag — tracked
  here, mirrors the per-source `COMPILE_DEFINITIONS` handling already shipped.
  **CBLAS reference tests — build-lens flag, then GREEN.** With every kernel
  compiling, the last failure was linking the `ctest/` reference-test programs
  (`x?cblat?`): `duplicate symbol: cblas_xerbla`. Each test compiles its own
  `ctest/c_xerbla.c` (defining `cblas_xerbla`) AND links the library, whose
  `cblas_xerbla` is force-included because the converter inlines cmake OBJECT
  libraries as `alwayslink=True` cc_library deps — a real `.a` archive wouldn't
  pull the overridden object. `ctest`/`utest` are gated by `NO_CBLAS`/`ONLY_CBLAS`
  (NOT `BUILD_TESTING`, which only gates `lapack-netlib/TESTING`; `NOFORTRAN=1`
  already drops the Fortran `test/`), so `openblas.conf` sets `NO_CBLAS=1` to
  scope out the C-interface veneer + its reference tests — the same
  "build the library, not its test tree" move eigen/cutlass apply. **With that,
  OpenBLAS is GREEN: `0/0/0 ok ok`, the whole ~2460-target C-LAPACK library
  `bazel build //...`s clean.** Follow-up (converter): don't force a test
  target's own symbols in via an `alwayslink` dep (so the reference tests build
  too without `NO_CBLAS`). Real reference-Fortran LAPACK (no `C_LAPACK`) is
  separate future work — needs a Bazel Fortran ruleset (none exists in the BCR;
  `edbaunton/rules_fortran` is an empty stub).

### Optional toolchains (unlock fuller surveys)

Some members need a language toolchain beyond the default C/C++/cmake/
ninja to survey at full fidelity. None are required for the core corpus;
install them when you want the deeper surface.

- **Fortran (`gfortran`)** — lets OpenBLAS (and eigen's bundled reference
  BLAS/LAPACK) configure with their **real** Fortran path instead of the
  C-only fallback. Surveying OpenBLAS *with* gfortran is what surfaced
  the `non-cc-language-source` idiom (the converter emits `.f` sources
  into `cc_*` srcs, which Bazel's cc rules can't compile). On
  Debian/Ubuntu: `apt-get install -y gfortran` (≈ GNU Fortran 13). The
  portable default stays `-DNOFORTRAN=1 -DC_LAPACK=1` for hosts without
  it; install gfortran to see the Fortran-target gap.
- **CUDA toolkit (`nvcc` + a CUDA-12-compatible host `gcc`)** — required for
  **cutlass** and **cuda-samples** to pass cmake configure at all (they
  `enable_language(CUDA)` / `find_package(CUDAToolkit)`); without it the survey
  stops at configure. The hook's `BSB_PROVISION_CUDA=1` path installs apt's
  `nvidia-cuda-toolkit` (CUDA 12.0) plus `gcc-12`/`g++-12` (nvcc 12.0 caps the
  host compiler at gcc 12). cutlass's **header library** build needs nvcc only
  to configure. To actually **compile `.cu`** (cuda-samples), two more pieces:
  (1) `scripts/provision-cuda-root.sh` assembles a self-contained CUDA root
  (Debian scatters CUDA across `/usr`; rules_cuda's local toolchain wants one
  tree), and (2) the build lens points rules_cuda's `cuda.toolkit` at it and
  steers nvcc's `-ccbin` at gcc-12 via `--repo_env=CC` — both wired by
  `scripts/build-lens/cuda-samples.conf`. Building device code that *runs*
  additionally needs the NVIDIA driver (not required to compile/link).

These are container/CI-environment provisioning notes: the ephemeral
survey container doesn't ship every toolchain by default. The repo's
**SessionStart hook** (`.claude/hooks/session-start.sh`, registered in
`.claude/settings.json`) handles this for Claude-Code-on-the-web
sessions. It provisions:

- **gfortran** (default) — the OpenBLAS/eigen Fortran path.
- **buildifier** (default, via `go install`) — the lens-2
  canonical-form check, so `survey-gazelle` / the split gate don't pay a
  per-run install.
- **CUDA toolkit** (`BSB_PROVISION_CUDA=1`) — cutlass / cuda-samples;
  opt-in because it's multi-GB. Installs `nvidia-cuda-toolkit` + `gcc-12`
  (nvcc 12.0's host-compiler cap). For the `.cu` compile path (cuda-samples)
  also run `scripts/provision-cuda-root.sh` to assemble the self-contained
  CUDA root rules_cuda needs (it prints the path; export it as `BSB_CUDA_ROOT`
  for `scripts/build-lens/cuda-samples.conf`).
- **gazelle_cc toolchain warm** (`BSB_WARM_GAZELLE=1`) — pre-builds the
  `gazelle_cc` binary into the persistent survey cache
  (`SURVEY_GAZELLE_BZL_CACHE`) so the first `make survey-gazelle` is
  fast; opt-in because it's a ~2-minute cold build needing BCR egress.

The hook deliberately **does not install bazelisk**: the base container
already ships a working real `bazel`, and this sandbox blocks
`releases.bazel.build` (bazelisk's fetch 403s), so a bazelisk on PATH
would shadow + break the working bazel. The hook is web-only (gated on
`$CLAUDE_CODE_REMOTE`), idempotent, and non-interactive; local dev and CI
manage their own toolchains (CI via
`.github/actions/install-cmake-toolchain` and friends).

### Provenance / network notes

- The CI / sandbox network policy allowlists most hosts but **not
  `gitlab.kitware.com`** (it returns HTTP 403 "Host not in allowlist").
  That's the one exception: **VTK is fetched from the
  `github.com/Kitware/VTK` mirror** rather than its canonical GitLab home.
  Everything else fetches from its canonical home — including **eigen from
  `gitlab.com/libeigen`, which IS reachable** (only Kitware's GitLab is
  blocked, not gitlab.com generally).
- Fetches are **shallow** (`--depth 1`) — the survey only needs a tree to
  configure, not history.

## The intent-capture lens (6th lens, opt-in)

The lenses above are deterministic — they count what the converter *knows* it
couldn't do (rejections, idiom, coverage, the conversion-todos worklist) or
mechanically-diffable per-TU drift (compile-commands fidelity). The
**intent-capture** lens is the qualitative complement: an agent-as-oracle "what
did we miss?" pass that hunts the **silent** intent loss — a dropped test
target, an install layout, an option default, a visibility constraint, a
build-time codegen step — that compiles fine and is flagged nowhere. It's the
inverse of the conversion-todos producer, so a real miss it surfaces that isn't
already a todo/rejection is a **producer/lowering gap**.

It's opt-in and cost-gated because it calls an LLM. Turn it on with
`SURVEY_INTENT=1` and a pluggable judge command in `INTENT_LENS_JUDGE` (the
judge reads the prompt on stdin and writes the findings JSON on stdout).

**The judge must be a CAPABLE agent with filesystem access to the bundle.** The
prompt (`converter/cmd/intent-lens prompt`) hands the judge the project-derived
context it needs — that the output targets **Bazel 9** (native `cc`/`sh` rules
removed, so the BUILDs load `@rules_cc` / `@rules_shell` / `@rules_pkg` /
`@bazel_skylib` providers), that the BUILDs are **gazelle-cc-maintained**, and the
paths to the converted bundle (`MODULE.bazel`, every `BUILD.bazel`/`.bzl`) and
the original CMake sources — then asks it to READ them and report net-new misses.
So the judge needs (a) a model strong enough to reason over the bundle and (b)
read access to the bundle + cmake-source paths.

Local-CLI judge (`claude -p`) in the **remote/cloud environment** needs two
tweaks — the bare `claude -p` the older docs showed does NOT work here:

```sh
# - env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES: the remote shell exports this,
#   which makes a nested `claude -p` demand --output-format=stream-json; unset it.
# - --add-dir /tmp: the judge is sandboxed to the repo, but the survey out-dir +
#   cmake sources live under /tmp; grant access so it can read them. (Point the
#   survey at in-repo paths instead and you can drop --add-dir.)
# - SURVEY_SKIP_BUILD=1: run the convert + fidelity + intent lenses but skip the
#   (redundant, slow) `bazel build //...` — for refreshing lens rows on an
#   already-green corpus.
# - SURVEY_BAZEL_BUILD selects which projects the lenses act on (the lenses live
#   on the build-lens path); SURVEY_SKIP_BUILD then drops the build itself.
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
SURVEY_BAZEL_BUILD=fmt SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1 \
  scripts/run-survey.sh --out-dir /tmp/survey-lens fmt=$FMT_DIR
```

The judge is heavy (it reads the whole bundle): budget several minutes per
project and bump `SURVEY_BAZEL_BUILD_TIMEOUT` for big members (grpc/llvm/vtk list
100+ files). A non-`claude` judge is any command that takes the prompt on stdin
and emits the findings JSON on stdout.

For each build-lens-selected project it writes `<out>/<name>/intent-capture.json`
— the judge's findings, each triaged **net-new vs already-flagged** (deduped
against that element's own `conversion-todos.json` + `rejections.json`) and
bucketed by severity. The `summary.txt` table gains a **`missed` column** = the
net-new finding count (`-` when the lens didn't run). Because the judge is
non-deterministic the output is a **triage queue, not a pass/fail gate** —
net-new findings are the producer-gap candidates to investigate, and unlike the
other columns `missed` is **not comparable across runs** (a different judge pass
can return a different number); treat it as a pointer into `intent-capture.json`,
not a metric to diff. The pipeline (`scripts/intent-capture-lens.sh` →
`converter/cmd/intent-lens prompt|triage`) can also be run standalone on any
converted bundle; the deterministic halves are gated by
`scripts/meta-intent-capture-lens.sh` (stub judge). See the intent-capture lens
item in `ROADMAP.md` for the open scoring/grounding questions.

### Full-corpus lens snapshot (2026-06-08)

Both opt-in lenses were run over the **whole corpus** (judge = `claude -p`;
build lens skipped — the corpus is already build-green). The full per-member
output is committed under [`survey-artifacts/`](survey-artifacts/) (triaged +
raw judge findings, the exact prompts, and the signature-grouped fidelity
diffs). The numbers below are a **snapshot pointer into those artifacts**, not
a gate: `missed` (intent net-new) is judge-non-deterministic and not
comparable across runs (see the lens caveats above).

| Member | Fidelity (matched TUs) | Intent `missed` | net-new High |
| --- | --- | --- | --- |
| abseil | 156 | 7 | 2 |
| boost-core | —† | 6 | 2 |
| brotli | 36 | 8 | 3 |
| catch2 | 107 | 11 | 3 |
| cuda-samples | 0‡ | 0 | 0 |
| curl | 213 | 9 | 4 |
| cutlass | —† | 12 | 5 |
| eigen | —† | 8 | 2 |
| fmt | 29 | 7 | 2 |
| glm | 1 | 8 | 2 |
| glog | 20 | 0 | 0 |
| googletest | 4 | 5 | 2 |
| grpc | 1061 | 7 | 1 |
| libevent | 40 | 7 | 1 |
| libpng | 24 | 9 | 2 |
| libxml2 | 52 | 10 | 3 |
| llvm | 2054 | 29 | 14 |
| mbedtls | 113 | 14 | 5 |
| nlohmann-json | —† | 6 | 2 |
| openblas | 6277 | 9 | 3 |
| protobuf | 286 | 11 | 5 |
| sdl | 259 | 9 | 2 |
| spdlog | 8 | 10 | 3 |
| vtk | 4218 | 8 | 4 |
| zlib | 17 | 5 | 3 |
| zstd | —§ | 8 | 2 |

† **header-only** library — no `CppCompile` TUs, so no compile-db fidelity row
(`boost-core`, `cutlass`, `eigen`, `nlohmann-json`).
‡ `cuda-samples` `.cu` sources lower to `CudaCompile`, which the lens's
`CppCompile` aquery doesn't see (0 TUs).
§ `zstd` fidelity is **blocked by a real converter regression** (not a survey
artifact): split-emit's cross-package relabel emits an invalid subpackage
label `//elements/zstd:lib/libzstd.so` (where `elements/zstd/lib` is a
subpackage). zstd is otherwise docs-green, so this is a main-drift regression
to fix; tracked in `ROADMAP.md`.

#### Producer-gap themes (the intent backlog)

The net-new intent findings are **producer/lowering-gap candidates** — intent
the converter silently dropped. Across the corpus the **77 high-severity**
net-new findings cluster into six recurring themes (full detail, with
`evidence` + `cmake_ref` per finding, in each member's
`survey-artifacts/<member>/intent-capture.json`):

1. **Dropped link libraries** (25× high) — system/threading linkopts CMake
   resolves but the converter omits: `-lm` (`brotli`, `libpng`, `libxml2`),
   `-ldl` (`libxml2`, `llvm`), `-lpthread` (`googletest`, `spdlog`, `zstd`,
   `grpc`, `llvm`'s `${LLVM_PTHREAD_LIB}`). Also build-type-conditional
   defines hardcoded on (LLVM's `LLVM_ENABLE_ABI_BREAKING_CHECKS`,
   `LLVM_ENABLE_PLUGINS`, …, all forced `1` regardless of `//config`),
   dropped `target_compile_features` (`googletest`'s PUBLIC `cxx_std_17`),
   and `openblas`'s missing SONAME/VERSION (no versioned `.so` symlinks).
2. **Unmodeled install/export layout** (25× high) — the convert builds the
   artifacts but ships no install tree: no `pkg_files` for libs / public
   headers / binaries / `.pc` files (`curl`, `protobuf`, `zlib`, `sdl`,
   `libevent`, `fmt`, `openblas`, …), and the `find_package(CONFIG)` entry
   points (`<Pkg>Config.cmake` / `<Pkg>Targets.cmake`) are never generated
   (`eigen`, `catch2`, `zstd`, `cutlass`, `protobuf`, `nlohmann-json`).
   This is the single biggest cluster and the most mechanical to close.
3. **Absent targets / subpackages** (9× high) — whole targets with no
   `BUILD.bazel`: `abseil`'s 7 interface subpackages, `llvm`'s 19/20
   backends under default `LLVM_TARGETS_TO_BUILD=all`, `mbedtls`'s
   programs, `vtk`'s `VolumeAMR` module.
4. **Dropped test suites** (10× high) — `enable_testing()` trees lowered
   nowhere: `abseil` (232 `absl_cc_test`), `glm` (~130), `sdl` (~50),
   `catch2`, `boost-core`, `mbedtls`, `vtk`, `openblas`.
5. **Unrepresented codegen** (5× high) — `configure_file` / script codegen
   with no genrule: `vtk`'s libproj `proj_config.h`, `mbedtls`'s
   `test_certs.h`, `curl`'s `configurehelp.pm` (bakes a convert-time temp
   path), `cutlass`'s `version_extended.h`.
6. **Optional-feature deps** (3× high) — `LLVM_ENABLE_ZLIB` / `_ZSTD` /
   `_OPENCSD` conditional deps not linked, so `Compression.cpp` would fail
   to link.

These are the concrete converter-improvement leads; pick a theme and the
artifacts give the per-member evidence to drive a fix + a regression guard.
Each theme is tracked as its own entry in `ROADMAP.md` (the six intent-lens
producer-gap bullets, biggest cluster first).

## Running a faithful survey

The cardinal rule: **a survey number is only comparable to another survey
number taken the same way.** The pitfalls below all produced misleading
counts in practice.

### 1. Survey the project's real CMake root, not a monorepo wrapper

For multi-project monorepos, point the survey at the **buildable
subproject**, not the repo root:

- **llvm:** survey `llvm-project/llvm`, **not** `llvm-project/`. The
  monorepo root's CMake references sibling dirs (`benchmarks/`,
  `examples/*`) that a shallow clone may not populate, inflating the
  `unsupported-source-path` (missing-include-dir) count with pure
  clone artifacts. (This is the exact mistake that once made llvm look
  like it had ~450 rejections; the real, comparable number comes from
  surveying `llvm/`.)

### 2. Never reuse a source tree that's had an in-source `cmake` run

If you manually ran `cmake -S <src> -B <src>` (or any in-source configure)
against a corpus checkout, it leaves a `CMakeCache.txt` / `CMakeFiles/` in
the source tree. A subsequent survey can then trip the project's
in-source-build guard or read stale cache. Always survey a **pristine**
checkout:

```sh
git -C <src> clean -fdx        # or re-clone
```

The converter itself configures into an `os.MkdirTemp` build dir, so it
never dirties the source — the risk is only from manual probing.

### 3. Read the rejection codes; don't trust the headline count

The headline "N rejections" lumps together genuinely different things.
Always break down by `code`, and separate the **benign** category:

- `unsupported-source-path` with message *"include dir … doesn't exist on
  disk; treated as empty"* is **benign** — it's cmake's forward-declared
  include shape, which the converter handles by design. It's recorded as a
  synthetic rejection only so `--diagnostics` surfaces it. **Exclude it
  before comparing "real" refusal counts.**

```sh
# real rejections = total minus the benign missing-include-dir notices
python3 - <<'PY'
import json, collections
d = json.load(open("/tmp/survey-out/<project>/rejections.json"))
c = collections.Counter(x["code"] for x in d)
benign = sum(1 for x in d if "doesn't exist on disk" in x.get("message",""))
print("by code:", dict(c))
print("benign missing-include-dir:", benign)
print("real rejections:", len(d) - benign)
PY
```

The `bazel-idiom.json` report (counted by `Code`, capitalised) is the
**native-intent** signal — `*-toolchain-feature-needed`,
`find-package-dep-unresolved`, etc. `find-package-dep-unresolved` is
expected in a standalone survey (no imports manifest); it resolves in a
real `.bst` element graph.

## Commands

```sh
# One-time: build the converter (the survey uses it directly).
make converter

# Fetch the default corpus (the cheap four). Add fetch-llvm / fetch-vtk
# for the large two.
make fetch-survey

# Survey the default corpus. The survey is driven by the script, not a
# make target; with no project args it surveys the four corpus projects
# at their Makefile-pinned dirs. Output dir defaults to /tmp/survey-out.
#
# DEFAULT MODE is faithful: multi-config (SURVEY_BUILD_TYPES=auto) +
# split-packages (SURVEY_SPLIT_PACKAGES=1). auto detects each project's
# own declared CMAKE_CONFIGURATION_TYPES (so no config's intent is
# dropped); split emits one BUILD.bazel per directory (the gazelle model
# the converter ultimately targets). Both are the most representative
# surface, so they're on by default — opt out below when you only need a
# narrower/faster pass.
scripts/run-survey.sh
SURVEY_OUT_DIR=/tmp/my-out scripts/run-survey.sh   # custom out dir

# Survey one ad-hoc project (faithful flags baked into the script).
scripts/run-survey.sh myproj=/path/to/cmake/root

# Survey llvm correctly (the subdir, not the monorepo root).
make fetch-llvm
scripts/run-survey.sh llvm=$LLVM_DIR/llvm

# Opt OUT of multi-config (single-config Release surface only):
SURVEY_BUILD_TYPES=single scripts/run-survey.sh
# Force a fixed config subset (escape hatch; not faithful if the project
# declares more — drops RelWithDebInfo/MinSizeRel/custom configs):
SURVEY_BUILD_TYPES=Release,Debug scripts/run-survey.sh
# Opt OUT of split-packages (single monolithic BUILD.bazel):
SURVEY_SPLIT_PACKAGES=0 scripts/run-survey.sh
# Narrowest/fastest pass (single-config monolithic):
SURVEY_BUILD_TYPES=single SURVEY_SPLIT_PACKAGES=0 scripts/run-survey.sh

# --- Opt-in lenses (off by default; each acts on build-lens-selected projects).
# 4th lens — build: does `bazel build //...` succeed? (see "The build lens")
SURVEY_BAZEL_BUILD=fmt scripts/run-survey.sh fmt=$FMT_DIR
# 5th lens — compile-commands fidelity (per-TU defines/-std/includes drift):
SURVEY_BAZEL_BUILD=fmt SURVEY_COMPILE_DB=1 scripts/run-survey.sh fmt=$FMT_DIR
# 6th lens — intent-capture (agent-as-oracle "what did we miss?"); needs a
# CAPABLE judge in INTENT_LENS_JUDGE with read access to the bundle (writes
# <out>/<name>/intent-capture.json + the `missed` column). See "The
# intent-capture lens" for the judge contract + the remote-env tweaks.
# Run BOTH non-build lenses on an already-green corpus WITHOUT rebuilding:
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
SURVEY_BAZEL_BUILD=fmt SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1 \
  scripts/run-survey.sh --out-dir /tmp/survey-lens fmt=$FMT_DIR
```

> **Multi-config under the default `auto`.** The converter folds every
> non-primary configuration's flag / src / dep deltas onto the primary
> configuration's targets as `//config:<name>` `select()` arms, so
> multi-config *intent* is captured and a clean multi-config survey records
> **no** `unsupported-target-type` rejection (it used to emit a blanket one
> per project; that was stale). The fold's one residual is a target built
> *only* in a non-primary configuration — the primary walk never sees it,
> so it's dropped and flagged precisely (by target name). The matching
> `//config:<name>` `config_setting`s are emitted by
> `convert-element-cmake --out-config-settings` (a `//config` package: a
> `string_flag build_type` + one `config_setting` per non-sanitizer
> config), so the output is self-contained — select a config at build time
> with `--//config:build_type=<name>`. Multi-config is now a supported path
> in both strict and diagnostic mode; strict mode refuses only the
> config-only-target residual above.

Each project lands `rejections.json` + `bazel-idiom.json` +
`coverage.json` under the out dir, with a `summary.txt` table (one column
per lens: `rejections` / `idioms` / `coverage`). The survey passes
`--diagnostics` (collect-and-continue) so one run enumerates the whole
surface instead of aborting on the first refusal.

## Multi-platform survey (platform/arch `select()`)

A single cmake configure sees only the platform it ran on — an
`if(WIN32) target_sources(... win32.c)` branch is invisible to a Linux
configure. The lower-side #217 trace partition recovers *some* of the
other arms from one configure's trace, but the authoritative way to
capture every platform's intent is to **configure per platform and
fold**. `scripts/survey-multiplatform.sh <name>=<src>` (also `make
survey-multiplatform`) does this:

```sh
make survey-multiplatform                       # sdl + brotli by default
scripts/survey-multiplatform.sh sdl=$SDL_DIR
SURVEY_MP_PLATFORMS="linux windows" scripts/survey-multiplatform.sh ...
```

`SURVEY_MP_PLATFORMS` controls the matrix. The default, **`auto`**,
derives each project's platform set from **its own `if()`/`elseif()`
predicates**: cmake's `--trace-expand` records every predicate it
*evaluates*, including the branches it didn't take, so a single native
Linux configure still reveals that the project tests `if(WIN32)` /
`elseif(APPLE)` (the same recognizer the converter's #217 partition
uses). The survey then only spins up cross-toolchain configures for the
platforms the project actually branches on — e.g. brotli auto-detects
`linux windows` (it has WIN32 but no APPLE arms), skipping a wasted
darwin configure. An explicit space-list (`SURVEY_MP_PLATFORMS="linux
windows"`) forces that set for every project.

For each resolved platform it runs `convert-element-cmake
--out-ir-json`, then folds the cells with `fold-element` into one BUILD
carrying real `+ select({@platforms//os:*})` arms. The non-native
platforms use **synthetic toolchain files**
(`scripts/survey-toolchains/<os>.cmake`) that set `CMAKE_SYSTEM_NAME` and
force the compiler check, so cmake evaluates the platform `if()` branches
and emits a codemodel **without a real cross-compiler** (nothing is
built — only the File API reply + trace are consumed). A platform a
project can't configure is dropped from the matrix; the fold runs over
those that succeeded. The summary reports `platforms` (cells folded) and
`select_targets` (rules that gained a platform `select()` — the
multi-platform signal).

Like the gazelle harness, this is heavier than the per-convert loop (N
configures + a fold per project) and surfaces real fold limitations on
divergent projects — e.g. it found that `ArtifactName` legitimately
differs per OS (`.so`/`.dll`/`.dylib`; the fold now tags rather than
errors), and that a per-OS `configure_file` produces a divergent
`GenruleCmd` the fold can't yet merge (a genrule has one `cmd`; modeling
a per-platform genrule is a follow-on). Those show up as `FOLD FAILED`
datapoints rather than crashing the run.
