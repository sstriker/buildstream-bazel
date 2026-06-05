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

State per project across the four lenses (each lens is defined in the
sections below). The three **convertibility** lenses report finding
counts — `0` is healthy; the **build** lens reports the survey's
`build` token. Per-project build-lens detail is in *Build-lens status*
below; the full corpus roster + rationale is under *The corpus*.

| Project | Rejections | Idiom | Coverage | Build lens |
| --- | --- | --- | --- | --- |
| **fmt** | 0 | 0 | 0 | `ok` |
| **libxml2** | 0 | 0 | 0 | `ok` |
| **brotli** | 0 | 0 | 0 | `ok` |
| **glm** | 0 | 0 | 0 | `ok` |
| **googletest** | 0 | 0 | 0 | `ok` |
| **abseil** | 0 | 0 | 0 | `ok` |
| **zstd** | 0 | 0 | 0 | `ok` (repo-root overlay) |
| **glog** | 0† | 30 | 0 | `ok` |
| **eigen** | 0 | 16‡ | 0 | `ok` |
| **protobuf, curl, …** | rej (ext `find_package`) | — | — | `skip(rej)` |

`rej` = surveys with rejections, so the build lens skips it; for
protobuf / curl these are honest external `find_package` deps
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
Large members surveyed for convertibility but not yet driven through
the build lens (SDL, grpc, llvm, VTK, OpenBLAS, …) live under *The
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
| protobuf, curl, … | `skip(rej)` | Honest external `find_package` deps (resolved in a real `.bst` element graph, not standalone). |

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
  `PATH`** to configure. Without CUDA the survey stops at cmake configure
  (itself a datapoint, but not the idiom/refusal surface). They're fetched
  so the corpus is whole; survey them on a CUDA-equipped host.
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
  visibility. The **remaining** blocker is genuinely beyond `.conf` + these
  fixes: OpenBLAS's `GenerateNamedObjects` codegen (cmake/utils.cmake:421)
  `file(WRITE ...)`s ~1951 per-routine wrappers each `#include`-ing the real
  kernel by ABSOLUTE configure-time path (`#include "<source-root>/lapack/getf2/
  zgetf2_k.c"`); the converter bakes that path verbatim, so the Bazel compile
  fails "No such file" (the convert-host path isn't in the sandbox). Greening
  needs a lower pass that rewrites source-root-absolute `#include`s in generated
  content to in-tree references AND stages each included kernel source as a
  textual input on the consuming compile (resolving include path +
  cross-package visibility) — a sizable, well-scoped converter feature, not a
  `.conf` tweak.

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
- **CUDA toolkit (`nvcc` + driver)** — required for **cutlass** and
  **cuda-samples** to pass cmake configure at all (they `enable_language(CUDA)`
  / `find_package(CUDAToolkit)`); without it the survey stops at
  configure. Install the CUDA toolkit (`nvcc` on `PATH`) and, for any
  step that runs device code, the NVIDIA driver. Survey these on a
  CUDA-equipped host; they're fetched so the corpus is whole regardless.

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
  opt-in because it's multi-GB.
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
