# VTK conversion survey (research)

Empirical survey of converting **VTK v9.4.2** (full tree) with
`convert-element-cmake`, to scope what a VTK fidelity/build gate needs.
Run via `scripts/run-survey.sh vtk=$VTK_DIR` (self-driving: configures
VTK with the File API + `--trace-expand`, multi-config
Debug/Release/RelWithDebInfo, `--diagnostics` collects every Tier-1
refusal and continues).

## Headline

VTK's refusal surface is dominated by **one** addressable category and
has **one** genuine converter gap:

| Signal | Count (default) | After `--cmake-script-bake` |
|---|---|---|
| `unsupported-custom-command-script` (`cmake -P` codegen) | **705** | **0** |
| `unsupported-execute-process` (git version stamp) | 3 | 3 (expected round-2 fallback) |
| **Total Tier-1 rejections** | **708** | **3** |
| `empty-cc-library` idiom (third-party modules) | 2 | (still present) |
| `dropped-link-dep` coverage (third-party modules) | 6 | (still present) |

**`--cmake-script-bake=true` collapses 708 → 3.** The 3 residual are the
`git describe` version stamp (`CMake/VTKDetermineVersion.cmake`), which
is the expected stamp→round-2 fallback, not a new gap.

## Breakdown

### 1. `cmake -P` script codegen — 705 (CLOSED by an existing flag)

VTK runs two `cmake -P` generators as custom commands:

- `vtkEncodeString.cmake` — **702** call sites. Encodes shader/template
  text into C string arrays (`*.cxx` from `*.glsl` etc.).
- `vtkHashSource.cmake` — **3** call sites.

These are refused by default (the converter doesn't execute arbitrary
`cmake -P` at convert time without opt-in). Opting into the lift
resolves **all 705** — verified by re-converting with
`--cmake-script-bake=true`: residual rejection count drops to the 3
git-stamp items.

Which lift to use — a transition trajectory, not a static ranking. The
converter's documented preference order for `cmake -P` is
**runner → trace → bake** (refusal message in `genrule.go` + flag help),
optimizing faithfulness-and-least-effort-to-a-green-build. Read through
the **transition-tool** lens (success = "downstream is plain Bazel, you
don't need this or cmake anymore", `ROADMAP.md` preamble), the three
options aren't ranked — they're **phases of a burn-down**:

- **`--cmake-script-runner=<label>` — the right EARLY route.** Runs
  cmake at build time, so it's faithful + auto-refreshing AND cheap: it
  defers the expensive per-script native reimplementation. During the
  transition the converted build legitimately depends on
  cmake-on-executor; that's fine *while transitioning*.
- **Native Bazel rule (§1b) — the END-of-transition target.** What
  runner-served scripts get migrated INTO so cmake drops out. No cmake,
  no converter, auto-refreshing, pure Bazel.
- **`--cmake-script-bake=true` — a side-variant.** Drops build-time
  cmake without a native rewrite, but freezes the output bytes
  (`warnConvertTimeBaking` flags them) and keeps the *converter* in the
  loop (re-bake on input change). Useful for the long tail of rare
  scripts not worth native-izing.

The metric to drive is the count of `cmake -P` sites still **runner-**
**served**, trending to **zero** as the common idioms (embed, hash) move
to native rules — at which point cmake is fully shed. (Worth surfacing
as an audit-tag census so the burn-down is measurable.)

All 705 are liftable any of these ways: both `vtkEncodeString` and
`vtkHashSource` are pure, hermetic, deterministic functions of a single
declared `INPUT` (+ literal `-D` args), explicit `DEPENDS`, no hardcoded
host paths.

### 1b. Native conversion of the embed/hash codegen (the end-of-transition target)

For these two scripts specifically, a **Bazel-native rule is achievable
and beats both runner and bake** for a transition tool:

- `vtkEncodeString` is the universal "embed a file as a C array" idiom.
  A native `cc_embed`-style rule (a Starlark rule + a tiny hermetic
  Go/C++ tool shipped in `rules_buildstream_bazel`) reproduces it with
  **no cmake**. It is *faithful* where it matters: the symbol name comes
  straight from `-Doutput_name=`, and the runtime string value is just
  the input file's bytes — any correct escaping yields the same nm
  symbol set (passes the fidelity gate) and the same runtime value.
  Byte-identical generated-source *text* is irrelevant.
- `vtkHashSource` → a trivial native hash rule (hashlib); no cmake.

Why it's feasible:

1. **The parameters are already on the wire.** The `add_custom_command`
   passes everything as `-D` flags (`-Dsource_file`, `-Doutput_name`,
   `-Dexport_symbol`, `-Dexport_header`, `-Dbinary`, `-Dnul_terminate`,
   the ABI-mangle args). The converter pattern-matches
   `cmake -P <known-encoder>.cmake` + extracts the `-D` args → emits the
   native rule fully parameterized; it need not understand the cmake
   function at all.
2. **Precedent.** The converter already does idiom→native lowering
   (`install(EXPORT)`→`cc_import`/`pkg_files`, sanitizer→`--features`,
   the `install-compat-alias` rule #350, the `cmake_configure_file`
   rule). "Embed/hash codegen → native `cc_embed`/`cc_hash`" is the same
   move.
3. **It generalizes.** Embed-as-C-array and hash-to-header are not
   VTK-specific (LLVM, Qt, many projects do this), so a repo-provided
   `cc_embed` rule + recognizer is reusable leverage, not a special-case.

Cost / caveats: the native tool must cover the encoder *modes* the
project uses (string vs `BINARY`/hex, `NUL_TERMINATE`, export macros,
ABI mangling) — bounded (~50 lines of script to mirror) but real;
recognition keyed on the encoder script basename is a per-encoder list
(a shape-based detector — "cmake -P that reads one input and writes a C
source embedding it" — generalizes further but is harder). Pragmatic
path: recognize the known encoders → native rule; fall back to **bake**
(not runner, for the transition reason above) for unrecognized scripts.

### 2. `vtk_module_third_party` forwarders — the REAL converter gap

The bundled third-party modules surface as both an `empty-cc-library`
idiom and `dropped-link-dep` coverage flags:

- `cc_library(hdf5)` and `cc_library(nlohmannjson)` emit **empty** (no
  srcs/hdrs/deps).
- `hdf5`'s `target_link_libraries(vtkhdf5_src vtkhdf5_hl_src)` arms are
  **dropped** — the in-codebase source targets `vtkhdf5_src` /
  `vtkhdf5_hl_src` are absent from the emitted `deps`.

VTK's module system wraps a vendored library as a thin forwarding
`cc_library` (`hdf5`) that `target_link_libraries` the actual
compiled inner targets (`vtkhdf5_src`, `vtkhdf5_hl_src`). The converter
drops those link arms, yielding an empty wrapper that wouldn't link.
This is the `vtk_module_third_party` forwarder shape called out in the
ROADMAP, and it's the one piece of NEW converter work VTK forces (cf.
#302's INTERFACE-library link-arm routing). Header-only third-party
(`nlohmannjson`) hits the same empty-wrapper shape from the other
direction (INTERFACE lib whose headers weren't routed).

### 3. git version stamp — 3 (expected)

`CMake/VTKDetermineVersion.cmake:30` runs `git describe` into an
OUTPUT_VARIABLE. Classified `[stamp]` → intended round-2 fallback (no
configure_file consumer to lift it onto). Not a new gap.

### 4. Minor multi-config artifacts

`CMAKE_INTDIR` / `NDEBUG` `missing-define` and a `Wrapping/Tools`
`missing-include` notice — Ninja Multi-Config bookkeeping the verify
pass flags; cosmetic.

## Recommended sequencing for a VTK gate

1. **Cheap win first:** a VTK *survey* corpus entry already works
   (`make fetch-vtk` + `run-survey.sh`); wire it as a tracked survey
   target so the rejection surface is watched.
2. **First build gate (early transition):** enable the `cmake -P` lift
   with `--cmake-script-runner=<label>` — the cheap, faithful route that
   defers the native rewrites (the buildbarn runner image already ships
   cmake). Gets conversion clean to 3 expected fallbacks. Target a module
   that does NOT pull a bundled third-party (a pure VTK::CommonCore-tier
   leaf) so the build is green before the §2 forwarder fix.
3. **Native `cc_embed` / `cc_hash` lowering (§1b) — the burn-down
   target.** Convert `vtkEncodeString` / `vtkHashSource` to a repo
   `cc_embed`-style rule + tiny hermetic tool so those sites stop being
   runner-served and the converted project needs neither cmake nor the
   converter at build time. This is what drives the runner-served count
   toward zero (the end-of-transition state); reusable beyond VTK. Bake
   stays available for the rare long-tail script not worth native-izing.
4. **The forwarder fix (§2)** is the converter change VTK uniquely
   motivates — route the third-party module wrapper's
   `target_link_libraries(<inner>_src …)` arms into the wrapper's
   `deps` so the wrapper is non-empty and links its bundled sources.

## Reproduction

```sh
make fetch-vtk                       # github.com/Kitware/VTK mirror, v9.4.2
SURVEY_OUT_DIR=/tmp/vtk-survey scripts/run-survey.sh vtk=/tmp/vtk
# default (no bake): 708 rejections (705 cmake -P, 3 git stamp)

build/bin/convert-element-cmake --source-root /tmp/vtk \
  --out-build /tmp/BUILD --diagnostics --cmake-script-bake=true \
  --rejections-report /tmp/rej.json
# residual: 3 (git stamp only)
```
