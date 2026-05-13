# Operator-side gazelle step — going from genrule to custom ruleset

## Context

The converter emits faithful, verbose Bazel rules — `cc_library` /
`cc_binary` / `cc_test` / `cc_import` for the well-known shapes, and
**`genrule` as the catch-all for everything else** (`add_custom_command`
in CMake, custom `command:` blocks in Meson, trace-recovered tool
invocations that don't fit cc/py kinds). That preserves the literal
input semantics — critical for incremental migration — but leaves the
project-B BUILD shape under-specified relative to what an operator
maintaining a Bazel project long-term would want. They probably want
`proto_library` + `cc_proto_library` for protobuf, `cc_grpc_library`
for gRPC, a custom `cc_embed_data` for resource embedding, etc.

Phase 7's plumbing (`# keep` markers, `cc_index.json`,
`python_modules.json`, MODULE.bazel `# gazelle:*` directives) makes the
emitted shape gazelle-friendly. This doc describes the **operator-side
workflow** for going from the converter's genrule-heavy shape to a
gazelle-managed custom-ruleset shape — and the MODULE.bazel overlay
mechanism that lets operators wire in their own `bazel_dep`s (gazelle,
gazelle_cc, custom rulesets) without the converter touching them.

## Workflow

```
+---------------------------------------+
| write-a                               |  render project A + B scaffolding;
|                                       |  emit overlay.MODULE.bazel stub
+--------------------+------------------+
                     |
                     v
+---------------------------------------+
| bazel build A                         |  converters emit per-element
|                                       |  BUILD.bazel.out
+--------------------+------------------+
                     |
                     v
+---------------------------------------+
| stage A→B                             |  copy converted BUILDs into B
+--------------------+------------------+
                     |
                     v
+---------------------------------------+
| build-cc-index B                      |  populate tools/cc_index.json +
|                                       |  tools/python_modules.json
+--------------------+------------------+
                     |
                     v
+---------------------------------------+
| (operator-driven)                     |  one-time, before first
| bazel run //:gazelle                  |  bazel build B run
+--------------------+------------------+
                     |
                     v
+---------------------------------------+
| bazel build B                         |  the operator's everyday
|                                       |  builds from this point
+---------------------------------------+
```

Steps 1-4 are automated (write-a + the orchestrator). Step 5 is
**operator-driven, one-time, opt-in** — described in detail below.
Step 6 is the operator's normal Bazel workflow forever after.

## The MODULE.bazel overlay

`write-a` emits two MODULE.bazel-flavored files at project B's root:

- **`MODULE.bazel`** — converter-owned. Re-rendered on every write-a
  run. Carries the rule-set-agnostic `bazel_dep`s the converter knows
  it emits (`rules_cc`, conditionally `rules_python`,
  `bazel_skylib`); carries the gazelle-config directives; ends with
  an unconditional `include("//:overlay.MODULE.bazel")`.

- **`overlay.MODULE.bazel`** — **operator-owned**. Written by
  write-a once with a comment-only stub if it doesn't exist. The
  converter never touches it after that initial write. This is where
  operators put:
  - Extra `bazel_dep()` declarations (gazelle, gazelle_cc, custom
    rulesets like `rules_proto`, `rules_grpc`).
  - `use_extension()` blocks for custom Bazel module extensions.
  - `register_toolchains()` / `register_execution_platforms()` for
    operator-supplied toolchains.

The unconditional `include()` means an operator can drop a single
declaration into `overlay.MODULE.bazel` without editing the
converter-owned `MODULE.bazel` — and the next write-a run won't fight
them.

**Why a file, not a flag on the .bst / project.conf**: input files
(`.bst`, `project.conf`) describe the source-side intent of the
build; they shouldn't carry post-conversion Bazel-side knobs. The
overlay file lives in project B alongside the actual MODULE.bazel,
matching the operator's mental model ("project B is mine — I edit it
here").

## Genrule → custom ruleset rewriting

This is the load-bearing part of the post-conversion workflow.

### Baseline: every genrule carries `# keep`

Phase 7a emits a whole-rule `# keep` marker on every `genrule(...)`
(closing-paren suffix comment). gazelle honors that marker and won't
touch the rule on subsequent `gazelle fix` runs. **By default, the
converter's literal-CMake fidelity wins.**

### Opt-in: remove `# keep`, let gazelle's custom extension rewrite

To convert specific genrules into native rules:

1. **Wire the custom extension into `overlay.MODULE.bazel`**:
   ```
   bazel_dep(name = "gazelle", version = "0.40.0")
   bazel_dep(name = "gazelle_proto", version = "...")  # third-party
   ```
   (Or your own custom gazelle plugin — see `bazel-contrib/`
   ecosystem for examples.)

2. **Remove the `# keep` marker** from the genrule(s) you want
   rewritten. e.g., in
   `elements/myelem/BUILD.bazel`:
   ```diff
    genrule(
        name = "myelem_proto_gen",
        srcs = ["myelem.proto"],
        outs = ["myelem.pb.cc", "myelem.pb.h"],
        cmd = "$(location @protobuf//:protoc) ...",
   -)  # keep
   +)
   ```

3. **Run `bazel run //:gazelle`** — the custom extension scans the
   un-kept genrules, recognizes the `protoc` cmd pattern, and
   rewrites:
   ```diff
   -genrule(
   -    name = "myelem_proto_gen",
   -    srcs = ["myelem.proto"],
   -    outs = ["myelem.pb.cc", "myelem.pb.h"],
   -    cmd = "$(location @protobuf//:protoc) ...",
   -)
   +proto_library(
   +    name = "myelem_proto",
   +    srcs = ["myelem.proto"],
   +)
   +cc_proto_library(
   +    name = "myelem_cc_proto",
   +    deps = [":myelem_proto"],
   +)
   ```

4. **Update downstream `deps`**. The downstream `cc_library` that
   used to depend on `:myelem_proto_gen` now needs to depend on
   `:myelem_cc_proto`. gazelle_cc's header-scan resolver
   (Phase 7b/c) handles this automatically when the operator
   re-runs `gazelle` — `#include "myelem.pb.h"` resolves via
   `cc_index.json` (Phase 7c had added `myelem.pb.h →
   //elements/myelem:myelem_cc_proto` when the custom extension
   wrote the new rule).

5. **`bazel build`** — verify the rewrite is functionally
   equivalent.

### Realistic candidates for rewriting

| Input pattern | Genrule cmd matches | Operator's custom rule |
| --- | --- | --- |
| protobuf | `protoc ... --cpp_out=...` | `proto_library` + `cc_proto_library` |
| gRPC | `protoc ... --grpc_out=...` | `cc_grpc_library` |
| flatbuffers | `flatc ... --cpp ...` | `flatbuffer_cc_library` |
| Cap'n Proto | `capnp compile ...` | `capnp_cc_library` |
| SWIG bindings | `swig ... -python ...` | `py_swig_library` |
| Resource embed | `xxd -i` / `objcopy --add-section` | `cc_embed_data` |
| Code generators | project-specific patterns | project-specific custom rules |

Each of these has a well-known cmd-line signature; a gazelle
extension that matches on the signature and emits the canonical rule
is roughly 50-100 lines of Go per pattern.

## Targeted vs full-scan gazelle invocations

Cold full-scan `gazelle fix` on a 100-element project takes seconds
to a minute (it walks every `BUILD.bazel`, reparses, decides
rewrites). Two reductions:

1. **`cc_indexfile`** + **`python_module_mapping`** (Phase 7b's
   MODULE.bazel directives) — gazelle uses the precomputed indexes
   for resolution instead of re-scanning every BUILD's hdrs / py
   srcs. Big savings on the resolver side.

2. **Per-package targeting**:
   ```
   bazel run //:gazelle -- elements/myelem elements/other
   ```
   only rewrites the listed packages. ~O(packages_listed) instead
   of O(workspace). When the operator knows which elements they
   want gazelle to touch — e.g., the protobuf ones — this is the
   fast path.

The orchestrator (`orchestrator/cmd/orchestrate`) already tracks
`res.Converted` (the elements that re-converted on this run). A
future Phase 8b could plumb that list into a targeted gazelle
invocation as an optional step — `--enable-gazelle`-style opt-in,
default off until the custom-extension story stabilizes per
operator preference.

## Why we don't run gazelle by default

Three reasons:

1. **Custom-extension wiring is operator-specific**. The
   `bazel_dep`s for `gazelle_cc`, `gazelle_proto`,
   `<custom-rule-set>_gazelle_plugin`, etc., aren't knowable at
   convert time — they're part of the operator's downstream
   tooling choice. The overlay-file mechanism lets each operator
   declare what they want without the converter prescribing.

2. **Genrule fidelity is sometimes load-bearing**. The literal
   `cmd = "..."` shape preserves CMake's exact invocation, which
   is critical for early-incremental-migration debugging. Forcing
   a rewrite to `proto_library` at convert time would lose the
   debuggability of "what did cmake do here, exactly". The
   operator opts into the rewrite when they're ready to take
   ownership.

3. **Bazel-dep churn**. Running gazelle at convert time means
   running a bazel invocation inside the converter pipeline,
   which complicates the hermeticity story (bazel needs internet
   to fetch its registry index, deps, etc.). Keeping it as a
   separate operator step keeps the converter's action graph
   tight.

## Cross-references

- `docs/design/build-output-conventions.md` — the contract this
  workflow targets.
- `ROADMAP.md` — Phase 8 (operator overlay) and Phase 8b (queued:
  optional orchestrator-driven gazelle step).
- `cmd/write-a/main.go`'s `moduleBazelB` + the new `overlay.MODULE.bazel`
  emission.
- `cmd/build-cc-index/main.go` — Phase 7c's index-population tool
  the gazelle resolution depends on.
