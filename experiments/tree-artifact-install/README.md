# Spike: TreeArtifact (`declare_directory`) as the `install_tree.tar` replacement

**Question under test:** can the coarse install-tree transport — today
an opaque `install_tree.tar` produced by project B's per-element
install genrule — be re-expressed as a Bazel **TreeArtifact**
(`ctx.actions.declare_directory`), and does that buy the file-level
CAS dedup the ROADMAP wants *without* the repo-rule alternative's
RBE-incompatibility?

**Answer: yes.** This self-contained workspace builds and proves the
three mechanisms a real migration needs.

## What's here

| File | Role | Maps to (real pipeline) |
|---|---|---|
| `rules/install.bzl` → `install_tree` | install root as a TreeArtifact | the install genrule's `outs = ["install_tree.tar"]` in `cmd/write-a/handler_autotools_native.go` |
| `rules/consume.bzl` → `build_against` | downstream builds against the dep's tree **in place** | `autotoolsDepExtractCmd`'s `tar -xf */install_tree.tar -C $DEP_PREFIX` loop |
| `rules/install.bzl` → `pick_file` | project one file out of the tree as a plain `File` label | the round-2 fallback's `cc_import`/`sh_binary` stubs + `_install_tree_extract` genrule |

The rules drive host `cc`/`ar` directly via `run_shell`, so the proof
needs no `rules_cc`/cc-toolchain wiring — the point under test is the
TreeArtifact plumbing, not toolchain resolution. Runs in WORKSPACE
mode to stay offline (no BCR fetch).

## Reproduce

```sh
cd experiments/tree-artifact-install
export BAZELISK_BASE_URL="https://github.com/bazelbuild/bazel/releases/download"  # if releases.bazel.build is blocked
bazel build //...
bazel run //:app    # -> app: foo_add(2,3)=5
bazel run //:app2   # -> app2: foo_add(10,20)=30
```

## What it proves

1. **The install root is a real Directory, not a tarball.**
   `bazel-bin/foo_install/usr/{include/foo.h,lib/libfoo.a}` — a
   browsable tree. One line (`declare_directory`) replaces
   `outs = ["install_tree.tar"]`.

2. **Downstream consumers build against the tree in place — no
   `tar -xf`, no per-consumer `$DEP_PREFIX` copy.** The `app`
   compile action's command line is:

   ```
   cc "src/main.c" \
      -I "bazel-out/.../foo_install/usr/include" \
      -L "bazel-out/.../foo_install/usr/lib" -lfoo -o ".../app"
   ```

   with `Inputs: [bazel-out/.../foo_install, src/main.c]` — the
   Directory is a direct input. `bazel aquery //...` shows **zero**
   tar/untar actions in the entire graph (mnemonics are only
   `InstallTree`, `BuildAgainst`, `PickFile`).

3. **Per-target projection works** — `pick_file` pulls
   `usr/lib/libfoo.a` out of the tree into a standalone `File`
   (`bazel-bin/libfoo_a/libfoo.a`, a valid `ar` archive) that a
   `cc_import` could consume. This is the mechanism that replaces the
   extract genrule.

4. **No per-edge duplication.** Two consumers (`app`, `app2`) share
   the one `foo_install` Directory. Editing only `src/main.c` re-runs
   a single action (the `app` recompile); `InstallTree` and `app2`
   are reused from cache. On RBE this Directory is one merkle tree in
   CAS that every consumer references — which is exactly the
   `tar_bytes + Σ extract-genrule bytes` duplication the ROADMAP
   flags for the round-2 fallback, eliminated.

## How this maps to the real migration (not done here)

This spike is a model, not a wiring of `cmd/write-a`. A real
migration would:

- Move the project-B install step from a `genrule` (which can only
  declare *file* outputs) to a custom rule using `declare_directory`.
  `rules_buildstream_bazel/rules/traces.bzl` is the in-repo precedent
  for a custom rule with declared outputs.
- Replace `autotoolsDepExtractCmd`'s untar loop with direct
  `-I<tree>/usr/include` / `-L<tree>/usr/lib` references.
- Replace the round-2 `_install_tree_extract` genrule with a
  `pick_file`-style projection feeding the `cc_import` stubs.
- The cross-workspace A↔B transport is unaffected: the rendezvous
  already moves directories as REAPI `output_directories`
  (`docs/design/rendezvous.md`), so the install side moving from tar
  to Directory goes *with* that grain.

## Why this clears the bar the repo rule didn't

The repo-rule install was ruled out because it can't remote-execute
(loading-time work, no RBE, weaker hermeticity). TreeArtifact actions
are ordinary Bazel actions: they run on RBE, do no loading-time work,
and stay as hermetic as any other action. Same dedup win, none of the
disqualifiers.
