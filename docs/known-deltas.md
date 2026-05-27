# Known deltas

What the converter doesn't (yet) get right, and why. Two flavours:

- **Conversion deltas** — observed differences between what cmake
  models and what the converter emits. Surfaced by the fixture
  corpus under `converter/testdata/sample-projects/`.
- **Fidelity deltas** — observed differences between artifacts built
  by cmake directly and artifacts built by `convert-element-cmake` +
  Bazel. Surfaced by `make e2e-fidelity` / `make e2e-fidelity-fmt`.

The bar for narrowing fixes: every fix that *removes* information
from the converted output (header narrowing, dep filtering, etc.)
needs a high-confidence signal — preferably from cmake's trace-expand
record of resolved command calls, never a basename-match heuristic.
Cosmetic-only deltas (information that's redundant but correct) stay
open as documentation rather than getting heuristic fixes that
introduce false-positive risk.

## Open conversion deltas

### subdir-library — over-broad hdr collection (cosmetic, not correctness)

**Fixture**: `converter/testdata/sample-projects/subdir-library/`
(top-level CMakeLists adds `src/util/` via `add_subdirectory`; both
define cc_library targets).

**Surfaced**: `cc_library(name = "util")` emits
`hdrs = ["include/toplib.h", "include/util.h"]` — every `.h` file in
the project. Both CMakeLists declared `include/` as
`target_include_directories(PUBLIC include)`, so `discoverHeaders`'
walk returns every header to every target.

**Why this is OK today**: Bazel allows the same file in multiple
cc_libraries' hdrs. The redundancy is cosmetic noise in the BUILD
output, not a build-time correctness issue — both libraries compile
and consumers link correctly.

**Why an early heuristic fix was reverted**: a basename-match narrow
("drop `util.h` from toplib's hdrs because `util` is a different
target's name") false-positives on projects where a header
coincidentally shares a name with an unrelated target.

**Why we won't pursue the deterministic alternative yet**: scanning
source files for `#include "..."` directives is deterministic (no
name guessing), but expands the converter's action input set to
include every `.c` / `.cpp` source file it reads. Every source-file
edit then invalidates the converter's cache. The current behaviour
keeps re-runs gated on `CMakeLists` / cmake-cache changes (rare).
Trading rare re-runs for precise hdrs isn't worth it for cosmetic
duplication.

### cmake 4.x CMP0026 — legacy `get_target_property(... LOCATION)` fails at configure

**Surfaced by**: real-world packages still using the pre-3.0 idiom
for resolving an executable target to its on-disk path (most visibly
[yasm](https://github.com/yasm/yasm)).

**Symptom**: cmake 4.x removed the OLD behaviour of CMP0026 entirely;
configure fatal-errors with `The LOCATION property may not be read
from target "..."`. `cmakerun.annotateConfigureFailure` recognises
the sentinel and surfaces a `[hint]` block pointing at workarounds.

**Why convert-element-cmake doesn't auto-fix**: rewriting
`CMakeLists.txt` is source-mutating; doing it inside the converter
would either modify the operator's source tree (destructive) or
duplicate the tree into a scratch copy (doubles disk cost for large
checkouts). The source-key model also keys on the original source
bytes, so a silent rewrite would split the cache without an audit
trail. The default is to surface the diagnostic; an opt-in shim
(`--cmp0026-shim`) wraps `get_target_property` at configure time to
route LOCATION queries to `$<TARGET_FILE:<tgt>>`.

**Workarounds (in preference order)**:

1. **Patch the unpacked source**:
   ```sh
   find . \( -name CMakeLists.txt -o -name '*.cmake' \) \
     -exec sed -i -E \
       's/get_target_property\(([^ ]+) +([^ ]+) +LOCATION\)/set(\1 $<TARGET_FILE:\2>)/g' \
       {} +
   ```
   Thread through Bazel's `http_archive.patch_cmds` so the rewrite
   runs before `convert-element-cmake` configures the tree. Caveat:
   matches only the single-line three-space-separated form.

2. **Run with `--cmp0026-shim`** — installs a `CMAKE_PROJECT_TOP_LEVEL_INCLUDES`
   override that routes LOCATION queries to `$<TARGET_FILE:<tgt>>`.
   Caveat: returns a *generator expression*, not a configure-time
   path; code that string-composes the value at configure time
   (`message(STATUS "${LOC}")`, `if("${LOC}" MATCHES …)`) sees the
   literal `$<TARGET_FILE:foo>` text and likely misbehaves.

3. **Pin cmake to a 3.x release** (`CMAKE_VERSION=3.28.3` is the
   default in `Makefile`). cmake 3.x emits a deprecation warning but
   resolves LOCATION. In cmake 4.x the policy is gone entirely and
   `-DCMAKE_POLICY_DEFAULT_CMP0026=OLD` is rejected.

## Resolved conversion deltas

- **multi-language compile-group split ✓** — targets with ≥ 2
  distinct compile-group languages split into a wrapper `cc_library`
  (public surface) plus one private sub-library per language with
  that language's srcs + flags. See `splitMultiLanguage` in
  `converter/internal/lower/lower.go`.
- **configure_file generated headers ✓** — lower walks the
  `--trace-expand` JSON for `configure_file(...)` calls, reads the
  rendered output bytes (live in production; captured by
  `tools/fixtures/record-fileapi.sh` for fixtures), and emits a
  genrule. Targets whose codemodel-recorded includes contain the
  build dir get the genrule's output in `hdrs`.
- **PRIVATE include scoping ✓** — `target_include_directories(...
  PRIVATE ...)` paths now become per-target `copts = ["-I<dir>"]`
  (compile-only, not propagated to consumers), while PUBLIC paths
  stay in `includes`.
- **find_package STATIC IMPORTED deps ✓** — for STATIC libraries
  (whose codemodel `target.dependencies[]` is empty because no link
  step runs), lower falls back to the trace's `target_link_libraries`
  call records and resolves IMPORTED names through
  `imports.LookupCMakeTarget`.
- **subdir-library includes dedup ✓** — `lower.ToIR` now dedups the
  includes slice (preserving order).

## Open fidelity deltas

Catalog of observed differences between cmake-built and
converted-then-bazel-built artifacts under the M5b fidelity gate.

### hello-world (`converter/testdata/sample-projects/hello-world`)

Status: ✅ symbol-tier passes. The single-file fixture exists to
smoke-test the harness itself rather than the converter; if
hello-world ever fails, it's the harness, not the converter.

### fmt (`make fetch-fmt`, `FMT_VERSION = 11.0.2`)

Status: 🔧 in-progress. The harness plumbing is wired
(`TestE2E_Fidelity_Fmt_SymbolEquivalent`) but the bazel-build step
exercises converter surfaces hello-world doesn't (genex resolution,
multi-TU `cc_library`, `<INSTALL_INTERFACE:>` filtering). Each delta
the harness surfaces lands here as a discrete sub-section with
reproducer + triage notes.

When a delta is observed, append a section like:

```
### <Fixture> / <Target> / <symptom-tier>: <one-line summary>

**Reproducer**: which test, which target, what the diff looks like.
**Root cause**: which converter file/function emits the problematic shape.
**Status**: open | fix-in-progress | wontfix-with-rationale.
```

`SYM_LOST` and `SYM_NEW` mean "in cmake but not bazel" / "in bazel
but not cmake" in `fidelity.DiffSymbols.Format()` output.

## Adding a new fixture

1. Drop a cmake project under
   `converter/testdata/sample-projects/<name>/`.
2. `tools/fixtures/record-fileapi.sh <name>` records the File API
   reply into `converter/testdata/fileapi/<name>/`.
3. Run `convert-element-cmake` manually to produce the BUILD; compare
   against expectation. Pin as
   `converter/testdata/golden/<name>/BUILD.bazel.golden` either
   directly or via the test's `-update` flag.
4. Add a `TestEmit_<Name>_Golden` to
   `converter/emit/bazel/emit_test.go` that loads the fixture +
   golden + asserts equivalence.
5. Document any surfaced gaps under "Open conversion deltas" above.

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

**Status**: open. Two closure paths:
- **Match by adding flags to copts**: ad-hoc, leaks host state into BUILD.bazel.
  Reasonable for one-off operators who need bit-exact symbol-set parity.
- **Match by adding features to the converted cc_toolchain**:
  `feature("fortify_source")` + `feature("stack_protector")` definitions with
  `--features=fortify_source` enabled by default in the toolchain config.
  Architecturally cleaner; keeps BUILD.bazel hermetic. Tracked as a Next item
  in `ROADMAP.md` ("Toolchain-feature parity vs. cmake's default Release
  hardening flags").
