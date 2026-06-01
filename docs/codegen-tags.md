# Codegen tag taxonomy

> Stability: **append-only.** Tag names below are stable; new facets
> become new tags, existing tags don't change meaning. Same
> stability rule as [`failure-schema.md`](failure-schema.md).

Every Bazel rule produced by recovering an `add_custom_command` from a
`build.ninja` carries a stable `cmake-codegen` tag (and zero or more
sub-tags) so the entire converted project can be audited with `bazel
query` without scanning rule bodies.

## Producer-side tags

Applied to the `genrule` that the converter emits.

| Tag | When emitted | Stability |
|---|---|---|
| `cmake-codegen` | Always, on every recovered genrule. | append-only |
| `cmake-codegen-driver=<name>` | Always. `<name>` is the first command-token after `cd ... &&` (or the first token if no chdir), with wrappers (`env`, `sh -c`, `taskset`, …) stripped via a recognizer list. Falls back to `unknown` if extraction fails — never omitted. `<name>` is `cmake_e` for cmake -E recoveries, `file_generate` for file(GENERATE) recoveries, `configure_file` for configure_file recoveries. | append-only |
| `cmake-codegen-cmake-e` | Command invokes `${CMAKE_COMMAND} -E <op>` and the converter translated the op to a native Bazel idiom (e.g. `cp $< $@`). | append-only |
| `cmake-codegen-execute-process-op=<op>` | execute_process-derived cmake -E call carries the op name so audits can split lifted ops without re-parsing the cmd. cmake -E builtins use the op name (`touch` / `copy` / `copy_if_different` / `copy_directory` / `copy_directory_if_different` / `create_symlink` / `rename` / `configure_file`); raw POSIX commands lifted via their cmake -E equivalent use the raw driver basename (`cp` → copy, `ln` → create_symlink-as-copy, `mv` → rename-as-copy; raw `touch` shares the `touch` label since it is the same operation). Benign no-op ops (`make_directory` / `remove` / `remove_directory` and the raw `mkdir` / `rm` / `rmdir` analogs, plus install-compat-alias `create_symlink` / `ln` whose source and link both anchor nowhere) emit no genrule and so carry no tag — there is nothing to anchor. | append-only |
| `cmake-codegen-configure-file` | configure_file-derived genrule (either lifted or legacy bytes-embedded). | append-only |
| `cmake-codegen-file-generate` | file(GENERATE)-derived genrule (either lifted or legacy bytes-embedded). Distinguishes from configure_file via the driver=file_generate facet on the same rule. | append-only |
| `cmake-codegen-genex-resolved` | file(GENERATE) call carrying a `$<...>` generator expression that the converter **resolved** — either via the (a) Go-side `internal/genexeval` evaluator (cmd wire `--genex-context=`) or the (b) structured-base64 capture (cmd wire `--genex-values=`). Rendered output is no longer content-load-bearing in srckey; cmake-configure-file re-evaluates at Bazel time. Phase 3 of the generator-parity uplift collapsed the former four-way split (`cmake-codegen-file-generate-genex{,-lifted,-evaluated,-cross-package}`) into this one positive tag for resolved genexes; the (a)-vs-(b) distinction now lives only in the cmd wire, not the tag set. Co-occurs with `cmake-codegen-lifted`. | append-only |
| `cmake-codegen-genex-unresolved` | file(GENERATE) call with a `$<...>` the lift could NOT resolve — short-circuited to the legacy bytes-embedded shape because neither the (a) evaluator nor the (b) capture could anchor the value. Mutually exclusive with `cmake-codegen-lifted` and `cmake-codegen-genex-resolved` on the same rule; the rendered output stays content-load-bearing in srckey. Audits hunting conversions not yet at genex parity key on this. **Note:** cmake also allows generator expressions in `OUTPUT` (and `CONDITION`); the trace records the literal `$<...>` so the lifter can't map it back to the on-disk filename without a genex evaluator. v1 drops these calls entirely (no genrule emitted, no tag — there's nothing to anchor). | append-only |
| `cmake-codegen-genex-cross-package` | file(GENERATE) whose body references a cross-package `$<TARGET_FILE:Pkg::tgt>` the converter could neither resolve same-package nor through the imports manifest. The emitted genrule is a **refusal stub** that fails at bazel-build time on purpose (soundness gate) rather than baking wrong bytes. Not a resolution — kept distinct from `-resolved`. | append-only |
| `cmake-codegen-lifted` | Genrule emits via a Bazel-time tool (`//tools:cmake-configure-file`) reading a values dict + a template (srcs entry for INPUT form, or `--content-base64` inline blob for CONTENT form). The template body — not the rendered output — drives the cmd, so editing the template invalidates the genrule through Bazel's source graph rather than through convert-element-cmake rerun. Applied by the configure_file, file(GENERATE), and cmake -E configure_file lifters. | append-only |
| `cmake-codegen-tool-from-target` | The driver tool is itself a target inside this element (typical of generator binaries built earlier in the same project). Useful for build-graph layering checks. | append-only |
| `cmake-codegen-source-only` | Output is consumed only as a `srcs`/`hdrs` entry of a downstream cc_library/cc_binary — i.e. the codegen exists purely to feed the compile graph. | append-only |
| `cmake-codegen-script` | Command runs `${CMAKE_COMMAND} -P script.cmake`. Architectural refusal: the converter emits this tag on the failing rule's placeholder so the operator sees the exact site, then exits with `failure.json` `code: unsupported-custom-command-script`. | append-only |
| `cmake-codegen-cmd-genex-resolved` | Standalone custom-command genrule whose `add_custom_command` argv (recorded literal in the trace; cmake resolves genexes away in `build.ninja`) carried a `$<...>` generator expression, and every **path-bearing** genex was lifted to a `$(location :t)` label + tools dep (so the cmd stays portable). Path-bearing covers the whole TARGET_FILE family the converter scans — `$<TARGET_FILE[_DIR/_NAME]:t>`, `$<TARGET_LINKER_FILE[_DIR/_NAME]:t>`, `$<TARGET_SONAME_FILE:t>` — plus `$<TARGET_OBJECTS:t>`. A command carrying only value genexes (`$<CONFIG>` and the like) also classifies resolved — the resolved value bakes correctly for a single configure. Co-occurs with `cmake-codegen-standalone-custom-command`. | append-only |
| `cmake-codegen-cmd-genex-unresolved` | Standalone custom-command genrule whose `add_custom_command` argv carried a path-bearing genex (any TARGET_FILE-family op or `$<TARGET_OBJECTS:t>`) whose target did NOT map to a label — its cmake-resolved (recording-machine-specific) path is baked into the cmd as a non-portable literal. Today this covers `$<TARGET_OBJECTS:t>` (object lists aren't in the artifact map) and cross-element TARGET_FILE-family refs (the producing target lives outside this element's codemodel). Audits hunting custom-command genex residue key on this. Mutually exclusive with `-resolved` on the same rule. | append-only |

## Consumer-side tag

Applied to any `cc_library` / `cc_binary` whose `srcs` or `hdrs`
includes a path that comes from a `cmake-codegen`-tagged genrule.

| Tag | When emitted |
|---|---|
| `has-cmake-codegen` | The target depends on at least one codegen output transitively at the source-list level. |

## Why two-sided tagging

A single producer tag would force consumer-discovery queries to walk
the dep graph (slow at project scale, breaks across aliasing/renames).
A consumer-side `has-cmake-codegen` tag answers "which compile units
consume codegen?" in one query, independent of how the genrule is
labelled or aliased.

## Common queries

```sh
# Every recovered codegen rule in the project.
bazel query 'attr("tags", "cmake-codegen", //...)'

# Codegen rules driven by a specific tool.
bazel query 'attr("tags", "cmake-codegen-driver=python3", //...)'

# Targets that consume any codegen.
bazel query 'attr("tags", "has-cmake-codegen", //...)'

# Codegen rules that translate to native Bazel idioms (no cmake at runtime).
bazel query 'attr("tags", "cmake-codegen-cmake-e", //...)'

# Lifted-shape codegen (BUILD.bazel content decoupled from rendered output).
bazel query 'attr("tags", "cmake-codegen-lifted", //...)'

# file(GENERATE) calls that fell back to legacy because a generator
# expression couldn't be resolved — remaining genex-parity work targets this set.
bazel query 'attr("tags", "cmake-codegen-genex-unresolved", //...)'

# file(GENERATE) calls whose generator expression the converter resolved
# (via the Go-side evaluator or the structured-base64 capture).
bazel query 'attr("tags", "cmake-codegen-genex-resolved", //...)'
```

A wrapper at `tools/audit/list-codegen.sh` exposes these query shapes
plus a few cross-joins (codegen rules with their immediate consumers,
codegen rules grouped by driver tool).

## Stability promise

- Tag names listed above are stable and append-only after this document
  is published.
- New facets become new tags; existing tags don't change meaning.
- One-time exception (Phase 3, generator-parity uplift): the four
  `cmake-codegen-file-generate-genex{,-lifted,-evaluated,-cross-package}`
  tags were renamed/collapsed into `cmake-codegen-genex-resolved`,
  `cmake-codegen-genex-unresolved`, and `cmake-codegen-genex-cross-package`.
  The meaning is preserved (resolved/unresolved/refusal); the (a)-vs-(b)
  resolution-path facet was dropped because no consumer keyed on it.
- The audit script's flag surface is the same: removed flags get
  preserved as no-ops with a deprecation log line for one minor version.
