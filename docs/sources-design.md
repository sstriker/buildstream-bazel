# Sources design: build-without-the-bytes for write-a + project A/B

Captures the architecture for source resolution + materialization
across `cmd/write-a`, project A (the meta workspace), and project B
(the consumer workspace), per the design discussion threading
through PRs #46–#49.

## Goal

`bazel build //...` against either project resolves and consumes
sources without checking out the full source set on the local
machine — sources live content-addressed in CAS, and Bazel
streams them through the executor when running actions
(`--remote_executor` + populated CAS = build-without-the-bytes).

Three layers consume the same source identity:

- `write-a` reads source metadata (kind, url, ref) at render time
  and emits Bazel labels + a digest for each source.
- Project A's per-element genrules consume sources to feed
  `convert-element`.
- Project B's per-element targets (`cc_library` and friends)
  consume sources to compile downstream.

All three reference the same content; cache invalidation is keyed
by the source's CAS Directory digest.

## Constraints to preserve

1. **Identity stability**. Same `(kind, url, ref)` → same CAS
   Directory digest, across projects and across time. A source
   bump produces a different digest both A and B see.
2. **No duplication of fetch effort**. A is meta of B; both
   reference the same digest. Re-fetching is a bug.
3. **`write-a` stays small**. Render-time decisions don't depend
   on source bytes; metadata (kind/url/ref/digest) is enough.
4. **No hard dependency on FUSE or local download**. With
   remote-execution + populated CAS, the bytes never land on the
   developer's disk.

## Architecture

A custom `module_extension` declared in the meta-project (project
A's `MODULE.bazel`) walks the .bst graph and produces one
external repo per source identity. Both project A and project B
`use_repo` from the same extension; element BUILDs reference
sources via labels like `@src_<key>//:tree`. The extension
produces:

```python
# project A's MODULE.bazel
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

The repo rule under each `@src_<key>//` is a thin shim. It
resolves the source's CAS Directory digest from `sources.json`,
then `ctx.symlink`s `external/src_<key>` at the path where a
long-running FUSE daemon exposes that Directory. Concretely:

- A user-level daemon (`cmd/cas-fuse`, derived from buildbarn's
  `bb_clientd`) mounts a single root, e.g.
  `/var/cache/cmake-to-bazel/cas/`, at startup and serves CAS
  Directory content under digest-addressed paths (mirroring
  bb_clientd's `<instance>/blobs/directory/<hash>-<size>/...`
  layout).
- The repo rule reads no source bytes; it just resolves a path.
- A `tree` filegroup in the generated `BUILD.bazel` exposes
  every staged file:

```python
# Generated @src_a1b2c3.../BUILD.bazel
filegroup(
    name = "tree",
    srcs = glob(["**/*"]),
    visibility = ["//visibility:public"],
)
```

Element-level BUILDs reference these repos in two contexts:

```python
# project A's elements/expat/BUILD.bazel
genrule(
    name = "expat_converted",
    srcs = ["@src_a1b2c3...//:tree"],
    outs = [...],
    cmd = "$(location //tools:convert-element) ...",
    tools = ["//tools:convert-element"],
)
```

```python
# project B's elements/expat/BUILD.bazel (after the driver
# stages convert-element's output)
cc_library(
    name = "expat",
    srcs = ["@src_a1b2c3...//:tree"],  # via converter's output
    ...
)
```

Both projects reference `@src_a1b2c3...//:tree` — same digest,
same CAS object, no duplication.

### Why a module extension and not repo rules at workspace top

Module extensions defer resolution to evaluation time when the
graph (which sources, which digests) is computed. A static set of
`http_archive` declarations in the workspace would require write-a
to re-emit `MODULE.bazel` on every source-graph change; the
extension lets write-a just produce the input data
(`bst_paths` + project-conf metadata) and the extension does the
walk.

## Source-key derivation

Already in `cmd/write-a/source_cache.go`: `sourceKey()`
returns `SHA256(kind | url | canonical_ref)`. Two callers:

- `loadElement` consults `--source-cache` for pre-staged trees
  (the bridge that lets the existing `--source-cache` flow keep
  working).
- The module extension uses the same function to name its
  generated repos.

Both layers compute identical keys for identical inputs. The
generated repo names match what write-a records on each
`resolvedSource` entry, so the two halves stay in sync.

For language-package source kinds (`kind:cargo2`, `kind:go_module`,
`kind:pypi`, `kind:cpan`) where `ref` is a vendored list of
registry entries, the canonical form is the YAML-encoded node —
deterministic across re-loads.

## CAS infrastructure

The flow has two halves: how the CAS gets *populated*, and how
Bazel *consumes* what's there.

### Population: `bst source push`

The CAS that backs the build is assumed configured (any REAPI
ContentAddressableStorage + Remote Asset implementation; the
`make buildbarn-up` deployment is one example we use end-to-end).
Population is done with **`bst source push`** itself — BuildStream
already knows how to walk a graph, fetch each source, and upload
Directory digests; we don't reimplement that in Go. A thin driver
wraps it for the demo:

- `make fdsdk-source-push FDSDK_DIR=...` runs `bst source push`
  against the FDSDK graph with the project's CAS endpoint
  configured.

A direct Go-side uploader is **deferred**. URL-fetch fallback is
also **deferred** — v1 requires a populated CAS.

### Consumption: FUSE daemon (`cmd/cas-fuse`)

`bst source push` writes Directory digests, not `(url, sha256) →
blob` Remote-Asset entries. That rules out stock `http_archive`
even with `--experimental_remote_downloader` (Bazel only does
`FetchBlob`, never `FetchDirectory`). We need to consume Directory
digests directly — which means a custom FS view of CAS.

Architecture: a long-running user-level daemon mounts a single
root and serves CAS Directories lazily. Repo rules `ctx.symlink`
into the mount; bytes are pulled from CAS only on actual reads.
The daemon is `cmd/cas-fuse`, built by lifting buildbarn's
existing FUSE stack:

- `bb-remote-execution/pkg/filesystem/virtual/` — generic
  `Directory`/`Leaf` abstraction, backend-agnostic.
- `bb-remote-execution/pkg/filesystem/virtual/fuse/` — FUSE shim
  over `hanwen/go-fuse/v2` (Linux). Clean package boundary, no
  worker / `RunCommand` entanglement.
- `bb-clientd/pkg/filesystem/virtual/` — the CAS-Directory
  factory we want (`decomposed_cas_directory_factory.go`).
- `bb-remote-execution/pkg/filesystem/virtual/configuration/` —
  mount glue: FUSE on Linux, in-tree NFSv4 server + `mount(2)` on
  macOS (no macFUSE / FUSE-T third-party dep), WinFsp on Windows.

Reuse estimate: ~500 LOC of glue if we adopt buildbarn's
`program.Group` + protobuf-jsonnet config conventions; ~1500-2500
LOC if we want a dependency-light binary that inlines the small
amount of mount glue. Either way, vastly cheaper than building
equivalent functionality (especially the macOS NFSv4 server) from
scratch (5-10k+ LOC).

### Why one mount, not one per repo

A single mount with digest-addressed paths under it (per
bb_clientd) — repo rules `ctx.symlink` at
`<mount>/<instance>/blobs/directory/<hash>-<size>/`. Per-repo
mounts would mean hundreds of FUSE mounts at FDSDK scale, and on
macOS each NFSv4 mount needs `sudo` (deal-breaker for dev
ergonomics).

### Ref-update semantics

Identity-by-digest does the heavy lifting. New ref → new
`sourceKey` → new digest → new `@src_<newkey>//` declared, old
falls out of `use_repo`. Symlink target points at the new digest's
path; old digest's content stays in CAS untouched. No mutation in
place, no inflight-read race.

Bazel re-evaluates the module extension when `sources.json`
changes (its declared input). It doesn't need to watch the
filesystem — we never overwrite paths.

### Full BwoB via xattr-served digests

Bazel ships a flag pair that closes the loop end-to-end without
any dev-side byte traffic for digesting:

```
--unix_digest_hash_attribute_name=user.bazel.cas.digest
--digest_function=SHA256
```

When set, Bazel reads the named extended attribute on each input
file and treats its value as the file's content digest, skipping
the read-and-hash step it would otherwise perform. `cmd/cas-fuse`
serves exactly that xattr (key `user.bazel.cas.digest`, value =
hex SHA-256 of the file's bytes — already known from the CAS
FileNode digest). Result:

- ✓ Executor side: workers read from CAS via REAPI.
- ✓ Dev disk: source trees never materialise as durable files.
- ✓ Dev network: bytes don't flow at all for digesting; they only
  flow when an action actually reads the content (which usually
  doesn't happen for inputs to remote actions — the executor
  fetches them itself).

The build invocation gets these alongside the existing
remote-execution flags:

```
bazel build \
  --remote_executor=grpc://... \
  --remote_cache=grpc://... \
  --remote_download_minimal \
  --unix_digest_hash_attribute_name=user.bazel.cas.digest \
  --digest_function=SHA256 \
  --repo_env=CAS_FUSE_MOUNT=/var/cache/cmake-to-bazel/cas \
  //elements/<leaf>:<leaf>
```

`--repo_env=CAS_FUSE_MOUNT=...` points the source-extension's
repo rule at the daemon's mount; the rule `ctx.symlink`s
`<mount>/blobs/directory/<digest>/` into each declared
`@src_<key>//` repo.

## `cmd/write-a`'s source access pattern

write-a doesn't read source bytes at render time. The current
exception — `kind:cmake`'s read-set narrowing — switches from
adaptive feedback to **explicit inclusion/exclusion patterns**.

### From feedback-driven to pattern-driven narrowing

Today's flow (`--read-paths-feedback`):

1. First run: every file flows as real to convert-element. The
   converter writes `read_paths.json` listing what it actually
   read.
2. Subsequent runs: write-a reads the previous `read_paths.json`
   and zeros files outside the set.

This is non-deterministic: backing out to an older source revision
where a previously-read file is now important produces a too-narrow
set. Action-cache hits become false-negatives across version
bumps.

New flow:

1. Each cmake element optionally ships a `<element>.read-paths.txt`
   file (committed alongside the .bst) with `glob`-style include /
   exclude patterns:
   ```
   include CMakeLists.txt
   include cmake/*.cmake
   include include/**/*.h
   exclude include/internal/*
   ```
2. **Default when no file exists**: the entire source tree is
   real (equivalent to `include **/*`). This matches the
   conservative pre-narrowing behaviour — convert-element runs
   against every file. The patterns file is an opt-in tightening
   for elements where the action-cache benefit is worth the
   maintenance cost.
3. write-a reads the patterns (or applies the default) and
   computes RealPaths / ZeroPaths without ever touching source
   bytes — patterns operate on the path universe, which the
   source's CAS Directory exposes via metadata listings (no byte
   read needed).
4. Pattern generation is explicit and out-of-band. A
   `--regenerate-read-paths` mode of `convert-element` (or a
   sibling tool) traces one run and writes the pattern file; the
   author commits it. Drift is human-noticed (build hits a
   missing-file error in cmake) rather than action-cache-stable.

Pros over feedback:
- Deterministic: same source → same patterns → same action key.
- Survives version bumps: a pattern that includes
  `cmake/Find*.cmake` keeps including new entries as they land.
- Makes the read set reviewable in PR.

Cons:
- Author burden the first time. Mitigated by the regenerate
  tool.
- Drift over many bumps. Mitigated by re-running the regenerate
  tool periodically (CI job, not a build-time concern).

Today's `--read-paths-feedback` flag is removed when this lands —
no transitional release. Patterns (or the default) are the only
input.

### What write-a needs from CAS at render time

For the narrowing to work without reading bytes, write-a needs
the path universe of each source. Two options:

A. **Universe from patterns themselves.** If patterns enumerate
   explicit file paths (no globs), no listing needed.
B. **Universe from CAS metadata.** A Directory protobuf in CAS
   carries a recursive file listing; write-a fetches just the
   metadata via the CAS API (`GetTree` RPC) without downloading
   blobs.

Option A is simpler but author-heavy. Option B preserves glob
expressiveness without byte reads. The implementation will be
B; the metadata-only fetch is a small net call, not a download.

## Project.conf `options:` → `string_flag` + `select()`

Per the design discussion, options become Bazel-native config:

- Each `options: <name>` declared in project.conf produces a
  `string_flag(name = "//options:<name>", build_setting_default = "<default>")`
  in project A's `BUILD.bazel`.
- Each `(?):` branch keyed on that option becomes a
  `config_setting` + `select()` arm.
- For `target_arch` specifically, the existing `@platforms//cpu:*`
  pathway stays — those `select()`s reference platforms-namespace
  labels.
- For non-arch options (FDSDK's `prod_keys`, `bootstrap_build_arch`,
  etc.), the `string_flag` shape is the Bazel-native expression.

This replaces the current static-fold pass (`foldStaticConditionals`
) for those options that map to user-configurable flags.
Static fold remains for hardcoded defaults the user can't
override (`host_arch` is determined by the build host, not a
flag).

The choice between static-fold and string_flag per option:

| Option | Treatment | Rationale |
|---|---|---|
| `target_arch` | `select()` over `@platforms//cpu:*` | Bazel-native target platform |
| `bootstrap_build_arch` | `string_flag` + `select()` | User-configurable |
| Other arch-typed options | `string_flag` + `select()` | User-configurable |
| `host_arch` | static (host platform) | Build-time host fact |
| Boolean / element-typed options | `string_flag` + `config_setting` | User-configurable |

The pipeline handler's per-arch resolution loop extends
to also iterate over option values declared in project.conf, with
each combination producing a `select()` arm. Combinatorial
explosion is bounded — most options have 1–3 values.

## Migration order

Per the agreed sequence:

1. **This PR (#50)**: design doc only. Captures direction.
2. **Project.conf completion** (#51 and follow-ups): options as
   string_flag + select(); aliases parsed (used by the source-fetcher
   to translate `github:`, `sourceware:` etc.); environment block
   wired into per-element genrule `env` attrs.
3. **`cmd/cas-fuse` daemon** (#52): user-level FUSE daemon
   lifted from `bb_clientd`. Mounts a single root, serves CAS
   Directories at digest-addressed paths. Linux first; macOS
   NFSv4 path follows. Includes a `make cas-fuse-up` lifecycle
   helper for dev workflows.
4. **Sources scaffold** (#53 and follow-ups):
   - `rules/sources.bzl` module extension that reads
     `sources.json` and declares per-digest repos.
   - Per-repo rule `ctx.symlink`s into the daemon's mount.
   - `make fdsdk-source-push` driver wrapping `bst source push`
     to populate CAS for FDSDK end to end.
5. **Cmake narrowing via patterns** (#54): pattern parser;
   default-when-absent (entire tree real); remove
   `--read-paths-feedback` outright.
6. **End-to-end demonstration** (#55): one FDSDK kind:cmake
   element built end-to-end against project B with CAS-backed
   sources, dev disk never materialising the source tree. The
   reality-check probe extends to confirm
   `bazel build //elements/expat:expat` succeeds against the
   CAS-populated workspace + running `cmd/cas-fuse`.

## Open questions

1. **Granularity of generated repos.** One repo per source
   identity (current proposal), or one repo per element with all
   its sources composed? The latter is fewer repos and matches
   the "element name as directory name" desire from the design
   discussion. Per-source is more granular and lets two elements
   sharing a source dedup. Lean toward **per-source**.

2. **`MODULE.bazel` regeneration triggers.** When the .bst graph
   changes, the module extension's `bst_paths` input changes,
   triggering re-evaluation. But the user-facing `MODULE.bazel`
   should be stable; only the extension's data file regenerates.
   write-a writes a `sources.json` (or similar) under `tools/`
   that the extension reads; that's the actual changing input.

## Out of scope for this design

- **Per-element source-graph caching at the orchestrator level.**
  The existing `orchestrator/internal/sourcecheckout` provides
  per-element caching against an on-disk dir; the new flow keeps
  that as the source-of-truth for "what got fetched" but moves
  the consumption to CAS. The CAS / orchestrator-cache split is a
  separate refactor.
- **Cross-junction source sharing.** Multi-project graphs where
  junctions reference sources from another project's
  `bst source push` namespace. v1 handles single-project graphs.
