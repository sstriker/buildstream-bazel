# Survey corpus & how to run a faithful survey

The **survey** runs the cmake converter over real-world CMake projects in
diagnostic mode and counts what it can't yet lift natively. It's the
instrument we use to answer "is the converter getting better, and where is
intent still being lost?" — see `docs/codemodel-consumption-audit.md` for
the analysis framing and `scripts/run-survey.sh` for the driver.

This document is the single source of truth for **which projects are in the
corpus** and **how to survey them faithfully** (so two runs are comparable).

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
```

Each project lands `rejections.json` + `bazel-idiom.json` under the out
dir, with a `summary.txt` table. The survey passes `--diagnostics`
(collect-and-continue) so one run enumerates the whole surface instead of
aborting on the first refusal.
