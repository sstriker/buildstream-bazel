# Sources + Build Without the Bytes

How source resolution and materialization work across `cmd/write-a`,
project A (meta workspace), and project B (consumer workspace), and
why Bazel 9 + `bb_clientd` is the production CAS-aware mount path.

## Goal

`bazel build //...` against either project resolves and consumes
sources without checking out the full source set on the local
machine — sources live content-addressed in CAS, and Bazel streams
them through the executor when running actions. Three layers consume
the same source identity:

- `write-a` reads source metadata (kind, url, ref) at render time
  and emits Bazel labels + a digest for each source.
- Project A's per-element genrules consume sources to feed
  `convert-element-cmake` / `convert-element-trace` /
  `convert-element-meson` / `convert-element-pyproject`.
- Project B's per-element targets (`cc_library` and friends) consume
  sources to compile downstream.

All three reference the same content; cache invalidation is keyed by
the source's CAS Directory digest.

## Constraints

1. **Identity stability** — same `(kind, url, ref)` → same CAS
   Directory digest, across projects and across time.
2. **No duplication of fetch effort** — A is meta of B; both
   reference the same digest. Re-fetching is a bug.
3. **`write-a` stays small** — render-time decisions don't depend on
   source bytes; metadata is enough.
4. **No hard dependency on local download** — with remote-execution
   + populated CAS + `bb_clientd`, the bytes never land on the
   developer's disk.

## Architecture

A custom `module_extension` declared in the meta-project (project A's
`MODULE.bazel`) walks the `.bst` graph and produces one external
repo per source identity. Both project A and project B `use_repo`
from the same extension; element BUILDs reference sources via
`@src_<key>//:tree`. The extension produces:

```python
sources = use_extension("@cmake_to_bazel//rules:sources.bzl", "sources")
sources.for_graph(
    bst_paths = [
        "elements/components/expat.bst",
        "elements/components/aom.bst",
        # ...
    ],
    project_root = "//",
    cas_endpoint = "//config/cas:address",
)
use_repo(sources, "src_a1b2c3...", "src_d4e5f6...", ...)
```

Each `@src_<key>//` repo rule is a thin shim. It resolves the CAS
Directory digest from `sources.json`, then `ctx.symlink`s
`external/src_<key>` at the path where `bb_clientd` exposes that
Directory under the configured mount. The repo rule reads no source
bytes; it just resolves a path under `CAS_FUSE_MOUNT` using the
prefix stored in `CAS_DIRECTORY_PREFIX`. The generated `BUILD.bazel`
exposes the staged files as a `tree` filegroup.

### Source-key derivation

`sourceKey()` in `cmd/write-a/source_cache.go` returns
`SHA256(kind | url | canonical_ref)`. Two callers compute identical
keys for identical inputs:

- `loadElement` consults `--source-cache` for pre-staged trees (the
  bridge that lets the existing `--source-cache` flow keep working).
- The module extension uses the same function to name its generated
  repos.

For language-package source kinds (`kind:cargo2`, `kind:go_module`,
`kind:pypi`, `kind:cpan`) where `ref` is a vendored list of registry
entries, the canonical form is the YAML-encoded node — deterministic
across re-loads.

### Why a module extension and not workspace-top repo rules

Module extensions defer resolution to evaluation time when the graph
(which sources, which digests) is computed. A static set of
`http_archive` declarations would require write-a to re-emit
`MODULE.bazel` on every source-graph change; the extension lets
write-a just produce the input data (`bst_paths` + project-conf
metadata) and the extension does the walk.

### Why one mount, not one per repo

A single mount with digest-addressed paths under it — repo rules
`ctx.symlink` at `<mount>/<prefix>/directory/<hash>-<size>/`.
Per-repo mounts would mean hundreds of FUSE mounts at FDSDK scale,
and on macOS each NFSv4 mount needs `sudo` (deal-breaker for dev
ergonomics).

### Ref-update semantics

Identity-by-digest does the heavy lifting. New ref → new
`sourceKey` → new digest → new `@src_<newkey>//` declared, old falls
out of `use_repo`. Symlink target points at the new digest's path;
old digest's content stays in CAS untouched. No mutation in place,
no inflight-read race. Bazel re-evaluates the module extension when
`sources.json` changes (its declared input).

## Populating the CAS

The flow has two halves: how the CAS gets *populated*, and how Bazel
*consumes* what's there.

**Production population** uses `bst source push` — BuildStream
already knows how to walk a graph, fetch each source, and upload
Directory digests to a configured REAPI ContentAddressableStorage +
Remote Asset endpoint. The `make buildbarn-up` deployment is one
end-to-end example.

**Dev / test population** uses `cmd/source-push`, a Go-side
uploader:

```sh
source-push tree  --cas=<addr> --src=<dir>
source-push graph --cas=<addr> --source-cache=<dir>
```

`make fdsdk-source-push` drives the graph variant against the full
FDSDK with the project's CAS endpoint configured.

## Bazel 9 + `bb_clientd` — the supported BwoB path

Bazel 7/8 closed the BwoB loop without any dev-side byte traffic via:

```
--unix_digest_hash_attribute_name=user.bazel.cas.digest
--digest_function=SHA256
```

A FUSE daemon served the xattr on every file node; Bazel read the
xattr and skipped the read-and-hash step entirely. **Bazel 9 dropped
the flag pair without a direct replacement.** The Bazel-9-native
answer is `--experimental_remote_output_service=`, which points
Bazel at a gRPC daemon that materialises both inputs and outputs
lazily and reports their digests. Bazel trusts those reported digests.

`bb_clientd` (buildbarn's client-side companion daemon) implements
`RemoteOutputService` in addition to serving a FUSE/NFS mount. The
full BwoB invocation:

```sh
bazel build \
  --remote_executor=grpc://... \
  --remote_cache=grpc://... \
  --remote_download_minimal \
  --experimental_remote_output_service=unix:///path/to/bb_clientd.sock \
  --repo_env=CAS_FUSE_MOUNT=/var/cache/cmake-to-bazel/cas \
  --repo_env=CAS_DIRECTORY_PREFIX=cas//blobs/sha256 \
  //elements/<leaf>:<leaf>
```

Result on Bazel 9 + `bb_clientd`:

- ✓ Executor side: workers read from CAS via REAPI.
- ✓ Dev disk: source trees never materialise as durable files.
- ✓ Dev network: bytes don't flow for digesting; Bazel trusts the
  daemon's pre-computed digests (same effect as the xattr fast-path
  on Bazel 7/8).
- ✓ Output side: build artifacts also materialised lazily
  (`--remote_download_minimal`).

Local end-to-end exercise: `tools/e2e-hello-bbclientd.sh` (also
`make e2e-hello-bbclientd`). Skips cleanly when `bb_clientd` /
Bazel ≥ 9 aren't on PATH.

**`bb_clientd` is a companion daemon paired with Bazel 9**, in the
same way `bazelisk` pairs with `bazel`. Adopting it is **not** an
adoption of "the buildbarn ecosystem" or a commitment to running
buildbarn end-to-end. Plenty of projects run `bb_clientd` alongside
non-buildbarn executors (EngFlow, BuildBuddy, NativeLink) — the
daemon talks plain REAPI to whatever CAS endpoint you point it at,
and plain `RemoteOutputService` to Bazel.

`bb_clientd` is itself a buildbarn project and builds with Bazel
(not the Go toolchain). Pre-built binaries are published on every
push to main at `github.com/buildbarn/bb-clientd/releases`; that's
the install path for the dev loop. Source builds via
`bazel run //cmd/bb_clientd` work too. `go install` does not — the
repo's go.mod has `replace` directives for `rules_go`'s sake that
make the Go toolchain reject the build but that Bazel honours
correctly.

### Mount layout

`bb_clientd` serves the following well-known layout under its
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

Path-template parameterisation lives in `cmd/write-a/sources_bzl.go`
and `cmd/write-a/traces_bzl.go`. Both rendered `.bzl` files build
the symlink target as
`<CAS_FUSE_MOUNT>/<CAS_DIRECTORY_PREFIX>/directory/<digest>`.
Default prefix is `blobs` (the flat layout the retired in-tree
`cmd/cas-fuse` daemon served — see [Retired components](#retired-components)).
bb_clientd users pass:

```
--repo_env=CAS_DIRECTORY_PREFIX=cas/<instance>/blobs/<digest_function>
```

to land on bb_clientd's canonical layout (with the daemon's default
empty instance + sha256 digest function, that collapses to
`cas//blobs/sha256`).

### Why this option

Options surveyed for the Bazel 9 xattr-replacement: (A)
`http_archive` + `repository_cache`, (B) `RemoteOutputService` via
bb_clientd, (C) a Java BlazeModule plugin, (D) accept the cost. We
picked B. It's Bazel 9's intended replacement for the dropped xattr
fast-path; picking anything else is picking a workaround. Output-side
BwoB comes along free. The `rules/sources.bzl` shape stays — just
the daemon serving the mount changes. Option A remains available as
a fallback if bb_clientd's operational footprint turns out badly for
some workflow we care about.

## write-a's source access pattern

write-a doesn't read source bytes at render time. `kind:cmake`'s
read-set narrowing uses **explicit inclusion/exclusion patterns**
rather than feedback-driven narrowing:

1. Each cmake element optionally ships a `<element>.read-paths.txt`
   committed alongside the `.bst` with `glob`-style include/exclude
   patterns.
2. Default when no file exists: the entire source tree is real
   (equivalent to `include **/*`).
3. write-a reads the patterns (or applies the default) and computes
   RealPaths / ZeroPaths without ever touching source bytes —
   patterns operate on the path universe, which the source's CAS
   Directory exposes via metadata listings.
4. Pattern generation is explicit and out-of-band: a
   `--regenerate-read-paths` mode of `convert-element-cmake` traces
   one run and writes the pattern file; the author commits it.

This is deterministic across version bumps (same source → same
patterns → same action key) and makes the read set reviewable in PR.
Drift is human-noticed (build hits a missing-file error) rather than
action-cache-stable.

For the soundness audit (catches silent-cache-hit bugs when patterns
omit a load-bearing path), see
[`narrowing-audit.md`](narrowing-audit.md).

## Project.conf `options:` → `string_flag` + `select()`

`options:` declared in `project.conf` lower to Bazel-native config:

- Each option produces a `string_flag(name = "//options:<name>",
  build_setting_default = "<default>")` in project A's `BUILD.bazel`.
- Each `(?):` branch keyed on that option becomes a `config_setting`
  + `select()` arm.
- For `target_arch` specifically, the existing `@platforms//cpu:*`
  pathway stays.
- For non-arch options (FDSDK's `prod_keys`,
  `bootstrap_build_arch`, etc.), `string_flag` is the Bazel-native
  expression.

The choice between static-fold and string_flag per option:

| Option | Treatment | Rationale |
|---|---|---|
| `target_arch` | `select()` over `@platforms//cpu:*` | Bazel-native target platform |
| `bootstrap_build_arch` | `string_flag` + `select()` | User-configurable |
| Other arch-typed options | `string_flag` + `select()` | User-configurable |
| `host_arch` | static (host platform) | Build-time host fact |
| Boolean / element-typed options | `string_flag` + `config_setting` | User-configurable |

The pipeline handler's per-arch resolution loop extends to also
iterate over option values declared in `project.conf`, with each
combination producing a `select()` arm. Combinatorial explosion is
bounded — most options have 1–3 values.

## Retired components

Earlier iterations of this design shipped an in-tree `cmd/cas-fuse`
daemon + `internal/casfuse` library that served the Bazel-7/8 xattr
fast-path. Both were retired once `bb_clientd` became the production
direction: the in-process FUSE tests were a recurring environmental
flake source, the CI jobs covering them (`cas-fuse-e2e`,
`bazel9-fuse-sources`, `hello-fuse-e2e`) were testing a code path
with no production consumers, and `bb_clientd` covers both Bazel
generations through the `RemoteOutputService` protocol.

`bb_clientd` is the only CAS-aware mount path the project supports.
The still-active CAS client / packer / tree code that used to live
in `internal/casfuse` was merged into `internal/cas`.
