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

2. **Are we emitting non-idiomatic Bazel?** — the `bazel-idiom.json`
   report (`bazelidiom.Audit`, runs on every convert). This is the one
   lens that is **fully automated and per-run**: `empty-srcs`,
   `empty-cc-library`, `empty-cc-import`, `test-with-no-entry`,
   `raw-toolchain-feature-flag`, etc. Treat any non-zero count here as a
   real defect to drive to zero (this is what surfaced the eigen
   empty-srcs and VTK empty-cc-library work).

3. **Did we lose intent vs. the CMakeLists?** — **not automated; this is
   the adversarial lens.** Lenses 1 and 2 are *self-reported*: the
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

   **Candidate for automation:** the highest-signal, lowest-false-positive
   deterministic lens-3 check is **dependency-coverage** — a
   `target_link_libraries` entry that resolves to an in-codebase target
   but lands in none of `deps` / `implementation_deps` / `data` and raises
   no `unresolved-link-dep` is a silent dropped edge. That check would
   have caught #302. It's scoped as a future addition; the broader
   codemodel→BUILD differ is deliberately *not* pursued (false-positive
   cost exceeds signal, per the relocation cases above).

## The corpus

| Project | Why it's in the corpus | Source | Fetch |
| --- | --- | --- | --- |
All versions/dirs are pinned as overridable `*_VERSION` / `*_DIR` vars in
the Makefile (`make fetch-*` clones each at its pinned tag).

| Project | Why it's in the corpus | Source (pinned tag) | Fetch |
| --- | --- | --- | --- |
| **abseil** | Deeply modular; many INTERFACE deps-only wrapper libraries (exercises interface-library synthesis); doubles as the feature-flag idiom oracle. | github.com/abseil/abseil-cpp (`ABSEIL_VERSION`) | `make fetch-abseil` |
| **protobuf** | `find_package` deps (ZLIB), protoc custom-command codegen, install(EXPORT) config-mode producer. | github.com/protocolbuffers/protobuf (`PROTOBUF_VERSION`) | `make fetch-protobuf` |
| **googletest** | `enable_testing()` + add_test / gtest_discover_tests; INTERFACE genex defines (`$<BUILD_INTERFACE:...>`). | github.com/google/googletest (`GTEST_VERSION`) | `make fetch-googletest` |
| **eigen** | Header-only INTERFACE library; config-mode export/components. | gitlab.com/libeigen/eigen (`EIGEN_VERSION`) | `make fetch-eigen` |
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

Each project lands `rejections.json` + `bazel-idiom.json` under the out
dir, with a `summary.txt` table. The survey passes `--diagnostics`
(collect-and-continue) so one run enumerates the whole surface instead of
aborting on the first refusal.
