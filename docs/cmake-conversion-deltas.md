# CMake conversion deltas

What `convert-element-cmake` doesn't (yet) get right when modelling
cmake source trees — i.e. differences between what cmake's File API +
trace records and what the converter emits in `BUILD.bazel`. Surfaced
by the fixture corpus under `converter/testdata/sample-projects/`.

For artifact-level differences between cmake-built and
converted-then-Bazel-built outputs see
[`docs/fidelity-deltas.md`](fidelity-deltas.md).

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
- **`includes = [""]` workspace-root emission ✓** — surfaced during
  the May 2026 close-gaps survey; Bazel rejects `includes=[""]` with
  "resolves to the workspace root, which would allow this rule and
  all of its transitive dependents to include any file in your
  workspace". Fix landed in PR #253.

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
