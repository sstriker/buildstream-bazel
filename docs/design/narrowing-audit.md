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
#    (a) cmake elements: convert-element emits the oracle as a
#    sibling JSON file when --out-cmake-configure-reads is set.
convert-element \
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
- `convert-element --out-cmake-configure-reads <path>` — writes the
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
the oracle. Per-element opt-in (passing `--source-root` into the
round-2 install genrule's build-tracer / trace-publish
invocations) is on the roadmap; today the trace oracle is opt-in
per binary invocation, not per element.

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

## Known true-positive: `*.h.in`

The cmake configure-file fixture trips an obvious miss:

```
$ audit-narrowing --patterns=... --cmake-reads=... --out=u.txt
$ cat u.txt
src/config.h.in
```

This isn't a false positive. Convert-element's `lower/configure_file.go`
reads cmake's already-rendered `config.h` bytes from the build dir
and base64-embeds them into a recovered genrule's `cmd`. The
genrule has no `srcs`, so the genrule's Bazel cache key depends on
its `cmd` string, which depends on the `*.h.in` content. The patterns
must include `*.h.in` for the conversion action to be sound.

The fix is the **configure_file lift** queued in `ROADMAP.md`:
emit the substitution as actual codegen (a small `cmake-configure-
file` tool + a values sidecar), wire `*.h.in` as a real `srcs`
input, and `*.h.in` becomes name-only for srckey purposes. Until
then the audit will keep flagging it; that's the warning working
as intended.

## Files of interest

- `cmd/audit-narrowing/main.go` — the audit CLI.
- `cmd/audit-narrowing/main_test.go` — exercises both oracles +
  clean / dirty / mixed cases.
- `internal/readpaths/patterns.go` — shared pattern matcher (`Rule`,
  `Patterns`, `Parse`, `Format`, `Match`).
- `converter/internal/ninja/configure_reads.go` — cmake-side
  `ProjectToSourceTree`.
- `converter/internal/ninja/graph.go` — `ReconfigureInputs()`.
- `converter/cmd/convert-element/main.go` — `--out-cmake-configure-reads`
  emission.
- `internal/tracenorm/canonicalize.go` — Options.SourceRoot,
  openat filtering, fd-strip.
- `internal/tracenorm/reads.go` — `ExtractReads`.
- `cmd/build-tracer/main.go` — `--source-root` flag, strace
  fallback opens.
- `cmd/build-tracer/native_linux_amd64.go` — native ptrace openat
  handler.
- `cmd/trace-publish/main.go` — defensive re-canonicalize with
  matching `--source-root`.
- `cmd/write-a/srckey.go` — emits `srckey-patterns.txt` for
  trace-driven kinds.
- `cmd/write-a/handler_cmake.go` — emits `srckey-patterns.txt`
  for kind:cmake.
- `cmd/write-a/read_paths_patterns.go` — `writeNarrowingPatterns`,
  `withCMakeListsRule`.
