# Codebase map

A developer-oriented tour of the repo. For the architectural story
see [`architecture.md`](architecture.md). For dev-loop commands and
the per-handler test map see [`../CONTRIBUTING.md`](../CONTRIBUTING.md).

## Binaries (`cmd/` and `converter/cmd/`)

The things you actually run.

| Binary | Role |
|---|---|
| `cmd/write-a` | Static renderer. Parses the `.bst` graph and writes project A + B BUILD files. Per-kind dispatch in `handler_*.go`. |
| `converter/cmd/convert-element-cmake` | The cmake converter. cmake File API + `--trace-expand` → `BUILD.bazel.out` + cmake-config bundle. |
| `converter/cmd/convert-element-meson` | The meson converter. `meson setup` + `meson introspect` → `BUILD.bazel.out`. |
| `converter/cmd/convert-element-pyproject` | The pyproject converter. Static parse of `pyproject.toml` + source-tree walk per build-backend (flit / hatchling / setuptools / poetry-core). |
| `cmd/convert-element-trace` | Trace-driven converter shared by `kind:autotools` / `make` / `manual` / `script` / `makemaker` / `modulebuild`. Parses execve trace + optional `make -np` db → `cc_library` / `cc_binary`. |
| `cmd/build-tracer` | Process tracer. Native ptrace (Linux/amd64) with strace fallback; `--source-root` opts in to openat capture for the narrowing audit. |
| `cmd/trace-publish` | Writes a canonicalized trace + make-db to the REAPI ActionCache under `SyntheticActionDigest(srckey, platform)`. |
| `cmd/trace-lookup` | Action-time AC reader the `trace_load` rule shells to. |
| `cmd/stage-b` | Copies project A's `BUILD.bazel.out` files over project B's per-element BUILDs; reports the changed-element set. |
| `cmd/finalize-b` | Post-convergence strip pass. Removes `trace_load` / `trace_build` / intermediate filegroups from converged project B; produces a standalone deliverable. |
| `converter/cmd/derive-toolchain` | Runs cmake against a probe project; emits `cc_toolchain_config.bzl` + `toolchain.cmake` for per-element conversions to share the probe. |
| `cmd/audit-narrowing` | Diffs an element's narrowing patterns against an action-time read oracle. See [`design/narrowing-audit.md`](design/narrowing-audit.md). |
| `cmd/source-push` | Packs a source tree into CAS via REAPI. Dev/test only; production uses `bst source push`. |
| `cmd/bst-translate` | Rewrites `.bst` sources to `kind:remote-asset` (CAS-backed). |
| `cmd/run-manifest` | Snapshots a built project A into the run-manifest shape the regression diff operates on. |
| `cmd/orchestrate-diff` | Compares two runs; exit 2 on regression. |
| `cmd/orchestrate-history` | Queries fingerprint history for churn / drift. |
| `cmd/build-cc-index`, `cmd/relax-keeps` | Gazelle-roundtrip support. |
| `cmd/cmake-configure-file` | Bazel-time tool that emits the lifted `configure_file` genrule's output (template + values dict → rendered file). |
| `converter/cmd/render-project-a` | Sibling renderer to write-a; emits project A only (research path). |
| `converter/cmd/probe-cell`, `probe-cell-fixture` | cmake-probe-cell helpers used by the toolchain probe-skip cache. |
| `converter/cmd/fold-element`, `unify-toolchains` | Multi-platform conditional folding (`select()` synthesis). |

## Shared packages (`internal/` and `converter/internal/`)

| Package | Role |
|---|---|
| `internal/cas` | CAS client + packer + tree (REAPI CAS + ActionCache surface). |
| `internal/manifest` | Imports-manifest schema; cross-element name → Bazel label resolver. |
| `internal/element` | `.bst` project loader, dep graph, kind filtering. |
| `internal/shadow` | Path-only-stat shadow-tree creator + read-path tracer. |
| `internal/synthprefix` | Per-element `CMAKE_PREFIX_PATH` stub trees (cmake-config bundle layout). |
| `internal/readpaths` | Shared pattern matcher for write-a + audit-narrowing. |
| `internal/tracenorm` | Trace canonicalization, openat filtering, synthetic AC key construction. |
| `internal/sourcecheckout` | Resolves source spec → local tree (local / git / remote-asset). |
| `internal/exports` | Parses `<Pkg>Targets.cmake` into the imports manifest. |
| `internal/configurefile` | Pure-Go cmake `configure_file` substitution (the runtime side of the lift). |
| `internal/genexeval` | cmake generator-expression evaluator (used by the lifted `file(GENERATE)` shape). |
| `internal/empfold`, `internal/allowlistreg`, `internal/regression`, `internal/fidelity`, `internal/bsttranslate` | per-feature support packages — see their package docs. |
| `converter/internal/lower` | The brain. cmake codemodel + ninja + trace → IR. Most converter bugs land here. |
| `converter/internal/fileapi` | cmake File API parsers (codemodel-v2, toolchains-v1, cmakeFiles-v1, configurelog). |
| `converter/internal/ninja` | `build.ninja` parser (custom, ~400 lines) — used for `add_custom_command` / `RERUN_CMAKE` recovery and `configure_reads` extraction. |
| `converter/internal/cmakerun` | Drives `cmake --trace-expand`, drops File API queries, scrubs the environment. |
| `converter/internal/cmakeargv` | cmake argv lexer (used by backtrace-driven keyword recovery). |
| `converter/internal/emit/bazel` | IR → `BUILD.bazel`. |
| `converter/internal/emit/cmaketoolchain`, `emit/bazeltoolchain` | Probe-skip cache + cc_toolchain emission. |
| `converter/internal/emit/sanitizerfeatures` | cmake sanitizer configs → cc_toolchain `--features`. |
| `converter/internal/bazelidiom` | Audit pass: surfaces tags the audit reads. |
| `converter/internal/bazelconstraints` | Bazel `config_setting` synthesis for cmake's per-config branches. |
| `converter/internal/configfold` | Multi-config (Debug/Release) fold into `select()`. |
| `converter/internal/ctest` | `ctest` test list parser → `cc_test`. |
| `converter/internal/exportshape` | install(EXPORT) bundle synthesis (cmake-config + per-config Targets-*.cmake). |
| `converter/internal/failure` | `failure.json` schema + Tier-1 classifiers. See [`failure-schema.md`](failure-schema.md). |
| `converter/internal/verify` | Post-emit verification passes. |
| `converter/internal/cli` | Flag parsing + exit codes. |
| `converter/internal/toolchain` | cmake probe + variant fold. |

## Rules + scaffolding

- **`rules_buildstream_bazel/`** — in-repo Bazel module referenced
  by rendered project A + project B. Not published to BCR — tightly
  coupled to write-a's emit shape. `finalize-b` removes the
  `bazel_dep` when no surviving element references it. Contents:
  - `rules/traces.bzl` — `trace_load` rule (action-time AC consumer
    of the round-2 rendezvous).
  - `rules/zero_files.bzl` — zero-byte stub materializer (shadow-tree
    primitive).
  - `rules/sources.bzl` — sources module extension (CAS-FUSE-backed
    external repos).

- **`tools/`** — operator helpers (`bst` wrapper, `converge.sh`
  fixpoint driver, audit + install scripts, e2e harnesses for the
  bb_clientd / buildbarn substrates).

- **`scripts/`** — render-gate shell scripts (`meta-*.sh`). One per
  handler family or shape; the test map is in
  [`CONTRIBUTING.md`](../CONTRIBUTING.md#render-gates).

- **`testdata/`** — `meta-project/` holds end-to-end fixtures driven
  by the render gates; `fdsdk-subset/` carries the FDSDK probes;
  `fuse-fixtures/` covers the FUSE-served-sources path.

- **`deploy/buildbarn/`** — local-dev REAPI cluster (bb-storage +
  bb-scheduler + bb-worker + bb-runner-bare via docker-compose).

## Reading order for a new developer

1. `converter/cmd/convert-element-cmake/main.go` — converter pipeline
   in ~80 readable lines.
2. `converter/internal/lower/lower.go` — where most converter logic
   actually lives.
3. `cmd/write-a/main.go` — multi-element driver: `.bst` graph in,
   two-pass meta-project out.
4. `scripts/meta-hello.sh` — full two-pass round-trip (write-a →
   `bazel build` A → `stage-b` → `bazel build` B → smoke binary).
5. `converter/cmd/convert-element-cmake/fidelity_e2e_test.go` — the
   e2e test that proves the stack produces the same artifacts cmake
   would.
