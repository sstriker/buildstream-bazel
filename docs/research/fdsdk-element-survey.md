# FDSDK element survey (Phase 0)

Source: `gitlab.com/freedesktop-sdk/freedesktop-sdk` @ `ba490d000` (2026-04-28).

This doc grounds the per-kind translator status against what FDSDK
actually contains. See [`ROADMAP.md`](../ROADMAP.md) for current
per-kind coverage status.
Numbers are direct counts off `*.bst` files in the repo. Update this
doc when the survey is re-run against a newer FDSDK snapshot.

## 1. Element-kind breakdown (1 092 total)

| Kind | Count | % | Plugin source | Plan-time row |
|---|---|---|---|---|
| `autotools` | 274 | 25.1 % | buildstream-plugins | yes (coarse v1) |
| `meson` | 134 | 12.3 % | buildstream-plugins | yes (fine) |
| `pyproject` | 115 | 10.5 % | community | **missing** |
| `manual` | 104 | 9.5 % | core | yes (coarse) |
| `stack` | 96 | 8.8 % | core | yes (trivial) |
| `cmake` | 75 | 6.9 % | buildstream-plugins | yes (done) |
| `make` | 59 | 5.4 % | buildstream-plugins | **missing** |
| `script` | 53 | 4.9 % | core | **missing** |
| `filter` | 42 | 3.8 % | core | yes (structural) |
| `flatpak_image` | 26 | 2.4 % | community | **missing** |
| `compose` | 25 | 2.3 % | core | **missing** |
| `import` | 22 | 2.0 % | core | **missing** |
| `collect_manifest` | 18 | 1.6 % | community | **missing** |
| `collect_initial_scripts` | 15 | 1.4 % | local | **missing** |
| `makemaker` | 14 | 1.3 % | community | **missing** |
| `junction` | 8 | 0.7 % | core | yes (orchestration) |
| `snap_image` | 6 | 0.5 % | community | **missing** |
| `collect_integration` | 2 | 0.2 % | community | **missing** |
| `check_forbidden` | 2 | 0.2 % | community | **missing** |
| `flatpak_repo` | 1 | 0.1 % | community | **missing** |
| `modulebuild` | 1 | 0.1 % | community | **missing** |

The plan covered 7 kinds (cmake, meson, autotools, manual, stack, filter,
junction) totalling 67 % of FDSDK. **The other 33 % falls into 14 kinds
the plan didn't enumerate.** Three buckets emerge:

- **Buildsystem variants** — `make` (5.4 %), `pyproject` (10.5 %),
  `script` (4.9 %), `makemaker` (1.3 %), `modulebuild` (0.1 %). Each
  is a coarse-genrule-shaped translator: command-list driven, no
  introspection. Fold them into the same translator pattern as
  `manual`.
- **Install-tree manipulation** — `compose` (2.3 %), `import` (2.0 %),
  `filter` (3.8 %), `flatpak_image` (2.4 %), `snap_image` (0.5 %),
  `flatpak_repo` (0.1 %). These don't run a build; they slice or
  re-package the install tree of their dependencies. Trivial
  `filegroup`/`pkg_tar` translators.
- **Manifest / artifact-policy** — `collect_manifest` (1.6 %),
  `collect_initial_scripts` (1.4 %), `collect_integration` (0.2 %),
  `check_forbidden` (0.2 %). FDSDK-specific glue. Mostly small
  generators, one is a CI-style assertion. Plumb in last.

## 2. Source-kind breakdown

Top-level `sources:` entries (excluding nested `kind: registry` rows
inside `cargo2` / `pypi` / `cpan` ref lists):

| Kind | Count | Plugin source |
|---|---|---|
| `git_repo` | 530 | community |
| `local` | 147 | core |
| `patch` | 65 | buildstream-plugins |
| `tar` | 64 | core |
| `go_module` | 53 | community |
| `git_module` | 45 | community |
| `cpan` | 13 | community |
| `pypi` | 10 | community |
| `cargo2` | 10 | community |
| `zip` | 8 | community |
| `docker` | 5 | buildstream-plugins |
| `remote` | 3 | core |
| `patch_queue` | 3 | community |

`git_repo` (FDSDK's preferred git source) is the dominant kind, not
`git`. The plan listed `git` as already handled; if `git_repo`'s
shape differs (it carries a `track` glob and uses URL aliases), the
sourcecheckout path needs a small adapter. The language-package
sources (`go_module`, `cargo2`, `cpan`, `pypi`) carry vendored ref
lists with nested `kind: registry` entries — fixture-relevant for
language elements but orthogonal to the per-kind translator
interface.

## 3. Per-kind config surface

For each kind that appears ≥5 times, the union of `config:` subkeys
seen in the wild plus the most common `variables:` subkeys (where
real per-element customization lives — BuildStream plugins consume
these as defaults).

### Build-driving kinds

| Kind | `config:` subkeys seen | Top `variables:` |
|---|---|---|
| `autotools` (274) | install-commands ×71, configure-commands ×26, build-commands ×11, strip-commands ×2 | `conf-local` (107), `autogen` (27), `build-dir` (21), `make-args` (12), `conf-link-args` (8) |
| `meson` (134) | install-commands ×28, configure-commands ×1 | `meson-local` (102), `command-subdir` (6), `arch-conf` (3) |
| `cmake` (75) | install-commands ×10, configure-commands ×4, build-commands ×1 | `cmake-local` (61), `command-subdir` (8), `license-files-extra` (8), `arch-conf` (4), `conf-root` (2) |
| `make` (59) | install-commands ×26, build-commands ×9, configure-commands ×5 | `make-args` (40), `command-subdir` (10), `make-install` (6), `optimize-debug` (5), `notparallel` (5) |
| `pyproject` (115) | build-commands ×9, install-commands ×4 | `strip-binaries` (40), `command-subdir` (2) |
| `manual` (104) | install-commands ×96, build-commands ×32, configure-commands ×2, strip-commands ×1 | `strip-binaries` (76), `optimize-debug` (6), `fontdir` (6), `confdir` (6) |
| `script` (53) | commands ×44 | (script-specific: `uuidnamespace`, `install-root`, `snap-target`, `entrypoint`) |
| `makemaker` (14) | (none — pure defaults) | — |

`<kind>-local` (`cmake-local`, `meson-local`, `conf-local`, `autogen`)
is the dominant per-element customization point: it's a
whitespace-joined string of `-D...` / `--enable-...` flags appended
to the plugin's default invocation. The translator must honor it.

`command-subdir` recurs across cmake, meson, make, pyproject — the
build runs in a subdir of the source tree. Translator-level concern.

### Trivial / structural kinds

| Kind | `config:` subkeys | Notes |
|---|---|---|
| `stack` (96) | none | just `depends:`, no build |
| `filter` (42) | include ×25, exclude ×15, include-orphans ×26 | split-rules application over parent's output |
| `import` (22) | target ×11 | copies `sources:` into a target dir; no build |
| `compose` (25) | exclude ×21, include-orphans ×7, integrate ×1 | re-emits parent install tree minus exclusions |
| `flatpak_image` (26) | directory ×26, metadata ×26, exclude ×18, include ×4 | wraps a directory subtree as a flatpak image |
| `snap_image` (6) | directory ×6, metadata ×6, exclude ×6, include ×4 | snap analogue |
| `collect_manifest` (18) | path ×4 | walks deps' manifests into one file |
| `collect_initial_scripts` (15) | path ×15 | walks deps' init scripts into one file |
| `collect_integration` (2) | script-path ×2 | script-glob aggregation |
| `check_forbidden` (2) | forbidden ×2 | CI assertion: bail if any path matches |
| `flatpak_repo` (1) | arch ×1, repo-mode ×1 | one-off |
| `modulebuild` (1) | none | one-off Perl module |
| `junction` (8) | options ×6, map-aliases ×5 | resolves at orchestration time, not emit time |

## 4. Features the translator may want to defer

Patterns the BuildStream plugins support that show up in FDSDK and
are worth flagging early:

- **`command-subdir`** (cmake 8, meson 6, make 10, pyproject 2): the
  build runs in a subdir of the source. Trivial to plumb; just need
  to remember it exists.
- **`%{...}` variable substitution**: every plugin's command lists
  reference project-defined variables (`%{install-root}`,
  `%{prefix}`, `%{libdir}`, `%{gcc_triplet}`, `%{build-triplet}`).
  FDSDK defines these in `project.conf` + `include/_private/*.yml`.
  Translators that emit command lists (manual, script, autotools
  install-commands, make build-commands, etc.) must resolve them
  before emit.
- **Conditional variable blocks** — `(?):` selectors keyed on
  `target_arch == "..."` (LLVM example: per-arch `targets` strings).
  Translation-time resolution against the `target_arch` chosen in
  conversion config.
- **YAML directives** — `(@):` (include), `(>)` (append),
  `(<)` (prepend). Already handled at the YAML-load layer; not a
  translator-internal concern.
- **`public.bst.split-rules`** — drives Bazel-side filegroup slices
  for `filter` consumers. Element-emit-time concern; structurally
  identical across kinds.
- **`environment:` per-element overrides** — autotools 12, manual 8,
  meson 5, make 11. Mostly `LD_LIBRARY_PATH` workarounds at build
  time. Plumb through to the action's env.
- **`environment-nocache:`** — manual 4, make 1, script 2. Marks env
  vars that should NOT contribute to the action key. Action-cache
  correctness concern; trivial once the rest of the env path exists.

Things to **skip until needed**:

- Custom plugin reflection / dynamic kind registration. FDSDK's
  `project.conf` is fixed; we read it once at startup.
- `workspace:` source kind (not present in FDSDK).
- BuildStream's substitution-engine recursion edge cases beyond
  what FDSDK actually uses; revisit if a translator panics on a
  real element.

## 5. Phasing implications

1. **Composition kinds** (`stack`, `filter`, `import`, `compose`,
   `flatpak_image`, `snap_image`, `flatpak_repo`, `collect_manifest`,
   `collect_initial_scripts`, `collect_integration`,
   `check_forbidden`) — all install-tree-manipulation or
   static-data emitters with no compile step. Together ~13 % of FDSDK.
2. **Command-list-driven kinds** (`script`, `pyproject`,
   `make`, `makemaker`, `modulebuild`, `manual`) share the
   command-list-driven coarse-genrule shape. Together ~36 % of FDSDK.
3. **Meson** is 12.3 % vs cmake's 6.9 % — the next-largest
   fine-grained-buildsystem footprint after autotools (25.1 %).
4. **Source-kind work**. `git_repo` (530 entries, dominant) is an
   alias-resolving variant of `git` — sourcecheckout needs a small
   adapter for URL aliases in `include/_private/aliases.yml`.

## Methodology

Counts derived by walking every `*.bst` under `elements/` and
parsing the `kind:` line plus the YAML structure under
`config:` / `variables:` / `public:` / `sources:`. Re-run via:

```
find elements -name "*.bst" -exec awk '/^kind:/{print $2; exit}' {} \;
```

Re-runs against newer FDSDK snapshots are cheap (single Python
script over ~10 MB of YAML). Update this doc when the percentages
shift materially or a new kind appears.
