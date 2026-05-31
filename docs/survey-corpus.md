# Survey corpus & how to run a faithful survey

The **survey** runs the cmake converter over real-world CMake projects in
diagnostic mode and counts what it can't yet lift natively. It's the
instrument we use to answer "is the converter getting better, and where is
intent still being lost?" — see `docs/codemodel-consumption-audit.md` for
the analysis framing and `scripts/run-survey.sh` for the driver.

This document is the single source of truth for **which projects are in the
corpus** and **how to survey them faithfully** (so two runs are comparable).

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
     `non-cc-language-source` (Fortran `.f` srcs in a cc_* rule — Bazel
     can't compile them; OpenBLAS's LAPACK targets + eigen's bundled
     reference BLAS/LAPACK), etc. Per-survey and fully automated; drive
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

Per-project survey caveats (faithful-survey rules, same spirit as the
llvm-subdir note below):

- **zstd:** the buildable CMake root is the **`build/cmake` subdir**, not
  the repo root — survey `$(ZSTD_DIR)/build/cmake`. (This subdir-under-an-
  umbrella layout is exactly what #303 fixed.)
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
  not Bazel-modelable).

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
survey container ships neither by default, so a setup step (or a
`SessionStart`-style hook) should `apt-get install gfortran` and stage
the CUDA toolkit when those members are in scope for a run.

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
scripts/run-survey.sh
SURVEY_OUT_DIR=/tmp/my-out scripts/run-survey.sh   # custom out dir

# Survey one ad-hoc project (faithful flags baked into the script).
scripts/run-survey.sh myproj=/path/to/cmake/root

# Survey llvm correctly (the subdir, not the monorepo root).
make fetch-llvm
scripts/run-survey.sh llvm=$LLVM_DIR/llvm

# Multi-config across ALL of each project's declared configuration types.
# SURVEY_BUILD_TYPES=auto runs a throwaway Ninja Multi-Config configure
# per project, reads back its CMAKE_CONFIGURATION_TYPES, and surveys with
# exactly those — so no config's intent is dropped. A fixed subset like
# "Release,Debug" would silently drop RelWithDebInfo/MinSizeRel/custom
# configs; an explicit comma list is still accepted as an escape hatch.
SURVEY_BUILD_TYPES=auto scripts/run-survey.sh

# Split-packages: one BUILD.bazel per directory (the gazelle model) —
# the shape the converter ultimately targets, for gazelle-compliant
# output. Composes with SURVEY_BUILD_TYPES.
SURVEY_SPLIT_PACKAGES=1 scripts/run-survey.sh
SURVEY_BUILD_TYPES=auto SURVEY_SPLIT_PACKAGES=1 scripts/run-survey.sh
```

Each project lands `rejections.json` + `bazel-idiom.json` +
`coverage.json` under the out dir, with a `summary.txt` table (one column
per lens: `rejections` / `idioms` / `coverage`). The survey passes
`--diagnostics` (collect-and-continue) so one run enumerates the whole
surface instead of aborting on the first refusal.
