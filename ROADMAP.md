# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`) fail intermittently for environment reasons
  (cmake-config bundle staging on the CI runner; userns / fuse permissions on
  Ubuntu 24.04 runners; bazel 9 toolchain expectations). These don't reflect
  product issues but they make PR review noisier than it should be.

## Next

- **Reproducible `find_package` host-installs.** The grpc/protobuf build-lens
  `.conf` files hardcode `/tmp/absl-install` (host-installed abseil) and the
  umbrella deps list is a snapshot of the pinned abseil. Fold the abseil (and
  protobuf, for grpc) host-installs into the `SessionStart` hook so the lens is
  reproducible without a manual prep step. (Carried from protobuf; grpc itself
  is green.)

- **Green the remaining heavyweight corpus members: vtk (tail), cuda-samples.**
  25/26 are green (protobuf + sdl + vtk + grpc landed). Remaining:
  - **vtk** — configures + converts with 0 rejections, and the 2026-06-10
    re-run under the data-label + fused-source fixes ANALYZES fully green
    (2,527 targets; previously an 80-missing-input hard abort on IOInfovis).
    The `--keep_going` recount: **6,606/6,608 actions ran, 2,405/2,527
    top-level targets built, exactly 3 ROOT failures** (the rest of the gap
    is transitively blocked by them). REMAINING TAIL (well-diagnosed):
    - **wrap-hierarchy genrule EXECUTION** (`vtkCommonCore-hierarchy.txt` et
      al.): analysis staging is fixed (the `.args`/`.data` response files are
      real cross-package labels now), but the genrule cmd still references
      them by cmake build-dir-relative path (`@Common/Core/CMakeFiles/…args`)
      instead of `$(location …)`, carries ninja depfile plumbing (`-MF …d` +
      `cmake -E cmake_transform_depfile …` with an absolute cmake path), and
      the BAKED args/data content embeds convert-time absolute `-I`/source
      paths that need re-anchoring to exec-root form. Three mechanical fixes
      in the genrule-rewrite family.
    - **proj_db** (`cmake -P generate_proj_db.cmake` fails at
      `include(sql_filelist.cmake)` — relative include not staged in the
      genrule's cwd at build time; plus the `$<TARGET_FILE:VTK::sqlitebin>`
      built-tool reference, see the built-tool genrule recovery note in
      `scripts/build-lens/vtk.conf`).
    - **vtkProbeOpenGLVersion LINK** (1 binary): undefined `vtkFXAAFilterFS`
      / `vtkTextureObjectVS` — vtkEncodeString-generated shader-string
      symbols (the cc_embed lift) not reaching the probe binary's link;
      likely a missing dep edge or alwayslink on the embedded-shader objects
      under static archive linking. Distinct mechanism from the two genrule
      items.
    - The earlier ~19 configure_file same-dir config-header failures
      (`kwsysPrivate.h` / `proj_config.h` / `pugiconfig.hpp`) had their
      converter fix shipped previously; the 2026-06-10 sweep shows no
      compile-phase recurrences of that class.
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

- **Derive build-lens link mode from the project's static config (drop the
  per-member `--dynamic_mode=off` knobs).** The build lens forces
  `BUILD_SHARED_LIBS=OFF` (the forced-static alignment), so every surveyed
  project's codemodel reports `STATIC_LIBRARY` targets and cmake links its test
  executables against the static archives — pulling in ALL objects, including
  `-fvisibility=hidden` internals the tests reference. Bazel's DEFAULT
  `--dynamic_mode=default` instead builds those cc_libraries as `.so`s in
  fastbuild/dbg, which don't export the hidden internals, so the cc_tests fail
  to link. Today this is hand-patched per member via `.conf` `BAZEL_FLAGS=
  --dynamic_mode=off` (glog, llvm) and re-threaded into the symbol-fidelity
  lens's release rebuild. The faithful, DERIVED fix: when the surveyed config is
  all-static (codemodel has `STATIC_LIBRARY` targets and no `SHARED_LIBRARY`/
  `MODULE_LIBRARY`), the build lens should default `--dynamic_mode=off` — i.e.
  build the link model the project actually uses — and the per-member knobs drop
  out. Note this is a LINK-MODE change, not a fidelity one (it doesn't alter the
  `.a`/`.lo` archives the symbol-fidelity lens compares — only whether
  test/binary linking is static), so the payoff is robustness + dropping knobs,
  not a fidelity number. It needs a build-lens corpus re-green first: forcing
  static linking can surface ODR / duplicate-symbol issues a dynamic build
  tolerated (cf. the curl shared/static SIGSEGV precedent above). Fold it in when
  `SURVEY_SHARED`'s default flips (link mode gets re-validated corpus-wide
  anyway), or as its own deliberate re-green pass.

- **Test-target coverage — enable the scoped-out members' tests.** The build
  lens builds `//...`, which already INCLUDES test targets where the project's
  tests need no extra infra (fmt, libxml2, glog, glm, googletest, abseil
  surface, curl test PROGRAMS). The `add_test`→`cc_test` lowering itself is
  sound and shape-agnostic — it's driven by cmake's generated
  `CTestTestfile.cmake` (`converter/internal/ctest`), so it captures every registration
  (`add_test(name exe)` AND `add_test(NAME … COMMAND …)`) once the executable +
  its registration are CONFIGURED. So a member's "no `cc_test`" is never a
  lowering bug; it's that tests weren't configured (a missing test dep, or a
  `.conf` `BUILD_TESTING`/`*_TESTS=OFF`). The remaining members scope tests out
  via a `.conf` flag, each for a concrete reason — to enable, resolve that
  reason:
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
  - **openblas** (`BUILD_TESTING=OFF`): the Fortran ruleset gate is LIFTED —
    `fortran_library` (//rules:fortran.bzl) now compiles the real reference
    LAPACK + BLAS, and both the Fortran and C_LAPACK shapes survey green
    (`openblas` / `openblas-clapack`). Remaining: the BLAS test EXECUTABLES
    (`?blat1/2/3`) are Fortran-only `add_executable`s, so retagFortranTargets
    degrades them to (non-runnable) fortran_library — running them as real
    cc_test/fortran binaries (a `fortran_binary` rule) is the follow-up.
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

- **Include over-propagation is the `root_headers` element-root grant, NOT
  per-target include scope (THE real include-fidelity item — DETECTION shipped,
  fix parked).** A convert-time diagnostic now flags it:
  `EmitSplit`'s `overGrantedIncludeRoots` emits a `cmake-include-over-grant`
  warning to stderr naming each nested include-root re-exported via the
  element-root header-lib forwarding (OpenBLAS prints `lapack-netlib/LAPACKE/
  include`), so the over-grant is visible on every split convert without a lens
  run. The fix below is therefore safe to defer. Define-scope
  (#535) and link-dep-scope (#536) made those axes faithful via cmake's
  usage-requirement signal; the include analog (B1/B2, #539 — directory-scoped
  `include_directories()` + the `INTERFACE_INCLUDE_DIRECTORIES` whitelist →
  private `-I` copt) shipped and is build-safe corpus-wide, but a compile-db
  sweep with it active shows it moves **zero** of the include over-propagation:

  | member | over-propagated include | consumer TUs cmake never gave it to |
  |---|---|---:|
  | OpenBLAS | `lapack-netlib/LAPACKE/include` | 1,724 |
  | mbedtls | `3rdparty/everest/include/everest{,/kremlib}` | 108 |
  | catch2 | `generated-includes` | 107 |
  | libevent | `test` | 17 |

  The cause is structural, not scope: under `--split-packages`, a public
  include-root becomes a synthesized header lib (with `includes=["."]`), and the
  **`root_headers` / `element_root_headers` element-root grant** publicly `deps`
  those libs and is itself depped broadly (so any element-root-relative include
  resolves anywhere). The header lib exists whenever *some* target exports the
  dir PUBLIC (OpenBLAS's `LAPACKELIB` legitimately does), so B1/B2 can't suppress
  it — and `root_headers` then grants its `includes` to every consumer, not just
  the dir-owner's real consumers. **The fix is the root-grant breadth:** make a
  target dep only the header libs it actually needs (precise per-target
  header-lib wiring), or split the aggregate so it provides the element-root
  `-I` + the headers-as-inputs WITHOUT re-propagating each member lib's own
  `includes`. **Precise mechanism:** `headerLibTarget`
  (`converter/emit/bazel/split.go:404-432`) makes each include-root header lib
  `deps` every STRICT-DESCENDANT include-root header lib for recursive
  reachability — and that dep propagates each descendant's `includes=["."]` (its
  bare `-I<dir>`). cmake only grants element-root-RELATIVE reachability (`#include
  "lapack-netlib/LAPACKE/include/lapacke.h"`), NOT the bare path (`-I…/include`
  for `<lapacke.h>`), so the forwarding over-grants; same shape in
  `rootHdrAggTarget` (the `element_root_headers` aggregate). The fix exposes the
  descendant's HEADERS as inputs (re-homed via `include_prefix`, or a hdrs-only
  filegroup) WITHOUT its `includes`. Bazel has no "deps for hdrs but not
  includes" slot, so this is a split redesign. High-value (≥1,950 TUs across ≥4
  members: OpenBLAS/mbedtls/catch2/libevent) but architectural — full corpus
  build re-green required.

- **Interface-driven linkopt scoping (`INTERFACE_LINK_OPTIONS`) — deferred to
  the shared-lib work; masked under forced-static.** The fourth usage-
  requirement axis: Bazel `linkopts` on a `cc_library` propagate transitively to
  linkers, but cmake's `LINK_OPTIONS` (private) don't — only
  `INTERFACE_LINK_OPTIONS` do, so a private link option over-propagates IN
  PRINCIPLE. But two things make it a non-issue to fix right now: (1) the
  converter populates `LinkOpts` from the codemodel's LINK command fragments,
  which a STATIC_LIBRARY barely has (an archive is `ar`, no link step), and the
  build lens forces `BUILD_SHARED_LIBS=OFF` — so there's no measurable
  over-propagation to validate against; (2) Bazel has no "local linkopts" slot
  (unlike `local_defines` / `implementation_deps`), so a private link option has
  no clean non-propagating home on a static lib — the faithful move would be to
  DROP a non-exported `LINK_OPTIONS` on a non-binary target, which risks losing a
  genuinely-needed flag without a validation signal. Revisit alongside
  `SURVEY_SHARED=1` / `cc_shared_library`, where private `.so` link options
  actually matter and are measurable via the link-order lens.

- **Symbol-fidelity lens — SHIPPED (v1, opt-in `SURVEY_SYMBOL_FIDELITY`).**
  Wired into `run-survey.sh` as the LAST lens — runs after the build, only when
  the build lens passed (the pipeline ordering: structural → build →
  symbol-fidelity). For each selected member with a per-member config
  `scripts/build-lens/<name>.symfidelity` (`SYMFID_TARGET` + `SYMFID_ARTIFACT`
  or `SYMFID_{CMAKE,BAZEL}_ARTIFACT` [+ `SYMFID_CMAKE_FLAGS`]) it reuses
  `scripts/run-fidelity.sh` (the self-contained cmake-build → convert → bazel
  build → `cmd/fidelity-compare` A-B-C with benign auto-classification) and the
  member's `testdata/fidelity/<name>.allowlist.txt`, writing
  `<out>/<name>/symbol-fidelity.json` (`ok`/`FAIL`); members without a config
  self-skip. Validated: `SURVEY_BAZEL_BUILD=zlib SURVEY_SYMBOL_FIDELITY=1` →
  `zlib: symbol-fidelity -> ok` (seeded `zlib.symfidelity`). **v1 scope /
  follow-ups:** seed `.symfidelity` for the other fidelity fixtures (spdlog /
  fmt / catch2 / libpng / nlohmann-json — their Makefile params already exist)
  and the broader corpus; reuse the build lens's `build-ws` bazel artifacts
  instead of run-fidelity's own from-scratch bazel build (an optimization); a
  survey summary column. Design rationale: the
  build lens (`SURVEY_BAZEL_BUILD`) proves the converted graph builds
  under `bazel build //...`; the compile-commands lens
  (`SURVEY_COMPILE_DB`) proves per-TU
  flag parity at *analysis* time. Neither proves the **emitted artifact
  carries the same symbols** cmake's does — the question the CI `fidelity`
  job already answers for a fixed fixture set (zlib / fmt / spdlog / catch2
  / libpng / nlohmann-json) via `cmd/fidelity-compare`. Bring that
  comparison to the **whole survey corpus** as a new opt-in lens
  (`SURVEY_SYMBOL_FIDELITY`, gated like the build lens —
  `auto`/`all`/name-list, with the same `skip(no-bazel)` / `skip(rej)` /
  `skip(convert)` short-circuits and an `ok`/`FAIL`/`skip(...)` column).
  Unlike the compile-db lens it needs BOTH halves built, not just analysis:
  (1) the Bazel build the build lens already produces (`build-ws`, `bazel
  build //...`), from which the converted `.a` (library-side) / consumer
  `.o` (consumer-side) symbols are saved; and (2) a **from-scratch cmake
  build** of the same source (configure + compile + archive, in the build
  lens's static `BUILD_SHARED_LIBS=OFF` shape + the `.conf` cmake-defines so
  both sides align) whose symbols are the ground truth. Then diff the two
  symbol sets with `cmd/fidelity-compare`, **reusing** the existing harness
  (`scripts/run-fidelity.sh`'s library- and consumer-side modes + the benign
  auto-classification: FORTIFY / stack-protector hardening, C++
  template-instantiation pairs, `.o` vs `.pic.o`), not reimplementing it.
  Each corpus member gets its **own allowlist of accepted drift** (the
  `testdata/fidelity/<name>.allowlist.txt` shape; absent/empty = "no deltas
  tolerated") — so a member's known-benign symbol deltas are recorded
  per-project and a new impactful delta is a real signal. Report-only,
  written per-project (`<out>/<name>/symbol-fidelity.json`). Boundaries: on
  top of the build lens's bazel, the cmake build half needs cmake + a
  C/C++ toolchain on PATH (so the lens self-skips when either is absent);
  per-config alignment is handled by forcing the static shape on both
  sides, and the basename / relative-suffix symbol-keying caveats carry over
  from the existing harness. **Complements, doesn't replace,** the
  fixed-fixture CI `fidelity` job: that job is the *blocking* guard on the
  curated set; this lens is the *broad, opt-in, allowlist-per-member* sweep
  across the whole corpus (the symbol-level sibling of how the build +
  compile-db lenses already widen their fixed-fixture CI gates to the
  corpus).

- **Source-narrowing-compatibility lens — SHIPPED (v1, opt-in
  `SURVEY_NARROWING_COMPAT=1`).** `scripts/narrowing-compat-lens.sh` (wired into
  `run-survey.sh` as a STRUCTURAL lens — runs before the build) converts the real
  source tree (capturing the read-set via `--out-read-paths`), makes a copy with
  every SOURCE/HEADER file zeroed except the read-set (build-system files —
  CMakeLists/`*.cmake`/`*.in` — stay real so cmake still configures), re-converts
  with the same flags, and asserts a byte-identical `BUILD.bazel.out` (modulo the
  source-root + ephemeral cmake-build-dir paths). A diff is a narrowing-soundness
  bug — the converter secretly read a zeroed source byte — and the diff names the
  affected srcs/hdrs. `ok`/`FAIL`/`skip(...)`, report-only
  (`<out>/<name>/narrowing-compat.json`). Validated: zlib / spdlog / insrc → ok;
  fmt self-skips (configure-time link on zeroed sources). **v1 scope / follow-ups:**
  zeros source/header bytes (the narrowing target) but keeps ALL `*.cmake`/`*.in`
  real rather than only the read-set ones (avoids the `include(<name>)`-arg vs
  file-path mismatch in the read-set); per-zeroed-file bisection to pinpoint the
  exact culprit (the diff already names the srcs/hdrs); a survey summary column.
  Complements the static `narrowing-audit` (`cmd/audit-narrowing`) lower bound
  with empirical proof. Design rationale: the orchestrated path (project A →
  project B) runs `convert-element-cmake` against **zero-stub sources** — the
  narrowing /
  FUSE source layer presents real bytes only for the declared read-set and
  0-byte stubs for everything else (see `docs/design/sources.md`,
  `docs/design/narrowing-audit.md`). The converter's translation is meant
  to be a pure function of the codemodel + trace + the build-system files
  (CMakeLists / `.cmake` / `configure_file` inputs), **not** of the `.c` /
  `.cpp` / `.h` source content — so a BUILD that differs when those *bytes*
  are zeroed is a hidden byte-dependency that would make the orchestrated
  convert diverge from (or be wrong vs.) the survey-time one. Today the
  `narrowing-audit` (`cmd/audit-narrowing`) guards this **statically** — it
  compares the per-element narrowing patterns against cmake's
  configure-reads oracle — but it is explicitly *"a high-signal lower bound
  … not a proof"*: an empty undercoverage report is necessary-but-not-
  sufficient for soundness. This lens is the **empirical proof** that
  closes the gap: for each surveyed project, make a copy with every source/
  header file truncated to 0 bytes **except** the element's declared
  narrowing read-set (CMakeLists.txt is always real / special-cased;
  `*.cmake` modules and `configure_file` `*.in` templates only insofar as
  the element's read-set names them — where it doesn't and the convert
  depends on one, that omission is exactly the narrowing gap this lens
  catches), re-run the *same* convert (same flags,
  including the now-default `--emit-source-comments`, which reads CMakeLists
  comments, not source bytes — so it must stay byte-identical too), and
  assert the emitted `BUILD.bazel.out` is **byte-for-byte identical** to the
  real-source convert. A diff is a narrowing-soundness bug — and the diff
  names exactly which zeroed file the converter secretly depended on, the
  actionable signal the static audit can't give. Opt-in via a `SURVEY_*`
  knob (e.g. `SURVEY_NARROWING_COMPAT`), gated/short-circuited like the
  other lenses, `ok`/`FAIL`/`skip(...)` column, report-only
  (`<out>/<name>/narrowing-compat.json` listing the diverging files). Needs
  only cmake (the convert's own configure) — no Bazel build — so it's
  cheaper than the build/symbol lenses and can run wherever the diagnostic
  convert does. Boundaries: it surfaces real byte-dependencies as `FAIL`s,
  which is the point; a member with a *known/accepted* dependency the
  read-set deliberately keeps real (a `file(READ <src>)`-style configure
  input) is handled by that file being in the read-set (kept real), not by
  an allowlist — keeping the lens's contract strict (zero tolerated diffs).
  Complements the static `narrowing-audit` gate (lower bound) with the
  dynamic proof (does narrowing actually preserve convert output?), the
  sibling of the build/compile-db/symbol lenses for the source-mount
  dimension.

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

- **One remaining flag drop (system/threading-linkopt theme).** The bare
  system-library link drop that headlined this theme is fixed (`-`-prefixed
  `libraries`-role fragments route to linkopts), and the build-type-conditional
  configure_file values (LLVM's `LLVM_ENABLE_ABI_BREAKING_CHECKS`) now ship via
  the per-config bake (`--per-config-bake`: detection-gated single-config
  re-configures whose differing write_file bodies render as
  `content = select({"//config:<name>": …})`; gate
  `scripts/meta-cmake-per-config-bake.sh`). Remaining:
  - dropped `target_compile_features` (googletest's PUBLIC `cxx_std_17`) — the
    target's own compile already gets `-std=c++17` via the `LanguageStandard`
    lift; only PUBLIC propagation to consumers is missing, which Bazel's native
    `cc_library` can't express transitively (no `exported_copts`). Needs a design
    call, not a quick fix.
  - per-config bake residue: the lift covers the write_file bake tier; the
    LIFTED configure_file tier (`--lift-configure-file`, values-dict driven)
    still substitutes one configure's variable dump for all arms, and a
    non-text (base64-genrule) body can't carry select arms — both degrade to
    the primary config's view, tagged/un-tagged respectively.

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
  **SHIPPED (2026-06).** Recovered the declaring scope via the cmake trace
  **frame stack**: an earlier idea — set `pkg.SubPackages[name]` from the trace
  `add_library(<name> INTERFACE)` call's `AddLibraryCall.File` — does NOT work
  for abseil, because its interface libs are declared via the `absl_cc_library`
  **function** in `CMake/AbseilHelpers.cmake` (`add_library(${_NAME} INTERFACE)`
  at line 321), so the trace's `file` for that call is the helper module, not the
  declaring `absl/<m>/CMakeLists.txt`. So `TraceEvent` now decodes cmake's
  `frame` depth and `AddLibraryCall.DeclFile` resolves to the nearest enclosing
  `CMakeLists.txt` frame (`declaringScopeFile`); `lower` sets `pkg.SubPackages`
  from it via `subPackageDirFromFile`. The repo-ROOT include-root caveat resolved
  itself — split wires `element_root_headers` as a dep so `#include
  "absl/<m>/<h>.h"` still resolves from the subpackage. Validated end-to-end:
  `SURVEY_BAZEL_BUILD=abseil SURVEY_SPLIT_PACKAGES=1 run-survey.sh` →
  `abseil 0 0 0 … ok ok` (0-rej, analyze + `bazel build //...` both green under
  `--split-packages` + multi-config); 21 sub-package BUILD.bazel files, 18
  interface libs placed (`atomic_hook` → `absl/base`). `pkg.SubPackages` is
  consumed only by `EmitSplit`, so non-split output is byte-identical. Guards:
  `TestExtractAddLibrary_DeclFileFromFrameStack` +
  `TestToIR_TraceInterfaceLib_PlacedInDeclaringSubPackage`.

- **Lower dropped test trees to `cc_test` — investigated; not a lowering bug,
  folded into "Test-target coverage."** The intent lens flagged no `cc_test` for
  abseil (232 `absl_cc_test`), glm (~130), sdl (~50), catch2, boost-core,
  mbedtls, vtk, openblas. Investigation (2026-06): this is the same
  configure-scope/enablement story as theme 4, NOT a converter gap — the
  `add_test`→`cc_test` lowering is sound and shape-agnostic (driven by cmake's
  `CTestTestfile.cmake`; proven by fmt/libxml2/glog), so the absences are tests
  that weren't CONFIGURED: mbedtls (`ENABLE_TESTING=OFF`) + openblas
  (`BUILD_TESTING=OFF`) explicitly scope tests off in their `.conf`; abseil's
  tests need GTest (not wired); the rest are dep-availability / faithful-survey-config
  gaps. The actionable enablement work (wire each member's test dep) is tracked
  per-member under "Test-target coverage" above; there's no separate lowering
  fix to make here.

- **Optional-feature conditional deps (find_package under a feature flag, 3×
  high).** LLVM's `LLVM_ENABLE_ZLIB` / `_ZSTD` / `_OPENCSD` deps aren't linked,
  so `Compression.cpp` would fail to link. Same find_package→linkopt mechanism
  as the bare-link fix, tracked distinctly because the dep is gated on a CMake
  feature option the converter must honor (or default).

- **`configure_file` / script-codegen genrule coverage — specific instances
  (5× high).** Remaining generated headers with no genrule: mbedtls's
  `test_certs.h` / `test_keys.h` (Python-script `add_custom_command` codegen —
  needs the python-script genrule recovery, shared with the mbedtls test-tree
  work) and cutlass's `version_extended.h`. Fixed so far:
  - **vtk's libproj `proj_config.h`** — its `configure_file(cmake/proj_config.cmake.in
    src/proj_config.h)` lives in an `include()`d module (`cmake/ProjConfig.cmake`)
    with a RELATIVE output. `recoverConfigureFiles` anchored relative outputs to
    `dir(CallFile)` (the module's `cmake/` dir), but `include()` doesn't change
    `CMAKE_CURRENT_BINARY_DIR` — cmake writes to the INCLUDER's scope
    (`vtklibproj/src/`), so the computed path was wrong and the output silently
    dropped. Now anchored to the deepest codemodel directory SCOPE containing the
    call file (`dirScopeRel`), which is the includer for an included module and
    `dir(CallFile)` for a normal CMakeLists call (unchanged). Guarded by
    `TestRecoverConfigureFilesFromCalls_IncludedModuleRelativeOutput` +
    `TestDirScopeRel`.
  - **curl's `configurehelp.pm`** (correctness) — convert-time temp path
    `/tmp/convert-element-build-*/` baked into output; `reanchorConvertTimePaths`
    scrubs the ephemeral build/source-dir prefixes. (Sibling check still worth
    doing: whether `file(GENERATE)` bakes the same prefixes and needs the scrub.)

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

- **execute_process file-producing lift — keyword expansion (fixture-driven).**
  `liftFileProducing` conservatively refuses WORKING_DIRECTORY / ENVIRONMENT /
  TIMEOUT / INPUT_FILE / ERROR_FILE (execute_process.go ~1620-1630); every
  such refusal now surfaces in `conversion-todos.json` as a structured
  `execute-process-refusal` todo with file:line + argv, which is the demand
  signal to lift on. The argv-declared codegen shape (`tool <in…> <out…>`,
  inputs/outputs in the argv) LIFTS now — classification from the
  configure's on-disk evidence, no convert-time re-execution; gate
  `meta-cmake-execute-process-argv-codegen.sh`. UNSPECIFIED outputs (not
  in the argv at all) also lift declaratively — File-API consumed
  build-dir sources as demand, ninja's output set as exclusion, argv
  linkage via directory-operand containment (enumerated outs, re-run with
  the operand → `$(RULEDIR)/<dir>`) or derived-name correlation (bake
  tier); single-claim ambiguity rule, declines stay loud refusals; gate
  `meta-cmake-execute-process-unspecified-outs.sh`. NESTED cmake builds
  (the superbuild-at-configure idiom: `execute_process(${CMAKE_COMMAND}
  -S … -B …)` + `cmake --build`) also LIFT now — pass 1 detects the
  (src, build) pair, a warm second pass stages File API queries into the
  nested build dir and re-configures, and the nested reply lowers
  recursively (labels anchored at the outer root) and merges: nested
  targets land in the outer BUILD, archive link fragments wire to their
  labels, and nested configure-generated headers bake; gate
  `meta-cmake-nested-cmake.sh`. Documented residues: no nested TRACE (we
  can't inject argv into the project's own cmake call), so the nested
  configure_file ladder degrades to the header bake; not-lifted nested
  builds (offline runs, `--two-pass-genex=false`) surface as a warning +
  `nested-cmake-not-lifted` todo instead of the historical Tier-1 abort;
  doubly-nested builds warn from the inner lowering. Remaining deferred
  variant: a bake fallback for non-PATH-portable tools on the
  argv-declared shape (capture the configure's bytes via bakeFileTarget
  instead of re-running). Assessed mechanical cost ordering when a fixture lands:
  ENVIRONMENT (`env 'A=B'` prefix; guard values embedding convert-time abs
  paths) → INPUT_FILE (`< "$(location <rel>)"` + srcs when source-anchored;
  refuse build-dir stdin chaining) → ERROR_FILE (a SECOND output: the shared
  single-out `"$@"` cmd template must switch to per-output `$(location)`) →
  WORKING_DIRECTORY (`cd` breaks execroot-relative `$(location)`/`$@` — every
  reference needs `$$PWD`-absolutizing and the anchor contract changes) →
  TIMEOUT (keep refusing absent evidence; silently ignoring changes failure
  semantics). Sibling gap narrowed but open, same demand channel:
  side-effect WRITERS whose outputs are neither consumed (so no File-API
  demand) nor dir/stem-correlatable still refuse loudly — a
  `--cmake-script-trace`-style strace/fsmonitor capture for arbitrary
  tools is the research item.

- **PCH forced-include lift — fidelity residue.** The lift (shipped: cmake's
  `target_precompile_headers` forced-include semantics expand into ordered
  `-include` copts, incl. REUSE_FROM; gate `scripts/meta-cmake-pch.sh`;
  corpus acceptance verified — the sdl build+compile-db lens re-run went
  build `ok` with the prior 223-TU `missing_in_bazel: ["-include"]` copt
  mismatch collapsed to ZERO mismatches) has three documented v1 residues to
  revisit if a corpus member trips on them: (1) cmake's generated cmake_pch
  header carries `#pragma GCC system_header`, so warnings INSIDE declared PCH
  headers are suppressed under cmake but can fire under the direct `-include`
  (a `-Werror` project could break — the fallback is materializing a literal
  mirror of cmake_pch.h[xx] and force-including that one file); (2) a
  per-config-VARYING PCH list rides the primary configuration's view — the
  multi-config fold strips the per-config `cmake_pch` arm tokens
  (`filterPCHCoptArm`) rather than re-expanding the list per `//config:*`
  arm; (3) the expanded pairs append at the tail of copts rather than the
  cmake_pch `-include`'s original compile-line position, so a target that
  ALSO adds its own non-PCH forced include sees a different forced-include
  processing order than under cmake (matters only when one forced header
  depends on the other's macros).

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
