# Fidelity deltas

Observed differences between artifacts built by cmake directly and
artifacts built by `convert-element-cmake` + Bazel — i.e. is the
converter producing output that, once Bazel-built, matches what the
original cmake build would produce?

Surfaced by the productionized fidelity harness:

- `make e2e-fidelity-compare-{zlib,spdlog,fmt}` — library-side gates
  (cmake-built `.a` vs Bazel-built `.a`).
- `make e2e-fidelity-compare-{zlib,fmt,spdlog}-consumer` — consumer-side
  gates (a small consumer .c/.cpp compiled twice; diff the resulting
  `.o`s).

The classifier (`cmd/fidelity-compare`) auto-classifies the benign
categories below and exits non-zero on impactful ones. Per-project
allowlists at `testdata/fidelity/<name>.allowlist.txt` suppress
operator-acknowledged benign entries.

For cmake → converter modelling differences (independent of any
Bazel build) see
[`docs/cmake-conversion-deltas.md`](cmake-conversion-deltas.md).

## Fidelity vs. survey: two complementary oracles (read this first)

A recurring confusion — worth pinning so future sessions don't relitigate
it: **fidelity and survey are different harnesses measuring orthogonal
things, and neither subsumes the other.**

- **Survey** (`scripts/run-survey.sh`, `docs/survey-corpus.md`) runs in the
  **faithful** shape: **multi-config + split-packages by default**
  (`SURVEY_BUILD_TYPES=auto`, `SURVEY_SPLIT_PACKAGES=1`). That shape is the
  Bazel-idiomatic **end-state** we're converging on — one BUILD per
  directory, every declared config folded into `//config:<name>`
  `select()` arms. Survey's job is to catch **intent loss**: did the
  converter silently drop a config, a target, an include edge? Intent loss
  only becomes visible *in* the faithful shape, so survey maximizes shape
  fidelity.

- **Fidelity** (`scripts/run-fidelity.sh`, this doc) deliberately runs the
  **opposite** shape: **single-config Release + single monolithic BUILD**.
  Its job is symbol equivalence — diff the cmake-built `.a` against the
  Bazel-built `.a`, expecting matching exported symbols. That diff is
  *cleanest* on one config + one archive: multi-config raises "which
  config's archive do I diff?" and split raises "which package's archive?".
  Collapsing to single/single isn't a weakness — **it's what makes the
  symbol oracle sharp**: with no `select()` arms or package boundaries to
  confound the `nm` diff, a single dropped/renamed/miscompiled symbol
  surfaces immediately, divergence the faithful-shape survey would never
  even attempt to detect.

So yes — survey is "stronger" on **shape faithfulness** (it builds the
end-state), and fidelity is "stronger" on **symbol precision** (it isolates
artifact divergence). A clean survey can pass while fidelity catches a
miscompile; a clean fidelity can pass while survey catches intent lost only
under multi-config/split. Run **both**. If you ever extend fidelity to the
faithful shape, do it as an *addition* (per-config symbol diffs), not a
replacement — don't give up the single/single sharpness.

## Open per-project deltas

Catalog of observed differences for each productionized fixture.

### hello-world (`converter/testdata/sample-projects/hello-world`)

Status: ✅ symbol-tier passes. The single-file fixture exists to
smoke-test the harness itself rather than the converter; if
hello-world ever fails, it's the harness, not the converter.

### fmt (`make fetch-fmt`, `FMT_VERSION = 11.0.2`)

Status: ✅ shipped — `make e2e-fidelity-compare-fmt` + `-fmt-consumer`
gates both pass with allowlist suppressing benign template-instantiation
deltas (3 lib-side, 4 consumer-side entries — same benign category,
inlining decisions differ between cmake's distro toolchain and Bazel's
hermetic toolchain).

### spdlog (`make fetch-spdlog`, `SPDLOG_VERSION = v1.14.1`)

Status: ✅ library + consumer both shipped. `make
e2e-fidelity-compare-spdlog` passes with an allowlist suppressing 5
template-instantiation deltas from spdlog's vendored fmt headers.
`make e2e-fidelity-compare-spdlog-consumer` passes with an empty
allowlist (63/63 exported symbols match, 0 impactful). spdlog is a
*compiled* library — its CMake sets `target_compile_definitions(spdlog
PUBLIC SPDLOG_COMPILED_LIB)`, so a consumer of the converted target
compiles in compiled-lib mode (out-of-line refs into `libspdlog.a`).
The harness replays that PUBLIC define on the cmake-side consumer
compile (`--consumer-cmake-cflags '-DSPDLOG_COMPILED_LIB'`, since the
bare `-I<install>/include` compile wouldn't otherwise carry it) and
compiles both sides at `-O2` so the template-instantiation symbol sets
are comparable — without the matched opt level the Bazel side's default
`-O0` fastbuild emits every instantiation as an unpaired weak symbol.
Both are harness-methodology fixes, not converter deltas.

### zlib (`make fetch-zlib`, `ZLIB_VERSION = v1.3.1`)

Status: ✅ shipped — `make e2e-fidelity-compare-zlib` + `-zlib-consumer`
gates both pass with empty allowlists. 105/105 lib-side exact match.

### nlohmann-json (`make fetch-nlohmann-json`, `JSON_VERSION = 3.11.3`)

Status: ✅ consumer-side shipped — `make
e2e-fidelity-compare-nlohmann-json-consumer` passes (0 impactful deltas).
Header-only INTERFACE library: no static archive, so this is consumer-side
only. The converter lowers the `INTERFACE_LIBRARY` target to a
`cc_library(hdrs = [...], includes = ["include"])` (the
`cmake-codegen-interface-library-from-trace` synthesis) a consumer can
`deps` on, which is what unblocked this gate.

### Catch2 (`make fetch-catch2`, `CATCH2_VERSION = v3.5.3`)

Status: ✅ library-side shipped — `make e2e-fidelity-compare-catch2` passes
(0 impactful deltas). Needs `--lift-configure-file` (the lib `#include`s the
configure_file-generated `catch2/catch_user_config.hpp`); `run-fidelity.sh`
threads it via `--convert-flags` and auto-stages
`//tools:cmake-configure-file`. The converter wires the genrule's
`generated-includes/` output dir into the cc_library's `includes` (the
`addBuildDirIncludes` fix) so the angle-bracket include resolves. Catch2 is
template-heavy: ~100 deltas are std::/compiler-internal template-instantiation
variance, now auto-classified benign by the classifier
(`stdlib-template-instantiation-*`); the 5-entry allowlist covers Catch's own
template destructors (Clara `ResultValueBase`, `Detail::unique_ptr`).

### libpng (`make fetch-libpng`, `LIBPNG_VERSION = v1.6.43`)

Status: ✅ library-side shipped — `make e2e-fidelity-compare-libpng` passes
(0 impactful). The "hard" fixture: it exercises four otherwise-deferred
mechanisms at once — `cmake -E create_symlink` install aliases skip (PR
#350), `cmake -P` script headers (`pnglibconf.h`, …) bake via
`--cmake-script-bake`, `find_package(ZLIB)` resolves to `@zlib` via
`--imports-manifest` (`testdata/fidelity/libpng-imports.json`), and
`--bazel-external` supplies the zlib BCR module. One allowlisted delta:
`floor` (cmake's distro toolchain emits the libm reference; Bazel's inlines
the builtin). cmake side needs zlib dev headers on the host.

### Recording a new delta

When the harness surfaces a new impactful delta during gate
development, capture it as a discrete sub-section:

```
### <Fixture> / <Target> / <symptom-tier>: <one-line summary>

**Reproducer**: which test, which target, what the diff looks like.
**Root cause**: which converter file/function emits the problematic shape.
**Status**: open | fix-in-progress | wontfix-with-rationale.
```

## Cross-project Bazel-build fidelity survey (close-gaps campaign, May 2026)

Status: 🟢 all 6 surveyed projects shipped as productionized gates (zlib,
fmt, spdlog, nlohmann-json, Catch2, libpng). VTK / LLVM are the next tier
(project-specific configure flags + tooling; see `ROADMAP.md` "A-B-C
fidelity harness" entry).

Manual harness for the initial survey: for each project, run
`convert-element-cmake --cmake-build-dir <cmake-build>` to produce
`BUILD.bazel`; stage it in a Bazel workspace alongside the project's
sources; `bazel build` a representative target; compare with the
cmake build's equivalent artifact.

| project | bazel target | bazel size | cmake size | exported syms (both) | only-cmake | only-bazel |
| --- | --- | --- | --- | --- | --- | --- |
| zlib 1.3.1 | `//:zlibstatic` | 148K | 149K | 105 / 105 | 0 | 0 |
| fmt 10.2.1 | `//:fmt` | 268K | 264K | 174 / 182 (cmake) / 186 (bazel) | 4 | 8 |
| spdlog 1.13.0 | `//:spdlog` | 1.5M | 1.5M | 1340 / 1345 (cmake) / 1341 (bazel) | 5 | 1 |
| nlohmann-json 3.11.3 | `//:test_main` | builds | builds | header-only INTERFACE; no library to diff | — | — |
| Catch2 3.5.3 | `//:Catch2` | needs `--lift-configure-file` + staged `//tools:cmake-configure-file` | — | — | — | — |

Symbol-set deltas are explained by:
- **distro hardening**: cmake `cc` references `__*_chk` / `__stack_chk_fail`;
  Bazel's hermetic toolchain doesn't (closed by `--probe-distro-hardening` +
  `derive-toolchain --inherit-distro-hardening`).
- **inlining decisions**: fmt's 4+8 extra symbols are C++ template
  instantiations that survive at slightly different optimization decisions
  under cmake's distro spec vs. Bazel's hermetic toolchain. Not a converter
  issue.

Converter bug surfaced and fixed during the survey: `includes = [""]`
(Bazel rejects "resolves to the workspace root, which would allow this
rule and all of its transitive dependents to include any file in your
workspace"). All workspaces in the table above were stripped of
`includes=[""]` entries for the comparison; the upstream converter fix
landed in close-gaps PR #253.

Catch2's blocker is the documented `--lift-configure-file` opt-in: the
`catch_user_config.hpp` header is configure-time-generated and consumers
`#include` it. Without the lift flag the converter elides the build-dir-
rooted output, leaving the cc_library missing the header. With the flag and
a staged `//tools:cmake-configure-file` tool, the genrule materializes the
header from the cmake-substituted `.hpp.in`.

### The A-B-C harness shape

`docs/architecture.md` already names the three projections; the fidelity
harness uses them as oracle slots:

- **Project A** — the cmake source tree (CMakeLists.txt + sources +
  `CMAKE_BUILD_TYPE=Release` configure). The unmodified input.
- **Project B** — the converted Bazel project (converter's `BUILD.bazel`
  + the same source tree + a minimal bzlmod `MODULE.bazel` declaring
  `rules_cc` / `rules_pkg` as bazel_deps). The output we're validating.
- **Project C** — the cmake-built artifact (oracle). What B should
  reproduce up to documented-benign deltas. Always works because
  cmake's the source of truth for the project's build graph.

The May 2026 survey ran A→C and A→B by hand; the productionized
harness (`scripts/run-fidelity.sh` + `cmd/fidelity-compare`) does the
same three projections + classifier in one invocation.

### Delta classifier

When a B-vs-C diff appears, classify before triaging. The classifier
in `cmd/fidelity-compare/internal/classifier/` automates these rules;
the prose below mirrors what it implements.

**Benign — informational; don't block:**
- `__*_chk` / `__stack_chk_*` undefined refs only in C (cmake's distro
  hardening spec defaults — closed by enabling the
  `fortify_source`/`stack_protector` cc_toolchain features).
- C++ template instantiations present in one side only when the same
  mangled root appears with different instantiation parameters
  (different inlining heuristics under different toolchains).
- Archive member name suffix `.o` vs `.pic.o` (Bazel hermetic toolchain
  prefers PIC; converted output sets `features=["pic"]`).
- `.text` section size deltas under ~20% per object (compiler codegen
  variation between toolchain versions).
- `ar` archive metadata (mtime, mode bits, padding).

**Impactful — block / file as converter bug:**
- Exported symbol present in C but not B (Bazel build under-produces).
- Exported symbol present in B but not C, NOT explained by C++
  template-instantiation pattern (Bazel build over-produces or has a
  symbol-visibility bug).
- Absolute host paths (`/tmp/...`, `/home/...`, etc.) embedded in B
  but not C (hermeticity leak — the cmake build dir or source tree
  is bleeding into the artifact).
- Missing transitive dependency surfaced as a runtime / link-time
  unresolved symbol when the artifact is consumed.
- Archive member missing entirely (a source file dropped from `srcs`).

**Configuration mismatch — operator action required:**
- Missing `--lift-configure-file` for projects that #include a
  `configure_file`-generated header (Catch2).
- Missing `--cmake-script-bake` for projects with cmake -P script
  outputs in the build graph (libpng, VTK).
- Missing `--inherit-distro-hardening` when the operator wants
  symbol-set parity with the cmake build's hardening flags.

### ELF dynamic-section classifier (shared-lib ABI)

The symbol-set classifier above abstracts away binary structure — the
right call for static `.a` archives, where section/relocation byte-diffs
are toolchain noise. The shared-library ABI surface a symbol-NAME set
can't express is classified separately, by `cmd/elf-fidelity-compare`
(`readelf -d` / `--version-info` on a cmake-built `.so` vs a Bazel-built
`.so`). Do NOT byte-diff whole ELF files — only the dynamic/ABI facts
below carry a fidelity signal.

**Benign — informational; don't block:**
- `DT_NEEDED` on a C/C++ runtime / libc-family soname only on one side
  (`libc.so.6`, `libm.so.6`, `libstdc++.so.6`, `libgcc_s.so.1`,
  `ld-linux-*`, …) — the hermetic Bazel toolchain and the host distro
  toolchain link the runtime differently.
- `SONAME` minor/patch suffix difference with the SAME ABI-major
  (`libfoo.so.1` vs `libfoo.so.1.2.3`).
- `DT_RUNPATH`-vs-`DT_RPATH` FORM difference over the same path set
  (new-style tag vs legacy), and non-host-leak rpath entries.
- Version node / NEEDED soname listed in the per-member allowlist
  (`testdata/fidelity/<name>.elf-allowlist.txt`).
- (Open: BuildID, distro-default NEEDED, version-node BASE = soname.)

**Impactful — block / file as converter bug:**
- A PROJECT (non-runtime) `DT_NEEDED` dropped from the Bazel side (lost
  runtime dependency) or added on the Bazel side (over-linking).
- `SONAME` ABI-MAJOR mismatch (`libfoo.so.1` vs `libfoo.so.2`) or a
  missing SONAME on the Bazel side — a consumer links against the
  soname, so the wrong/absent handle breaks the runtime link.
- A `.gnu.version_d` version node present on only one side — the SAME
  symbol names under a different version tag bind to a different
  versioned symbol, an ABI break the nm-set compare passes clean.
- A host-leak `DT_RPATH`/`DT_RUNPATH` (`/tmp/...`, `/home/...`, build /
  scratch trees) baked into the Bazel artifact (hermeticity leak).

## Cross-fixture: distro hardening defaults

**Symptom**: cmake-built artifacts reference `__*_chk` (FORTIFY_SOURCE) and
`__stack_chk_fail` / `__stack_chk_guard` (stack-protector) symbols; Bazel-built
artifacts from the converted BUILD.bazel do not.

**Reproducer**: surfaced by the zlib convert-and-build comparison
(`bazel build //:zlibstatic` vs. `cmake --build . --target zlibstatic`,
`nm -u libz.a` vs. `nm -u libzlibstatic.a`). The cmake archive has 47 undefined
symbols; the Bazel one 46; the diff is `__snprintf_chk`, `__vsnprintf_chk`,
`__stack_chk_fail`.

**Root cause**: the system `/usr/bin/cc` on Debian/Ubuntu (and most distros)
applies `-D_FORTIFY_SOURCE=2 -fstack-protector-strong` via the spec file even
when no CFLAGS are passed. cmake's compile_commands.json doesn't capture this
(the spec defaults are baked into the compiler invocation, not into the
user-visible command line). Bazel's hermetic cc_toolchain has no equivalent
default; the converted BUILD.bazel doesn't lift the flags because they were
never visible to the converter in the first place.

**Detection**: `convert-element-cmake --probe-distro-hardening` compiles a
trivial stub with the host cc, inspects the resulting object file's undefined
symbols, and emits a stderr warning naming the detected flags with a remediation
recipe. Diagnostic-only — the probe doesn't change BUILD.bazel emit decisions.

**Status**: closed — `bazeltoolchain` emits opt-in `fortify_source` and
`stack_protector` cc_toolchain feature definitions (shipped in the close-gaps
campaign; toolchain feature template at `examples/sanitizer-features/
toolchain/features.bzl`). Default-off so existing operators see no change;
opt-in via `derive-toolchain --inherit-distro-hardening` (`on` forces the
features; **`auto`** runs the host-cc hardening probe at derive time and
enables them only if the host actually applies distro defaults — so deriving
on the same host cmake built with reproduces that build's hardening without
the operator having to pass anything explicitly). Opt out per-build with
`--features=-fortify_source` / `--features=-stack_protector`. The classifier
auto-classifies these undefined-symbol deltas as benign (no allowlist entry
needed).
