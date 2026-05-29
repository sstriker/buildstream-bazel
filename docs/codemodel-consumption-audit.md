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
| `Target.compileGroups[].includes[].isSystem` | 0 | partial | survey: 0 usage; `-isystem` route not implemented |
| `Target.compileGroups[].precompileHeaders` | varies | tag-only | `cmake-codegen-pch` tag emitted; no PCH lift |
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
| `Directory.installers[type=file]` | 732 | ✓ | filegroup lift |
| `Directory.installers[type=target]` | 419 | ✓ | install-target routing |
| `Directory.installers[type=directory]` | 7 | ✓ | filegroup lift |
| `Directory.installers[type=export]` | 5 | ✓ | export-config tag |
| `Directory.installers[type=script]` | 10 | ✗ | dropped silently |
| `Directory.installers[type=code]` | 10 | ✗ | dropped silently |

## Genuine gaps (with survey impact)

### 1. `install(SCRIPT)` / `install(CODE)` — 20 dropped silently in LLVM

cmake exposes these as `Directory.installers[].type={script,code}`.
Survey: LLVM has 10 SCRIPT + 10 CODE; other projects have 0. The
converter currently silently drops them.

Typical contents (from LLVM's `cmake/modules/`): post-install
patching of cmake config files (substitute `@PACKAGE_INIT@`
tokens, rewrite `<...>_DIR` paths), chmod adjustments, symlink
creation. None of these have a clean Bazel translation — Bazel's
install story is operator-side via `rules_pkg`.

**Recommended action**: emit a `cmake-codegen-install-script`
tag on the synthetic install-* filegroup so operators see them
in the audit pass, paired with a one-line warning summarising
the count. Don't try to lift; the install logic isn't portable.

### 2. `Target.compileGroups[].includes[].isSystem` — 0 in survey

Bazel `cc_library.includes` is non-system (`-I<dir>`). cmake's
`target_include_directories(t SYSTEM ...)` should ideally route
to `copts = ["-isystem<dir>"]` (lossy: not transitive) or be
handled via a wrapped `cc_library` whose own includes are
`-isystem`-flagged via cc_toolchain.

**Survey is empty** — none of the 6 surveyed projects use SYSTEM
includes. Defer until a fixture demands it.

### 3. `Target.compileGroups[].precompileHeaders` — tag-only

The PCH header set is recorded in the codemodel; current handling
emits a `cmake-codegen-pch` tag. Bazel `cc_library` has no native
PCH attribute — Bazel-idiomatic PCH is a cc_toolchain feature
(`pch` flag set wired by the operator's cc_toolchain config).

**Recommended action**: keep tag-only. PCH lift requires
operator-side cc_toolchain coordination; documented in
[`operator-toolchain-features.md`](operator-toolchain-features.md).

## Not-gaps (correctly unconsumed)

| Field | Why we skip |
|---|---|
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
| `target_link_options` | Bazel `cc_library.linkopts` already PRIVATE-equivalent; PUBLIC/INTERFACE link_options lossy in Bazel (no transitive linkopts) — would require split-target trick. Low value. |
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

**Survey-actionable gaps**: 1 (install SCRIPT/CODE surfacing in
LLVM). Everything else is either empty in the survey, has no
clean Bazel translation, or is correctly unconsumed (IDE-only
metadata).

**Cross-cutting story**: the codemodel-v2 + trace coverage in the
converter is comprehensive for the C/C++ subset Bazel can express.
The remaining work isn't "consume more codemodel fields" — it's
"polish the lift quality for what's already consumed" (per-source
defines via `splitCompileGroups`, alias-name sanitization,
genrule cmd normalisation, etc., all of which landed in this
session's PRs #285-#298).
