# Codegen tag taxonomy

> Status: **stub for M2.** The taxonomy below is the contract M2 step 3
> emits against; this file is the user-facing reference. M2 fills in
> precise emission rules and adds the audit script. Once published, tag
> names are append-only — same stability rule as `failure-schema.md`.

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
| `cmake-codegen-execute-process-op=<op>` | execute_process-derived cmake -E call carries the op name (`touch` / `copy` / `copy_if_different` / `configure_file`) so audits can split lifted ops without re-parsing the cmd. | append-only |
| `cmake-codegen-configure-file` | configure_file-derived genrule (either lifted or legacy bytes-embedded). | append-only |
| `cmake-codegen-file-generate` | file(GENERATE)-derived genrule (either lifted or legacy bytes-embedded). Distinguishes from configure_file via the driver=file_generate facet on the same rule. | append-only |
| `cmake-codegen-file-generate-genex` | file(GENERATE) call with a `$<...>` generator expression in INPUT / CONTENT — the lift short-circuited to the legacy bytes-embedded shape because configurefile.Substitute doesn't evaluate genex. Mutually exclusive with cmake-codegen-lifted on the same rule. Future genex-evaluation work (see ROADMAP "Generator-expression evaluation in lifted genrules") targets exactly this set. **Note:** cmake also allows generator expressions in `OUTPUT` (and `CONDITION`); the trace records the literal `$<...>` so the lifter can't map it back to the on-disk filename without a genex evaluator. v1 drops these calls entirely (no genrule emitted, no tag — there's nothing to anchor); the same Later roadmap bullet covers the path forward. | append-only |
| `cmake-codegen-lifted` | Genrule emits via a Bazel-time tool (`//tools:cmake-configure-file`) reading a values dict + a template (srcs entry for INPUT form, or `--content-base64` inline blob for CONTENT form). The template body — not the rendered output — drives the cmd, so editing the template invalidates the genrule through Bazel's source graph rather than through convert-element rerun. Applied by the configure_file, file(GENERATE), and cmake -E configure_file lifters. | append-only |
| `cmake-codegen-tool-from-target` | The driver tool is itself a target inside this element (typical of generator binaries built earlier in the same project). Useful for build-graph layering checks. | append-only |
| `cmake-codegen-source-only` | Output is consumed only as a `srcs`/`hdrs` entry of a downstream cc_library/cc_binary — i.e. the codegen exists purely to feed the compile graph. | append-only |
| `cmake-codegen-script` | Command runs `${CMAKE_COMMAND} -P script.cmake`. Architectural refusal: the converter emits this tag on the failing rule's placeholder so the operator sees the exact site, then exits with `failure.json` `code: unsupported-custom-command-script`. | append-only |

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

# file(GENERATE) calls that fell back to legacy because of a generator
# expression — the future genex-evaluation work targets exactly this set.
bazel query 'attr("tags", "cmake-codegen-file-generate-genex", //...)'
```

A wrapper at `tools/audit/list-codegen.sh` exposes these query shapes
plus a few cross-joins (codegen rules with their immediate consumers,
codegen rules grouped by driver tool).

## Stability promise

- Tag names listed above are stable and append-only after this document
  is published.
- New facets become new tags; existing tags don't change meaning.
- The audit script's flag surface is the same: removed flags get
  preserved as no-ops with a deprecation log line for one minor version.
