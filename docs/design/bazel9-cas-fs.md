# Bazel 9 CAS-aware filesystem (xattr fast-path replacement)

The point of the FUSE-served sources route (`cmd/cas-fuse`,
`docs/sources-design.md`) is that **a developer's machine never
holds the full source set**. Bytes stay in CAS, served lazily,
and the executor reads them via REAPI. Dev-machine resource
budgets (disk, RAM) stay modest regardless of how large the
source graph gets; the heavy lifting moves to the remote-execution
services.

Bazel 7/8 closed the loop without any dev-side byte traffic for
digesting via:

```
--unix_digest_hash_attribute_name=user.bazel.cas.digest
--digest_function=SHA256
```

`internal/casfuse` serves exactly that xattr key on every file
node. With the flag set, Bazel reads the xattr and skips the
read-and-hash step entirely.

**Bazel 9 dropped the flag pair without a direct replacement.**
On Bazel 9 the FUSE mount still resolves correctly, but Bazel
reads each input file through FUSE and hashes the bytes itself.
The functional outcome (executor-side BwoB, no source-tree
materialisation on dev disk) is preserved; the
"don't-even-read-bytes-locally" optimisation is not.

This doc analyses the replacement options and commits to a
direction.

## What got lost, concretely

For each input file an action depends on, Bazel's local digest
phase:

| Bazel 7/8 with the flag | Bazel 9 (today) |
|---|---|
| Open file. | Open file. |
| Read `user.bazel.cas.digest` xattr. | (xattr ignored.) |
| Use xattr value as the digest. | Read full file bytes via FUSE → CAS round-trip. |
| Skip body read entirely. | Hash bytes locally. |

Cost on a `bazel build //...` against a graph of N files of
total size S:

- **Bazel 7/8 with flag**: O(N) xattr reads (no body bytes).
- **Bazel 9**: O(S) bytes pulled through FUSE → page cache, plus
  O(S) hash CPU. Bazel's File Digest Cache (FDC) caches
  `(path, mtime, size) → digest` so repeated builds without
  source changes don't re-hash, but the **first build** of any
  fresh checkout — and any build after a daemon restart that
  wipes FDC — pays the full cost.

The "first build" hit is what the user-visible BwoB story
depends on: a developer cloning a fresh tree should not spend N
× per-file network round-trips just to compute action keys.

## Options surveyed

### A. `http_archive` + `repository_cache` (input-side, Bazel-native)

Rework `rules/sources.bzl` so each source file flows through
Bazel's `http_file` / `http_archive` mechanism with a known
sha256. When sha256 is given, Bazel **trusts it** for action
keying without reading the file's bytes.

Implementation outline:

1. write-a emits a per-source flat manifest:
   `[{path: "configure", sha256: "abc...", url: "..."}, ...]`.
2. `_src_repo` is rewritten to declare one `http_file` per file
   (or one `http_archive` per source if we tarball-pack on
   the fly). The sha256 is the bytes' CAS digest.
3. `--repository_cache=<dir>` points at a directory we
   pre-populate with content-addressable symlinks pointing into
   the cas-fuse mount: `<repo_cache>/content_addressable/sha256/<X>/file`
   → `<fuse-mount>/blobs/sha256/<X>`.
4. Bazel's repo phase finds each file in the cache by sha256,
   doesn't re-fetch, doesn't re-hash, stages it into the action
   input tree.

**Pros**:

- 100% Bazel-native primitives. No patching, no plugin SPI, no
  Java.
- The sha256-trust path is the standard BwoB shape that
  everyone using `http_archive(sha256=…)` already gets.
- Reversible: if it underperforms, we can fall back to FUSE +
  hash-local.

**Cons**:

- Rework of `rules/sources.bzl` + write-a's source-emitter is
  non-trivial.
- `http_file` materialises bytes into `<repository_cache>` —
  even if the file is just a symlink chain into the FUSE mount,
  Bazel may resolve it on the way in.
- Per-file `http_file` declarations explode the external repo
  count (one per file, not per source). May hit Bazel's external
  repo scaling limits at FDSDK scale.

### B. `RemoteOutputService` via bb_clientd

Bazel 9 keeps the `--experimental_remote_output_service=` flag,
which points Bazel at a gRPC server that materialises inputs
and outputs lazily and tells Bazel which digests they have.
Bazel trusts those digests. The canonical implementation is
`bb_clientd`, buildbarn's client-side daemon, which serves a
FUSE/NFS mount and speaks the `RemoteOutputService` protocol.

**`bb_clientd` is a companion daemon paired with Bazel 9, in
the same way `bazelisk` pairs with `bazel`.** Adopting it is
**not** an adoption of "the buildbarn ecosystem" or a
commitment to running buildbarn end-to-end. Plenty of projects
run `bb_clientd` alongside non-buildbarn executors (EngFlow,
BuildBuddy, NativeLink, …) — the daemon talks plain REAPI to
whatever CAS endpoint you point it at, and plain
`RemoteOutputService` to Bazel.

bb_clientd is itself a buildbarn project and **builds with
Bazel** (not the Go toolchain). Pre-built binaries are
published on every push to main at
`github.com/buildbarn/bb-clientd/releases`; that's the install
path for the dev loop. Source builds via `bazel run
//cmd/bb_clientd` work too. `go install` does not — the repo's
go.mod has `replace` directives for `rules_go`'s sake that make
the Go toolchain reject the build but that Bazel honours
correctly.

Implementation outline:

1. Add `bb_clientd` to `deploy/buildbarn/`'s service set,
   configured against the existing CAS endpoint. The daemon
   runs locally on each dev machine (and on each CI runner) —
   not centrally, not via docker-compose.
2. Pass `--experimental_remote_output_service=unix:///path/to/bb_clientd.sock`
   to bazel invocations. Bazel uses the daemon's mount as both
   input and output namespace.
3. Update `rules/sources.bzl` so the repo rule resolves source
   trees via the daemon's mount path. The current
   `ctx.symlink(<dir-digest>)` shape carries over essentially
   unchanged once the path template absorbs bb_clientd's
   canonical layout (see below).
4. Decide what to do with our own `cmd/cas-fuse` /
   `internal/casfuse` once bb_clientd is the production path.
   Likely outcomes:
     - Keep `internal/casfuse` for the in-process FUSE the
       Go test suite uses (it doesn't need a separate
       daemon process).
     - Mark `cmd/cas-fuse` deprecated; remove once the
       hello-fuse e2e gate runs against bb_clientd.

**Pros**:

- Bazel-9-native: this *is* Bazel 9's intended replacement
  for the dropped xattr fast-path. No flag drift, no plugin
  SPI risk.
- Covers both inputs and outputs in one daemon. Output-side
  BwoB (lazy materialisation of build artifacts) lands as
  a free side effect of solving the input-side problem.
- Pre-built: `bb_clientd` is a stable, tested binary
  shipping with buildbarn releases. Adopting it is a
  configuration exercise, not a code-authoring one.
- Doesn't touch `rules/sources.bzl`'s structural shape.
  The repo rule still ctx.symlinks into a digest-addressed
  mount; only the daemon serving the mount changes.

**Cons**:

- New external dependency: bb_clientd binary + jsonnet config.
  Mitigated by treating it as a Bazel companion (like
  bazelisk), not a platform commitment.
- Operational learning curve: jsonnet config is more involved
  than `cmd/cas-fuse`'s flag-only invocation. Mitigated by
  shipping a working config under `deploy/buildbarn/` so users
  start from a known-good template.
- `internal/casfuse`'s xattr-set machinery becomes purely a
  test-infrastructure concern (Bazel reaches the bytes via
  bb_clientd, not directly).

### C. BlazeModule plugin (Java SPI)

Write a Bazel Java plugin that intercepts the file-digest
function and consults a side-channel digest source (FUSE xattr,
or a query against our REAPI CAS).

**Pros**:

- Restores the exact xattr semantics Bazel 7/8 had.
- Doesn't touch the source-staging architecture.

**Cons**:

- Java-on-Bazel-internals work. Bazel's plugin SPI shifts
  release-to-release; maintenance burden.
- Not clear the plugin SPI in Bazel 9 actually exposes the file
  hashing path. Risk of dead-end after substantial investment.
- We'd own a Bazel plugin alongside the rest of the stack —
  another binary, another release cadence.

### D. Accept the cost

Run on Bazel 9 as-is. Rely on Bazel's File Digest Cache to
amortize re-hashes across builds.

**Pros**:

- Zero infra work.

**Cons**:

- First-build cost on a fresh checkout is full O(S). For
  FDSDK-scale source graphs this is minutes of network +
  CPU.
- Daemon restarts (CI runners, IDE refreshes, Bazel server
  pid-restarts) wipe FDC and re-pay the cost.
- Doesn't deliver what the project's BwoB pitch promises.

## Decision: B (`RemoteOutputService` via bb_clientd)

Reasoning:

- It's Bazel 9's intended replacement for the dropped xattr
  fast-path. Picking anything else is picking a workaround;
  picking B is picking the supported path.
- bb_clientd is a Bazel-9 companion daemon, **not** an
  adoption of buildbarn end-to-end. The same daemon talks
  REAPI to non-buildbarn CAS endpoints (EngFlow, BuildBuddy,
  NativeLink, our own `make buildbarn-up` stack) and plain
  `RemoteOutputService` to Bazel. Adopting it doesn't lock the
  project into a particular executor.
- Output-side BwoB (lazy materialisation of build artifacts)
  comes along free.
- The `rules/sources.bzl` shape stays — just the daemon
  serving the mount changes from `cmd/cas-fuse` to
  `bb_clientd`, and the path template absorbs bb_clientd's
  canonical layout.
- Option A remains available as a **fallback** if bb_clientd's
  operational footprint turns out badly for some workflow we
  care about. The repository_cache route stays usable as an
  escape hatch.

## bb_clientd mount layout

The daemon serves the following well-known layout under its
configured `mount_path`:

```
<mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/
                                              /file/<digest>
                                              /executable/<digest>
                                              /tree/<digest>/
                                              /command/<digest>
<mount>/outputs/<workspace>/   — Bazel per-build output paths
<mount>/scratch/               — writable scratch namespace
```

`rules/sources.bzl` will need to learn this prefix shape (it
historically expected `<mount>/blobs/directory/<digest>/` —
the simpler `cmd/cas-fuse` layout). The integration step is
either parameterising the repo rule's path template (so
`cas/<instance>/blobs/<digest_function>/` prefixes are
configurable) or switching the rule outright to bb_clientd's
shape and retiring `cmd/cas-fuse` from the dev path.

## Where the wiring lives

Per `CLAUDE.md`, this doc describes how the system works; shipped-vs-queued
state lives in `ROADMAP.md`. Pointers to the in-tree pieces:

- Daemon lifecycle: `make bb-clientd-up` / `make bb-clientd-down`,
  config at `deploy/buildbarn/config/bb_clientd.jsonnet`.
- Path-template parameterisation lives in
  `cmd/write-a/sources_bzl.go` and `cmd/write-a/traces_bzl.go`.
  Both rendered .bzl files build the symlink target as
  `<CAS_FUSE_MOUNT>/<CAS_DIRECTORY_PREFIX>/directory/<digest>`.
  Default prefix is `blobs` (the flat layout `cmd/cas-fuse`
  serves). bb_clientd users pass
  `--repo_env=CAS_DIRECTORY_PREFIX=cas/<instance>/blobs/<digest_function>`
  to land on the bb_clientd canonical layout (with the daemon's
  default empty instance + sha256 digest function, that
  collapses to `cas//blobs/sha256`).
- Local end-to-end exercise: `tools/e2e-hello-bbclientd.sh`
  (also `make e2e-hello-bbclientd`). Brings up buildbarn +
  bb_clientd, runs `cmd/source-push` to upload, then drives
  `bazel build` with
  `--experimental_remote_output_service=unix://<grpc_sock>`
  and the parameterised `CAS_DIRECTORY_PREFIX`. Skips cleanly
  when bb_clientd / Bazel ≥ 9 aren't on PATH; not yet wired
  into the GitHub Actions CI workflow because the runners
  don't ship bb_clientd by default. The CI job named
  `bazel9-fuse-sources` (in `.github/workflows/ci.yml`) runs
  the in-process Go test `TestBazel9_FuseSourcesEndToEnd` from
  `internal/casfuse/`, which exercises Bazel 9 against the
  `cmd/cas-fuse` mount path — different code path from the
  bb_clientd gate above.
- `cmd/cas-fuse` stays in-tree as the flag-only fallback for
  setups that don't want a bb_clientd dependency (air-gapped
  CI runners, the in-process casfuse / hello-fuse tests).
