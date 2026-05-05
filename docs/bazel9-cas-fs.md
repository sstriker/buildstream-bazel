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
Bazel at a gRPC server (canonically `bb_clientd`) that
materialises **outputs** lazily. bb_clientd also serves a Bazel
**input** mount and tells Bazel about it via the same protocol's
filesystem hooks; Bazel then trusts the digests bb_clientd
reports for both inputs and outputs.

Implementation outline:

1. Add `bb_clientd` to `deploy/buildbarn/`'s service set,
   configured against the existing CAS endpoint.
2. Replace `cmd/cas-fuse` with `bb_clientd` (or run them
   side-by-side during transition).
3. Pass `--remote_output_service=unix:///path/to/bb_clientd.sock`
   to bazel invocations.

**Pros**:

- Maintained by the same team that maintains the executor stack
  we already depend on (buildbarn).
- Mature Bazel-9 integration; bb_clientd is the canonical
  reference implementation.
- Covers both input and output materialisation in one daemon —
  closer to the long-term BwoB shape.

**Cons**:

- **`bb_clientd` is a substantial dependency** (Java-style
  config, jsonnet-driven, full REAPI client + filesystem stack).
  Adopting it pulls in a large swath of buildbarn's runtime.
- Replaces `internal/casfuse`'s minimal Go FUSE layer with a
  binary we don't control. Reduces our ability to evolve the
  source-staging shape independently.
- The `RemoteOutputService` proto's primary axis is OUTPUT
  materialisation. Its INPUT-side guarantees (digest trust
  without re-hash) are coupled to the daemon's input-mount
  configuration, which is more opinionated than what we want.

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

**Pick A (`http_archive` + `repository_cache`).** Reasoning:

- It's the smallest infrastructure change that restores the
  "Bazel never re-hashes inputs" property on Bazel 9. We use
  Bazel's existing sha256-trust path; no patching, no plugin.
- The cas-fuse mount stays — it just gets a small additive
  feature (flat sha256-keyed view alongside the existing
  Directory-keyed view). `internal/casfuse`'s xattr-set
  machinery stays in tree (still useful for non-Bazel clients
  and as the proto-level source of digest truth).
- Reversibility: if A underperforms (external-repo scaling,
  symlink-resolution surprises), Option B (`bb_clientd`) is
  still on the table. We don't burn that option by trying A
  first.
- B remains a sensible **eventual** direction — it's where the
  rest of the buildbarn-aligned stack is heading, and the
  output-side gains are real. But it's a bigger jump than the
  next concrete step warrants.

## Next concrete steps

In rough order, sized for one PR each:

1. **cas-fuse: flat sha256-keyed view.** Add a top-level
   `<mount>/blobs/sha256/<hash>` namespace that resolves to
   the FileNode at that hash (via the existing CAS client).
   The Directory-keyed view stays. Tests in
   `internal/casfuse/fs_linux_test.go`.
2. **Repository-cache prepopulator.** A small `tools/cas-prepop`
   (or cas-fuse subcommand) that walks a sources manifest and
   creates symlinks at
   `<repo-cache>/content_addressable/sha256/<X>/file` → the
   flat sha256 path under the FUSE mount. Idempotent; safe to
   re-run.
3. **`rules/sources.bzl` rewrite.** Switch the per-source repo
   shape from `ctx.symlink(<dir-digest>)` to per-file
   `http_file(sha256=…, urls=["file://..."])`, with the URL
   resolving via the prepopulated repo cache. Probably gated
   on a feature flag (`--repo_env=CAS_FUSE_BAZEL9=1`) for the
   first pass so we can A/B against the legacy shape.
4. **Hello-fuse e2e gate.** Extend `tools/e2e-hello-fuse.sh`
   to exercise the new shape. Profile the digest phase via
   `--profile=...` and report the wall-clock delta against the
   legacy mount-only shape.
5. **External-repo-count probe.** At FDSDK scale a per-file
   `http_file` declaration is ~thousands of repos. Measure the
   evaluation-time cost; if it bites, fall back to per-source
   `http_archive(tarball)` with sha256-trust. The packing logic
   would live in cas-fuse / a sibling tool.

Each step is independently testable; (1) and (2) can land
without (3), and the legacy shape keeps working until (3)
flips the default.

## What this PR ships

- This doc (`docs/bazel9-cas-fs.md`).
- `tools/e2e-hello-fuse.sh` no longer passes the dropped flag
  pair (lines 132–155). Was supposed to be PR #71's change but
  hadn't landed; folded in here for honesty between the doc
  and the runnable script.
- `ROADMAP.md`'s Bazel 9 CAS-FS bullet now points at this doc
  and reflects the chosen direction.

No code changes; the next concrete step (cas-fuse flat sha256
view) is a follow-up PR.
