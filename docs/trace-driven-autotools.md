# Trace-driven kind:autotools (B → A feedback)

## Why

`kind:cmake` consumes cmake's File API codemodel — a structured
description of what cmake *would* build. Autotools has no such
introspection surface; the conversion has to derive Bazel
targets from what `make` *actually does*.

The B→A feedback shape: the autotools build runs inside a
process tracer; the trace is the introspection signal we
otherwise lack.

> **Current state.** Round-2 is the default since it was shipped
> (see [`ROADMAP.md`](../ROADMAP.md) and
> [`docs/design/autotools-round2-rendezvous.md`](design/autotools-round2-rendezvous.md)).
> Pass `--autotools-round1` to `write-a` to opt into the legacy
> single-genrule shape. The round-1 shape is described in the
> section "Round-1 (legacy) shape" below.

## Round-2 (default): split converter + install genrules

Round-2 splits the work between the two workspaces:

- **Project A** (pass 2) holds a converter genrule (`<elem>_build`)
  that reads `trace.log` + `make-db.txt` from the REAPI ActionCache
  (via `@trace_<elem>//:trace`, resolved by `cmd/trace-lookup`) and
  emits `BUILD.bazel.out` with native cc rules. On an AC miss the
  converter emits a placeholder and project B's coarse genrule
  remains the buildable target.
- **Project B** (pass 3) holds a coarse install genrule
  (`<elem>_install`) that runs `./configure && make && make install`
  under `build-tracer`, post-processes `make-db`, and calls
  `cmd/trace-publish` to register the trace in the AC under
  `SyntheticActionDigest(srckey)`.

See [`docs/build-structure.md`](build-structure.md) for the
generated BUILD shapes and
[`docs/design/autotools-round2-rendezvous.md`](design/autotools-round2-rendezvous.md)
for the AC rendezvous protocol.

## Round-1 (legacy) shape: single-genrule

Activated by `--autotools-round1`. Each `kind:autotools` element
renders a single genrule that:

1. Stages the element's source tree into `$BUILD_ROOT`,
   extracts upstream autotools deps' `install_tree.tar`
   into a shared `$DEP_PREFIX`, exports `CPPFLAGS` /
   `LDFLAGS` pointing at it.
2. Wraps `./configure && make && make install` in
   `build-tracer`, which captures every successful
   `execve` under the build sandbox into a temp trace
   file (`$AUTOTOOLS_TRACE`).
3. Dumps the post-build Makefile state via `make -np` to
   `make-db.txt` (target-specific variables, prerequisite
   edges, fully variable-resolved after configure ran).
4. Runs `convert-element-autotools` against the trace +
   make-db, emitting `BUILD.bazel.out` (native `cc_library`
   / `cc_binary`) + `install-mapping.json` (sidecar
   mapping install destinations to producing rules).
5. Tars the install tree as `install_tree.tar` with
   determinism flags (`--mtime=@0 --sort=name --owner=0
   --group=0 --numeric-owner`).

```mermaid
sequenceDiagram
    participant WA as cmd/write-a
    participant Bazel as bazel build (project A)
    participant BT as build-tracer
    participant Build as ./configure + make + make install
    participant CEA as convert-element-autotools

    WA->>WA: Render install genrule with tracer + converter wired in
    Bazel->>BT: $(location //tools:build-tracer) --out=$AUTOTOOLS_TRACE -- sh -c '...'
    BT->>Build: fork + ptrace; capture every execve
    Build-->>BT: install tree at $INSTALL_ROOT
    BT-->>Bazel: $AUTOTOOLS_TRACE (mktemp; not a Bazel output)
    Bazel->>CEA: $(location //tools:convert-element-autotools) --trace=$AUTOTOOLS_TRACE
    CEA-->>Bazel: BUILD.bazel.out + install-mapping.json
    Bazel->>Bazel: tar install_tree.tar (deterministic flags)
    Note over Bazel: One action, four outputs:<br/>install_tree.tar + BUILD.bazel.out + make-db.txt + install-mapping.json<br/>cache key = full element source + tools + dep tars
```

**Caching**: one Bazel action covers the build AND the
conversion. Whenever the install action's input bytes
change (source edit, dep tar change, tool change), the whole
thing re-runs — including the converter. There is no
separate narrow cache key for the convert step today; see
"The narrow-cache experiment we tried" below for why we
attempted to split + reverted.

For an unchanged element with byte-identical inputs (same
source, same toolchain, same dep tars), Bazel's action cache
hits and the whole genrule is skipped. For a downstream
element whose dep's install tar is byte-identical (no
content change), the downstream's cache also hits. The
deterministic-tar flags are what make this transitivity
possible: an upstream rebuild that produces identical
content yields a byte-identical tar, so consumers don't
cascade-rebuild.

## What influences buildgraph stability

The converted `BUILD.bazel.out` is a function of the trace +
make-db. For the BUILD to stay stable across edits, the
trace + make-db structure must stay stable. Things that
**can** make the structure change (and therefore the
BUILD churn):

- **configure outputs** — `config.h`, generated Makefiles.
  Any autoconf check whose result changes will propagate
  into `HAVE_*` defines, which can change compile commands
  via `#ifdef` branches, which changes the trace.
- **Makefile rule structure** — different targets, new
  prerequisites, recipe substitutions. Anything that
  changes which `cc` / `ar` / `ld` invocations make
  produces.
- **Compile / link command lines** — explicit `-D`, `-I`,
  `-L`, `-l` args; per-target `CFLAGS` overrides;
  conditional source inclusion in `_SOURCES` lists.
- **Toolchain** — gcc version, host headers (autoconf
  feature detection results vary by glibc version).
- **System library availability** — `pkg-config` results,
  `AC_CHECK_LIB` outcomes.

Things that **don't** change the trace structure (these
are the cases where ideally the BUILD would stay stable
even though they change the source content):

- Comment edits / formatting in `.c` / `.cpp` / `.h`.
- Changes to function bodies that don't alter the set of
  symbols compiled / linked.
- Changes to dep `.h` / `.a` content (commands stay the
  same; only the resulting `.o` / binary changes).

Today these "irrelevant" edits still re-run the whole
single-genrule action because Bazel sees the source bytes
in `glob([...])` change. The "Roadmap" below describes
what would actually narrow this.

## The narrow-cache experiment we tried

We split the install genrule into `<elem>_install` (build,
full source input → `install_tree.tar` + `trace.log` +
`make-db.txt`) + `<elem>_converted` (convert, narrow input
= trace + make-db → `BUILD.bazel.out`). The intent was
the cmake-handler-style narrowing: a comment-only source
edit re-runs `_install` (because input bytes changed) but
hits `_converted` because trace + make-db come out
byte-identical.

The intent didn't survive contact with reality. Two clean
runs of the same build produce non-byte-stable
`trace.log` and `make-db.txt`:

- `trace.log`: pid prefix on every line (`9194  execve(...)`
  vs `9213  execve(...)`); cc1/as/collect2 internal temp
  paths (`/tmp/ccABC.s`, `/tmp/ccDEF.o`,
  `/tmp/ccGHI.res` for collect2's resolution file).
- `make-db.txt`: `# N files, M impossibilities` line that
  varies with `.deps/` state; `# Last modified <timestamp>`
  lines for every file.

Bazel hashes input bytes for the action cache key, so the
sibling `_converted` rule's cache **always** missed when
its `_install` dependency re-ran. The split delivered no
narrowing — the convert step ran in lockstep with the
build, exactly what splitting was meant to prevent.

Reverted. Single genrule restored. The split's
infrastructure (`SiblingRules` hook on `pipelineExtension`
+ the `<elem>_converted` codegen) was deleted to keep the
handler simple. The dep-wiring + deterministic-tar work
**stayed**: those are useful infrastructure independent
of the split decision.

The split is plausibly worthwhile when the determinism
prerequisites land. Re-introduce when (a) trace
normalization makes `trace.log` byte-stable and (b)
`make -np` post-processing makes `make-db.txt` byte-stable.

These prerequisites are now met — see the round-2 description
at the top of this doc.

## B → A feedback (round-2, shipped)

Round-2 realized the split with the normalization prerequisites
satisfied. The shipped design:

1. **Round 1** — write-a renders project A with a converter
   genrule that accepts trace input, and project B with the
   coarse install genrule. Bazel build of project B runs
   configure / make / make-install under `build-tracer`,
   post-processes `make-db`, and calls `trace-publish` to
   write an AC entry under `SyntheticActionDigest(srckey)`.
2. **Round 2** — on the next `bazel build` of project A, the
   `@trace_<elem>//:trace` filegroup resolves to the AC hit.
   The converter genrule reads the trace and emits fine-grained
   cc rules. **Cache miss** → same coarse fallback as round 1.

See [`docs/design/autotools-round2-rendezvous.md`](design/autotools-round2-rendezvous.md)
for the AC rendezvous protocol.

## Re-conversion thrash

Round-2's chicken-and-egg shape (B publishes the trace; A
re-runs to consume it; B then rebuilds against the new
fine-grained rules) means a graph-affecting source edit costs
**two passes (four `bazel build` invocations) to settle**:

1. `bazel build` of project A — converter genrule's
   `@trace_<elem>//:trace` resolves to the OLD AC entry (or
   misses on a fresh srckey). Project A renders the coarse
   fallback / placeholder BUILD.bazel.
2. `bazel build` of project B — coarse install genrule
   runs `configure && make && make install`, publishes the new
   trace under `SyntheticActionDigest(srckey)`.
3. Next `bazel build` of project A — converter sees the
   new trace, emits fine-grained `cc_library` / `cc_binary`
   rules. BUILD.bazel changes shape.
4. Next `bazel build` of project B — sees a different
   action graph; recompiles every TU.

The reason a single `bazel build` can't fold this into one
pass: bazel resolves external repos at invocation start, so
the trace AC entry B publishes mid-build can't appear in A's
load phase until the next bazel invocation. Two passes is
the minimum.

### What insulates non-graph edits

The cycle above only fires when an edit changes the
content-narrowed `srckey`. Per-kind narrowing patterns
(`autotoolsSrckeyPatterns()`, `makeSrckeyPatterns()`, …)
exclude `.c` / `.cpp` / `.S` content from srckey — those
files contribute by **path** but not by **content**. So:

- **Comment-only edit in `foo.c`** → srckey unchanged →
  trace AC stays valid → A cache-hits → no thrash. B reruns
  because source bytes changed, but B's outputs are
  byte-stable for content-equivalent input (per
  `tracenorm` + `make -np` post-processing) so B's
  consumers cache-hit too.
- **Adding `bar.c`** → srckey changes → full 4-step thrash.
- **Editing `Makefile.in` / `configure.ac` / `*.m4` /
  `config.h.in`** → srckey changes → full 4-step thrash.
- **Editing a header in the source tree** → srckey changes
  (headers stay content-included for autotools because
  preprocessor switches in headers can change which
  compile commands the Makefile emits).

### Settling the thrash automatically

There's no good "previous run" metric to surface here:
independent CI nodes and developer machines share state
only through the content-addressed CAS / AC, not through
sidecar files. Trying to compute a re-conversion delta
locally would tell each node a different story.

The pragmatic alternative is to just **always run the
settle loop**. Invoke `bazel build` for project A and
project B back-to-back twice:

```text
bazel build <projectA targets>     # may render coarse fallback
bazel build <projectB targets>     # publishes new traces
bazel build <projectA targets>     # consumes new traces, fine-grained
bazel build <projectB targets>     # picks up new BUILD.bazel.out
```

This works because Bazel's persistent daemons + content-
addressed action cache make a redundant invocation cheap
when nothing changed: the second A+B pair is a no-op for
elements whose trace AC was already warm. Only elements
whose srckey moved this build pay the second pair's cost,
and only for the actions actually affected.

`scripts/build-with-settle.sh` wraps the four invocations
behind a single shell command. CI workflows and dev-loop
make targets that build kind:autotools / kind:make round-2
elements should call it instead of `bazel build` directly.
The script is intentionally dumb — no BEP parsing, no
conditional skipping — because the overhead of the extra
invocations is bounded by Bazel's daemon and dwarfed by
the configure/make work the first pair already paid.

A smarter loop (parse BEP for `trace-publish` action
events, only re-invoke when an AC entry actually moved)
would shave the steady-state case to one A+B pair plus a
no-op A. Worth doing once we have data showing the dumb
loop's overhead is non-trivial; not worth doing
speculatively.

### When round-2 isn't worth it

The round-2 default is good for elements where the
fine-grained rules give Bazel real parallelism /
incrementality wins on day one — large autotools projects
where `make` already serializes more than it needs to and
Bazel can interleave compiles across deps.

It is **not** worth it when:

- The element rarely changes (a stable
  bottom-of-the-stack lib like libffi, zlib): the steady-
  state cache-hit rate is already near 100% with the
  coarse install genrule, and the round-2 thrash on the
  occasional graph edit pays a higher cost per edit than
  the coarse rebuild it replaced.
- The element is a one-off conversion target: the
  developer wants the converter to run once, capture
  `BUILD.bazel.out`, commit it, and reclassify the .bst
  as `kind:bazel`. The round-2 register/refresh cycle
  is wasted machinery in this workflow.

For both cases, the recommended workflow today is:

- **Don't pass `--convert-element-autotools` by default.**
  kind:autotools elements then render as the unmodified
  coarse install_tree.tar pipeline; project B's downstream
  cache stays warm across edits and the trace AC isn't
  involved.
- **Migration to kind:bazel**: pass
  `--convert-element-autotools` + `--build-tracer-bin`
  + `--trace-publish-bin` + `--trace-lookup-bin` for the
  one-off conversion build. Capture
  `bazel build //elements/<name>:<name>_build`'s
  `BUILD.bazel.out` artifact, copy it into the .bst's
  source tree, change the .bst's `kind` to `bazel` (the
  passthrough handler), commit. Subsequent builds bypass
  the converter entirely.

A per-element opt-in (turning round-2 on for `glib`
specifically while keeping `zlib` coarse) isn't wired
today — `autotoolsConfig.round2Enabled` is global.
Adding it is a small refactor (per-element
`Bst.Round2: bool` field driven from a project.conf
knob); the settle loop above keeps the global default
livable in the meantime, so the per-element refinement
can wait for a workload that demonstrably needs it.

## Open items (future work)

### Direct-dep narrowing in `convert-element-autotools`

Today the converter resolves every `-l<name>` link flag
against the in-trace archive set + the imports manifest.
Future enhancement: filter the emitted `cc_library` /
`cc_binary` `deps` to just those that match a declared
direct dep of the element (from the .bst's `depends` /
`build-depends`). Trust Bazel to propagate transitive
`cc_library` deps. Caveats:

- System libs (`-lc`, `-lpthread`, `-lm`,
  `-ldl`) come from `cc_toolchain`, not declared deps.
  Filter must spare those (or move them to a separate
  toolchain-libs allowlist).
- Doesn't apply to `kind:cmake`. Cmake's File API
  gives explicit per-target dep info — the cmake
  converter already builds the right dep set.
  Direct-dep narrowing is a fix for kinds that infer
  deps from execve flags (autotools, make).

### pkgconfig synthesis

Today the project synthesizes cmake-config files
(`<DepPkg>Config.cmake`) for each kind:cmake dep so
consumers' `find_package(... CONFIG)` resolves at
genrule action time. The pkg-config analog: synthesize
`.pc` files for each cc_library export so consumers
using `pkg-config --cflags --libs <name>` resolve at
genrule action time.

Shape:

- New writer in `synthprefix` (or a sibling package)
  emits `<lib>.pc` text for each exported cc_library:
  `Name`, `Description`, `Version`, `Cflags`,
  `Libs`, `Requires` (transitive deps via their .pc
  names).
- Per-element BUILD produces a `pkgconfig-bundle.tar`
  filegroup (analog to `cmake_config_bundle`).
- Consumers' install genrules extract dep
  `pkgconfig-bundle.tar`s into
  `$DEP_PREFIX/usr/lib/pkgconfig` and export
  `PKG_CONFIG_PATH` so `pkg-config` resolves.

Open questions:

- Which `.pc` fields to populate from BUILD metadata
  (Bazel's cc_library doesn't carry `Description` or
  `Version`).
- How to handle `Requires.private` (transitive deps
  used only at link time).
- Whether to emit `.pc` for autotools-converted
  cc_library exports too (so cross-kind consumers
  see consistent pkg-config metadata).

## Reducing trace overhead

The current `cmd/build-tracer` is ptrace-based (native
linux/amd64 + strace shim fallback elsewhere). Two known
issues:

- **Slow**: ptrace stops every traced process at every
  syscall enter/exit. For build systems with many short-
  lived processes (configure churns through hundreds of
  small probes; make spawns one process per recipe step),
  the per-syscall stop/resume cost adds up. Folklore
  estimates 2-10× slowdown vs untraced baseline; we
  haven't measured ours specifically.
- **amd64-only**: native backend depends on amd64
  register layout + syscall numbers. Other arches fall
  back to the strace shim, adding a host-strace
  dependency.

Alternatives to evaluate:

| Approach | Overhead | Privilege | Limitations |
|---|---|---|---|
| ptrace (current) | 2-10× | none | slow, amd64-only native |
| `LD_PRELOAD` exec hook | low | none | static binaries bypass; Bazel sandbox env propagation needs verification; macOS uses `DYLD_INSERT_LIBRARIES`; misses `execveat` syscall (separate from `execve` since glibc 2.34) |
| eBPF (`execsnoop`-style) | very low | typically root / `CAP_BPF` | Bazel sandboxes typically don't grant the cap; tracepoint-based variants are arch-agnostic; legacy kprobe variants are arch-specific (just like our ptrace today) |
| Linux audit subsystem | low (kernel-level) | root | output format awkward to parse |

We don't capture file I/O today — only execve arg lists.
If a future use case wants input/output attribution
(beyond what the converter derives from `-c` / `-o`
args), `LD_PRELOAD` could hook `openat` too — but the
"file access ≠ dep truth" caveat is real and severe:
compilers probe many headers they don't depend on
(speculative include-path searches, system headers).
mmap-based I/O bypasses the hook entirely.

For the stable-trace property the 2-phase design needs,
the tracer choice is independent of normalization: any
tracer's output gets canonicalized in post-processing.
Switching to `LD_PRELOAD` is mainly a performance win,
not a correctness one.

## Critical-path / perfetto

Bazel's `--profile=profile.gz` covers cross-action
parallelism and waterfall analysis at the workspace
level. What it can't show is **within a single genrule
action** — e.g., which compile / link is on the critical
path inside the autotools install genrule, where the
build is parallelism-bottlenecked.

A future enhancement: `cmd/build-tracer --emit=perfetto`
writes timestamped events in
[Perfetto's JSON trace format](https://perfetto.dev/docs/instrumentation/track-events).
Open in [ui.perfetto.dev](https://ui.perfetto.dev) for a
timeline view of every cc/ar/ld invocation in the
genrule, per-process, with parent/child relationships.
Useful for:

- Diagnosing slow autotools builds (long-tail cc1
  invocations, configure probes that dominate).
- Tuning `make -j` parallelism (if a sequential
  bottleneck dominates, more cores don't help).
- Comparing tracer overhead across implementations
  (ptrace vs LD_PRELOAD timeline overlay).

Independent of the 2-phase design — purely a debugging
add-on. Output mode flag.

## Wishlist: RBE service-managed tracing

The DIY tracer wrapper is fine for v1 but adds:
- A privileged process tracer (`strace` / `bwrap` / a
  small ptrace Go binary) inside the action sandbox.
- An action-output side channel (`trace.log` alongside
  `install_tree.tar`).
- Trust that the tracer's output is deterministic across
  re-runs of the same action.

If buildbarn / buildgrid / EngFlow / similar RBE services
expose process tracing as a server-side feature, we'd:
- Drop the in-action tracer wrapper.
- Receive the trace as a standardized RBE side channel
  (an `Action.tracing_uri`-shaped field on the response
  or similar).
- Inherit the RBE service's determinism / sandbox
  semantics rather than implementing them ourselves.

This is parked as a wishlist item for the RBE community —
trace export from the executor would benefit anyone doing
build introspection of opaque tools, not just our
converter.

## Component status

What works today (`cmd/convert-element-autotools/`):

- **Parser**: strace text-format input (`-f -e
  trace=execve -s 4096 -o <path>`); recognizes top-level
  compiler driver invocations (cc / gcc / g++ / clang)
  and filters out gcc-internal `cc1` / `as` / `collect2`
  / `ld` sub-process noise.
- **Emitter**: cross-event correlation
  (compile-only `cc -c x.c -o x.o` paired with archive
  `ar rcs libfoo.a x.o` paired with link `cc -o app
  app.c -lfoo`) drives `cc_library` (per archive) +
  `cc_binary` (per link). Single-step compile-and-link
  invocations become `cc_binary`.
- **Object correlation**: keys by exact `.o` path with
  basename fallback — distinguishes libtool's
  `.libs/foo.o` (PIC) from `foo.o` (non-PIC) so the
  right archive picks up the right compile event.
- **Dep resolution**: `-l<name>` resolves to `:<name>`
  for in-trace archives or to a cross-element Bazel
  label via the imports manifest. Default-toolchain
  flags (`-O2`, `-fPIC`, `-g`, `-DNDEBUG`) stripped;
  `-D<name>=<val>` routes to the rule's `defines`.
- **make-db hint**: per-target CFLAGS preservation —
  trace + make-db cross-reference detects
  `hotloop.o: CFLAGS += -O2`-style overrides and keeps
  the per-target intent in `copts` even when the global
  CFLAGS would be stripped as default-toolchain.
- **Install mapping**: `install-mapping.json` sidecar
  captures install destinations (bin/, lib/, include/,
  share/) with rule cross-references; consumed by
  Phase-4 typed-filegroup work.

What's wired in `cmd/write-a` (`autotoolsHandler`): when
`--convert-element-autotools` + `--build-tracer-bin` are
set, renders the install genrule with the tracer wrap +
converter step inline. Emits `imports.json` next to the
BUILD when there are cross-element deps. Pipes upstream
autotools deps' install tars into a shared `$DEP_PREFIX`
with `CPPFLAGS` / `LDFLAGS` exports. Without those
flags, falls back to the unmodified coarse
install_tree.tar pipeline.

End-to-end gates (all skip the bazel build phase
gracefully when bazel ≥7 / strace aren't on PATH):

- `make e2e-meta-autotools-native` —
  autotools-greet / single cc_binary recovery.
- `make e2e-meta-autotools-multitarget` —
  autotools-multitarget / multiple cc_library +
  cc_binary + per-target CFLAGS + install layout.
- `make e2e-meta-autotools-tu-optflags` — per-target
  `hotloop.o: CFLAGS += -O2` preservation.
- `make e2e-meta-autotools-libtool-pic` — libtool
  dual-compile (`.libs/foo.o` PIC + `foo.o` non-PIC)
  resolution via exact-path correlation.

The 2-element `autotools-transit` fixture
(`testdata/meta-project/autotools-transit/` — `mathlib`
+ `calc` build-depending on it) is in the repo for
future multi-element gating; no shell driver yet.

## Other parked items

In rough priority order:

1. **Per-target CFLAGS cross-validation**. Detect when the
   trace-recorded copts diverge from what the Makefile
   declared (e.g., environmental override broke a
   per-target flag). Surface diffs as comments in
   BUILD.bazel.out — audit-grade. Needs a fixture where
   the divergence happens; the autotools-multitarget
   fixture's helper.o uses recipe-level `-Wall` instead
   of target-specific vars, so this gap surfaces only
   when a real-world fixture exercises it.
2. **Makefile target-name authority**. When the Makefile
   target name differs from the trace's `-o` argument
   basename, prefer the Makefile's. Mainly for shared libs
   with versioned filenames (`libfoo.so.0.1.0`) where
   Bazel's natural rule name differs from the on-disk
   output. No current fixture exercises this — defer until
   a real-world shared-versioned target surfaces the need.
3. **Phony-target recipe parsing beyond `install:`**.
   `check:` recipes describe test invocations (Bazel
   cc_test); custom phony targets (`docs:`, `dist:`)
   might surface other typed slices. The current parser
   only walks `install:`.
4. **Native ptrace beyond linux/amd64**. The native
   ptrace backend is amd64-only today (register layout,
   syscall number, calling convention are arch-specific).
   Adding aarch64 / armv7 / ppc64le requires roughly 50
   lines of arch-specific code per arch
   (`native_linux_<arch>.go` with the right register
   struct field accesses + syscall numbers). Other
   GOOS/GOARCH combos fall back to the strace shim
   transparently.

## libtool coverage

Autotools projects often drive compile / link through
[GNU libtool](https://www.gnu.org/software/libtool/), which
wraps `cc` / `ar` calls to add platform-specific shared-lib
plumbing. A libtool-driven recipe looks like:

```
libtool --mode=compile cc -c foo.c -o foo.lo
libtool --mode=link cc foo.lo -o libfoo.la
```

Under the hood, libtool invokes `cc` and `ar` in two passes
(PIC + non-PIC compile; static archive + shared object). The
trace captures those underlying invocations, so the existing
correlation pipeline handles libtool builds without
libtool-specific code:

- Underlying `cc -c -fPIC ... -o .libs/foo.o` and
  `cc -c ... -o foo.o` both surface as compile events.
  The correlator keys by exact `.o` path (with basename
  fallback), so `.libs/foo.o` and `foo.o` resolve to
  their own compile events and the static + PIC archives
  pick up their own copts/defines.
- `ar rcs .libs/libfoo.a foo.o` surfaces as an archive
  event. `stripLibPrefixSuffix` recovers `foo` as the
  cc_library rule name regardless of the `.libs/` prefix.
- libtool-specific text wrappers (`.lo`, `.la`) aren't
  recognized by our `.o` / `.a` extension predicates and
  get silently skipped — which is correct: they're
  metadata files, not real build artifacts.

Bottom line: **libtool builds work today via the existing
trace pipeline**; richer libtool semantics (`.la` metadata
for cross-element dep resolution) deferred until a fixture
surfaces the gap.

## Standard autotools project as test bed

The hand-rolled `testdata/meta-project/autotools-multitarget/`
fixture exercises the full surface today (multiple cc_library
+ cc_binary outputs, multiple install dests, per-target
CFLAGS). A separate "real-world standard autotools project"
test bed (libpng-static / GNU hello / a coreutils slice) is a
nice-to-have for confidence at scale; deferred until the
spike itself surfaces a gap the multitarget fixture can't
cover.
