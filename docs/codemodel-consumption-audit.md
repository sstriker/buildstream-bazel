# Codemodel + cmake-spec consumption audit

What `convert-element-cmake` currently consumes from cmake's
File API codemodel-v2 + trace stream, what it doesn't, and what
that omission actually costs. Audit performed against the 6-project
survey (LLVM, VTK, fmt, json, libpng, zlib) on the post-#298 main —
2244 targets and 541 directories surveyed.

For the operator-side flag-set discussion that complements this
audit, see [`operator-toolchain-features.md`](operator-toolchain-features.md).

## Corpus expansion run (abseil / protobuf / googletest / eigen)

Four projects added to maximise pattern coverage over the original
six. Reproduce with `make fetch-survey && make converter &&
scripts/run-survey.sh` (pins in the `Makefile`; the runner drives
`convert-element-cmake --source-root … --diagnostics` per project).
Run against cmake 3.28.3 on the post-#306 main:

| Project | cc targets | Tier-1 rejections | bazel-idiom findings |
|---|---:|---:|---|
| abseil-cpp `20260107.1` | 209 | 0 | 0 |
| protobuf `v6.31.1` | 122 | 0 | 6 (standalone; see below) |
| googletest `v1.17.0` | 9 | 0 | 0 |
| Eigen `3.4.1` | 492 | 1 | 0 |

Two genuine new datapoints:

- **protobuf — `find_package(ZLIB)` is a cross-element dep.** Every
  `protoc` / plugin / test binary links `find_package(ZLIB)` (`libz.so`),
  so standalone (the survey runs each project on its own) 6 targets emit
  `find-package-dep-unresolved` and the dep is dropped from `deps`. Not a
  converter bug — it's the config-mode-consumer shape the six leaf
  libraries never exercised. In a real `.bst` element graph (zlib as a
  sibling kind:cmake element) this resolves through the orchestrated
  producer→consumer export channel rather than a hand-authored manifest:
  the zlib producer's convert run synthesizes a `lib/cmake/ZLIB/
  ZLIBConfig.cmake` bundle (keyed on the trace-recovered
  `install(EXPORT … NAMESPACE ZLIB::)` stem) plus an `exports.json`
  mapping `ZLIB::ZLIB` → its Bazel label; write-a stages both into the
  consumer's convert genrule (`--prefix-dir` + `--exports-in`), and
  `CMAKE_FIND_PACKAGE_PREFER_CONFIG` makes `find_package(ZLIB)` resolve
  to the producer bundle instead of the host `libz`. The
  `TestE2E_CMakeConsumer_NamespaceDiffersFromProject` gate proves the
  end-to-end edge for a project-name ≠ namespace ≠ target case. The
  standalone survey deliberately leaves the 6 findings visible rather
  than masking them with a bespoke imports manifest.
- **Eigen — 1 × `unsupported-execute-process`.** `test/CMakeLists.txt`
  runs `c++ --version | head -n 1` — a multi-COMMAND pipeline the
  execute_process classifier refuses (concurrent stages with stdout
  chaining). Confined to the test tree; the library graph converts
  clean.

abseil (the idiom oracle) and googletest convert with **zero** raw
feature-flag findings — consistent with the post-#247 `liftRawFeatureFlags`
result on LLVM/VTK/fmt; the feature-flag lift generalises to abseil's
209 targets without a single `raw-toolchain-feature-flag` residue.
The codemodel census table above is unchanged: the expansion surfaced
no newly-consumed-or-dropped field, only the two lift-quality / operator-input
datapoints noted here.

## Codemodel field census

| Field | Survey usage | Consumed? | Notes |
|---|---|---|---|
| `Target.{name,id,type}` | 2244 | ✓ | primary key |
| `Target.backtrace` + `backtraceGraph` | 2244 | ✓ | provenance comments |
| `Target.compileGroups[]` | 557 | ✓ | sources + flags per group |
| `Target.compileGroups[].compileCommandFragments` | 576 | ✓ | flag set |
| `Target.compileGroups[].defines` | 569 | ✓ | `defines = [...]` |
| `Target.compileGroups[].includes` | 576 | ✓ | `includes = [...]` |
| `Target.compileGroups[].language` | 576 | ✓ | language routing |
| `Target.compileGroups[].languageStandard` | 493 | ✓ | `-std=` injection |
| `Target.compileGroups[].sourceIndexes` | 576 | ✓ | per-group source split |
| `Target.compileGroups[].includes[].isSystem` | 0 | ✓ | PRIVATE system include → `-isystem<dir>` copts; PUBLIC rides `cc_library.includes`, which Bazel already emits as `-isystem` + transitive |
| `Target.compileGroups[].precompileHeaders` | varies | tag-only | `cmake-codegen-pch` tag; PCH lift kept tag-only by decision (operator cc_toolchain feature) |
| `Target.dependencies` | 2151 | ✓ | `deps = [...]` |
| `Target.dependencies[].backtrace` | 2151 | ✗ | per-dep call-site provenance; niche |
| `Target.artifacts` | 558 | ✓ | tool-from-target lift |
| `Target.archive` | 112 | ✓ | static lib classification |
| `Target.link.commandFragments[role=libraries]` | 9483 | ✓ | `linkopts` / `deps` |
| `Target.link.commandFragments[role=flags]` | 753 | ✓ | `linkopts` |
| `Target.link.language` | 443 | ✓ | linker language |
| `Target.install.destinations` | 296 | ✓ | install lift |
| `Target.nameOnDisk` | 555 | ✓ | tool-from-target lift |
| `Target.sources[].path` | 15932 | ✓ | `srcs` / `hdrs` |
| `Target.sources[].isGenerated` | 3695 | ✓ | genrule routing |
| `Target.sources[].compileGroupIndex` | 6535 | ✓ | per-group split |
| `Target.sources[].backtrace` | 15932 | ✗ | per-source provenance; niche |
| `Target.sources[].sourceGroupIndex` | 15932 | ✗ | IDE-only; no Bazel analogue |
| `Target.sourceGroups` | 2229 | ✗ | IDE-only; no Bazel analogue |
| `Target.folder` | 1874 | ✗ | IDE folder name; no Bazel analogue |
| `Target.fileSets` | varies | ✓ | `target_sources(... FILE_SET HEADERS ...)` |
| `Target.launchers` | 0 | surfaced | cross-compile emulator / test launchers (codemodel-v2 minor 7, cmake 3.29+); parsed + warned (`surfaceLauncherTargets`); routing fixture-gated |
| `Target.link.commandFragments[].backtrace` | 9483 | ✗ | per-fragment provenance; niche |
| `Directory.installers[type=file]` | 732 | ✓ | filegroup lift |
| `Directory.installers[type=target]` | 419 | ✓ | install-target routing |
| `Directory.installers[type=directory]` | 7 | ✓ | filegroup lift |
| `Directory.installers[type=export]` | 5 | partial | `cmake_config_bundle` filegroup is consumed (cross-element staging); the sibling `cc_import` / `_hdrs` facade is **dormant** — see "Cross-element export facade" below |
| `Directory.installers[type=script]` | 10 | surfaced | warned with script file + source site, not lifted (`install_script_surface.go`) |
| `Directory.installers[type=code]` | 10 | surfaced | warned with source site, not lifted (`install_script_surface.go`) |
| `Directory.installers[].targetInstallNamelink` | varies | ✓ | parsed; `.so` namelink symlink intentionally not reproduced (Bazel imports by artifact, not SONAME) |
| `Directory.installers[].{scriptFile,backtrace}` | varies | ✓ | parsed; feed the install-script warning's site/script naming |
| `Directory.{backtraceGraph}` | 541 | ✓ | parsed; resolves installer backtrace → file:line |
| `Directory.installers[].{targetId,targetIndex,targetIsImportLibrary,isForAllComponents}` | varies | ✗ | `type==target` routed via `Target.install` instead; **not in the struct** |
| `ConfigDirectory.{parentIndex,childIndexes}` | 541 | ✗ | directory-tree topology; redundant — see "Index cross-references" below; **not in the struct** |
| `ConfigProject.{parentIndex,childIndexes}` | varies | ✗ | project-tree topology; same redundancy; **not in the struct** |

## Cross-element export facade — dormant (don't extend; investigated 2026-05-31)

The Phase 6 declarative `install(EXPORT)` projection
(`internal/exportshape`) emits three things per producer element:

- a **`cmake_config_bundle`** filegroup (the synthesized
  `<Pkg>Config.cmake` / `<Pkg>Targets.cmake`),
- one **`<name>_import` `cc_import`** per exported library, and
- one **`<name>_hdrs`** headers target.

**Only the bundle is consumed.** The live cross-element path is:
`write-a` stages the producer's `cmake_config_bundle` into the
consumer's convert genrule (`$PREFIX`), the consumer's cmake
re-resolves `find_package(<Pkg> CONFIG)` against it, and the recovered
dep edge points at the producer's **from-source `cc_library`**
(`//elements/prod:prod`). `scripts/meta-cross-cmake.sh` is the gate.

The **`cc_import` and `_hdrs` facades are emitted but consumed by
nothing** (grep: no dep references; the manifest's `cmake_import_labels`
field has no reader). The `cc_import` is moreover **latent**: it's
`cc_import(static_library = "lib/libprod.a")` — a literal *install-tree*
path that does **not** exist in the from-source Bazel build (the
from-source `cc_library` produces `libprod.a` in `bazel-bin`, not
`lib/libprod.a`, and nothing materializes it).

**Why not extend it (a consumer resolving onto the facade):** a
metadata-only facade can never be buildable — you can't have a linkable
artifact without producing it. The only buildable prebuilt path is
**round-2's `pipeline_install`** (runs `cmake --install` → a real
TreeArtifact; `pick_file` `cc_import`s project actual files out of it).
So:

- **From-source producer + consumer** → the from-source dep is already
  correct, buildable, idiomatic. The facade adds nothing.
- **Prebuilt/fallback producer** → round-2's install facade is the
  buildable answer (currently intra-element only).

A consumer-side resolver onto the codemodel `cc_import` facade was built
and reverted once this surfaced (PR #349 history). If cross-element
consumption of a *prebuilt* producer is ever a real need, the work is to
**expose round-2's `pipeline_install` facade cross-element** — not to
rebuild a (non-buildable) facade from the codemodel. A future
`bazelidiom` audit check could flag the dormant/latent `cc_import` as
dead emission.

## Genuine gaps (with survey impact)

### 1. `install(SCRIPT)` / `install(CODE)` — 20 in LLVM, warned not lifted

cmake exposes these as `Directory.installers[].type={script,code}`.
Survey: LLVM has 10 SCRIPT + 10 CODE; other projects have 0.

Typical contents (from LLVM's `cmake/modules/`): post-install
patching of cmake config files (substitute `@PACKAGE_INIT@`
tokens, rewrite `<...>_DIR` paths), chmod adjustments, symlink
creation. None of these have a clean Bazel translation — Bazel's
install story is operator-side via `rules_pkg`.

**Status (closed)**: `surfaceInstallScriptInstallers`
(`internal/lower/install_script_surface.go`) emits a stderr
warning naming each dropped directive with its `scriptFile`
(for install(SCRIPT)) and its cmake source site (`file:line`,
resolved through the directory's `backtraceGraph`). We still
don't lift — the install logic isn't portable — but the
omission is fully auditable. The remaining work is genuinely
out of scope (no Bazel analogue for install-time cmake
execution).

### 2. `Target.compileGroups[].includes[].isSystem` — consumed

Earlier framing claimed Bazel's `cc_library.includes` emits `-I`;
it actually emits **`-isystem` + transitive** (Bazel docs: "Each
string is prepended with `-isystem` and added to COPTS … added for
this rule and every rule that depends on it"). So a cmake
`target_include_directories(t SYSTEM PUBLIC dir)` already maps
faithfully onto `includes` — system flavour *and* transitive
propagation both preserved — with no extra handling.

The one place the SYSTEM keyword changes the converter's output is
the **PRIVATE** include path, which rides compile-only copts (cmake
PRIVATE doesn't propagate to consumers, and Bazel's `includes` is
consumer-visible, so PRIVATE has to go on copts). There the lift now
chooses `-isystem<dir>` for `SYSTEM PRIVATE` and `-I<dir>` for plain
PRIVATE (`lower.go`, gated on `CompileInclude.IsSystem`), so the
warning-suppressing SYSTEM flavour survives. Tests:
`system_includes_test.go`.

**Status (closed)**: `isSystem` is consumed where it affects output
(PRIVATE → `-isystem`) and faithful where Bazel already handles it
(PUBLIC via `includes`). Survey remains empty, so the path is pinned
by hand-built fixtures rather than a corpus project.

### 3. `Target.compileGroups[].precompileHeaders` — tag-only, by decision

The PCH header set is recorded in the codemodel; current handling
emits a `cmake-codegen-pch` tag. Bazel `cc_library` has no native
PCH attribute — Bazel-idiomatic PCH is a cc_toolchain feature
(`pch` flag set wired by the operator's cc_toolchain config).

**Status (closed — won't lift)**: kept tag-only by decision. A real
PCH lift can't live in the converter alone: it needs the operator's
cc_toolchain to define the `pch` feature, which is a cross-boundary
handshake (the converter emits BUILD; the cc_toolchain is operator-
owned). PCH is a build-speed optimisation, not a correctness
requirement — the converted target compiles identically without it —
so the tag (which keeps the omission auditable) is the right
terminal state until an operator-toolchain contract exists. Tracked
in [`operator-toolchain-features.md`](operator-toolchain-features.md).

## The other File API object kinds

The codemodel is the bulk of what we consume, but cmake's File API
emits four more object kinds (plus the index). `index.go`'s
`SupportedObjectMajors` queries all five — `codemodel`, `cache`,
`cmakeFiles`, `toolchains`, `configureLog` — so no object kind is
ignored wholesale. The field-level residue:

| Object kind | Coverage | Residue |
|---|---|---|
| `cache-v2` | **complete** | All entry fields (`name`/`value`/`type`/`properties`) parsed; only specific named entries (`CMAKE_<LANG>_COMPILER_ID`, `BUILD_SHARED_LIBS`, …) are read downstream via `Cache.Get`. No spec gap. |
| `toolchains-v1` | **complete** | `language`, `sourceFileExtensions`, `compiler.{id,path,version,target,implicit.*}` all parsed and used for cc_toolchain shaping / implicit-dir filtering. No spec gap. |
| `cmakeFiles-v1` | **complete** | Parses `paths` + `inputs[].{path,isGenerated,isExternal,isCMake}` + `globsDependent[]` (cmakeFiles-v1.1, cmake 3.29+). The `globsDependent` matched `paths` are folded into the configure-inputs oracle (`OutCMakeConfigureReads` in `convert-element-cmake`) so a `file(GLOB … CONFIGURE_DEPENDS)` match that picks up a new file invalidates the converter's cache the same way it re-triggers cmake — the ninja RERUN_CMAKE edge only carries the glob *stamp*, so this object is the authoritative match record. |
| `configureLog-v1` | **complete** | Handles `try_compile-v1`, `try_run-v1`, `find_package-v1`, `find-v1` (4.3+ polymorphic `found`), `message-v1`, with the full YAML node retained in `Event.Raw` for forward-compat kinds. No known missing event kind. |
| index file | partial | `objects` / `cmake.version` / `cmake.generator` consumed. `paths.ctest` / `paths.cpack` parsed but unused — there is no CTest/CPack consumption through the File API (CTest has its own parser at `internal/ctest/parse.go`; CPack has no Bazel analogue). Not a gap. |

## Index cross-references as source of truth

The codemodel cross-references its arrays two ways: stable string
**ids** (`Target.id`, `dependencies[].id`, `exportTargets[].id`) and
positional **indices** (`ConfigTargetRef.directoryIndex` /
`projectIndex`, `ConfigDirectory.{project,target}Index{es}`, and the
tree-topology `parentIndex` / `childIndexes`).

**These are the authoritative truth for membership and dependency
edges, and we already lean on them** — not on heuristics:

- Dependency edges resolve by **id** (`TargetDependency.Id` →
  `Reply.Targets[id]`), never by name-matching.
- Sub-package placement resolves by **index**:
  `subPackageDir` (`lower.go`) takes `ConfigTargetRef.DirectoryIndex`
  → `cfg.Directories[i].Source`, i.e. the target's *declaring*
  directory as cmake recorded it, using the directory's own
  authoritative `source` path. This is strictly better than
  string-munging package boundaries out of individual source-file
  paths, and it's what we do.

The one cross-reference we **don't** consume is the directory/project
**tree topology** (`parentIndex` / `childIndexes`). It doesn't matter:
a Bazel package is defined by its filesystem path, and each directory
already carries its full relative `source` path, so the flat
`directoryIndex → source` mapping is sufficient — the parent/child
links are redundant for package placement. They'd only matter if we
tried to mirror cmake's *logical* `add_subdirectory` grouping, which
Bazel can't honour when it diverges from on-disk layout anyway. So
**the indices are the better source of truth, we use the ones that
carry information, and the topology links are correctly skipped.**

## Goal: close all spec-coverage gaps — **done**

The field-level residue identified in this audit has been retired:

1. **`cmakeFiles.globsDependent`** (correctness) — ✅ parsed into the
   `CMakeFiles` struct; matched `paths` fold into the configure-inputs
   oracle (`OutCMakeConfigureReads`) so `CONFIGURE_DEPENDS` re-triggers
   the converter's cache the way it re-triggers cmake.
2. **`targetInstallNamelink`** (fidelity) — ✅ parsed on
   `DirectoryInstaller`. The `.so` namelink symlink is intentionally
   **not** reproduced: Bazel resolves shared-library imports by
   artifact (`cc_import`), not by SONAME symlink, so the paired
   namelink-"only" installer is correctly dropped, not lossy. Now
   documented at the drop site in `directory_installers.go`.
3. **`scriptFile` + `backtrace`** on script/code installers — ✅ parsed
   (plus the directory `backtraceGraph`); the install-script warning
   now names the script file and the cmake `file:line` declaration
   site.
4. **`Target.launchers`** (cross-compile) — ✅ parsed; surfaced via
   `surfaceLauncherTargets` (a stderr warning naming the
   CROSSCOMPILING_EMULATOR / TEST_LAUNCHER per target). Full routing to
   a cc_toolchain / test wrapper stays fixture-gated — 0 in the survey,
   and Bazel has no first-class per-target run-launcher to route to.

Schema note: `globsDependent` (cmakeFiles-v1.1) and `launchers`
(codemodel-v2.7) both require cmake ≥ 3.29; the survey pin (3.28)
emits neither, so their parsers are pinned by hand-built fixtures in
`fileapi/newfields_test.go` rather than recorded replies.

Not on the list (correctly unconsumed): the tree-topology indices,
`paths.ctest`/`cpack`, per-fragment/per-source/per-dep backtraces, and
the IDE-only metadata in the next table.

## Not-gaps (correctly unconsumed)

| Field | Why we skip |
|---|---|
| `ConfigDirectory.{parentIndex,childIndexes}` | Directory-tree topology; redundant for path-keyed Bazel packages (see "Index cross-references") |
| `Target.folder` | IDE-only metadata; Bazel has no notion of project-tree folder |
| `Target.sourceGroups` | IDE-only "source group" UI buckets |
| `Target.sources[].sourceGroupIndex` | Same; IDE-only |
| `Target.sources[].backtrace` | Per-source provenance; existing target-level provenance covers the operator's "where did this come from?" question |
| `Target.dependencies[].backtrace` | Same: per-dep call site; target-level provenance suffices |
| `Target.compileGroups[].languageStandard.backtraces` | Provenance noise |
| `Target.frameworks` (macOS) | Out of survey scope; Linux fixture corpus |

## Trace stream — extractors not yet wired

The `internal/shadow` decoders surface these:

| Trace command | Extractor | Lower consumer |
|---|---|---|
| `target_include_directories` | ✓ | PUBLIC/PRIVATE partition |
| `target_link_libraries` | ✓ | Deps / ImplementationDeps routing |
| `target_compile_definitions` | ✓ | `defines` / `local_defines` routing |
| `target_compile_options` | ✓ | INTERFACE_* aggregation |
| `configure_file` | ✓ | Genrule lift |
| `file(GENERATE)` | ✓ | Genrule lift |
| `execute_process` | ✓ | Hoist / refusal |
| `set_source_files_properties` | ✓ | HEADER_FILE_ONLY, OBJECT_DEPENDS, LANGUAGE override |
| `add_custom_command` | ✓ | Genrule recovery |
| `add_custom_target` | ✓ | Standalone genrule naming |
| `add_library` (ALIAS) | ✓ | alias() rule |
| `add_dependencies` | ✓ | data-edge classification |

Not yet wired (with rationale):

| Trace command | Why not |
|---|---|
| `target_link_options` | **Won't-do (decided).** Bazel `cc_library.linkopts` is already PRIVATE-equivalent; PUBLIC/INTERFACE link_options are lossy in Bazel (no transitive linkopts) without a split-target trick. Low value, no survey demand — closed unless a corpus project surfaces a need. |
| `target_link_directories` | Codemodel folds into `Link.CommandFragments[role=libraryPath]` |
| `target_sources` | Codemodel exposes sources directly |
| `set_target_properties` | Probe-genex covers the properties Bazel cares about (POSITION_INDEPENDENT_CODE, VISIBILITY presets); the rest are IDE / debugger-only |
| `target_precompile_headers` | Codemodel exposes via `CompileGroup.PrecompileHeaders` — see PCH gap above |
| `install` | Directory installers come through codemodel; trace would be redundant |
| `enable_language` | Codemodel `LanguageStandard` covers what Bazel needs |
| `find_package` | Imports manifest is the contract; in-tree synthesis covered by ALIAS lift |
| `option` | cmake-time config; becomes a cache var. Bazel `config_setting` + `select()` is operator-side. |

## Cmake spec features outside the codemodel

Things cmake does that codemodel-v2 doesn't surface (so trace is the
only path):

| Feature | Status |
|---|---|
| `add_compile_options` (directory-level) | folded into per-target CompileGroups by codemodel |
| `link_directories` / `link_libraries` (directory-level) | folded into per-target Link by codemodel |
| `include_directories` (directory-level) | folded into per-target CompileGroups by codemodel |
| `add_definitions` (directory-level) | folded into per-target CompileGroups |
| `set(CMAKE_<LANG>_FLAGS ...)` | folded into per-target compileCommandFragments |
| `find_dependency` (cmake config module) | imports manifest path |
| `cmake_path` / `cmake_parse_arguments` | configure-time only; no runtime effect |
| `message(FATAL_ERROR)` | configure-time abort; not relevant if cmake succeeds |
| `cmake_policy(SET CMP0XXX)` | policy effects flow through to the codemodel |
| `if(POLICY ...)` gates | resolved at configure time |
| `function` / `macro` definitions | resolved by trace expansion |
| `string(REGEX REPLACE ...)` | configure-time |

These either fold through the codemodel automatically or are
configure-time-only with no runtime effect.

## Summary

**No File API object kind is uncovered wholesale** — all five
(`codemodel`, `cache`, `cmakeFiles`, `toolchains`, `configureLog`)
are queried and parsed, and as of the gap-closure work above the
field-level residue is retired too:

- The one **correctness gap**, `cmakeFiles.globsDependent`
  (CONFIGURE_DEPENDS cache invalidation), is parsed and folded into
  the configure-inputs oracle.
- The **fidelity / cross-compile** items — `targetInstallNamelink`,
  script/code `scriptFile`+`backtrace`, `Target.launchers` — are all
  parsed and either consumed (namelink, script-site naming) or
  surfaced with routing fixture-gated (launchers).
- Everything still unconsumed is so by design: IDE metadata,
  tree-topology indices, ctest/cpack paths, per-fragment backtraces.

`cache-v2` and `toolchains-v1` were already field-complete; the
codemodel + trace coverage is comprehensive for the C/C++ subset
Bazel can express. The remaining converter work is lift-quality
polish for fields already consumed (per-source defines via
`splitCompileGroups`, alias-name sanitization, genrule cmd
normalisation, etc., landed across PRs #285–#298).
