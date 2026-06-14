# Roadmap

This repo is a **transition tool**. Its success state is "you don't
need it anymore — your downstream builds are plain Bazel." Everything
below is in service of getting more BuildStream projects across that
transition cleanly.

## Now

- **grpc build-lens regression (NOT shared-specific — static control fails
  identically).** As of 2026-06-12 the grpc lens run is red in BOTH link
  modes at the same spot: `grpc_cpp_plugin`'s compile picks protobuf headers
  whose `FileDescriptor::name()` returns `string_view` (grpc v1.68's
  generator code wants `std::string`). Root shape: the find_package
  ATTRIBUTION of `/tmp/protobuf-install/lib/lib{protoc,upb,utf8_validity}.a`
  missed (the emitted target carries
  `cmake-codegen-find-package-attribution-missed=…` tags, the fragments are
  ELIDED, and no `@protobuf//:protoc_lib` dep is wired) — the conf's GREEN
  record says those attributed at green time. Pins are unchanged (grpc
  v1.68.0, BCR protobuf resolves 33.4, host protobuf-install 31.1 rebuilt by
  install-survey-deps.sh). Suspects: a converter-side attribution change
  since the grpc greening, or the rebuilt host install differing from the
  green-era one. Needs a bisect against the green-era converter or an
  install-tree diff; evidence preserved in the tags + this note.

- **CI baseline.** A handful of e2e jobs (`cmake + bwrap`,
  `bazel build downstream`) fail intermittently for environment reasons
  (cmake-config bundle staging on the CI runner; userns / fuse permissions on
  Ubuntu 24.04 runners; bazel 9 toolchain expectations). These don't reflect
  product issues but they make PR review noisier than it should be.

## Next

- **Faithful SHARED-library conversion (`cc_shared_library`) — remaining:
  LLVM re-green + edge cases.** The WHOLE POINT of shared is FIDELITY —
  to build what cmake would actually build (the survey forces
  `BUILD_SHARED_LIBS=OFF` for simplicity, but static is NOT the project's
  default; that forced-static is the deviation this work removes). The lift
  (`--emit-shared-libraries`, survey `SURVEY_SHARED=1`) is validated green on
  25 members: the original 9 probes (zlib, fmt, libxml2, brotli multi-lib,
  curl multi-package + the SIGSEGV root-cause, glog, spdlog, mbedtls
  multi-lib, protobuf) plus the 2026-06-12 corpus sweep (libpng, catch2,
  googletest, glm, nlohmann-json, boost-core, eigen, abseil, cryptoauthlib,
  zstd, libevent — after the dynamic_deps-on-cc_library fix it surfaced —
  sdl, OpenBLAS, openblas-clapack, and VTK: 2,884 targets, full `bazel build
  //...` green, the +156 over the static sweep being the cc_shared_library
  wrappers). Default emit is byte-identical (opt-in).
  **Remaining:** grpc (blocked on the lens regression tracked under Now —
  its red is mode-independent, so it neither blocks nor validates shared),
  the heavy LLVM (needs a bigger-disk host — the one member not coverable
  in a web-session container); carry the `.so` in runfiles for `bazel run`/test;
  `MODULE_LIBRARY` dlopen semantics; and consider flipping `SURVEY_SHARED`
  to the DEFAULT once the corpus is green under it (so green + the fidelity
  lens run against the config cmake produces).

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

- **Expand the survey corpus: BuildBox + BDE (new cmake patterns).** Two
  projects that exercise idioms the current corpus doesn't:
  - **BuildBox** (`gitlab.com/BuildGrid/buildbox`) — ONBOARDED, structural
    lenses GREEN; build-lens follow-on below. A monorepo of ~20 tools gated
    by tri-state `enable_tool(AUTO/ON/OFF)`, with REAPI proto codegen through
    a CUSTOM `protoc_compile()` wrapper (`cmake/BuildboxCommonProtoc.cmake`) —
    a different shape from the standard `protobuf_generate`/`grpc_cpp_plugin`
    macros the grpc/protobuf members use. Landed (`make fetch-buildbox` +
    `scripts/build-lens/buildbox.conf`, core-scoped `TOOLS/OCI/TESTING off`;
    needs the recent `/tmp/absl-install` for `absl::log_initialize`): the
    custom protoc codegen recovers cleanly as 42 genrules, and rejections /
    coverage / todos / narrowing-compat are all 0 (14 benign idiom findings).
    **Build-lens follow-on (the gate for compile-db / build / symbol / ELF —
    all 4 blocked on the same thing):**
    - **Codegen → RECOGNIZED NATIVE RULE, via an extensible recognizer
      registry in the converter (decided).** The converter RECOGNIZES a codegen
      invocation and emits the IDIOMATIC native Bazel rule directly — protoc →
      `proto_library` + `cc_proto_library` (+ `cc_grpc_library` for the service
      variant). Self-contained output (NO mandated gazelle run); gazelle drops
      to *maintainer*, not generator. This supersedes both the genrule-first
      framing AND the earlier "defer protos to a mandated gazelle run" idea.
      - **Mechanism (the extensibility seam):** a `CodegenRecognizer` registry.
        Each recognizer `Match`es a recovered custom-command by its DRIVER tool
        + argv shape (e.g. `protoc … --cpp_out`), and `Lower`s it to the native
        `ir` rule(s) + the consumer-dep wiring + the MODULE/load deps. First
        match wins; NO match → the generalized hermetic-genrule FALLBACK
        (below). Adding `flatc`/`thrift`/Qt `moc`/… is REGISTERING a recognizer,
        not core surgery — that's the extensibility.
      - **Why converter-emit, not defer-to-gazelle:** the converter has the
        DECISIVE signal — the actual custom-command argv from the codemodel /
        trace — so it recognizes by TOOL, not by file extension. That handles
        the ambiguous-input case gazelle can't: a common extension like `.xml`
        fed to *different* tools (uic vs gdbus-codegen vs glib-compile-resources)
        in different targets is disambiguated by the argv the converter sees,
        whereas gazelle (dispatching per source file, at best content-sniffing)
        can't reliably pick the generator. And it keeps the output
        SELF-CONTAINED (no mandated-gazelle-run contract change).
      - **Reuses existing machinery:** the cross-package `proto_library(deps=…)`
        edges come from each `.proto`'s `import` statements via the existing
        `protoImportClosure` walker (`genrule.go`); the consumer-dep wiring
        (`#include "x.pb.h"` → the `cc_proto_library`) rides the existing
        `CodegenHeaderConsumers`.
      - **Spike evidence (recast: it proves the TARGET SHAPE the recognizer
        emits, + that the dep-graph is computable).** Two `bazel run //:gazelle`
        spikes with `gazelle_binary(languages=["@gazelle//language/proto",
        "@gazelle_cc//language/cc"])`: (i) a 1-`.proto` + consumer generated the
        full chain — `proto_library`, `cc_proto_library`, consumer
        `implementation_deps`; (ii) a 2-`.proto` CROSS-PACKAGE import
        (`pkg/b/b.proto` imports `pkg/a/a.proto`, separate packages) →
        `proto_library(//pkg/b:…, deps=["//pkg/a:…_proto"])`, NO
        `import_prefix`/`strip_import_prefix` needed (workspace-relative import
        resolves directly), and `bazel build //...` GREEN. So the recognizer
        emits exactly that shape, and (ii) confirms the rooted-import case (the
        #625 layout fork) resolves with plain `proto_library` deps — computable
        from the `.proto` imports.
      - **The recognizer is also the OUTPUT AUTHORITY (a second payoff —
        recovery, not just emission).** A recognized tool has a PREDICTABLE
        output convention, so the recognizer derives the exact output set from
        the input(s) + flags — `foo.proto` + `--cpp_out` → `foo.pb.{h,cc}`;
        `--grpc_out` (+ `generate_mock_code=true`) → `foo.grpc.pb.{h,cc}` +
        `foo_mock.grpc.pb.h` — independent of how completely cmake recorded
        them. That fills the gap where the producer→output mapping is fuzzy in
        the codemodel/trace, making consumer wiring DETERMINISTIC for recognized
        tools (it replaces the inference the generic genrule recovery /
        `stageSiblingGeneratedHeaders` / `CodegenHeaderConsumers` do today). It
        also cross-checks: the derived set must be CONSISTENT with what cmake
        recorded — a mismatch means a non-standard invocation (see fidelity-mode
        below). And it's an applicability PRECONDITION: a tool whose output
        NAMES depend on file CONTENT (not derivable from input+flags) gets no
        recognizer — it stays in the genrule fallback.
      - **Fidelity-mode behavior (additive-only; safe to roll out
        incrementally).** Gated by the `--fidelity` dial:
        - **strict:** the recognizer FIRES → native rule. On no-confident-match
          or a derived-vs-recorded output MISMATCH, strict REFUSES (typed,
          loud) rather than emit a non-faithful genrule — "faithful native
          rule, or refuse."
        - **best-effort:** fires when confident (a strict improvement),
          otherwise FALLS BACK to today's genrule/recovery — so adding the
          registry can only no-op or IMPROVE, never regress; the current corpus
          behavior is the floor. (Mirrors the existing dial: best-effort already
          enables the fallback escape hatches; strict refuses.)
      - **MODULE deps + gRPC services:** protoc recognizer needs `@protobuf`
        (+ `rules_proto` for the `proto_library` load, or load from `@protobuf`).
        The `.grpc.pb.*` service side emits `cc_grpc_library` (its own
        recognizer). BuildBox's non-proto find_package deps (`absl::*`,
        `OpenSSL::*`, `gRPC::grpc++` runtime) still need a `buildbox-imports.json`
        → `@BCR` mapping + `EXTRA_BAZEL_DEPS`, mirroring `grpc.conf`.
      - **cc-fidelity boundary (unchanged):** there is NO recognizer for plain
        `cc_library` — the converter seeds those faithfully from the codemodel
        (real target boundaries / copts / defines / deps); gazelle_cc MAINTAINS
        them via the `# gazelle:cc_search` directives. Recognizers are for
        CODEGEN (a tool ran), not for ordinary compiled targets.
      - **Implementation pieces (state):** the generic native-rule `ir`
        substrate (`KindNativeRule` + `NativeRuleSpec`/`NativeAttr` + emit +
        auto-load) is SHIPPED, as is the `CodegenRecognizer` interface +
        registry + the protoc `--cpp_out` recognizer (output-authority
        cross-check included). The standalone-custom-command **dispatch** is
        SHIPPED behind the opt-in `--recognize-codegen` flag (off by default):
        a recognized protoc edge lowers to `proto_library` + `cc_proto_library`
        and the genrule disappears; everything else is byte-identical
        (`scripts/meta-cmake-protoc-recognize.sh` gates both halves +
        bazel-builds the `cc_proto_library`). The **consumer-dep
        generalization** is also SHIPPED: a `#include "foo.pb.h"` consumer gets
        a DIRECT `deps` edge to the native rule (`//:foo_cc_proto`), wired via
        the native-rule substrate (`Package.NativeRuleConsumerLabels` +
        `cc.OutToNativeConsumerDep`) so it generalizes to ANY recognized native
        rule. The `generated_includes` file-wrapper is excluded for native-rule
        outputs; the detection seeds native-declared outputs into the codegen
        consumer walk's producer set, so it spans recovered-genrule AND
        native-declared headers (gated by extension, not cmake's generated
        bit). No `# keep` on the native dep — gazelle resolves the idiomatic
        rule itself. Gated by `scripts/meta-cmake-protoc-consumer.sh`
        (`--split-packages`, both halves + a `//:use_foo` build).
        **Operator-extensible without recompiling** is SHIPPED too: a recognizer
        can be a Starlark `*.star` (`match(cmd)`/`lower(cmd)` + `native_rule`/
        `result` builtins) loaded via `--recognizers <glob>` and appended to the
        registry after the built-ins. Sandboxed + deterministic; the
        output-authority cross-check stays host-side (the script declares
        `derived_outputs`, Go validates). `recognizers/protoc.star` is the
        template; gated by `scripts/meta-cmake-recognizer-starlark.sh` (a
        non-built-in tool `gen_pb` whose operator script fires + builds).
        **Cross-package proto import deps + native-rule sub-package placement**
        are SHIPPED too: a recognized proto_library/cc_proto_library lands in the
        package owning its codegen output (`cc.NativeRuleSubPackage` →
        `Package.SubPackages`, so the basename srcs resolve), and a `.proto`'s
        source-tree `import`s are mapped to `proto_library` labels
        (`protoImportLabels` → the recognizer's `deps`), so `import
        "pkg/a/a.proto"` → `//pkg/a:a_proto`. Gated by
        `scripts/meta-cmake-proto-cross-package.sh` (multi-package render +
        `bazel build //pkg/b:b_cc_proto`). What's LEFT:
        - **Fold the codegen lifts under `--fidelity` (the master ladder).**
          The preference order (native rule ≻ live genrule ≻ bake ≻ refusal) +
          the dial mapping are codified in
          `docs/design/codegen-fidelity-ladder.md`. `--fidelity` already gates
          the recognizer's output cross-check (strict refuses a non-standard
          claim with a loud stub; best-effort falls back) — the first wiring
          increment, safe because the recognizer is opt-in. What's LEFT is the
          **default flip**: make `strict` imply the lift opt-ins on +
          `--bake-in=reject` + refuse-on-unsound (lift booleans become
          overrides). That's mechanically small but changes convert output
          corpus-wide (recognized protoc → native rules; stem-match bakes → live
          genrules), so it's gated on a **survey-corpus byte-sweep** that can't
          run in a web container. Deferred until that can run.
        - **Rebased `--proto_path` on the execute_process path.** The
          custom-command paths now handle a non-source-root `--proto_path` (the
          recognizer recovers the proto_path root from the proto src vs its
          canonical output name → places the rule in the proto_path-root package
          + sets `strip_import_prefix` + resolves imports relative to that root;
          gated by `meta-cmake-proto-path.sh`). The execute_process supply path
          can't recover the root the same way (its outputs aren't known until the
          recognizer supplies them), so a rebased proto_path via execute_process
          declines / falls back rather than mis-emitting — wire it through when a
          corpus member needs it.
        Fixture-driven; the existing grpc genrule path stays untouched until a
        grpc recognizer (`cc_grpc_library`) lands, then grpc can migrate.
    - **Auto-emit `tools` entries (the manifest is hand-authored today).**
      Host-codegen-tool hermeticization itself is SHIPPED: a `tools` section in
      the imports manifest maps a host codegen tool with no native rule (a
      project's own python/perl generator, `flatc`/`thrift` without rules, an
      absolute host binary) — by driver basename or absolute path — onto the
      label that provides it (`internal/manifest`'s `Tool`/`LookupTool`). The
      single tool-swap chokepoint (`rewriteToolFromTarget`) rewrites the matched
      token to `$(execpath <label>)` + adds the label to the genrule's `tools`,
      so it reaches BOTH genrule paths (standalone `add_custom_command` + the
      ninja build-dir-copy path) with no per-path opt-in; output anchoring to
      `$(RULEDIR)` and the input closure were already general. Gated by
      `scripts/meta-cmake-host-codegen-tool.sh` (render + bazel-build halves);
      see [`docs/codegen-recognizers.md`](docs/codegen-recognizers.md).
      Auto-DETECTION is now SHIPPED too: a recovered genrule driving an
      un-hermeticized host tool (not swapped, not a benign `cmake -E`/shell
      builtin) emits a `host-codegen-tool` conversion-todo grouped per driver,
      with the exact `tools` entry to paste (`actionable` for an absolute host
      path, `improvement` for a PATH basename) —
      `host_codegen_tool_todo.go`, asserted by the gate. What's LEFT is auto-
      DERIVATION of the *label*: the converter names the tool + the entry to
      author, but can't invent the providing label. A tool→label CONVENTION
      registry (e.g. `flatc`→`@flatbuffers//:flatc`, `protoc`→`@protobuf//:protoc`)
      or an orchestrator that knows the providing element would close the last
      gap so common generators hermeticize with zero manual authoring.
  - **BDE** (`github.com/bloomberg/bde`) — ONBOARDED (scoped to `groups/bsl`),
    structural lenses run; build-lens follow-on below. Metadata-driven target
    construction via the BdeBuildSystem (BBS): each group builds via one
    `bbs_setup_target_uor(${target})` that reads `*.mem`/`*.dep` metadata
    instead of explicit `add_library(...)`. Landed (`make fetch-bde` clones
    BOTH bde + bde-tools at the same tag — the top-level
    `find_package(BdeBuildSystem REQUIRED)` resolves via `CMAKE_PREFIX_PATH`
    pointed at the bde-tools checkout; `scripts/build-lens/bde.conf` surveys at
    the BDE root with the other groups + `standalones/` + bsl-irrelevant
    thirdparty (inteldfp/pcre2) pruned via `CONFIGURE_PRUNE_SUBDIRS`, bbryu
    kept). As the HONEST CAVEAT predicted, the metadata is invisible to the
    converter (the File-API codemodel resolves `.mem`/`.dep` + the `bbs_*`
    functions); the real stress is SCALE — `groups/bsl` alone emits 53
    `cc_library` + **729 per-component `.t.cpp` test-driver `cc_binary`s** from
    a tiny CMakeLists. Structural lenses: rejections 1, idiom 0, coverage 7,
    todos 2. The three follow-ups it surfaced:
    - **`python3` test-runner `add_test` → `cc_test`.** BBS registers each
      driver as `add_test(NAME … COMMAND python3 <bbs runner> <driver-exe>)`,
      not the bare executable, so all 729 drivers stay `cc_binary` and none
      lower to `cc_test` (folds into "Test-target coverage" / "Lower dropped
      test trees" — here the cause is the python-runner COMMAND shape, so the
      lowering needs to see through the BBS wrapper to the driver exe).
    - **`bde_xt_cpp_splitter.py` / `sim_cpp11_features.pl` codegen recovery.**
      The 1 rejection is `unsupported-execute-process` (9 calls): BBS runs
      these as a configure-time `execute_process` to split large `.xt.cpp`
      drivers + generate `_cpp03` variants — a side-effect file generator with
      no captured output channel, so it's not liftable as a configure value.
      Recover as a genrule (the host-codegen-tool hermeticization path) so the
      build lens stops self-skipping (`skip(rej)`).
    - **`bsl+bslhdrs` umbrella interface deps (coverage 7).** The aggregate
      header package's interface lib drops its 7 edges to the sibling package
      interface libs (`bsla`/`bslfmt`/`bslma`/`bslmf`/`bsls`/`bslstl`/`bslstp`)
      — the abseil-INTERFACE-deps class (#302); chase whether it's a real
      dropped edge or a benign trace-synth case.
    Build lens self-skips until the splitter codegen is recovered. `groups/bsl`
    is the bounded scope; the rest of the tree (bdl/bal/bbl/standalones) is the
    stretch once these land.
  BuildBox still needs the build-lens follow-on above; both members are wired
  with the standard corpus pieces (`make fetch-<member>` +
  `scripts/build-lens/<member>.conf` — see `docs/survey-corpus.md`'s roster).

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
  `zlib: symbol-fidelity -> ok` (seeded `zlib.symfidelity`). **Seeded so far
  (11):** zlib, fmt, spdlog, catch2, googletest, glog, libxml2, brotli,
  libpng, libevent (a `_GLOBAL_OFFSET_TABLE_` PIC artifact added to the
  auto-benign classifier), and mbedtls — whose seeding immediately paid for
  the lens: its first run was a true-positive `FAIL` (the in==out
  `link_to_source` execute_process drop surfaced the committed source as an
  output, and the build-dir-include attribution broadcast it into every
  sibling library's srcs — three-way source duplication, 8 over-exported
  symbols), fixed by surfacing NO output from the identity drop
  (`emitCopyGenrule`); the allowlist stays absent so any recurrence re-flags.
  **Remaining follow-ups:** a
  CONSUMER-SIDE lens mode for header-only members (nlohmann-json / glm /
  eigen — the CI consumer fixtures exist via `run-fidelity.sh
  --consumer-file`, but the survey lens only does library-side archives); a
  multi-archive UNION compare for language-partitioned targets (zstd's C/asm
  split emits `liblibzstd_static_{c,asm}.a` while `fidelity-compare` takes
  one `--bazel-artifact`, so zstd self-skips on no-artifact); the heavy
  members (curl / sdl / openblas / protobuf / grpc / llvm / vtk) as
  build-lens time permits; a survey summary column. (The earlier "reuse the
  build lens's build-ws artifacts" follow-up is DONE — the lens reuses the
  split build-ws and rebuilds only the release arm.) Design rationale: the
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

- **ELF dynamic-section fidelity lens (shared libs + executables) —
  SHIPPED (v1, opt-in `SURVEY_ELF_FIDELITY`); seed more members.** The
  dynamic-section sibling of the symbol-fidelity lens: where that lens
  compares EXPORTED-SYMBOL SETS of STATIC archives (`nm`) and deliberately
  abstracts away binary structure (the right call for `.a` —
  section/relocation byte-diffs are toolchain noise), this lens reads the
  dynamic/ABI surface a symbol-NAME set can't express, on BOTH a `.so` and
  an executable (PIE / `ET_EXEC`) — **SONAME**, the **DT_NEEDED**
  runtime-dependency list, **symbol versioning** (`.gnu.version_d` nodes —
  the SAME symbol names under different version tags is an ABI break the
  nm-set compare passes clean), and **DT_RPATH/DT_RUNPATH** (host-leak
  hermeticity). SHIPPED: `cmd/elf-fidelity-compare` (`readelf`-based
  extractor + benign/impactful classifier mirroring `cmd/fidelity-compare`'s
  shape), the `docs/fidelity-deltas.md` "ELF dynamic-section classifier"
  taxonomy, the self-contained `meta-elf-fidelity.sh` gate, and the
  8th, pipeline-last `run-survey.sh` lens (`SURVEY_ELF_FIDELITY`, after the
  symbol lens, build-lens-gated) — driven by
  `scripts/build-lens/<name>.elffidelity` (`ELFID_TARGET` / `ELFID_ARTIFACT`)
  + `testdata/fidelity/<name>.elf-allowlist.txt`, reporting
  `<out>/<name>/elf-fidelity.json`. It pairs with **Faithful
  SHARED-library conversion**: it requires `SURVEY_SHARED=1` (the build
  lens then converts with `--emit-shared-libraries` and builds the
  `cc_shared_library` `.so`, which the lens REUSES — dynamic metadata is
  config-invariant) and builds the cmake side with `BUILD_SHARED_LIBS=ON`;
  without `SURVEY_SHARED` it self-skips. REMAINING: seeded with `fmt`;
  add `.elffidelity` configs across the shared-validated corpus (zlib,
  libxml2, brotli, curl, glog, spdlog, mbedtls, protobuf) and curate each
  `.elf-allowlist.txt` against the real benign classes (BuildID, distro
  NEEDED, version-node BASE = soname) as members come online.

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

- **Common compile flags hoist — SHIPPED (both modes, opt-in;
  self-contained mode now hoists copts + defines + linkopts); remaining: a
  render gate / feature-mode lens validation / write-a threading.** Dedups the
  per-target repetition of cmake's project-wide `CMAKE_<LANG>_FLAGS` /
  link flags / compile definitions.
  `commonflags` (converter/internal/emit/commonflags) finds the longest PREFIX
  every cc target shares on an axis and strips it; prefix-only so the reapplied
  flags stay before the per-target ones (cmake's order). For the `local_defines`
  SET axis the lists are sorted first, so the prefix is the common LEADING
  sorted defines (conservative: hoists the common leading run, not the maximal
  common subset — never wrong, just not maximal). Two opt-in emit modes:
  - `--out-common-compile-flags-feature` — writes a `cmake_common_compile_flags`
    cc_toolchain `feature()` `.bzl` + tags each stripped target
    `features = [...]`. Operator-wired into whatever toolchain the build uses
    (same convention as sanitizerfeatures); a no-op until wired. **Copts only**
    (a feature is a compile-flag construct — no faithful home for link flags or
    the PRIVATE local_defines a toolchain feature would over-apply).
  - `--emit-common-compile-flags-bzl` — SELF-CONTAINED: writes
    `common_compile_flags.bzl` defining `COMMON_COPTS` (always) plus
    `COMMON_LOCAL_DEFINES` / `COMMON_LINKOPTS` (when those axes share a prefix),
    `load()`ed by each BUILD that references them as
    `copts = COMMON_COPTS + [delta]` (and the local_defines / linkopts analogs).
    No toolchain wiring; works with the default toolchain (validated: `bazel
    build` green on the emitted output). A copts-only project's `.bzl` is
    byte-identical to the copts-only era (the new constants emit only when
    non-empty).
  Off by default (byte-stable inline emission). The self-contained mode is
  **lens-validated** on the corpus (`SURVEY_HOIST_COMMON_COPTS=1`): brotli, fmt,
  and libxml2 `bazel build //...` green with the hoist on — libxml2 dedups a
  12-flag `-Wall -Wextra -Wshadow …` prefix that repeated per target, fmt the
  `-fvisibility*` pair. **BDE (`groups/bsl`) was the corpus's strongest
  demonstrator AND drove the defines/linkopts extension:** its one ~39k-line
  group `BUILD.bazel` repeated a project-wide BBS flag set VERBATIM on **745 of
  763 targets** — `copts=["-O3","-pthread"]`, `local_defines=
  ["BDE_BUILD_TARGET_OPT","NDEBUG","_POSIX_PTHREAD_SEMANTICS","_REENTRANT"]`
  (745×), and `linkopts` (`-O3`/`-lrt`, 721×) — ~13k of the 39k lines were this
  boilerplate; the self-contained hoist now collapses all three axes into the
  one `.bzl`. **Remaining:** (1) a render gate in the
  `meta-cmake-sanitizer-features` mold (assert strip + tag/load + `.bzl` shape on
  a fixture — now covering the defines/linkopts constants too) to put it under
  the gates-in-CI net; (2) the FEATURE mode's lens
  validation needs the survey to wire the emitted feature into a registered
  toolchain (the self-contained mode needs none); (3) `write-a` doesn't yet
  THREAD `--emit-common-compile-flags-bzl` into the orchestrated project-A/B
  converter rule, so the self-contained mode is operator/survey-only for now —
  the staging of the generated `common_compile_flags.bzl` IS handled on both
  delivery shapes (split: the packages TreeArtifact + stage-b's `stageSplitDir`;
  monolithic: stage-b's sidecar copy), so threading is the only piece left to
  make it a graph-level toggle. Sibling to the feature-flag lift above.

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

- **pkg-config harvester: `${pcfiledir}` substvar.** The harvester
  (`internal/harvest/pkgconfig.go`) parses `.pc` files directly (no `pkg-config`
  binary) and expands `${var}` substvars — top-down nested definitions plus
  `--define-prefix` relocation (the file's build-time `prefix=` is overridden by
  the harvest seed) are understood and tested. The gap is pkg-config's built-in
  **`${pcfiledir}`** (the directory containing the `.pc`): `vars` is seeded with
  `prefix` only, so `${pcfiledir}` expands to empty. The increasingly-common
  fully-relocatable idiom that derives paths from it rather than `prefix`
  (`libdir=${pcfiledir}/../lib`, `Cflags: -I${pcfiledir}/../include`) then
  silently drops its `-L`/`-I`. Fix is small and localized: seed
  `vars["pcfiledir"]` (and `pc_sysrootdir`) from the `.pc` file's own directory in
  `parsePkgConfig` before `parsePC` (the file path is already in hand), with a
  fixture mirroring a pcfiledir-relocatable `.pc`. Lower-priority sibling edges,
  same area: expansion is order-dependent (per-line accumulation vs pkg-config's
  lazy any-order resolution — a define-after-use `.pc` mis-expands to empty), and
  `$$` isn't unescaped to a literal `$`.

## Later (research / open questions)

- **`--lift-configure-file` default flip.** The lift tier is complete
  (template src + values dict + stamp values + verify pass +
  per-config arms); the default stays opt-in solely because downstream
  Bazel envelopes must stage //tools:cmake-configure-file. Flip the
  default (or auto-enable on a staged-tool signal) once the envelopes
  carry it — that converts the largest remaining bake population into
  real lifts with zero new machinery.

- **KindNativeRule outputs in --split-packages relocation.** The codegen-recognizer registry's native-rule substrate now participates in the OutToGenrule-keyed consumer wiring AND the nested-cmake merge re-home (producerOuts/applyNestedProducerReHome read the `out`/`outs` attrs generically). The split-packages emitter (emit/bazel/split.go) still keys producer-output placement/relocation on KindGenrule/KindWriteFile/KindCMakeConfigureFile, so a pkg_tar (or future http_file/proto) native rule re-homed into a sub-package wouldn't relocate its out. Generalize split's placement to the same kind-agnostic outputs accessor. Demand signal: a native-rule producer under --split-packages.
- **Genex-probe language gate — genex-wrapped link deps.** The probe's language-conditional skip now walks the INTERFACE_LINK_LIBRARIES closure (_cmtb_iface_lang_gate), so a $<COMPILE_LANGUAGE>/$<LINK_LANGUAGE> gate on a transitively-linked dependency's interface is caught — not just the target's own raw value. The walk follows BARE target deps only; a dep wrapped in a genex link entry ($<LINK_ONLY:dep>, $<BUILD_INTERFACE:dep>) or a bare system lib isn't queried, so a gate reachable solely through such an entry could still diverge. Closing it needs genex-entry target extraction in the hook. Demand signal: an abort whose gated dep is reachable only via a genex link entry.
- **Stage textual-include-of-SOURCE siblings (`#include "x.cu"` /
  `#include "x.c"`).** The sibling-header staging walk covers header
  extensions (incl. `.cuh`), but cuda-samples' eigenvalues quote-includes a
  `.cu` from its `.cuh` kernels (`bisect_util.cu` — the classic
  one-definition-per-arch idiom), which never stages and the compile misses
  it in the sandbox. Needs either an include-scan-driven staging channel or
  per-extension opt-in to the walk; cc_binary's no-hdrs-slot drop
  (quasirandomGenerator_nvrtc's `.cuh`) is the same family. Both samples are
  pruned in `cuda-samples.conf` until then (eigenvalues is the only
  non-toolkit-floor entry there).

- **genclass textual-impl includes: angle-include form.** The
  textual-include router now detects a header that textually `#include`s
  its implementation (a `.cc` or a non-self-contained impl header
  `.inl/.tcc/.ipp/.txx/.def/.inc`) and routes the impl to `textual_hdrs`
  — but only for **quote-form** includes (`#include "foo.inl"`, the glm /
  VTK `.txx` shape), matching the existing scanner's deliberate
  quote-only design. Libraries that use **angle-form** impl includes —
  Boost.Asio's `#include <boost/asio/impl/io_context.ipp>`, libstdc++'s
  `<bits/foo.tcc>` — aren't caught: those resolve against `-I` roots, so
  catching them needs the scanner to resolve angle includes against the
  target's include dirs. Until then an angle-included `.ipp`/`.tcc` stays
  in `hdrs` (works on a plain build; a `parse_headers`/`layering_check`
  build would try to compile the fragment standalone).
- **Conversion-latency: AST-direct BUILD emit (drop the text→Parse→Format
  round-trip).** Profiling real corpus converts (`--cpuprofile` + `--out-timings`)
  showed cmake's own configure dominates wall-clock (80–90% on small projects,
  and for probe-heavy giants like SDL the `try_compile` configure is ~80% — both
  external/unaddressable converter-side). Of the converter's *own* Go time, two
  costs were addressed: redundant trace re-parsing (`ParseTrace` memoize, ~37%
  off translation) and the generated-wrapper include regexp (substring
  pre-filter, ~0.3s on wrapper/config-heavy projects). The remaining universal
  Go cost is the BUILD emitter: `emit/bazel/emit.go` renders text via
  `text/template`, then runs it back through buildtools `Parse + Format` to
  canonicalize — so `build.Parse` re-parses text the converter just generated,
  ~20% of Go translation on every project (more on emit-heavy ones, ~37% on
  cryptoauthlib). The fix is the buildtools-AST-direct emit the package header
  comment already anticipates ("the plan calls for a buildtools-AST spike… it
  replaces the template here without changing the Emit signature"): build the
  `*build.File` AST and `Format` it, skipping the re-parse. Bounded to one
  package with a stable `Emit` signature, and the render gates pin BUILD output
  byte-for-byte, so any formatting drift fails a gate immediately — the safety
  net that makes an emitter rewrite tractable. Sibling, deferred: **coalescing
  the warm second passes** (genex-literal / stamp-set-trace / nested-cmake) into
  one combined warm configure + one re-lower — feasible (the hooks are
  orthogonal and none invalidates the try_compile cache) but only pays off when
  ≥2 warm passes co-fire on one project, which is likely rare; revisit only with
  a corpus survey showing multi-pass co-fire is common enough to justify the
  orchestration risk.

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
  tools is the research item. The `WORKING_DIRECTORY` + positional-source
  form of the nested-cmake recognizer (`cmake -G … .` / `cmake --build .` run
  IN the build dir, the dominant "download/build at configure" spelling) now
  LIFTS — `nested_cmake.go` resolves relative/positional/in-source dirs
  against `WORKING_DIRECTORY` instead of refusing; gate
  `scripts/meta-cmake-nested-cmake-workdir.sh`. NEW residue (marked by the
  **cryptoauthlib** corpus member, `make fetch-cryptoauthlib`): a
  **download-only** nested project — `project(mbedtls-download NONE)` whose
  `--build` step is an `ExternalProject_Add` that materializes sources into
  the *outer* build dir, which the outer then `file(GLOB)`s and compiles
  directly (no nested codemodel/targets to merge). The configure/build are
  now recognized (the warm pass runs; with no nested targets it degrades to a
  `nested-cmake-not-lifted` todo), but the materialized sources still surface
  as `unsupported-source-path` (cryptoauthlib: 5, → 4 `empty-cc-library`).
  Lift shape: treat the materialized build-dir sources as a
  genrule/repository-rule-vendored input set anchored at the outer root,
  rather than refusing them as out-of-tree. (The fetch script already pins +
  pre-fetches the tarball and repoints the ExternalProject `URL` at a local
  `file://` copy, so the configure-time download is hermetic — the natural
  seam for the repository-rule lift.)

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
