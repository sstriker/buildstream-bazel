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

**Emission point:** `cmd/convert-element/main.go` — wrapping
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

### `unresolved-link-dep` _(M2)_

A target's link library can't be resolved to either an in-element
target or an imports-manifest entry. Most common cause: missing
`find_package` dependency declaration upstream.

**Operator action:** add the providing element to the imports
manifest, or stub it like `non_cmake_stubs/glibc/`.

**Emission point:** `lower.lowerTarget` link-deps walk (M2).

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
