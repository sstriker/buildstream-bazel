# Narrowing-undercoverage audit

How to find out whether your per-element narrowing patterns are
**sound** — i.e., whether the cache-key surface of each element's
conversion action covers everything that actually matters at
action time.

This is a recipe for catching the silent-cache-hit class of
bugs: an edit to a file changes the conversion output, but the
file's content isn't in the action's cache key, so Bazel reuses
a stale `BUILD.bazel.out`. The narrowing patterns determine
which file contents make it into the cache key; the audit
described here reads two **read oracles** (one per kind family)
and reports paths the oracle says were read but the patterns
leave name-only.

## TL;DR

```sh
# 1. Render. write-a writes srckey-patterns.txt per element.
write-a --in <bst>/ --out-a <A>/ ...

# 2. Capture an oracle.
#    (a) cmake elements: convert-element-cmake emits the oracle as a
#    sibling JSON file when --out-cmake-configure-reads is set.
convert-element-cmake \
    --source-root <element-source-tree> \
    --out-build BUILD.bazel \
    --out-cmake-configure-reads cmake-reads.json

#    (b) trace-driven elements: build-tracer captures openat reads
#    when --source-root is set; the canonical trace.log is the
#    oracle.
build-tracer --source-root=<element-source-tree> \
             --out=trace.log \
             -- ./configure && make && make install

# 3. Audit. The undercomplete report lists paths the oracle says
#    were read but the patterns don't cover.
audit-narrowing \
    --patterns=<A>/elements/<elem>/srckey-patterns.txt \
    --cmake-reads=cmake-reads.json \
    --trace=trace.log \
    --out=undercomplete.txt

# Empty undercomplete.txt = clean. Non-empty = drift; investigate
# each path to decide whether to add it to the patterns or accept
# it as expected (allowlist mechanism is a follow-up; today the
# raw report is the surface).
```

## What "undercoverage" means precisely

For an element with source tree `S`, narrowing patterns `P`, and
read oracle `O ⊆ S` (the set of paths the oracle says were read
at action time), the **soundness invariant** is:

> ∀ p ∈ O, `P.Match(p) == true`

Violations are **undercoverage drift**: editing `p` would change
the action's output but Bazel won't re-run the action because
`p`'s content isn't keyed.

## The two oracles

### cmake oracle (build.ninja's `RERUN_CMAKE`)

CMake itself volunteers a list. In every generated `build.ninja`
there's a build edge:

```
build build.ninja: RERUN_CMAKE | <implicit inputs>
```

The implicit-input list is the set of files cmake's reconfigure-
detection machinery thinks should re-trigger configure. We project
this onto source-relative paths (dropping cmake-stdlib `/usr/share/
cmake-*` and build-tree artifacts like `CMakeCache.txt`).

Plumbing:

- `converter/internal/ninja.Graph.ReconfigureInputs()` — pulls the
  raw list out of the parsed graph.
- `ninja.ProjectToSourceTree(inputs, sourceRoot, buildDir)` — filters
  to in-source paths; sorts + dedupes.
- `convert-element-cmake --out-cmake-configure-reads <path>` — writes the
  projected list as a JSON array.

### trace-driven oracle (build-tracer's openat capture)

For autotools / make / makemaker / modulebuild / manual / script,
the equivalent of cmake's `RERUN_CMAKE` deps is "what files did
`./configure && make && make install` actually read." Build-tracer's
default trace mode captures **execve** events only (it's an
exec-set, not a read-set). The `--source-root` flag opts the
tracer into capturing **openat** events too, filtered to paths
inside the source tree.

Plumbing:

- `build-tracer --source-root=<src-root>` — both backends (native
  ptrace amd64 and the strace fallback) capture successful read-
  mode openat events. Path filtering + fd-stripping happen at
  canonicalize time so the trace.log byte schema stays stable
  across runs.
- `tracenorm.Canonicalize` (with `Options{SourceRoot: ...}`) —
  filters openat lines to source-relative paths and strips the
  volatile `= <fd>` return value.
- `tracenorm.ExtractReads(canonicalTrace)` — parses canonicalized
  openat lines back into a sorted source-relative read set.
- `trace-publish --source-root=...` — applies the same filter
  defensively before computing the AC digest, so cross-machine
  publishers stay byte-stable.

When `--source-root` is empty, openat events drop entirely —
preserves the legacy AC byte schema for elements not opted into
the oracle. Per-build opt-in via write-a's `--trace-source-root`
flag flips the round-2 install genrule's build-tracer invocation
to pass `--source-root="$$BUILD_ROOT"`; flipping the flag for
a build invalidates that build's previously-published AC entries
for trace-driven kinds (one-shot rebake). CI / e2e fixtures opt
in to populate the trace oracle for the audit gate; production
deployments stay on the legacy byte schema until they choose to
rebake.

## The audit tool

`cmd/audit-narrowing` is the consumer. Surface:

```
audit-narrowing \
    --patterns=<read-paths.txt-format file>   # required
    [--cmake-reads=<JSON array>]              # one or both
    [--trace=<canonicalized trace.log>]
    --out=<report path>                       # required
```

Behaviour:

- Loads patterns via `internal/readpaths.Parse`.
- Builds the union of oracle paths from cmake-reads + trace
  (extracted via `tracenorm.ExtractReads`).
- For each oracle path `p`: runs `patterns.Match(p)`. If false,
  record.
- Writes the sorted miss list to `--out` (one path per line).

Empty file = clean. Non-empty = drift. Exit status is always 0;
the report is the signal. CI gates that want hard-fail-on-drift
can `[ ! -s undercomplete.txt ]` and fail when it isn't empty.

## How write-a feeds the audit

`write-a` writes `srckey-patterns.txt` next to each element's
existing srckey artifacts. The file is in the same syntax as a
user-supplied `<element>.read-paths.txt`:

```
include configure.ac
include **/*.am
include **/*.h
exclude **/internal/**
```

Per kind:

- **cmake**: composed defaults (`cmakeHandler.DefaultReadPathsPatterns()`,
  currently nil) + per-element `<element>.read-paths.txt`, with an
  explicit `include CMakeLists.txt` rule prepended so cmake's
  always-real special case shows up as a pattern. Audit's plain
  `Match()` produces the same coverage answer
  `applyReadPathsPatterns` produces in the staging path.
- **autotools / make / makemaker / modulebuild**: the kind's
  `<kind>SrckeyPatterns()` (autoconf entry points + `*.am` /
  `*.in` / `*.m4` / `*.h` family).
- **manual / script**: `manualScriptSrckeyPatterns()` (today: nil
  → conservative no-narrow default; per-element overrides
  recommended).

For trace-driven kinds the file ships in project A alongside
`srckey.txt` and `srckey-breakdown.txt`. For cmake kind it
ships in project A's per-element directory.

## Limitations

Both oracles are themselves incomplete. Cases each can miss:

- **cmake / RERUN_CMAKE**: `file(GLOB)` without `CONFIGURE_DEPENDS`,
  per-config `find_package` paths, `try_compile` branches we didn't
  take. Also single-configuration: a probe gated on `CMAKE_BUILD_TYPE`
  surfaces only the path actually taken.
- **trace / openat**: relative paths drop (we don't carry per-call
  cwd context). Sub-process file reads via `/proc/self/...` shims
  bypass openat. Build-tracer needs `--source-root` to capture at
  all — without it the oracle is empty.

A non-empty undercoverage report is **necessary-but-not-sufficient**
evidence of drift; an empty report is **necessary-but-not-sufficient**
evidence of soundness. Treat the audit as a high-signal lower
bound on the narrowing-soundness question, not as a proof.

## `*.h.in` and the configure_file lift

By default, the cmake configure-file fixture trips a true-positive
miss:

```
$ audit-narrowing --patterns=... --cmake-reads=... --out=u.txt
$ cat u.txt
src/config.h.in
```

Convert-element's `lower/configure_file.go`
reads cmake's already-rendered `config.h` bytes from the build dir
and base64-embeds them into a recovered genrule's `cmd`. The
genrule has no `srcs`, so the genrule's Bazel cache key depends on
its `cmd` string, which depends on the `*.h.in` content. The patterns
must include `*.h.in` for the conversion action to be sound.

The fix shipped: the **configure_file lift**
(`internal/configurefile` package + `cmd/cmake-configure-file`
tool + `lower/configure_file.go` lifted-shape emission). When
write-a is invoked with `--cmake-configure-file-bin=<path>`:

  - The tool is staged into project A and project B `tools/`.
  - Per-element convert-element-cmake genrules pass
    `--lift-configure-file=true`, which makes lower emit a
    genrule with `srcs = ["<.h.in>"]` and
    `tools = ["//tools:cmake-configure-file"]`. The .h.in
    content drives the genrule's Bazel cache key directly
    (via `srcs`), not via convert-element-cmake's BUILD.bazel content.
  - `*.h.in` is now safely name-only for srckey purposes.

After enabling the lift, the oracle will still flag `*.h.in`
(cmake DOES read those files at configure time — that's a
fact-of-life). To silence the audit's false-positive in this
case, add an explicit exclude to the per-element pattern set:

```
# srckey-patterns.txt:
include CMakeLists.txt
include cmake/*.cmake
exclude **/*.h.in     # safe: the lift makes .h.in Bazel-srcs covered
```

The audit's `Match` semantics: `exclude` rules suppress the
include match. So an oracle path that matches an explicit
`exclude` is treated as covered (the user has affirmatively
declared it doesn't need to be in srckey content). Because
the lift makes that affirmation correct, the exclude is
sound rather than aspirational.

### Soundness across template edits

The "safe to exclude" claim relies on the lifted genrule's
substitution tool having access to whatever cmake variable
the template references — even ones the user adds in a
later edit without rerunning convert-element-cmake. With the
**full-namespace values dump** (see
`converter/internal/cmakerun/dump-vars.cmake`) every cmake
variable observed at configure time is captured into the
values JSON, so a `.h.in` edit that adds `@SOMETHING_NEW@`
resolves correctly through the Bazel-time tool as long as
`SOMETHING_NEW` was defined in cmake's namespace at the
captured configure run.

The volatile path-bearing variables (`*_BINARY_DIR`,
`CMAKE_PROJECT_TOP_LEVEL_INCLUDES`, etc.) are filtered out
of the dump (see `cmakerun.filterVolatilePaths`) so the
values JSON is byte-stable across cmake reinvocations of
the same source tree; the Bazel-time tool's verify-pass
catches the rare case where a template references a filtered
variable, and the converter falls back to the legacy
base64-cmd shape — soundness preserved.

### When the lift falls back to legacy

Some `.h.in` calls don't lift, even with the flag enabled:

- Offline/`--reply-dir` test paths where no cmake invocation
  ran, so no values dump exists. The per-template
  `configurefile.Extract` path takes over — recovers values
  from the rendered output. Sound for the templates the
  fixture captured against, but adds no protection if the
  template later mutates.
- Live runs where the verify-pass `Substitute(template, values,
  opts) != rendered` (an option Substitute hasn't modeled, or
  a value the dump's volatility filter dropped that the
  template references). The converter emits the legacy
  base64-cmd shape; `.h.in` stays load-bearing in srckey.

The render-time tag `cmake-codegen-lifted` distinguishes the
two shapes — operators can `bazel query
'attr("tags", "cmake-codegen-lifted", //...)'` to see which
templates are lifted. Don't add `exclude **/*.h.in` for
elements with non-lifted templates; the audit will continue
to flag those as undercoverage drift, correctly.

## `execute_process` and subprocess reads

`execute_process` is the dual of `configure_file` for narrowing
purposes:

- `configure_file(<.h.in> <.h>)` — cmake itself opens `.h.in`,
  parses it, substitutes variables, writes `.h`. The cmake oracle
  records `.h.in` in `RERUN_CMAKE`. The configure_file lift makes
  `.h.in` Bazel-srcs covered, so the audit's now-true-positive
  flag becomes a false positive — operators silence with an
  explicit `exclude **/*.h.in` (see previous section).
- `execute_process(COMMAND tool a b OUTPUT_FILE <out>)` — cmake
  forks/execs `tool` with `[a, b]` in argv. The subprocess opens
  `a` and `b` (or any other files); cmake itself does not. The
  cmake oracle (`RERUN_CMAKE`'s implicit-input list) tracks
  cmake-process file opens, not subprocess opens, so it does
  **not** flag the subprocess inputs.

Consequence: the converter's `execute_process` lift
(`converter/internal/lower/execute_process.go`,
`cmake-codegen-execute-process` tag set) needs no analog of
`exclude **/*.h.in`. Subprocess inputs are absent from the
oracle, so the audit is silent for them by default. The lift
adds them as Bazel `srcs` of the recovered genrule, which is
the correct place for runtime invalidation; convert-element-cmake's
own srckey doesn't need to include them because their content
does not affect the BUILD.bazel that convert-element-cmake emits.

Empirical check (`execute-process-cmake-e/` and
`execute-process-file-producing/` fixtures):

```
$ /tmp/convert-element-cmake --reply-dir=... --source-root=... \
    --out-build=BUILD.bazel \
    --out-cmake-configure-reads=cmake-reads.json
$ cat cmake-reads.json
["CMakeLists.txt"]
$ echo "include CMakeLists.txt" > srckey-patterns.txt
$ audit-narrowing --patterns=srckey-patterns.txt \
    --cmake-reads=cmake-reads.json --out=undercomplete.txt
$ wc -c < undercomplete.txt
0
```

Patterns that *do* require srckey coverage even with the
execute_process lift:

- cmake-side reads upstream of the call:
  `file(READ "manifest.txt" CONTENT)` followed by
  `execute_process(... ${CONTENT} ...)`. cmake reads
  `manifest.txt` → `RERUN_CMAKE` flags it → BUILD.bazel cmd
  embeds the resolved value → manifest.txt edits change
  BUILD.bazel → must be in srckey content. The audit catches
  this; the lift doesn't change the requirement.
- An in-tree generator that subsequently `include()`s a `.cmake`
  file. Same shape: the included `.cmake` is cmake-tracked, so
  RERUN_CMAKE flags it, and the audit demands srckey coverage.

In short: the lift only makes subprocess-read files Bazel-srcs
covered; the cmake-tracked reads continue to be the
audit's responsibility, exactly as before.

## Files of interest

- `cmd/audit-narrowing/main.go` — the audit CLI.
- `cmd/audit-narrowing/main_test.go` — exercises both oracles +
  clean / dirty / mixed cases.
- `internal/readpaths/patterns.go` — shared pattern matcher (`Rule`,
  `Patterns`, `Parse`, `Format`, `Match`).
- `converter/internal/ninja/configure_reads.go` — cmake-side
  `ProjectToSourceTree`.
- `converter/internal/ninja/graph.go` — `ReconfigureInputs()`.
- `converter/cmd/convert-element-cmake/main.go` — `--out-cmake-configure-reads`
  emission + `--lift-configure-file` toggle.
- `internal/tracenorm/canonicalize.go` — Options.SourceRoot,
  openat filtering, fd-strip.
- `internal/tracenorm/reads.go` — `ExtractReads`.
- `internal/configurefile/substitute.go` — cmake configure_file
  substitution rules implemented as a pure function.
- `internal/configurefile/extract.go` — reverse-extract values
  from cmake's rendered output.
- `cmd/cmake-configure-file/main.go` — Bazel-time substitution
  CLI; the tool the lifted genrule invokes.
- `converter/internal/lower/configure_file.go` — picks lifted vs
  legacy genrule shape per recovered call.
- `cmd/build-tracer/main.go` — `--source-root` flag, strace
  fallback opens.
- `cmd/build-tracer/native_linux_amd64.go` — native ptrace openat
  handler.
- `cmd/trace-publish/main.go` — defensive re-canonicalize with
  matching `--source-root`.
- `cmd/write-a/srckey.go` — emits `srckey-patterns.txt` for
  trace-driven kinds.
- `cmd/write-a/handler_cmake.go` — emits `srckey-patterns.txt`
  for kind:cmake; passes `--lift-configure-file=true` when
  `--cmake-configure-file-bin` is set.
- `cmd/write-a/read_paths_patterns.go` — `writeNarrowingPatterns`,
  `withCMakeListsRule`.
- `cmd/write-a/expected_drift.go` — `loadExpectedDrift`; reads
  `<elem>.expected-drift.txt` next to `<elem>.read-paths.txt`.
- `internal/readpaths/allowlist.go` — `Allowlist` type + parser
  + `Format` serializer shared between write-a and the audit.
- `scripts/audit-narrowing-walk.sh` — per-element walker the
  CI gate drives.
- `scripts/meta-audit-narrowing.sh` — CI gate's outer script:
  render meta-project + populate cmake oracle + run the walker.

## The allowlist (`srckey-expected-drift.txt`)

Per-element file declaring paths the audit may legitimately
report. Lives at `<elem>.expected-drift.txt` next to
`<elem>.read-paths.txt` in the source tree; write-a stages it
as `srckey-expected-drift.txt` in project A's per-element
directory next to `srckey-patterns.txt`. Format:

```
# Templates the configure_file lift refused (no values dump or
# Substitute hasn't modeled an option).
src/legacy/foo.h.in
include/bar.h.in
```

- One source-relative path per line; no glob grammar. Each
  entry is a deliberate per-path declaration that survives PR
  review (a glob could mask unrelated drift the operator
  didn't intend to silence).
- Comments: a line whose first non-whitespace character is `#`
  is a full-line comment. Inline comments are recognized only
  in the specific ` #` form (space-then-hash) — text from
  there to end-of-line is dropped. A `#` without a preceding
  space stays part of the path, so `weird#dir/foo.h` parses
  as the literal path. Blank / whitespace-only lines are
  ignored. Whitespace INSIDE a path is rejected (the audit's
  reports are slash-separated and whitespace-free, so an
  internally-spaced entry would never match).
- The file's syntax is identical to `audit-narrowing`'s output
  format: `cat audit-report.txt >> <elem>.expected-drift.txt`
  is a valid (manually-reviewed) silencing flow.

Inverse-tag audit query for spotting which entries should be
removed once a future lift covers them: `bazel query 'attr(
"tags", "cmake-codegen-lifted", //elements/<elem>:all)'` lists
the templates whose lifts succeeded. Any `.h.in` on the
allowlist whose corresponding genrule now carries
`cmake-codegen-lifted` is a stale entry safe to delete.

`cmd/audit-narrowing --allowlist=<path>` consumes the file;
entries in the allowlist are subtracted from the miss list
before the report is written. Missing file (`--allowlist=`
pointing at a typo'd path) fails fast — silently behaving as
"no allowlist" would noise the gate.

## The CI gate

`make e2e-audit-narrowing` drives the full chain end-to-end:

1. Render a meta-project (today: `hello-world.bst`) with
   write-a → project A.
2. For each kind:cmake element, invoke `convert-element-cmake` offline
   against the element's source tree with
   `--out-cmake-configure-reads=cmake-reads.json`, writing the
   oracle next to `srckey-patterns.txt` in project A.
3. Run `scripts/audit-narrowing-walk.sh` against project A's
   `elements/` directory. The walker discovers each element's
   patterns + (optionally) `srckey-expected-drift.txt`, locates
   the cmake / trace oracle, runs `cmd/audit-narrowing`, and
   accumulates per-element drift into a combined report.

Layered exit-status contract:

- `cmd/audit-narrowing` and `scripts/audit-narrowing-walk.sh`
  both exit 0 regardless of drift — they're policy-agnostic
  primitives that follow the "report is the signal, not the
  exit status" pattern.
- `scripts/meta-audit-narrowing.sh` (which `make
  e2e-audit-narrowing` calls) IS the policy layer: it exits
  non-zero when the combined report is non-empty, so the
  make invocation fails like any other check target.
- The CI step that calls `make e2e-audit-narrowing` uses
  `continue-on-error: true` to keep the job green while
  signal accumulates. Promotion to blocking is a real
  one-line YAML change — flip `continue-on-error` to false
  once the representative fixture set's allowlists have
  stabilized; the script's non-zero exit on drift will fail
  the step naturally.

The trace-side oracle requires `--trace-source-root` on the
write-a invocation that produced the meta-project; without it,
build-tracer drops openat events and the trace oracle is
empty. The CI gate today exercises only the cmake oracle for
kind:cmake — a trace-driven sibling gate (build-tracer + the
trace.log capture) is queued behind a build-tracer-on-CI
fixture landing.
