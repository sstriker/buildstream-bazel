<!--
A Marp slide deck companion to docs/design/conversion-architecture.md.

Render via:

  marp docs/design/conversion-architecture-slides.md            # HTML
  marp --pdf docs/design/conversion-architecture-slides.md      # PDF
  marp --preview docs/design/conversion-architecture-slides.md  # live preview

If marp isn't installed: `npm install -g @marp-team/marp-cli` or use the
VS Code "Marp for VS Code" extension. The file is plain markdown — readable
as-is in any markdown viewer, but the slide breaks (`---`) and metadata
(the YAML at the top) only resolve under marp.
-->
---
marp: true
theme: default
paginate: true
title: BuildStream → Bazel conversion architecture
---

# BuildStream → Bazel conversion architecture

A transition tool. Success state: you don't need it anymore — the
downstream team owns plain Bazel.

`docs/design/conversion-architecture.md` is the long form; this deck
is the 8-slide tour.

---

## The problem

- **BuildStream** drives FDSDK today. Every package builds in a
  sandbox via `bst`; deps flow through opaque install-tree archives;
  the graph is YAML (`.bst` per package).
- **Goal:** the same artifacts under **Bazel**, with native
  `cc_library` / `cc_binary` so Bazel's incremental build + remote
  execution + remote cache see the project at fine grain.
- Conversion isn't a one-shot translation — different element kinds
  expose different amounts of introspection. cmake/meson give us a
  build graph from sources alone; autotools and friends only reveal
  it by *running the build*.

---

## The two-project shape

```
write-a → project A + project B

project A   conversion-time meta-workspace
            (per-element converter genrules)
project B   the deliverable
            (cc_library / cc_binary, eventually pure Bazel)
```

- **Two independent bzlmod modules.** Nothing in Bazel's analysis
  graph connects A and B.
- **A is scaffolding.** Discarded after convergence; can be
  re-rendered from `.bst` on demand.
- **B is the artifact.** After `finalize-b`, no rules_buildstream_bazel
  reference; pure Bazel; downstream team owns it.

---

## The Bazel anti-pattern that forces a rendezvous

A `kind:cmake` element X with `find_package(Dep CONFIG)` against a
`kind:autotools` dep needs **Dep's build-config metadata at X's
pass-2 time** — but for a trace-based Dep that metadata only
exists after Dep's *pass-3 install build*.

Can't be a Bazel edge because:
- A and B are separate bzlmod modules.
- A repo rule in A that shells `bazel build` into B doesn't run on
  RBE and blocks Bazel startup.
- A structural A↔B edge means B can never be detached → B can
  never be the deliverable.

**Solution: indirect dependency through the REAPI ActionCache.**

---

## `trace_load` + `trace_build` — the rule pattern pair

Pass-3 writes:

```python
genrule(
    name = "greet_trace_build",
    cmd = "configure && make && make install && trace-publish ...",
    tags = ["trace_build"],     # queryable by the convergence driver
)
```

Pass-2 reads:

```python
trace_load(
    name = "greet_trace_load",
    srckey = "abc...",          # per-element narrowed digest
    expect_make_db = True,
    expect_config_bundle = True,
    trace_lookup = "//tools:trace-lookup",
)
```

Both live in `rules_buildstream_bazel/` — the in-repo Bazel module.

---

## Two cache layers, separate jobs

| Cache | What it catches | Key |
|---|---|---|
| **Bazel ActionCache** | Everything *within* one project build: incremental cc compiles, the converter genrule re-running on `.bst` edits, trace_load skipping when its inputs haven't changed | `(srcs digests, action_env, exec_props, …)` |
| **REAPI AC via SyntheticActionDigest** | The *trace + make-db* from pass-3 trace_build → pass-2 trace_load | `digest(Action{argv0="cmake-to-bazel/trace-publish-marker/v1", srckey, platform})` |
| **REAPI AC via SyntheticConfigDigest** | The *cmake-config bundle* synthesized from the install tree → cross-element `find_package` | `digest(Action{argv0="cmake-to-bazel/config-publish-marker/v1", srckey, platform})` |

The synthetic Action is never executed — only its digest is used as
a key/value lookup index. `--action_env=CONVERGE_GENERATION=<n>` is
the lever the driver bumps to force trace_load re-querying.

---

## The driver loop — `tools/converge.sh`

```
loop:
  ROUND++
  bazel build A's trace_loads with --action_env=CONVERGE_GENERATION=$ROUND
  bazel build A's converter genrules            # placeholder OR fine cc rules
  stage-b                                       # A/BUILD.bazel.out → B
  miss_markers = find $A/bazel-bin -name marker | grep -l "^miss"
  if empty(miss_markers): TERMINATE              # fixpoint reached
  bazel build B's trace_build targets            # configure + make + publish
  goto loop
```

- **Termination guaranteed** by the `.bst` DAG bound — each round
  resolves at least one frontier element.
- **Kind-agnostic** — `trace_load` rule + `trace_build` tag are the
  only dispatch surface. New kinds joining the trace-driven set
  work with no driver changes.
- **Offline mode** when `CAS_GRPC_ADDR` empty — equivalent to
  today's single-pass `bazel build A; stage-b; bazel build B`.

---

## `finalize-b` → end-state B

Converged-with-debris project B has:
- Fine cc rules (the deliverable).
- `trace_load` + `trace_build` + intermediate filegroups (debris).
- `bazel_dep` on `rules_buildstream_bazel`.

`finalize-b --in $B --out $B_FINAL` strips, per element with fine
cc rules:
- `trace_load(...)` targets.
- `genrule(... tags = ["trace_build"])` targets.
- Conversion-era filegroups (`:install_tree.tar`,
  `:cmake_config_bundle`, …).
- `load()` statements whose imported names are no longer used.

Then walks `MODULE.bazel`: if no `@rules_buildstream_bazel//`
reference survives, drop the `bazel_dep` + `local_path_override`.

**Result:** standalone Bazel. Downstream team owns plain Bazel.
The transition tool's success state.
