# Fidelity deltas

Observed differences between artifacts built by cmake directly and
artifacts built by `convert-element-cmake` + Bazel — i.e. is the
converter producing output that, once Bazel-built, matches what the
original cmake build would produce?

Surfaced by the productionized fidelity harness:

- `make e2e-fidelity-compare-{zlib,spdlog,fmt}` — library-side gates
  (cmake-built `.a` vs Bazel-built `.a`).
- `make e2e-fidelity-compare-{zlib,fmt}-consumer` — consumer-side
  gates (a small consumer .c/.cpp compiled twice; diff the resulting
  `.o`s).

The classifier (`cmd/fidelity-compare`) auto-classifies the benign
categories below and exits non-zero on impactful ones. Per-project
allowlists at `testdata/fidelity/<name>.allowlist.txt` suppress
operator-acknowledged benign entries.

For cmake → converter modelling differences (independent of any
Bazel build) see
[`docs/cmake-conversion-deltas.md`](cmake-conversion-deltas.md).

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

Status: ✅ shipped — `make e2e-fidelity-compare-spdlog` gate passes
with allowlist suppressing 5 template-instantiation deltas from
spdlog's vendored fmt headers.

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

Status: 🟢 5 of 6 projects shipped as productionized gates (zlib, fmt,
spdlog, nlohmann-json, Catch2); libpng catalogued as deferred — it needs a
converter slice (find_package→external-label resolution + install-symlink
tolerance), not a harness tweak (see `ROADMAP.md` "A-B-C fidelity harness"
entry).

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

**Status**: partial — `bazeltoolchain` now emits opt-in `fortify_source` and
`stack_protector` cc_toolchain feature definitions (shipped in the close-gaps
campaign; toolchain feature template at `examples/sanitizer-features/
toolchain/features.bzl`). Default-off so existing operators see no change;
opt-in via `--features=fortify_source,stack_protector` at Bazel-build time
once the toolchain template is wired in. The classifier auto-classifies these
undefined-symbol deltas as benign (no allowlist entry needed). Tracked under
`ROADMAP.md`'s "Toolchain-feature parity vs. cmake's default Release hardening
flags" Next bullet for the remaining auto-enable closure.
