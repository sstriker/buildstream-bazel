# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **Complexity lens — drive to green, then flip to blocking.**
  `make lint-complexity` (golangci-lint, complexity-only config in
  `.golangci.yml`: gocyclo / gocognit / cyclop / nestif / funlen / maintidx) is
  the code-complexity axis `go vet` / `gofmt` / `staticcheck` don't cover. It
  runs in the CI `Build + unit tests` job as **non-blocking
  (`continue-on-error`)** so its output is the gap-to-green worklist, not a wall.
  The burndown is underway (launch flagged 55; **40** now). The dominant
  remaining offenders are flagged by **gocognit** (the giants are cognitive, not
  cyclomatic): `lower.lowerTarget` (cognitive **713**),
  `convert-element-cmake`'s `run` (291), `lower.ToIR` (278), and `emit/bazel`
  `rewriteTarget` (181) / `planSplit` (167) — each its own focused pass.
  **What's left:** keep breaking these down via behavior-preserving extraction
  of cohesive sub-passes into helpers (the established pattern), then **drop
  `continue-on-error`** so the lens gates like the others. Tune thresholds in
  `.golangci.yml` if a class proves low-yield.

- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`) fail intermittently for environment reasons
  (cmake-config bundle staging on the CI runner; userns / fuse permissions on
  Ubuntu 24.04 runners; bazel 9 toolchain expectations). These don't reflect
  product issues but they make PR review noisier than it should be.

- **Wire the broader cmake render gates into CI.** The core render gates run in
  the CI `bazel-e2e` job via the `e2e-meta-cmake-render-gates` aggregate
  (Makefile `RENDER_GATES`): `meta-cmake-genex-probe`, `meta-file-generate`,
  `meta-cmake-genex-literal-twopass`, `meta-cmake-fileset-compiled-lib`,
  `meta-cmake-stamp-volatile`, and the two `meta-cmake-vcs-stamp{,-indirect}`
  gates. The aggregate guards on cmake + ninja up front (each gate self-skips
  its bazel≥9 build half), so it no-ops cleanly without the toolchain.
  **Follow-up:** the broader `meta-cmake-*.sh` family (install-export
  declarative, sanitizer-features, interface-genex-defines,
  probe-genex-object/utility, platform-partition-tier2, …) is still local-only;
  add each to `RENDER_GATES` once verified CI-safe (skip-clean + no
  heavy/special-toolchain or flaky-fetch dependence). Surfaced in #366 review.

## Next

- **Reproducible `find_package` host-installs.** The grpc/protobuf build-lens
  `.conf` files hardcode `/tmp/absl-install` (host-installed abseil) and the
  umbrella deps list is a snapshot of the pinned abseil. Fold the abseil (and
  protobuf, for grpc) host-installs into the `SessionStart` hook so the lens is
  reproducible without a manual prep step. (Carried from protobuf; grpc itself
  is green.)

- **Converter hang in `--diagnostics` mode on libevent's regress targets.** The
  `--diagnostics` convert of libevent spins indefinitely (observed 38+ min, no
  output) UNLESS `EVENT__DISABLE_TESTS=ON` — so the libevent lens scopes the
  regress tests off (libevent.conf), which also dodges a `test/regress.gen.c
  outside-build-dir` rejection. With tests off both converts complete in
  seconds with 0 rejections, so the loop is in the regress target graph (likely
  the custom-command / generated-source recovery over the regress test tree).
  Find + fix the loop so libevent's tests don't have to be scoped purely to
  avoid a hang. Lower priority than greening members, but a hang (vs a clean
  refusal) is a sharp edge worth removing.

- **Stage headers from a PRIVATE include dir with no public header lib.** How
  the existing machinery works (verified): lower emits a `target_include_
  directories(PRIVATE <dir>)` as an element-root-relative `-I<dir>` copt; split's
  `rewriteTarget` copt scan (split.go ~944) then keys on that `-I<dir>` and, IFF
  a header lib was synthesized for `<dir>` (i.e. `<dir>` is ALSO a public include
  root of some target, so it's in `incRoots`), wires the lib as a (private)
  header dep and drops the bare `-I`. That stages the headers + supplies the
  correct exec-root search path via the lib's `includes`. The GAP: a dir that is
  PRIVATE-only (no target lists it as a public include) gets no header lib, so
  the scan finds nothing, the `-I<dir>` stays element-relative (unresolved at the
  exec root), and the dir's headers are never staged. Blocks **mbedtls** (its
  always-on `framework` builds `mbedtls_test_helpers` → `#include
  "test/ssl_helpers.h"` from PRIVATE-only `tests/include`) and **sdl**
  (`SDL_uclibc` → `#include "SDL_internal.h"` from PRIVATE-only `src`).
  Implement (verified-safe shape — two pieces, BOTH required):
  1. **lower: discover the PRIVATE dirs' headers.** `includesForWalk`
     (lower.go ~2736, fed to `discoverHeaders`) starts from `irt.Includes`
     (the NON-private include attr) + each src file's dir — it does NOT include
     the PRIVATE `-I` copt dirs. So a PRIVATE-ONLY dir's headers never land in
     `irt.Hdrs`/`allHdrs`. Add the PRIVATE include dirs (the `emit` values from
     the `privateIncludeDirs` branch ~2459) to `includesForWalk` so their
     headers are discovered + declared.
  2. **split: synthesize + wire.** In planSplit add PRIVATE `-I` copt dirs to
     `incRoots` so `headerLibTarget` builds a lib (now non-empty thanks to #1);
     rewriteTarget's existing copt scan (~944) then wires it as a private header
     dep and drops the bare `-I`.
  WHY BOTH: with only #2, a PRIVATE-only dir gets an EMPTY header lib (its
  headers weren't discovered) AND the `-I` is dropped → regression. SDL's `src`
  happens to be safe with #2-only because another (non-private) target already
  declares `SDL_internal.h`, but mbedtls's PRIVATE-only `tests/include` is not —
  hence #1 is mandatory for a general fix. Needs full corpus re-validation (it
  touches header-lib synthesis broadly); two earlier point-fixes in this area
  regressed fmt and the cp-dir tests, so validate every green member before/after.

  RELATED mbedtls blocker (separate, AFTER header-staging): mbedtls GENERATES
  `query_config.c`, `error.c`, `version_features.c` via python scripts, then
  `link_to_source`-copies them, lowering to execute_process copy genrules with
  `srcs=[X]`+`outs=[X]` (in==out). Two failed shortcuts, both reverted: (a)
  "drop the dir copy when srcFileRel==outRel" broke legitimate `cp <srcdir>
  <build>` staging (the cp-dir-lift tests); (b) "drop the single-file copy"
  cleared the in==out and got mbedtls compiling 475/591, but then `ld: undefined
  symbol: query_config / list_config` — because X is GENERATED-ONLY (no usable
  committed source), so dropping its copy loses the content entirely. So the real
  fix is NOT to drop or rename the copy — it's to actually GENERATE these files:
  emit the python-script genrule that produces query_config.c et al., and
  recognize the link_to_source copy as redundant with it.

  REGRESSION LESSON (do not repeat): an earlier attempt "exec-root anchor the
  PRIVATE `-I` copt" (so `-Itests/libtest`→`-Ielements/<pkg>/tests/libtest`)
  made curl's unit-test build find headers it had in `srcs`, but it BROKE the
  split copt scan above for every member relying on it — the scan matches the
  element-relative form, so the exec-root form silently stopped wiring the header
  lib and regressed **fmt** (posix-mock-test: `#include <fmt/os.h>` "No such
  file", fmt's `include/` lib no longer wired). Reverted. The copt MUST stay
  element-relative; do the staging in split (header lib), not by rewriting the
  copt in lower.

- **Green the remaining heavyweight corpus members: vtk (tail), cuda-samples.**
  25/26 are green (protobuf + sdl + vtk + grpc landed). Remaining:
  - **vtk** — configures + converts with 0 rejections, analysis fully green
    (2359/2359), and `bazel build //...` compiles **~6,345 / 6,366 (~99.6%)**.
    REMAINING TAIL (~20, well-diagnosed):
    - **configure_file config-headers not wired to consumers (~19):**
      `kwsysPrivate.h` (15), `proj_config.h` (4), `pugiconfig.hpp` (3). A header
      `configure_file(... COPYONLY)` output, #included by BARE quote name from a
      same-dir source — cmake needs no `-I` (quote resolves same-dir), so
      `targetBuildIncs` never records it and the prefix-match attribution misses
      it; the consuming multi-language SUB-library never declares the generated
      header. A same-dir-attribution pass was added but DOESN'T engage for these:
      `lowerTarget`'s `t.Name` for the kwsys-consuming target is NOT "vtksys"
      (the converter renames on emit) and/or VTK's configure_files don't reach
      the `configureFiles` slice the attribution iterates — the precise
      recovery-path/target-identity needs one more instrumented pass. Fix lands
      the output in the consuming sub-lib's hdrs (rides `splitCompileGroups`'
      sharedHdrs).
    - **2 genrule-EXECUTION failures:** `proj_db` (`cmake -P
      generate_proj_db.cmake` fails at `include(sql_filelist.cmake)` — relative
      include not staged in the genrule's cwd at build time) and
      `vtkCommonCore-hierarchy.txt` (`vtkWrapHierarchy: couldn't open
      @…hierarchy.Debug.args` — the `.args` response-file, routed to `data`,
      isn't staged as a genrule input at the expected path). Both are build-time
      genrule input-staging fixes.
    - misc: `lz4.c` (1).
  - **cuda-samples** — a surveyed sample builds GREEN (needs CUDA provisioned:
    `apt-get install nvidia-cuda-toolkit gcc-12` + `scripts/provision-cuda-root.sh`
    → `BSB_CUDA_ROOT`; `BSB_CUDA_HOST_CC=/usr/bin/gcc-12`). `cpp/0_Introduction/
    vectorAdd` converts 0-rej and `bazel build //...` completes. REMAINING for the
    FULL suite: the `find_package(CUDAToolkit)` library group (a
    `cuda-samples-imports.json` mapping `CUDA::cublas` etc. → `@cuda//:…`) and the
    `9_CUDA_Tile`/tileiras whole-tree configure prune (or keep surveying buildable
    sample subdirs).

  DISK NOTE: the real ceiling is ~37 GB, and a clean session has ~25 GB free —
  ample for grpc/vtk builds. The earlier "disk-blocked" reading was stale
  prior-session survey dirs (`g-*`, `revisit`, `final-val`, …) accumulating under
  `/home/user/`; reclaim them between runs. Always `df /` + `du -xsh
  /home/user/*` before concluding disk is the limit, and clean per-project
  `.bzcache`/`build-ws` under `--out-dir/<member>/`.

- **Faithful SHARED-library conversion (`cc_shared_library`) — remaining:
  corpus-wide re-green + edge cases.** The WHOLE POINT of shared is FIDELITY —
  to build what cmake would actually build (the survey forces
  `BUILD_SHARED_LIBS=OFF` for simplicity, but static is NOT the project's
  default; that forced-static is the deviation this work removes). The lift
  (`--emit-shared-libraries`, survey `SURVEY_SHARED=1`) is validated green on 9
  probed members (zlib, fmt, libxml2, brotli multi-lib, curl multi-package +
  the SIGSEGV root-cause, glog, spdlog, mbedtls multi-lib, protobuf). Default
  emit is byte-identical (opt-in). **Remaining:** run the WHOLE build-lens
  corpus under `SURVEY_SHARED=1` (incl. sdl, OpenBLAS, the heavy LLVM/VTK) and
  fix fallout; carry the `.so` in runfiles for `bazel run`/test; `MODULE_LIBRARY`
  dlopen semantics; and consider flipping `SURVEY_SHARED` to the DEFAULT once the
  corpus is green under it (so green + the fidelity lens run against the config
  cmake produces).

- **Test-target coverage — enable the scoped-out members' tests.** The build
  lens builds `//...`, which already INCLUDES test targets where the project's
  tests need no extra infra (fmt, libxml2, glog, glm, googletest, abseil
  surface, curl test PROGRAMS). The remaining members scope tests out via a
  `.conf` flag, each for a concrete reason — to enable, resolve that reason:
  - **spdlog** (`SPDLOG_BUILD_TESTS=OFF`): tests need `find_package(Catch2 3)`.
    Catch2 IS a corpus member (3.5.3) — wire it cross-element via the imports
    manifest + a host-install prefix (the protobuf↔absl pattern).
  - **nlohmann-json** (`JSON_BuildTests=OFF`): tests `#include` a generated
    `test_data.hpp` whose data is a `git clone` of `json_test_data` (network) —
    stage the data dir + point `JSON_TestDataDirectory` at it.
  - **mbedtls** (`ENABLE_TESTING=OFF`): test suites are `.c` generated from
    `.data` + `.function` by `generate_test_code.py` (python add_custom_command)
    — verify the converter recovers those as genrules.
  - **libevent** (`EVENT__DISABLE_TESTS=ON`): `regress` needs `regress.gen.c`
    from `event_rpcgen.py` (python codegen) — same genrule-recovery check.
  - **eigen** (`EIGEN_BUILD_TESTING=OFF`): ~900-target `-Werror` SIMD suite,
    self-contained (no ext dep/codegen) but a huge build — needs a scoped/
    sharded build, not `//...` in one shot. Deferred dev surface.
  - **openblas** (`BUILD_TESTING=OFF`): utest is C but the BLAS test surface
    pulls the Fortran reference — gated on the (deferred) Fortran ruleset.
  - **protobuf** (`protobuf_BUILD_TESTS=OFF`): needs googletest as a dep
    (BCR module / corpus member) wired like abseil's `GTest::gmock`.

- **Final corpus validation pass before declaring the converter "done."**
  Independent of any single feature: when the corpus is considered complete, do
  one clean-room full pass — every build-lens member fetched fresh, converted
  from scratch (no stale `build/bin` binary, no warm out-dir), `bazel build
  //...` green, AND the lens's run/execution checks green (the unit-test-style
  "does it actually run" probes, e.g. curl's unit tests passing) — on a machine
  with enough disk for the large members (LLVM `TOOLS=ON`, VTK) so nothing is
  scoped out for disk. Capture the result as the corpus's "all green, no cmake"
  baseline. This is the acceptance gate, distinct from the per-change
  re-validation the dev loop already does.

- **Make the host-system-library fallback EXPLICIT (hermeticity boundary).**
  When a `find_package`/`target_link_libraries` link fragment resolves to a
  standard system library (`/usr/lib*`, `/lib*`, `/usr/local/lib*`) and the
  imports manifest has no entry for it, the lower lifts it to a `-l<name>`
  linkopt (`converter/internal/lower/lower.go`: the `systemLibName(path)`
  sites — the find_package-attributed branch AND the attribution-missed
  branch). This is what makes LLVM's `opt`/`llc` link against host zlib. It
  is **not hermetic**: the build relies on the host toolchain's library
  search path containing `libz.so` etc. Today the lift is **silent** — there
  is no signal in the emitted BUILD that a target took a host dependency.
  Decide + implement the explicit contract: (a) emit a visible marker on
  every host-syslib lift (e.g. a `cmake-codegen-host-syslib=<name>` tag and
  an idiom-audit finding) so host coupling is auditable; and/or (b) gate the
  lift behind an opt-in flag (default: refuse with a typed failure pointing
  at the imports manifest), so the hermetic path (map `<Pkg>::<Pkg>` →
  a BCR module like `@zlib//:zlib` via the manifest) is the default and
  host-coupling is a conscious choice. The manifest is already the hermetic
  channel (abseil→googletest); this item is about not silently bypassing it.

- **Build curl's `docs/` manpage genrules.** curl's test surface now builds
  (`BUILD_TESTING=ON`), but the lens still scopes `docs/` off
  (`BUILD_LIBCURL_DOCS=OFF`). The `docs/` tree is manpage generation: genrules
  running perl helpers (`cd2nroff`/`managen`/`mkhelp.pl`) over ~300 `.md` files,
  often with a different shape than the test codegen (whole-directory `managen`
  inputs, `>`-redirect outputs, multi-input staging). Verify which build under
  the current anchoring and close the remainder, so docs build faithfully
  instead of being scoped away. Documentation surface, not library/test code, so
  lower priority than the test side that's now green.

- **Build-lens fidelity (compile-commands lens) — remaining: link-order
  project-archive layer.** The lens (`cmd/compile-commands-diff` +
  `scripts/compile-commands-lens.sh`, wired into `run-survey.sh` as the 5th lens
  `SURVEY_COMPILE_DB=1`, writing `<out>/<name>/fidelity.json`) diffs cmake's
  `CMAKE_EXPORT_COMPILE_COMMANDS=ON` db against Bazel's
  `aquery 'mnemonic("CppCompile",//...)'` per TU on defines, -std, includes,
  copts, and link-line ORDER (system-libs v1) — all LANDED & wired.
  Remaining (PARKED): extend the link-order check to compare ALL libraries in
  order (system libs AND project archives AND find_package/external deps), not
  just system libs, since the first-to-satisfy-a-symbol rule applies across all
  of them. Gated on cross-build-system identity matching for the non-system
  libs: map cmake's link-fragment path basename → target via `NameOnDisk`, and
  Bazel's mangled `-lelements_Szlib_Slibzlib` → target by reversing the solib
  escape (`_S`→`/`, `_U`→`_`, basename, strip `lib`) — both land on the cmake
  `Target.Name`, the common key; external/find_package libs map via the imports
  manifest's BazelLabel. Also handle Bazel `.a`-path link forms (static mode) vs
  the solib `-l` form (default dynamic), and the static-vs-dynamic caveat
  (dynamic linking is order-independent, so a project-archive order divergence
  only matters where Bazel links static). Caveats still open: TU keying by
  basename collides across dirs (disambiguate by normalized relative-suffix —
  zstd reports `matched: 0` because its `build/cmake` root and overlaid `lib/`
  sources never align under basename keying), and config alignment (cmake db is
  single-config).

- **Derive `target_libc` / target triple from the probed sysroot.**
  `builtin_sysroot` ships (the probe lifts `CMAKE_SYSROOT` into
  `toolchain.Model` and the emit sets `cc_toolchain_config`'s `builtin_sysroot`
  per (platform, kit), so Bazel passes `--sysroot=`; host builds emit none).
  Still baked: `target_libc` (the `defaultLibcFor` OS-name heuristic), the
  `abi_version = "local"` / `*_system_name` placeholders — really "what the
  sysroot would tell us." Derive them from the probed sysroot/compiler triple
  instead. (Also pair: `toolchain()` emits only `target_compatible_with`, never
  `exec_compatible_with`, so cross exec≠target resolution is unconstrained.)

- **Hermetic sysroot-as-toolchain-inputs.** `builtin_sysroot` tells the
  compiler *where* the sysroot is; for a sandboxed / RBE action to actually
  *contain* it the sysroot's files must be declared as toolchain inputs
  (`cc_toolchain.all_files` / `compiler_files` / `linker_files` / `libc_top`).
  The emit currently sets `all_files = ":empty"` (`unified.go`), i.e. a
  deliberately non-hermetic toolchain that leans on absolute host paths
  (`/usr/include`, `/usr/bin/gcc`) being present in the action — fine under
  local/host-mounted sandboxes, wrong for hermetic RBE. Materialize the sysroot
  tree as a Bazel repo (`new_local_repository` / `http_archive`) and wire
  `libc_top` / `all_files` so actions ship the sysroot. Larger; follows the
  `builtin_sysroot` item.

- **Agent-actionable prompts — AI post-pass (consumer) remains.** The
  deterministic **producer** (`conversion-todos.json`, on by default, wired
  through to project B via the `<name>_converted` convert genrule + `stage-b`)
  and the consumer **contract** shipped. **What's left:** the non-deterministic
  **AI post-pass** that consumes the report to author the Bazel form (an
  `sh_test`/`diff_test` driving the built CLI, one reusable macro per shared
  unit) — deliberately quarantined out of the converter so it stays a pure
  replayable function. It honors the contract: read `preamble` + `todos`, author
  one unit per `id` into the authored-output file (skip ids already present),
  turn `evidence.verification` into the test's assertion, emit plain idiomatic
  Bazel (no cmake re-invocation), and pass the **same render gates** as
  mechanical output (not trusted on faith).
  **Follow-up — root-package source exports for the post-pass.** A real-corpus
  dry-run (glog v0.7.1) surfaced a gap in the file-ownership split: when the
  post-pass authors a test into a *sibling* package (`tests/BUILD.bazel`), its
  call sites need the converter-owned root `BUILD.bazel` to `exports_files([...])`
  the cmake-test-only `.cc`/headers (sources no converted target lists, so the
  converter never exports them) — but the agent is forbidden from editing the
  converter-owned BUILD. Options: (a) converter exports test-referenced loose
  sources behind a stable `filegroup`; (b) post-pass authors a `tests/` package
  *with its own* `exports_files` by staging the sources there; (c) relax the
  no-edit rule to permit append-only `exports_files` blocks. Pick one when the
  consumer ships.

- **Intent-capture survey lens — remaining: corpus-level scoring + richer
  grounding.** The deterministic harness shipped: `converter/cmd/intent-lens`
  has `prompt` (assemble the grounded prompt) and `triage` (classify each
  finding net-new vs already-flagged, write `intent-capture.json`), with the LLM
  judgment quarantined to a pluggable command (`$INTENT_LENS_JUDGE`, e.g.
  `claude -p`; CI stubs it). `scripts/intent-capture-lens.sh` runs the pipeline;
  `run-survey.sh` wires it as the 6th, opt-in lens (`SURVEY_INTENT=1`);
  `scripts/meta-intent-capture-lens.sh` is the render gate; `summary.txt` carries
  a per-element `missed` column. A real-judge full-corpus pass ran (output under
  `docs/survey-artifacts/`, summarized in `docs/survey-corpus.md`) and surfaced
  the producer-gap themes below. **What's left:** (a) **corpus-level scoring** —
  roll the per-element triage queues into an aggregate signal beyond the per-row
  count (a confirmed-miss tally after human triage? severity-weighting? a stable
  subset that reproduces across judge passes?), since the `missed` column itself
  isn't run-comparable; (b) **richer grounding** — the dedup currently grounds
  on `cmake_ref` vs the todos/rejections; feeding the judge the cmake
  codemodel/fileapi facts (targets, tests, install rules) would let triage
  *verify* a claimed miss against structured truth, not just dedup it (sharpened
  if the todo producers populated structured `Anchor.File` uniformly — today only
  the rejection-mirror does).

The intent-lens producer-gap themes follow as their own entries (2026-06-08
full-corpus run; listed in intended work order — absent targets, dropped test
trees, optional-feature deps, codegen instances). Each member's
`docs/survey-artifacts/<member>/intent-capture.json` carries the per-finding
`evidence` + `cmake_ref` to drive a fix + a regression guard.

- **Install/export — residual follow-ups.** The emission gaps the intent lens
  flagged (install(TARGETS)→`pkg_files`, generated-header install, pkg-config
  `.pc`, `<Pkg>Config.cmake`/`<Pkg>ConfigVersion.cmake` generation) all landed.
  Residuals: the generated `<Pkg>ConfigVersion.cmake` is a permissive
  always-compatible stub (the project VERSION isn't in the codemodel); a
  multi-export package whose export is named outside the `*Targets.cmake` /
  `*Exports*.cmake` / `*-targets.cmake` conventions isn't picked up by the
  generated Config.cmake's glob — the robust fix is to plumb the sibling export
  names to `renderConfigFile` and emit explicit `include()`s (BuildInputs runs
  per-installer, so it can't see siblings today); and the generated Config.cmake
  doesn't reproduce a hand-authored `Config.cmake.in`'s richer semantics
  (transitive `find_dependency(<Dep>)`, `@PACKAGE_INIT@` `set_and_check`,
  `check_required_components`). Also: brotli's `.pc` isn't GENERATED at all (its
  `.pc.in` configure_file isn't lifted) — a codegen-lift gap tracked with the
  configure_file theme.

- **Two adjacent flag drops (system/threading-linkopt theme).** The bare
  system-library link drop that headlined this theme is fixed (`-`-prefixed
  `libraries`-role fragments route to linkopts). Two flag drops the lens
  surfaced alongside it remain:
  - build-type-conditional defines hardcoded `1` regardless of `//config`
    (LLVM's `LLVM_ENABLE_ABI_BREAKING_CHECKS` / `LLVM_ENABLE_PLUGINS` / …) —
    needs per-build-type values (a single configure captures one) + a `select()`.
  - dropped `target_compile_features` (googletest's PUBLIC `cxx_std_17`) — the
    target's own compile already gets `-std=c++17` via the `LanguageStandard`
    lift; only PUBLIC propagation to consumers is missing, which Bazel's native
    `cc_library` can't express transitively (no `exported_copts`). Needs a design
    call, not a quick fix.

- **Emit absent targets / subpackages — investigated; mostly configure-scope,
  one real layout gap left.** Investigation (2026-06): the intent lens diffs the
  cmake SOURCE TREE, but the converter faithfully emits only what the codemodel
  (the *configured* build) contains — so three of the four flagged sub-cases are
  NOT converter bugs, they're the build-lens's own reduced configure:
  - **llvm's 19 backends + Testing/* libs**: `llvm.conf` sets
    `LLVM_TARGETS_TO_BUILD=X86` + `LLVM_INCLUDE_TESTS=OFF` → never configured.
  - **mbedtls's `programs/` + test targets**: `mbedtls.conf` sets
    `ENABLE_PROGRAMS=OFF` + `ENABLE_TESTING=OFF`.
  - **vtk's VolumeAMR / GenericBridge / Benchmarks**: non-default VTK modules,
    not enabled by the lens's default module set.

  The remaining **abseil interface-subpackage** case IS a real gap — but a
  *layout* one, not a dropped target. Verified: cmake (even 4.3.3) does NOT emit
  pure-header `INTERFACE_LIBRARY` targets (no sources) into codemodel-v2 —
  abseil's codemodel has 119 targets, **0** INTERFACE_LIBRARY — so abseil's
  interface libs reach the converter only via the trace-synth path
  (`lowerInterfaceLibraries`), which emits them correctly but in the ROOT package
  (no `pkg.SubPackages` entry). Under `--split-packages` the 7 subdirs
  (algorithm/cleanup/functional/memory/meta/types/utility) therefore get no
  BUILD.bazel even though the libs themselves are present + consumable from root.
  (Placement-by-`SubPackages`-entry is regression-guarded by
  `TestEmit_Split_InterfaceLib_PlacedBySubPackageEntry`.)
  **Fix:** `lowerInterfaceLibraries` should set `pkg.SubPackages[name]` from the
  trace `add_library(<name> INTERFACE)` call's declaring CMakeLists dir
  (`AddLibraryCall.File`, relativized like `subPackageDir`), mirroring the
  codemodel path's per-target assignment (`lower.go:1439`).
  **Caveat — validate before shipping:** abseil's interface libs carry the
  repo-ROOT include root (so `#include "absl/<m>/<h>.h"` resolves), which sits
  ABOVE their declaring subpackage; moving them into subpackages interacts with
  split's header-lib / include-root machinery, so a full abseil
  `--split-packages` convert + build AND the other green members must be
  re-validated before landing (the fmt / cp-dir regression lesson).

- **Lower dropped test trees to `cc_test` — extend test-target coverage (10×
  high).** Extends "Test-target coverage" (above): the faithful survey convert
  emits no `cc_test` for abseil (232 `absl_cc_test`), glm (~130), sdl (~50),
  catch2, boost-core, mbedtls, vtk, openblas. **Caveat:** confirm each is a
  real `add_test`/`enable_testing` lowering gap vs. an intentional build-lens
  scope-out before fixing — some members deliberately disable their test tree.

- **Optional-feature conditional deps (find_package under a feature flag, 3×
  high).** LLVM's `LLVM_ENABLE_ZLIB` / `_ZSTD` / `_OPENCSD` deps aren't linked,
  so `Compression.cpp` would fail to link. Same find_package→linkopt mechanism
  as the bare-link fix, tracked distinctly because the dep is gated on a CMake
  feature option the converter must honor (or default).

- **`configure_file` / script-codegen genrule coverage — specific instances
  (5× high).** Generated headers with no genrule: vtk's libproj `proj_config.h`,
  mbedtls's `test_certs.h` / `test_keys.h` Python codegen, cutlass's
  `version_extended.h`. (The curl `configurehelp.pm` correctness case — a
  convert-time temp path `/tmp/convert-element-build-*/` baked into the emitted
  output — is fixed: `recoverConfigureFiles` now scrubs the ephemeral
  build/source-dir prefixes to package-relative paths via
  `reanchorConvertTimePaths`. A sibling check worth doing: whether
  `file(GENERATE)` bakes the same prefixes and needs the same scrub.)

- **A-B-C fidelity harness — remaining: VTK/LLVM gates.** The harness shipped
  CI-wired and **blocking** for the six fixtures (zlib, spdlog, fmt,
  nlohmann-json, Catch2, libpng — 0 impactful deltas each), with two
  complementary signals (library-side `.a` diff + consumer-side `.o` diff) and
  built-in benign-delta auto-classification (FORTIFY/stack-protector, template
  instantiations, `.o` vs `.pic.o`). **Remaining:** VTK / LLVM gates — need each
  project's specific configure flags + tooling and may need larger allowlists.
  LLVM's bazel-build lift is progressing (manual): the monorepo renders in the
  faithful multi-config + split-packages shape, real libraries compile under
  both `--//config:build_type={debug,release}`, the tablegen tool builds, its
  genrules run and emit headers, and consumers that `#include` those generated
  `.inc`s build green via the synthesized `generated_headers` wrapper libs. Still
  open there: the source-tree-input == build-tree-output genrule aliasing
  (`Remarks.exports` in-place rewrite) and the `pkg_files` install-glob
  re-anchoring.
  Acceptance: a converter regression that drops a symbol from the output
  artifact fails CI with a precise per-symbol diagnostic instead of being caught
  only when a downstream consumer breaks.

- **kind:meson Phase B multi-platform production promotion.** The per-platform
  fold for round-2 trace-driven kinds is done and uniformly green across
  kind:make / autotools / cmake-fallback / meson-fallback (render gates
  `scripts/meta-{meson,cmake,autotools}-round2*-multiplatform.sh`). The one thing
  left is *production* promotion of multi-platform meson — externally gated on a
  real FDSDK consumer at scale (today's gate uses the meson-greet smoke fixture);
  no converter/harness work outstanding, promote once a real consumer surfaces
  the need.

- **Trace-side narrowing-audit coverage.** The narrowing-audit gate is blocking
  for the cmake oracle, but the trace-side oracle (the build-tracer + trace.log
  path for round-2 trace-driven kinds) still needs a CI fixture:
  `--trace-source-root` is wired but no e2e job exercises it yet. Add a
  build-tracer-on-CI fixture so the trace-driven sibling gate can run too.

## Later (research / open questions)

- **Genrule command-rewrite token-replace consolidation (deferred from the
  2026-06-08 refactoring audit).** `replaceBareToken` (genrule.go) and
  `replaceBareAnchorAtBoundary` (lower.go) share the same whole-word
  token-boundary logic (space/`=`/`:` guards), and the genrule rewrite chain
  (`rewriteGenruleCmd` → `rewriteToolFromTarget` → `anchorGenruleOutputsToRuledir`
  → `reanchorBuildDirCopyGenrule`) does several similar path/flag substitutions.
  A shared `tokenReplace(str, matchers)` could unify them — but this is the
  correctness-sensitive path the LLVM `$(RULEDIR)`/exec-root anchoring fixes live
  in, so merge it deliberately with the genrule render gates as the guard, not as
  a casual dedup. (The audit's other broad candidate — a unified string-set/dedup
  family — was examined and declined: `stringSliceContains` is already a single
  shared helper, and the dedup variants are semantically distinct
  order-preserving / sorted-adjacent / skip-empty / append-unique forms, not true
  duplicates.)

- **Source-side AC narrowing for autotools.** Bazel's hermetic-action
  model says inputs in → outputs out; you can't have a byte be
  available to the action at exec time without it being in the AC
  key. So narrowing autotools is unavoidably a side-channel story.
  `docs/architecture.md` lays out three options (FUSE, host-fs
  source cache via `--repo_env`, write-a-time registry) and rules
  out two; the third is the path forward but the value-vs-complexity
  trade-off is open.

- **kind coverage — real semantics for the FDSDK-glue placeholders.** All four
  FDSDK-specific glue kinds (`collect_initial_scripts`, `collect_integration`,
  `check_forbidden`, `flatpak_repo`) now have v1 stub handlers (alongside the
  pre-existing `collect_manifest` stub) so FDSDK render reaches completion. Real
  plugin semantics deferred until an FDSDK fixture forces a bazel-build-time
  correctness need; per-kind cost-to-port is documented in
  `docs/fdsdk-coverage.md` (small for the install-tree-walk kinds; `flatpak_repo`
  is bigger — needs ostree at build time). `kind:flatpak_image` /
  `kind:snap_image` retain their structural treatment (filegroup composition over
  deps' install trees), which is the right shape regardless of upstream-plugin
  behaviour changes.

- **Dev-loop guidance for routing local Bazel at the executor.** Two slices
  landed (per-gate cmake prereq honesty + inline cmake-availability check in the
  kind:cmake render gates); today only ~5 targets still pin cmake on the dev's
  box (the converter's `-tags=e2e` Go tests, `e2e-audit-narrowing` +
  `e2e-meta-cmake-round2-fallback-storage-cost`, `record-fixtures`). Closing the
  gap for the bazel-build half — "dev with bazel installed but no cmake can still
  exercise the full e2e loop" — means routing the dev's local `bazel build`
  invocations at the buildbarn executor (the worker image already has cmake). The
  `e2e-meta-buildbarn-re` gate already exercises this shape; the missing piece is
  a documented `--config=remote` knob + CONTRIBUTING.md guidance so devs can opt
  in. The harder follow-on (wrapping `cmakerun.Configure` itself as a Bazel
  action so the converter doesn't need cmake at any layer) is a real
  architectural change; the open question is how the converter's in-process File
  API consumer reads the reply when the cmake-configure step runs on a remote
  node.

- **Two-species split: remotable, cacheable configure + convert.**
  The deeper architecture the item above leads to. `cmake configure`
  must run on the *target* platform P (its `try_compile`/`try_run`/
  `check_*`/`find_package` resolve against P), possibly a subset of
  platforms per element; the converter is a Linux/Go binary not built
  for every P. So split the welded `convert-element-cmake` (which execs
  cmake in-process via `cmakerun.Configure`) into two independently
  remotable+cacheable action species: `configure(element, P)` — native
  cmake on a P worker, **no Go**, emits a File API reply bundle — and
  `convert(element)` — Linux/Go, **no cmake**, consumes the per-platform
  bundles via the existing `--reply-dir` seam and folds them. The File
  API query is language-agnostic (five touch-files), so a configure
  action is just `cmake <argv>` with hooks staged as inputs; argv/hook
  construction stays a shared `cmakerun` function the planner (`write-a`)
  calls. The genex literal two-pass becomes a static
  `configure → analyze → litprobe → convert` graph whose `litprobe(P)`
  command branches on a 0-byte probe (no cmake when empty). **Hard
  invariant: the standalone path keeps working** — `convert-element-cmake
  --source-root` stays a complete, infrastructure-free, full-fidelity
  composition of the same steps; the serialized reply bundle is a complete
  interface so `--reply-dir` is byte-identical to in-process. Native-P
  configure also closes the `try_run` cross-compile fidelity gap
  (`docs/research/cmake_analysis.md` §7). Full design in
  `docs/design/remotable-configure-convert.md` (delete that doc once this
  lands).

---

For how the codebase works *today* (not just what's planned here), see
`docs/architecture.md` (architecture + interop contract + build-time
flow, all in one place) and `docs/codebase-map.md` (the developer-facing
repo tour). `ROADMAP.md` tracks only what's *left*; git history is the
record of what shipped.
