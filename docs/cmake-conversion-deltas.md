# cmake → Bazel conversion: known deltas

Empirically surfaced by the test corpus under
`converter/testdata/sample-projects/`. Each fixture's golden
records current converter behaviour; this doc captures the
real-world correctness gaps that the goldens accept today, so
they don't get lost as the corpus grows.

The corpus + this doc are the empirical alternative to design-
time correctness arguments — running cmake against a real
fixture surfaces the gaps that a hand-walk through lower / emit
won't find.

## Open deltas

The bar: every fix that *removes* information from the converted
output (header narrowing, dep filtering, etc.) needs a high-
confidence signal — preferably from cmake's trace-expand record
of resolved command calls, never a basename-match heuristic.
Cosmetic-only deltas (information that's redundant but
correct) stay open as documentation rather than getting
heuristic fixes that introduce false-positive risk.

### subdir-library — over-broad hdr collection (cosmetic, not correctness)

**Fixture**: `converter/testdata/sample-projects/subdir-library/`
(top-level CMakeLists adds `src/util/` via `add_subdirectory`;
both define cc_library targets).

**Surfaced**:
`cc_library(name = "util")` emits
`hdrs = ["include/toplib.h", "include/util.h"]` — every `.h`
file in the project. The `target_include_directories(...
PUBLIC include)` from both CMakeLists declared `include/`, so
discoverHeaders' walk returns every header to every target.

**Why this is OK today**: Bazel allows the same file in
multiple cc_libraries' hdrs. The redundancy is cosmetic noise
in the BUILD output, not a build-time correctness issue —
both libraries compile and consumers link correctly.

**Why an early heuristic fix was reverted**: a basename-match
narrow ("drop `util.h` from toplib's hdrs because `util` is a
different target's name") false-positives on projects where a
header coincidentally shares a name with an unrelated target
(e.g. a `util` executable that has nothing to do with a
`util.h` header in a different library).

**Why we won't pursue the deterministic alternative**: scanning
source files for `#include "..."` directives is deterministic
(no name guessing), but expands the converter's action input
set to include every `.c` / `.cpp` source file it reads. That
means every source-file edit invalidates convert-element-cmake's
cache and triggers a re-run. The current behaviour (read only
the codemodel / cmakeFiles / compile_commands / build.ninja)
keeps convert-element-cmake re-runs gated on CMakeLists / cmake-cache
changes, which are rare. Trading rare re-runs for precise hdrs
isn't worth it — the hdrs duplication is a cosmetic BUILD-file
diff, not a build-time correctness issue.

This delta stays open as documentation; no fix is planned.

### cmake 4.x CMP0026 — legacy `get_target_property(... LOCATION)` fails at configure

**Fixture**: not yet in tree. Surfaced by real-world packages
that still use the pre-3.0 idiom for resolving an executable
target to its on-disk path — `get_target_property(<var> <tgt>
LOCATION)` — most visibly [yasm][yasm] (four occurrences across
`YasmMacros.cmake` and `modules/preprocs/nasm/CMakeLists.txt`).

[yasm]: https://github.com/yasm/yasm

**Symptom**: cmake 4.x removed the OLD behaviour of CMP0026
entirely; configure fatal-errors with

```
The LOCATION property may not be read from target "...". Use the
target name directly with add_custom_command, or use the
generator expression $<TARGET_FILE>, as appropriate.
```

Convert-element-cmake annotates the failure at that point —
`cmakerun.annotateConfigureFailure` recognises the
`LOCATION property may not be read` sentinel and appends a
`[hint]` block to the surfaced error pointing operators at the
workarounds below.

**Why convert-element-cmake doesn't auto-fix**: rewriting
`CMakeLists.txt` / `*.cmake` files is source-mutating; doing
that inside the converter would either modify the operator's
source tree (destructive) or duplicate the tree into a scratch
copy (doubles the disk cost for large checkouts). The orchestrator's
source-key model also keys on the original source bytes, so a
silent rewrite would split the cache without a clear audit
trail. We surface the diagnostic, document the workarounds, and
let the operator choose.

**Workarounds (in preference order)**:

1. **Patch the unpacked source.** Replace each call with the
   generator-expression equivalent:

   ```sh
   find . \( -name CMakeLists.txt -o -name '*.cmake' \) \
     -exec sed -i -E \
       's/get_target_property\(([^ ]+) +([^ ]+) +LOCATION\)/set(\1 $<TARGET_FILE:\2>)/g' \
       {} +
   ```

   When pulling the package through Bazel's `http_archive`,
   thread this through `patch_cmds` so the rewrite runs before
   convert-element-cmake configures the tree.

2. **Pin cmake to a 3.x release.** The orchestrator's
   `Makefile` `CMAKE_VERSION` is `3.28.3` by default; cmake 3.x
   emits a deprecation warning but still resolves LOCATION,
   so it's a viable stopgap when patching upstream is
   impractical. `-DCMAKE_POLICY_DEFAULT_CMP0026=OLD` works
   inside cmake 3.x but only if `cmake_minimum_required(VERSION
   3.0+)` hasn't already forced CMP0026 to NEW; in cmake 4.x
   the policy is gone entirely and the override is rejected
   with `policy CMP0026 was removed`.

## Resolved deltas

### multi-language — per-language compile-group split ✓

**Fixture**: `converter/testdata/sample-projects/multi-language/`.

**Was**: emitted `cc_library` carried only the C compile
group's flags; C++ source would compile with `-std=c11` and
fail. Lower's `cg := t.CompileGroups[0]` assumed one language
per target.

**Now**: targets with ≥ 2 distinct compile-group languages
split into a wrapper cc_library (the user-visible name,
deps-only) plus one private sub-library per language with
that language's srcs + flags. Wrapper retains the public
surface (`hdrs`, `includes`, `linkstatic`, `visibility`,
install metadata); sub-libraries (`<name>_c`, `<name>_cxx`,
…) carry srcs + `copts` + `defines` extracted from each
CompileGroup's CompileCommandFragments. Single-language
targets stay one cc_library — split only fires at len(langs)
≥ 2. See `splitMultiLanguage` in
`converter/internal/lower/lower.go` and
`converter/testdata/golden/multi-language/BUILD.bazel.golden`.

### configure-file — generated header dependency missing ✓

**Fixture**: `converter/testdata/sample-projects/configure-file/`.

**Was**: the emitted `cc_library(name = "cfglib")` had no `hdrs`
reference to `config.h` and no genrule to produce it.
Bazel-build of the converted output would fail at compile time
because the include path resolved to nothing.

**Now**: lower walks cmake's `--trace-expand` JSON for
`configure_file(<input> <output> ...)` calls, reads the
rendered output bytes from the build dir (live in production;
captured in the fixture by `tools/fixtures/record-fileapi.sh`
mirroring the build-dir layout), and emits a Bazel genrule
whose cmd `base64 -d`'s the bytes into `$@`. Targets whose
codemodel-recorded includes contain the build dir get the
genrule's output added to their `hdrs` (with a
`has-cmake-codegen` tag). See
`converter/internal/lower/configure_file.go` and the
`gen_config_h` rule in
`converter/testdata/golden/configure-file/BUILD.bazel.golden`.

### visibility — PRIVATE includes leak as consumer-visible ✓

**Fixture**: `converter/testdata/sample-projects/visibility/`.

**Was**: `includes = ["include", "include/private"]` —
both PUBLIC and PRIVATE dirs surfaced in cc_library's
consumer-visible `includes`, propagating to every dependent's
compile command.

**Now**: lower walks the trace's
`target_include_directories(<target> PUBLIC ... PRIVATE ...)`
calls and partitions: PUBLIC dirs stay in `includes`
(consumer-visible), PRIVATE dirs become per-target
`copts = ["-I<dir>"]` (compile-only, not propagated). See the
`privateIncludeDirs` map populated in `lower.ToIR` and the
`includes` / `copts` split in
`converter/testdata/golden/visibility/BUILD.bazel.golden`.

Note: the PRIVATE *header* `include/private/internal.h` still
appears in `hdrs` because it's path-reachable through the
PUBLIC `include/` dir (cmake's PRIVATE keyword scopes the
include path, not which files live where on disk —
encapsulation is enforced by `install(... PATTERN "private"
EXCLUDE)`). Build-tree visibility is what the codemodel +
trace describe; that's what we faithfully encode.

### find-package STATIC — IMPORTED deps don't surface ✓

**Fixtures**: `converter/testdata/sample-projects/find-package/`
(SHARED — codemodel-driven path) and
`converter/testdata/sample-projects/find-package-static/`
(STATIC — trace-driven fallback).

**Was**: STATIC libraries' codemodel has empty
`target.dependencies[]` AND empty `target.link.commandFragments[]`
because no link step runs at archive time. So
`target_link_libraries(staticLib ZLIB::ZLIB)` produced no dep
edge in the converted BUILD.

**Now**: when `t.Type == "STATIC_LIBRARY"`, lower falls back
to the trace's `target_link_libraries` call records and
resolves IMPORTED target names through `imports.LookupCMakeTarget`.
The dedup against existing `t.Dependencies` keeps in-codebase
deps from double-counting. SHARED targets keep using the
codemodel link fragments (already covered by
`imports.LookupLinkPath`); the trace fallback only fires for
STATIC. Both paths are exercised by the find-package fixtures
above.

## Previously resolved deltas

### subdir-library — includes dedup ✓

`converter/internal/lower/lower.go` now dedups the includes
slice at IR-build time (preserving order). Before:
`includes = ["include", "include"]` for a target whose own
`target_include_directories` named "include" plus a PUBLIC dep
that also named "include". After: `includes = ["include"]`.

High-confidence fix: identical-string dedup, no semantic call
required.

## Reverted heuristics

### subdir-library hdrs — target-name basename match (REVERTED)

A previous attempt narrowed each target's hdrs by dropping
headers whose basename matched a different target's name.
Reverted because: a project with a header coincidentally
matching an unrelated target's name (e.g. a `util` executable
plus a `util.h` for a different library) would silently lose
the header from the right target. The cosmetic redundancy is
preferable to a false-positive narrow.

The right narrow uses `#include` scanning of source files —
deterministic, no name guessing — but is deferred until an
FDSDK-shape fixture surfaces the cosmetic noise as actually
mattering.

## Adding a new fixture

1. Drop a cmake project under
   `converter/testdata/sample-projects/<name>/`.
2. `tools/fixtures/record-fileapi.sh <name>` records the
   File API reply into `converter/testdata/fileapi/<name>/`.
3. Run convert-element-cmake manually to produce the BUILD; compare
   against expectation. Pin as
   `converter/testdata/golden/<name>/BUILD.bazel.golden` either
   directly or via the test's `-update` flag.
4. Add a `TestEmit_<Name>_Golden` to
   `converter/emit/bazel/emit_test.go` that loads the
   fixture + golden + asserts equivalence.
5. Document any surfaced gaps under "Open deltas" above.
