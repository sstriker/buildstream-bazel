# Tier-1 failure schema

> Stability: **append-only after this document is published.** Codes
> here are the orchestrator's dedup key; renames or removals
> retroactively invalidate the regression registry. Add new codes; don't
> reshape old ones.

The converter exits with one of three tiers:

| Exit | Tier | Meaning |
|---:|---|---|
| 0 | — | success |
| 1 | 1 | per-element conversion error; `failure.json` written if `--out-failure` is set |
| 65 | 2 | converter bug or malformed cmake output (uncaught error path) |
| 70 | 3 | infrastructure (sandbox spawn failed, disk full, etc.) |
| 64 | usage | bad CLI args |

Only Tier 1 carries a stable code surface. Tiers 2 and 3 are
operational signals; the orchestrator collects them by exit code, not
by message text.

## `failure.json` schema

Written when `--out-failure <path>` is set and the converter exits 1.

```json
{
  "tier": 1,
  "code": "<one of the codes below>",
  "message": "<human-readable, may include paths and snippets>"
}
```

Reserved future fields (parsers must ignore unknown keys):
- `context` (object) — structured trigger info (target name, file path, etc.)
- `remediation` (string) — operator-facing hint
- `at` (string) — RFC3339 timestamp

## Codes

### `configure-failed`

`cmake -S /src -B /build -G Ninja` exited non-zero. The wrapped cmake
error appears in `message`. Most common subcategories: missing
`find_package` dependency, syntax error in `CMakeLists.txt`, refused
generator-expression.

**Operator action:** read `message`; fix the package CMakeLists or
provide the missing dependency in the imports manifest.

**Emission point:** `cmd/convert-element-cmake/main.go` — wrapping
`cmakerun.Configure`'s error.

### `fileapi-missing`

`cmake` ran without error but the File API reply directory is missing
or empty. Either query stamps weren't placed before configure, or the
cmake version doesn't support the queried object kinds.

**Operator action:** verify cmake >= 3.20 (codemodel-v2 minimum);
check that no custom `--build` arg is suppressing the build dir.

**Emission point:** `fileapi.Load` — when no `index-*.json` exists.

### `fileapi-malformed`

Reply directory exists but a referenced object file failed to parse.
Could be a corrupted reply, an old cmake whose schema doesn't match
our typed structs, or a cmake bug.

**Operator action:** re-run with `-v` to see which file failed; if
schema-version-related, file an issue with the cmake major.minor.

**Emission point:** `fileapi.Load` — wrapping JSON unmarshal errors,
and `lower.ToIR` when a target referenced in codemodel isn't in
`r.Targets`.

### `ninja-parse-failed` _(M2)_

`build.ninja` failed to tokenize or referenced a rule it didn't
declare. Indicates the parser missed a syntactic construct.

**Operator action:** report with the offending `build.ninja` lines;
this is converter-side incompleteness, not user error.

**Emission point:** `ninja.Parse` (M2). Currently declared but not
emitted.

### `unsupported-target-type`

A target's CMake `type` is one the converter doesn't lower yet.
Examples: `OBJECT_LIBRARY`, `INTERFACE_LIBRARY` with file sets we
don't model.

`UTILITY` targets (`add_custom_target` / `add_dependencies` grouping
nodes) are silently skipped, not error-emitted: the underlying
`add_custom_command`s are recovered as genrules independently of the
utility node.

**Operator action:** if the target is essential, file an issue with
the target name and type; otherwise the orchestrator marks the
package excluded.

**Emission point:** `lower.lowerTarget` — the type switch's default
case.

### `unsupported-custom-command` _(M1 placeholder; refined in M2)_

In M1: any target with `isGenerated: true` source files. M2 splits
this:

- M2 narrows it to commands the converter cannot translate (e.g.
  `${CMAKE_COMMAND} -P script.cmake`, dynamic generator-expression
  driver tools).
- Pure-data ops (`${CMAKE_COMMAND} -E copy|touch|env|configure_file|
  make_directory|create_symlink`) translate to native Bazel idioms
  and don't trigger this code.
- Other custom commands lower to `genrule` with `cmake-codegen` tags
  (see `docs/codegen-tags.md`).

**Operator action:** review the custom command. If it's an
unevaluated cmake script or genex-driven tool, either pre-resolve in
upstream or write the rule by hand and import via the manifest.

**Emission point:** `lower.lowerTarget` (M1) and `lower/genrule.go`
(M2).

### `unsupported-custom-command-script` _(M2)_

Refinement of `unsupported-custom-command`: the command shells out to
`${CMAKE_COMMAND} -P some-script.cmake`. Translating this would
require a cmake interpreter at action time, which contradicts the
architecture (cmake at convert time only).

**Operator action:** rewrite the custom command in a real
language (Python/sh) so the recovery emits an honest `genrule`.

**Emission point:** `lower/genrule.go` — recognizer for `cmake -P`
in the parsed ninja command.

### `unsupported-execute-process`

A `CMakeLists.txt` calls `execute_process` at configure time and the
converter can't lift the call into a hermetic Bazel rule. cmake's
`execute_process` runs an arbitrary subprocess at configure time; the
liftability classifier (`converter/internal/lower/execute_process_classify.go`)
sorts each call into one of five buckets:

- `cmake-e` — invokes `cmake -E` with an op in the v1 supported set
  (copy / copy_if_different / touch). Translated to a build-time
  `genrule` with declared inputs and outputs; does not trigger this
  code.
- `file-producing` — declares an `OUTPUT_FILE`. Hoisted to a
  build-time `genrule` tagged `cmake-codegen-execute-process-hoisted`
  (the hoist moves work from configure-time to build-time, an
  auditable behaviour change). Does not trigger this code.
- `stamp` — looks like a version-stamp probe (git/hg/svn writing
  `OUTPUT_VARIABLE` without `OUTPUT_FILE`). Needs a
  `repository_ctx.execute()` analog; that infrastructure isn't built
  yet, so v1 refuses with this code.
- `probe` — looks like a host/toolchain probe (uname, gcc,
  pkg-config, python, etc. writing `OUTPUT_VARIABLE`). Should fold
  into `select()` over `@platforms` config_settings; v1 refuses.
- `unknown` — multi-COMMAND pipeline, opaque shell script, or any
  other unrecognized shape. v1 refuses.

The failure aggregates every unliftable call in the project into a
single Tier-1 emission so operators get one triage report listing
each `file:line`, bucket, reason, and argv.

**Operator action:** read the per-call list in `message`. For each
unliftable call, either rework the `CMakeLists.txt` (e.g. replace the
configure-time `execute_process` with `add_custom_command` so the
work runs at build time with declared inputs/outputs) or wait for the
Phase B round-2 cmake fallback (which routes the whole element
through a coarse `cmake configure + ninja + install` genrule, the
same shape kind:autotools round-2 uses for unconvertable autotools
projects).

**Emission point:** `lower.recoverExecuteProcess` —
`converter/internal/lower/execute_process.go`, called from
`lower.ToIR` after the trace decode.

### `unresolved-include` _(M2)_

A compileGroup include path resolves to neither the source root, the
prefix tree, nor any imports-manifest entry. Indicates a stray
host-leak or a missing dependency.

**Operator action:** add the providing element to the imports manifest;
if it's a legitimate host header, surface it through the toolchain.

**Emission point:** `lower.lowerTarget` (M2). Currently declared but
not emitted; M1's broad pass-through accepts everything.

### `bazel-canonicalize-failed`

The emitter assembled BUILD.bazel bytes that buildtools' parser
rejected (typically a syntax error at a specific line/column),
either because the templates regressed or because an upstream
lift smuggled an unescaped cmake value into a Starlark slot.
The `message` field carries the buildtools diagnostic and the
offending body so operators can isolate the offending line.

Before #210 this surfaced as an uncaught Go panic (Tier-2 exit
65 with no `failure.json`). The structured code lets the
orchestrator dedupe and lets operators see the failure shape
in the same shape as every other Tier-1 emission.

**Operator action:** read the snippet in `message`; if it's a
specific value the converter spliced in (cmake variable,
generator-expression value, source path), open an issue with
the value. Re-running with the issue's repro lets the
maintainer add a targeted escape / quoting fix.

**Emission point:** `cmd/convert-element-cmake/main.go` —
wrapping `bazel.EmitWithOptions`'s canonicalize error.

### `unresolved-link-dep` _(M2)_

A target's link library can't be resolved to either an in-element
target or an imports-manifest entry. Most common cause: missing
`find_package` dependency declaration upstream.

**Operator action:** add the providing element to the imports
manifest, or stub it like `non_cmake_stubs/glibc/`.

**Emission point:** `lower.lowerTarget` link-deps walk (M2).

### `missing-trace-data`

The converter was invoked with `--strict-trace` but no cmake
trace data was found (no `trace.jsonl` next to the build dir, or
the file is empty). Without trace events the converter can't
populate the PRIVATE/PUBLIC include partition, the
configure_file lift, IMPORTED-target dep recovery for static
libs, the platform-conditional source partition, and a few
other recovery passes that depend on trace-event inspection.

**Operator action:** re-run cmake with
`--trace-expand --trace-format=json-v1 --trace-redirect=<build>/trace.jsonl`,
or drop `--strict-trace` to accept the degraded recovery shape
(the converter still produces a structurally valid BUILD, just
with less precise lift). `--source-root` mode captures trace data
automatically; `--reply-dir` / `--cmake-build-dir` mode requires
the trace file to exist on disk.

**Emission point:** `cmd/convert-element-cmake/main.go` —
gated on `--strict-trace`.

### `dep-failed` _(M3a; refined in M3d)_

A transitive cmake dep of this element failed Tier-1; the
orchestrator short-circuited the dependent's conversion to surface
the root cause. The `message` field names the failing dep so
operators can jump straight to it. The dependent's converter is not
invoked; no shadow tree, no AC lookup happens.

Without this short-circuit, dependents would proceed against an
empty synth-prefix and produce a less helpful `configure-failed`
("package <X> not found"), masking the actual broken element.

**Operator action:** fix the named dep. Re-runs of the dependent
auto-recover once the dep succeeds (its action-key fingerprint
changes when the upstream output changes).

**Emission point:** `runner.processElement` early-out (M3a).

### `meson-setup-failed` _(kind:meson)_

`meson setup` returned non-zero, or its `meson-info/intro-*.json`
artifacts couldn't be parsed. The Tier-1 `message` field carries
the wrapped Go error (typically the `*exec.ExitError` plus a
short context string); meson's own stdout/stderr is streamed
through to the converter's stderr at run time, NOT captured into
`failure.json`. Operators iterating on a failure read the meson
output from the converter's stderr stream (or the orchestrator's
per-element log).

**Operator action:** investigate the meson failure as you would
in a normal meson invocation, reading the streamed stderr. If
the cause is a missing dependency (`dependency('foo', required:
true)`), expose it via the imports manifest or pre-stage it on
the executor.

**Emission point:** `cmd/convert-element-meson` `runMesonSetup`
return path + introspection load.

### `unsupported-meson-target-type` _(kind:meson)_

`meson introspect --targets` reported a target whose `type`
isn't one of `static library` / `shared library` / `both libraries`
/ `executable` / `custom` / `run`. v1 covers the cc-shaped types +
custom_target's lift-friendly subset; `jar` and unknown values
refuse here.

**Operator action:** if the offending target is necessary,
upgrade the converter to recognise it (and add a fixture). If
it's incidental, condition the meson.build to skip declaring
the target in this build (e.g. via an `if` guard the converter's
configure pass evaluates to false). v1 lowers every introspected
target regardless of `build_by_default`, so flipping that flag
alone won't suppress the refusal — a future converter version
could honor it; track in ROADMAP if it becomes a real operator
ergonomics issue.

**Emission point:** `cmd/convert-element-meson` `lower.Lower`
type-switch default arm.

### `unsupported-meson-subproject` _(kind:meson)_

A target's `subproject` field is non-null, or one of its sources'
absolute paths lies outside the `--source-root` prefix. v1 doesn't
recurse into meson subprojects (`subprojects/foo`); each subproject
should be its own kind:meson element.

**Operator action:** split the meson subproject into its own
element and add it to the imports manifest.

**Emission point:** `cmd/convert-element-meson` `lower.Lower`
subproject check + `relativizeSources`.

### `unsupported-meson-custom-target` _(kind:meson)_

A `custom_target()` whose shape v1 can't safely lift to a
`genrule`: zero or multi-entry `target_sources`, empty command,
opaque `@CURRENT_BUILD_DIR@` / `@SOURCE_ROOT@` / `@DEPFILE@` /
indexed `@INPUT0@` substitutions, embedded `@INPUT@` / `@OUTPUT@`
fragments (e.g. `--in=@INPUT@`), or a command that doesn't
reference `@OUTPUT@` (would render a genrule that ignores its
declared outputs).

**Operator action:** narrow the custom_target to the canonical
`command @INPUT@ -o @OUTPUT@` shape, or pre-generate the file
under sources and treat it as static.

**Emission point:** `cmd/convert-element-meson` `lower.lowerCustom`
+ `renderCustomCmd`.

### `unsupported-meson-generated-sources` _(kind:meson)_

A target's `target_sources` references `generated_sources`
(custom_target outputs wired into a target's `srcs` via
`files()` / generator outputs). v1 doesn't yet wire those into
the cc rule's srcs; the producer would need an in-Bazel genrule
the target's cc rule then depends on.

**Operator action:** consume the generator output as a static
file, or wait for the v2 generated-sources lift.

**Emission point:** `cmd/convert-element-meson`
`lower.fillSourcesAndFlags`.

### `unsupported-meson-cross-compile` _(kind:meson)_

A target's compile group has `machine: build` (cross-compile
build-machine inputs), which v1 doesn't model. The meson
intro-targets schema admits both `host` and `build` machines;
mapping a `build` group requires a separate Bazel toolchain
selection that v1 doesn't yet plumb.

**Operator action:** disable the cross-compile path for v1, or
pre-stage the build-machine artifacts.

**Emission point:** `cmd/convert-element-meson`
`lower.fillSourcesAndFlags`.

### `unresolved-meson-dependency` _(kind:meson)_

An external `dependency('foo')` resolved by meson but unbound
on the Bazel side: not present in the imports manifest (under
either `foo` or `foo::foo`) and lacking inline `compile_args` /
`link_args` in `intro-dependencies.json`.

**Operator action:** add the providing element to the imports
manifest (mirrors the kind:cmake convention), or pre-stage the
library + headers on the executor.

**Emission point:** `cmd/convert-element-meson` `lower.fillDeps`.

### `pyproject-parse-failed` _(kind:pyproject)_

`pyproject.toml` couldn't be read or contains malformed TOML
that go-toml's strict decoder rejected. The wrapped error
includes the file path and the parser's diagnostic.

**Operator action:** validate the TOML and fix the syntax,
then re-render. Python 3.11+ ships `tomllib` in the stdlib:
`python3 -c "import tomllib; tomllib.loads(open('pyproject.toml').read())"`.
On older Python, install `tomli` (a pure-Python backport of
the same API) and use it instead:
`pip install tomli && python3 -c "import tomli; tomli.loads(open('pyproject.toml').read())"`.
Any other TOML validator works too.

**Emission point:** `converter/cmd/convert-element-pyproject`
`parse.Load`.

### `unsupported-pyproject-backend` _(kind:pyproject)_

`[build-system].build-backend` is missing or names a backend
v1 doesn't model. v1 supports `flit_core.buildapi`,
`hatchling.build`, `setuptools.build_meta`, and
`poetry.core.masonry.api`.

**Operator action:** if the offending element ships a v1-
supported backend but doesn't declare it explicitly, add a
`[build-system]` block. Otherwise wait for the queued Phase B
install-plan fallback (per-element write-a-time dispatch that
routes refused elements through the pipeline shape) or re-render
without `--convert-element-pyproject` so the kind defaults to
the pipeline shape for every element.

**Emission point:** `converter/cmd/convert-element-pyproject`
`backends.Discover`.

### `unsupported-pyproject-package-discovery` _(kind:pyproject)_

A recognized backend whose discovery shape v1 can't statically
resolve — most commonly setuptools without an explicit
`[tool.setuptools].packages` config (setuptools' implicit
auto-discovery is version-dependent and we don't run it),
hatchling with VCS-tracked auto-discovery, or poetry without
`[tool.poetry].packages`.

**Operator action:** add an explicit `packages` list to the
relevant `[tool.<backend>]` section, or accept the pipeline
fallback.

**Emission point:** `converter/cmd/convert-element-pyproject`
`backends.discover<Backend>`.

### `unsupported-pyproject-c-extension` _(kind:pyproject)_

The source tree contains C / C++ / Cython / Rust files
(`*.c`, `*.cc`, `*.cpp`, `*.cxx`, `*.pyx`, `*.pxd`, `*.pxi`,
`*.rs`, `Cargo.toml`). v1 only converts pure-Python packages;
extension support requires rules_python's C-extension
toolchain or the queued Phase B install-plan fallback.

**Operator action:** wait for the Phase B follow-up, or
manually carve the extension out via the pipeline-shape
fallback.

**Emission point:** `converter/cmd/convert-element-pyproject`
`lower.refuseCExtensions`.

### `unsupported-pyproject-dynamic-metadata` _(kind:pyproject)_

`[project] dynamic = [...]` lists a load-bearing field
(`version`, `dependencies`, `scripts`, `gui-scripts`,
`entry-points`) that the build-backend would resolve at build
time (setuptools_scm, hatch-vcs, etc.). v1 doesn't run the
backend, so the resolved value isn't visible. Doc-only
dynamic fields (`readme`, `description`) are accepted.

**Operator action:** pin the dynamic field statically in
pyproject.toml (e.g. replace `dynamic = ["version"]` with an
explicit `version = "X.Y.Z"`), or accept the pipeline
fallback.

**Emission point:** `converter/cmd/convert-element-pyproject`
`lower.refuseDynamicMetadata`.

### `unresolved-pyproject-dependency` _(kind:pyproject)_

A `[project.dependencies]` entry (or a `[project.scripts]`
entry pointing at an external module) that isn't covered by
this element's own packages and isn't bound by the imports
manifest under either the bare distribution name or the
`<name>::<name>` convention bind. Distribution names are
PEP 503-normalized (lowercase, `[-_.]+ → -`) before lookup.

**Operator action:** add the providing element to the imports
manifest, or pre-stage the dependency on the executor.

**Emission point:** `converter/cmd/convert-element-pyproject`
`lower.resolveDeps` + `lower.lowerScripts`.

### `unsupported-pyproject-entry-point` _(kind:pyproject)_

A `[project.scripts]` / `[project.gui-scripts]` entry whose
value couldn't be parsed as PEP 621 `module:func` (missing
`:`, empty module/func, identifier that isn't a valid
Python identifier — each dotted component must match
`[A-Za-z_][A-Za-z0-9_]*` and there must be at least one
component, so digits-leading or empty components refuse;
strict-subset enforcement prevents arbitrary shell syntax
leaking into the generated entry-shim genrule's `cmd`),
or a name declared in both `[project.scripts]` and
`[project.gui-scripts]` (which would emit two `py_binary`
rules with the same target name).

**Operator action:** fix the entry-point spec in
pyproject.toml to a valid `module:func` form using only
Python identifier characters (letters, digits, underscore;
dots separating module components), or remove the duplicate
key from one of the two scripts tables. Once the queued
Phase B per-element fallback (ROADMAP) is shipped, refused
elements will route to the pipeline-shape coarse install
genrule automatically.

**Emission point:** `converter/cmd/convert-element-pyproject`
`lower.parseEntryPoint` + `lower.mergeScripts`.

## Stability rules

1. **Append-only.** Once a code is in this list, it stays. New
   conditions get new codes.
2. **Message format is freely editable.** The `code` is the dedup
   key; `message` is for humans.
3. **Codes are kebab-case lowercase.** ASCII letters, digits, hyphens.
4. **`context` keys, when added, follow the same append-only rule.**
   Removing a context key is a breaking change.
5. **A code marked _(M2)_ above is not yet emitted but is reserved.**
   Adding emission later is non-breaking.
