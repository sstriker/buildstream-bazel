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
node (`fs_linux.go:227`). With the flag set, Bazel reads the
xattr and skips the read-and-hash step entirely.

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
   The manifest replaces the current Directory-digest-keyed
   `sources.json` form.
2. `_src_repo` is rewritten to declare one `http_file` per file
   (or one `http_archive` per source if we tarball-pack on
   the fly). The sha256 is the bytes' CAS digest.
3. `--repository_cache=<dir>` points at a directory we
   pre-populate with content-addressable symlinks pointing into
   the cas-fuse mount: `<repo_cache>/content_addressable/sha256/<X>/file`
   → `<fuse-mount>/blobs/sha256/<X>`. (cas-fuse needs a flat
   sha256-keyed view for this — small additive feature.)
4. Bazel's repo phase finds each file in the cache by sha256,
   doesn't re-fetch, doesn't re-hash, stages it into the action
   input tree.

**Pros**:

- 100% Bazel-native primitives. No patching, no plugin SPI, no
  Java.
- The sha256-trust path is the standard BwoB shape that everyone
  using `http_archive(sha256=…)` already gets.
- Reversible: if it underperforms, we can fall back to FUSE +
  hash-local.

**Cons**:

- Rework of `rules/sources.bzl` + write-a's source-emitter is
  non-trivial.
- `http_file` materialises bytes into `<repository_cache>` — even
  if the file is just a symlink chain into the FUSE mount, Bazel
  may resolve it on the way in. Need to verify Bazel 9's actual
  behaviour with symlinked repository_cache entries.
- Per-file `http_file` declarations explode the external repo
  count (one per file, not per source). May hit Bazel's external
  repo scaling limits at FDSDK scale.

### B. `RemoteOutputService` via bb_clientd

Bazel 9 keeps the `--remote_output_service=` flag, which points
Bazel at a gRPC server that materialises inputs and outputs
lazily and tells Bazel which digests they have. Bazel trusts
those digests. The canonical implementation is `bb_clientd`,
buildbarn's client-side daemon, which serves a FUSE/NFS mount
and speaks the `RemoteOutputService` protocol.

Critical framing: **`bb_clientd` is a companion daemon paired
with Bazel 9, in the same way `bazelisk` pairs with `bazel`.**
Adopting it is **not** an adoption of "the buildbarn ecosystem"
or a commitment to running buildbarn end-to-end. Plenty of
projects run `bb_clientd` alongside non-buildbarn executors
(EngFlow, BuildBuddy, NativeLink, …) — the daemon talks plain
REAPI to whatever CAS endpoint you point it at, and plain
`RemoteOutputService` to Bazel.

Implementation outline:

1. Add `bb_clientd` to `deploy/buildbarn/`'s service set,
   configured against the existing CAS endpoint. The daemon
   runs locally on each dev machine (and on each CI runner) —
   not centrally, not via docker-compose. A `make cas-fuse-up`
   replacement (e.g. `make bb-clientd-up`) drives the
   lifecycle.
2. Pass `--remote_output_service=unix:///run/bb_clientd/grpc`
   (or similar) to bazel invocations. Bazel uses the daemon's
   mount as both the input root and the output root.
3. Update `rules/sources.bzl` so `_src_repo` resolves source
   trees via the daemon's mount path — minor adjustment;
   the current `ctx.symlink(<dir-digest>)` shape carries over
   essentially unchanged once `CAS_FUSE_MOUNT` is renamed to
   the bb_clientd mount path.
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
  bb_clientd, not directly). Not a regression — it was always
  the bb_clientd-equivalent in our smaller, in-process form.

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
- The user explicitly asked for a replacement; "live with it"
  isn't the answer.

## Direction

**Pick B (`RemoteOutputService` via bb_clientd).** Reasoning:

- It's Bazel 9's intended replacement for the dropped xattr
  fast-path. Picking anything else is picking a workaround;
  picking B is picking the supported path.
- bb_clientd is a Bazel-9 companion daemon, **not** an adoption
  of buildbarn end-to-end. The same daemon happily talks REAPI
  to non-buildbarn CAS endpoints (EngFlow, BuildBuddy,
  NativeLink, our own `make buildbarn-up` stack) and plain
  `RemoteOutputService` to Bazel. Adopting it doesn't lock the
  project into a particular executor.
- Output-side BwoB (lazy materialisation of build artifacts)
  comes along free. We don't have to scope it separately.
- The `rules/sources.bzl` shape stays — just the daemon serving
  the mount changes from `cmd/cas-fuse` to `bb_clientd`.
- Option A remains usable as a **fallback** if bb_clientd's
  operational footprint turns out badly for some workflow we
  care about (e.g. a developer on a constrained host who can't
  run the daemon). The repository_cache route stays available
  as an escape hatch.

## Next concrete steps

In rough order, sized for one PR each:

1. **Stand up bb_clientd in `deploy/buildbarn/`.** Add a
   `bb_clientd.jsonnet` configured against the existing CAS
   endpoint, with a FUSE mount at a well-known dev path
   (e.g. `/var/cache/cmake-to-bazel/bb_clientd/`). Document
   the `bb_clientd` binary install (recommend grabbing
   pre-built releases from buildbarn-storage). Add a
   `make bb-clientd-up` lifecycle target alongside the
   existing `make buildbarn-up`.
2. **Wire `--remote_output_service` into hello-fuse e2e.** Drop
   the `cas-fuse` daemon from `tools/e2e-hello-fuse.sh`'s
   pipeline; bring up bb_clientd instead; pass
   `--remote_output_service=unix:///path/to/bb_clientd.sock`
   to the bazel invocation. The script's structural checks
   (sources.json shape, BUILD references) stay; the bytes
   layer changes underneath.
3. **`rules/sources.bzl` mount-path rename.** The repo-rule
   shape doesn't change — `ctx.symlink(<mount>/blobs/directory/<digest>)`
   still works against bb_clientd's mount. The
   `CAS_FUSE_MOUNT` env-var and `rules/sources.bzl` doc strings
   get renamed to reflect that bb_clientd is the production
   serving daemon (e.g. `BB_CLIENTD_MOUNT`); `cmd/cas-fuse`
   stays for the in-process Go test suite.
4. **Profile + measure.** Extend the hello-fuse gate (and
   eventually the FDSDK probe) to report digest-phase wall
   time before/after the bb_clientd switch. We need a number,
   not a vibe, on what the xattr replacement actually buys.
5. **Migrate or deprecate `cmd/cas-fuse`.** Once (1)–(4) prove
   bb_clientd is the production path, decide between deleting
   `cmd/cas-fuse` (relying on bb_clientd everywhere) or
   keeping it as a slim Go alternative for environments that
   can't run bb_clientd. `internal/casfuse` likely stays
   either way (it's the in-process FUSE the Go test suite
   exercises directly).

Each step is independently testable. (1) lands without (2);
(2) doesn't break the legacy `cas-fuse` path because the
RUN_BAZEL=1 leg is opt-in.

The Option-A breadcrumbs (flat sha256 view, repository_cache
prepopulator) are still valid follow-ups if a fallback path
ever needs implementing — the doc keeps them under "Options
surveyed" rather than rewriting them out.

## Empirical findings (2026-05-05 probe)

Before committing to the bb_clientd direction I ran it locally
to find out whether the design's premise actually holds. Honest
readout — including a misstep I corrected on review:

| Probe | Result |
|---|---|
| Bazel 9.0.0 binary download + run | ✅ |
| FUSE in this container (`/dev/fuse` + `fusermount3` after install) | ✅ |
| `internal/casfuse` mount + xattr-serve (`TestMount_RealMountReadFile`) | ✅ |
| `--experimental_remote_output_service` flag exists in Bazel 9 | ✅ (with `experimental_` prefix; fixed the typo I'd had in `tools/e2e-hello-bbclientd.sh`) |
| `go install` bb_clientd | ❌ — and **misleading**, see correction below |
| **Pre-built `bb_clientd.linux_amd64` from `github.com/buildbarn/bb-clientd/releases`** | ✅ — statically-linked Go binary, downloads in 2 seconds |
| **bb_clientd starts against `deploy/buildbarn/config/bb_clientd.jsonnet`** | ✅ — after iterating the jsonnet against the actual proto schema |
| **bb_clientd FUSE mount serves `cas/` + `outputs/` + `scratch/`** | ✅ — `ls $mount` returns the canonical bb_clientd layout |
| `internal/casfuse`'s `TestMount_MultiDigestRoot` | ❌ — pre-existing EIO bug on lazy multi-digest resolution; tracked separately |
| Bazel 9 + casfuse + bzlmod build (`TestBazel9_FuseSourcesEndToEnd`) | ⚠️ skipped — this sandbox can't reach `bcr.bazel.build` (HTTP 403); CI runs the test on hosts with BCR access |

### Correcting the "build broken" misstep

My initial probe tried `go install` and `go build` against the
bb-clientd repo and reported the build broken. **That was the
wrong tool.** Buildbarn projects (including bb-clientd) build
with **Bazel**, not the Go toolchain. The repo's go.mod has
`replace` directives that exist for `rules_go`'s sake; they
make `go install` fail but Bazel honours them correctly. The
repo's own CI builds successfully on every push and **publishes
prebuilt binaries to GitHub Releases** (`linux_amd64`,
`linux_amd64_v3`, `linux_386`, `linux_arm`, `linux_arm64`,
`darwin_amd64`, `darwin_arm64`, `freebsd_amd64`,
`windows_amd64`, plus `.deb`).

The pre-built binary is the right install path for our
purposes. Building from source remotely (with `bazel build
//cmd/bb_clientd`) works too if a developer wants to verify
custom changes — `CONTRIBUTING.md`'s install section lists
both.

### Mount layout: ours vs. bb_clientd's

The verified bb_clientd mount layout is:

```
<mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/
                                              /file/<digest>
                                              /executable/<digest>
                                              /tree/<digest>/
                                              /command/<digest>
<mount>/outputs/<workspace>/   — Bazel per-build output paths
<mount>/scratch/               — writable scratch namespace
```

`rules/sources.bzl` today expects
`<mount>/blobs/directory/<digest>/`, which matches our own
`cmd/cas-fuse`'s simpler shape but not bb_clientd's. Closing
the gap is one small follow-up — either parameterise the repo
rule's path template (so `cas/<instance>/blobs/<digest_function>/`
prefixes are configurable) or switch the rule to bb_clientd's
shape and retire `cmd/cas-fuse` from the dev path. The design
doc tracks the choice as the first actual integration step.

### What this means for the picked direction

bb_clientd is the right pick AND it's runnable today. The
remaining work is the Bazel-side wiring + the mount layout
adjustment, in roughly this order:

1. **`rules/sources.bzl` mount-path rename + layout match.**
   `CAS_FUSE_MOUNT` becomes `BB_CLIENTD_MOUNT` (or stays as a
   generic env var); the path template absorbs the
   `cas/<instance>/blobs/<digest_function>/` prefix.
2. **CI job runs `tools/e2e-hello-bbclientd.sh`** end-to-end
   on a runner with BCR access. Today's bazel9-fuse-sources
   CI job exercises the no-bb_clientd path; the bbclientd
   gate gets added once the rule-side adjustment is in.
3. **Migrate or deprecate `cmd/cas-fuse`** once bb_clientd is
   the production path. `internal/casfuse` likely stays
   either way — its in-process FUSE serves the test suite
   without needing a separate daemon.

## What this PR ships

- This doc (`docs/bazel9-cas-fs.md`).
- `tools/e2e-hello-fuse.sh` no longer passes the dropped flag
  pair.
- `ROADMAP.md`'s Bazel 9 CAS-FS bullet now points at this doc.
- `deploy/buildbarn/config/bb_clientd.jsonnet`. **Verified
  locally** — bb_clientd starts against this config and
  serves the FUSE mount with the canonical `cas/`, `outputs/`,
  `scratch/` namespaces.
- `make bb-clientd-up` / `bb-clientd-down` lifecycle targets,
  pointing at the public release URL for the bb_clientd binary
  rather than asking developers to build from source.
- `tools/e2e-hello-bbclientd.sh` parallel acceptance gate.
  Skips cleanly without a bb_clientd binary on PATH.
- `CONTRIBUTING.md` gains an install-requirements section for
  developers running the verification paths locally
  (bb_clientd, fuse3, ca-certificates-java, Bazel 9).
- `internal/casfuse/bazel9_e2e_test.go` —
  `TestBazel9_FuseSourcesEndToEnd`: actually runs Bazel 9
  against a casfuse-served source tree using the
  `rules/sources.bzl` shape. Skips cleanly when BCR or
  Bazel ≥ 9 isn't reachable; CI is wired to install both and
  run the test.
