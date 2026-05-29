# CMake → Bazel: the `--split-packages` multi-package emit

`convert-element-cmake` defaults to emitting a single monolithic
`BUILD.bazel` per element. The opt-in `--split-packages` flag instead
emits **one `BUILD.bazel` per directory** — the "gazelle model" — so the
generated tree mirrors the `CMakeLists.txt` / `add_subdirectory()`
layout an operator already reads. This document describes the package
model, the header-library synthesis, the label rules, and the v1
boundaries.

The flag is off by default and the single-BUILD path is **byte-identical
to the pre-feature output** when off (pinned by
`TestEmit_SplitOff_ByteIdenticalToSingleGolden` and the unchanged
`TestEmit_*_Golden` goldens).

## The package model

Lowering records, out of band, the directory each codemodel target was
declared in: `ir.Package.SubPackages` maps a target name to its
element-root-relative declaring directory (`""` = the root package,
`"src/util"` for a target under `add_subdirectory(src/util)`). It is
populated from `ConfigTargetRef.DirectoryIndex` →
`ConfigDirectory.Source`, reconciled with the same `labelRoot` base
sources are relativized against (so dirs and srcs agree under the
workspace-root umbrella shape).

`SubPackages` carries the struct tag `json:"-"` — it never serializes
into `--out-ir-json`, so the multi-platform fold's JSON wire shape is
unperturbed by the feature.

The emit-time transform (`converter/emit/bazel/split.go`,
`EmitSplit`) computes the **package set** as:

    { declaring dir of each real target }  ∪  { each include-root dir }

Header subdirectories nested under an include root are **not** separate
packages — the include-root's header library globs them, preserving
cmake's single-`-I`-root include semantics.

The transform partitions the lowered package's targets by declaring
directory and renders each group through the **existing**
`EmitWithOptions` (no new IR tree type, no emit-internals fork) with a
per-package `BazelPackagePath` so the `# gazelle:cc_search` directive is
framed correctly per sub-package.

## Header-library synthesis

For each include-root directory `inc` (an entry in any target's
`Includes`), the transform synthesizes a header `cc_library` in package
`inc`:

- **name**: a deterministic sanitized name derived from `inc` —
  `include` → `include_headers`; nested `src/api` → `src_api_headers`;
  root → `root_headers`. Made unique against real targets.
- **hdrs**: every header (from any target's `Hdrs`) physically under
  `inc`, made package-relative to `inc`. (Longest-prefix assignment so
  nested include roots claim their own headers.)
- **includes** = `["."]` so consumers get `-I<inc>` transitively.
- **visibility** public.

Then for every real target `T` that referenced `inc` in its `Includes`,
the transform removes `inc` from `T.Includes`, removes the headers under
`inc` from `T.Hdrs`, and adds a dep on the synthesized header-lib label.
Deps are deduped.

## Label rules

- **Intra-element dep** `:x` → `//<base>/<dir(x)>:x` using `SubPackages`
  (`dir(x)==""` → `//<base>:x`). A dep on a synthesized / install-derived
  target (no `SubPackages` entry) resolves to the root package
  `//<base>:x`.
- **Re-relativized srcs**: each element-root-relative src is trimmed to
  its declaring dir. A source *not* under that dir (a cross-package
  source) is referenced as a label `//<base>/<srcdir>:<file>` and the
  owning package gains an `exports_files([...])` entry.
- **SourceKey regime**: when `--source-key` is set, srcs/hdrs are
  emitted as `@src_<key>//:` absolute, package-location-independent
  labels. The transform leaves those element-root-relative (it never
  re-relativizes them); only the local (`SourceKey==""`) regime trims
  paths to the sub-package.
- **Cross-element exports** (`exports.json`): an importable target's
  label becomes `//<base>/<subdir>:<target>` via `SubPackages` when
  split is on. Install-derived importable targets (`cc_import`s) stay at
  the root, so their labels keep the `//<base>:<target>` form. Aliases
  map to the underlying target's label and pick up its subdir for free.

## Output contract

- Standalone CLI: the root package writes to `--out-build`; a sub-dir
  `src/util` writes to `<dir(--out-build)>/src/util/BUILD.bazel`
  (`MkdirAll` as needed).
- The post-emission `bazelidiom.Audit` runs over **every** emitted BUILD
  (findings aggregated), not just one blob.

## v1 boundaries

- **OFF byte-identity**: when the flag is off, output is byte-identical
  to today's single-BUILD emit.
- **Mutually exclusive with `--out-ir-json`** (the multi-platform fold
  path round-trips IR through JSON, which omits `SubPackages`). Both set
  → a clear Tier-1 error.
- **Visibility** stays `//visibility:public` for split sub-packages (no
  `__subpackages__` — that would break the cross-element export
  channel). No visibility-tightening in v1.
- **Install-derived / synthesized targets** (filegroups from
  directory installers, `cc_import` + `cmake_config_bundle` from export
  installers, aliases, genrules, interface libs, per-language
  sub-libraries) all stay in the **root** package; their cross-element
  labels keep the element-root form.
- **Cross-package references from root-pinned targets**: a root-pinned
  install-derived target whose `srcs` name a directory that became its
  own package (e.g. `install(DIRECTORY include/ …)` lowering to
  `filegroup(srcs = ["include"])` once `include/` is a package) would
  otherwise emit a label that crosses a package boundary and fail to
  load the whole root package. The split transform detects this via
  `splitPlan.deepestPkg`: a `srcs`/`hdrs` entry owned by a *deeper*
  package than the rule's own is re-pointed to a cross-package file
  label (and the owning package gains an `exports_files`), and a bare
  packaged-*directory* entry — which has no expressible cross-package
  file label — is dropped. A filegroup left empty by such a drop is
  omitted entirely. `install(DIRECTORY)` of a directory that became its
  own package is therefore not re-emitted as a build rule under split;
  its content is served by the layout-independent `install_tree.tar`
  path, which is untouched.
## Orchestrator wiring (write-a / stage-b)

`cmd/write-a --split-packages` threads the mode end-to-end. A Bazel
genrule can't statically declare the discovered-at-action-time
sub-package set as `outs`, so on the split path the element is converted
by the `cmake_split_convert` custom rule
(`rules_buildstream_bazel/rules/cmake_packages.bzl`) instead of the
single-`BUILD.bazel.out` genrule. The rule's action runs
convert-element-cmake in `--split-packages` mode and declares the
per-sub-package BUILD tree as a **TreeArtifact**
(`ctx.actions.declare_directory`) — Bazel content-addresses each
generated BUILD file individually, so there is no opaque
`build-packages.tar` whose digest churns on any one BUILD changing.
`--out-build` points at `<packages>/BUILD.bazel` inside the TreeArtifact;
the rule also declares the scalar `read_paths.json`,
`cmake-config-bundle.tar`, and `exports.json` outputs. Per-element flag
logic (lift / fallback / fidelity / bake-in) is assembled by write-a and
passed through the rule's `converter_args` string attr, keeping the
Starlark mechanical (shadow-build + dep-extract + convert + bundle-tar,
mirroring the genrule bash); kind:cmake dep bundles ride `dep_bundles`
and `imports.json` / dep `exports.json` ride `aux`. The default (off)
path keeps the single `BUILD.bazel.out` genrule byte-for-byte.

The rule's action behavior is verified by CI/`bazel build` — there is no
local bazel in the dev sandbox, so the contract write-a owes the rule is
checked via render-shape assertions, and the action itself runs in CI.

`cmd/stage-b` consumes it: when project A's
`bazel-bin/elements/<name>/<name>_converted/packages` (the rule's
TreeArtifact directory; `declare_directory` path is
`<rule-name>/packages` and the rule name is `<name>_converted`) exists,
it merges the live directory into project B's `elements/<name>/` by
per-file digest — walking the tree, writing each BUILD under its
relative sub-package path (overwriting the root placeholder and creating
the sub-package BUILD files), and reporting the element changed only when
a staged file's content actually differs — the same idempotent "what
re-converted" signal the single-file path returns. The merge stages
regular files only (escaping symlinks are skipped, not followed) and
rejects any `..`-escaping relative path. Project B already co-locates
each element's sources with its BUILD (the single-BUILD shape builds
there with element-root-relative `srcs`), so distributing the BUILD
files across sub-directories of that same tree needs no extra source
staging.

## Gazelle stability

`gazelle -mode=diff` (with `gazelle_cc` compiled in) over a workspace
containing the emitted split BUILDs + source tree is **not** a hard gate
— it's evidence. The residual diff is dominated by an inherent model
difference: `gazelle_cc` assigns a target to the package where its
`.c` file physically lives, whereas the converter assigns it to the
directory whose `CMakeLists.txt` *declared* it (a target whose
`add_library` lives in the root CMakeLists but whose source sits in
`src/` lands at the root for us, in `src/` for gazelle). `buildifier
-mode=diff` *is* a no-op (the hard formatting contract), gated by
`scripts/meta-cmake-split-packages.sh`.
