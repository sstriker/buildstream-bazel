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
`cmake -P` at convert time without opt-in). Re-converting with
`--cmake-script-bake=true` (bake the script's output bytes at convert
time) resolves **all 705** — verified: residual rejection count drops to
the 3 git-stamp items.

Implication for a VTK gate: it needs `--cmake-script-bake=true` (or
`--cmake-script-runner=<label>` for build-time regeneration). Same flag
the libpng gate already uses. The caveat is the documented one — baked
output doesn't auto-refresh if a `.glsl` input changes (acceptable for a
fidelity gate; a runner tool is the alternative if live regen matters).

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
2. **Fidelity/build gate** needs `--cmake-script-bake=true`. With that,
   conversion is clean to 3 expected fallbacks — but a real `bazel
   build` of a VTK module that depends on a bundled third-party
   (hdf5-consuming modules) will fail until the
   **`vtk_module_third_party` forwarder gap (§2)** is fixed. A
   first VTK build gate should therefore target a module that does NOT
   pull a bundled third-party (a pure VTK::CommonCore-tier leaf), which
   should build once `--cmake-script-bake` is set.
3. **The forwarder fix (§2)** is the converter PR VTK uniquely
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
