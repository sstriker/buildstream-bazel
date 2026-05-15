# `cmd/finalize-b` — the deliverable-handover step

`cmd/finalize-b` takes a converged project B at `--in <path>`
and writes a stripped standalone Bazel project at `--out <dest>`.
The rendered project B is the conversion-time artifact —
filled with `trace_build` genrules, `trace_load` rules,
intermediate filegroups, and a `bazel_dep` on
`rules_buildstream_bazel`. After convergence each element either
has fine-grained cc rules (and the surrounding scaffolding is
dead) or still depends on the round-2 publish/lookup machinery
(scaffolding stays).

`finalize-b` performs the post-convergence cleanup:

- **Per-element BUILD pruning.** For elements with fine cc rules
  (`cc_library` / `cc_binary` / `cc_import` / `cc_test`),
  strip:
  - `trace_load(...)` targets (action-time AC lookup; useless
    once the converter emitted real cc rules).
  - `genrule(... tags = ["trace_build"])` targets (configure +
    build + install + publish; useless once consumers point at
    the cc rules).
  - Conversion-era intermediate filegroups: `:install_tree.tar`,
    `:cmake_config_bundle`, `:pkg_config_bundle`, `:build_bazel`.
  - `load()` statements whose imported names are no longer
    referenced (typically the
    `@rules_buildstream_bazel//rules:traces.bzl` import).

- **MODULE.bazel pruning.** After per-element cleanup, walk
  every BUILD looking for any remaining
  `@rules_buildstream_bazel//` reference. When none survives,
  remove `bazel_dep(name = "rules_buildstream_bazel")` and the
  matching `local_path_override(...)` block from `MODULE.bazel`.
  Other `bazel_dep`s (e.g. `rules_cc`) are untouched.

The tool is **idempotent** — running it on an already-finalized
project produces byte-identical output. And it's **reversible**
by virtue of being non-destructive: `--in` is never modified;
`--out` is written from scratch and refuses to overwrite an
existing path.

## When NOT to strip

An element is "unconverged" — and its scaffolding stays — when
its BUILD has no fine-grained cc rule. Unconverged shapes:

- Operator hasn't run convergence yet; trace hasn't published.
- The element's converter genuinely can't recover a fine graph
  from the trace (some round-2 fallback shapes deliberately
  stay coarse; see the kind:cmake / kind:meson Phase B fallback
  docs).
- The element's downstream consumers depend on the
  install_tree.tar filegroup (a coarse-shape Bazel consumer not
  going through cc rules).

The detection is conservative: presence of any `cc_library` /
`cc_binary` / `cc_import` / `cc_test` rule flips the BUILD into
"converged, prune the scaffolding." Elements with NEITHER cc
rules NOR a trace_build are also left alone (no decisions to
make — finalize-b is a no-op for them).

## Operator workflow

The expected flow after the convergence loop reaches fixpoint:

```sh
tools/converge.sh --project-a $A --project-b $B \
    --cas-grpc-addr $CAS

# At this point $B has all the converted cc rules + every
# element's full conversion scaffolding. The cc rules are
# what downstream Bazel builds care about; the scaffolding is
# bloat.

build/bin/finalize-b --in $B --out $B_FINAL

# $B_FINAL is the standalone deliverable: pure Bazel, no
# rules_buildstream_bazel reference for fully-converged graphs.
# Commit it to a downstream repo, or hand it off to a consumer
# team.
```

The finalized project's Bazel build is independent of
buildstream-bazel — it has the conversion-time tooling carved
out, so consumers don't carry the converter's complexity.

## Reference

| File | Role |
|---|---|
| `cmd/finalize-b/main.go` | the tool |
| `cmd/finalize-b/main_test.go` | unit coverage of strip + preserve + idempotence |
| `scripts/meta-finalize-b.sh` | render gate (synthetic converged-project fixture) |
| `tools/converge.sh` | upstream — the loop that produces a converged project B |

## Status

Shipped in the cross-element configure-step bootstrap PR stack
— see [`ROADMAP.md`](../../ROADMAP.md).

Not in v1:

- **Smart `tools/` cleanup.** finalize-b doesn't yet drop the
  conversion-era binaries (`build-tracer`, `trace-publish`,
  `trace-lookup`) from `tools/BUILD.bazel`'s `exports_files`
  list, even when no surviving target references them. The
  bytes are still in `tools/` after finalize-b runs; an
  operator can manually delete them or extend finalize-b. Adds
  ~20 LoC; deferred until a fixture surfaces the value.

- **Renaming carved-out elements.** finalize-b preserves
  element names verbatim; an operator who wants to rename an
  element post-conversion does it by hand.

- **Reverse mode.** No `finalize-b --restore` that re-injects
  the scaffolding (the conversion-time machinery is
  reproducible from `write-a` + the `.bst` source, so a reverse
  mode isn't needed in practice).
