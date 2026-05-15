# `kind:meson` round-2 fallback for unsupported native-render shapes

Phase A of the kind:meson native render (`converter/cmd/convert-element-meson`)
emits typed Tier-1 failures for shapes the v1 lowering can't model:
`unsupported-meson-subproject`, `unsupported-meson-custom-target`,
`unsupported-meson-generated-sources`, `unsupported-meson-cross-compile`,
`unresolved-meson-dependency`, `unsupported-meson-target-type`. Without
the fallback these exit the per-element converter genrule Tier-1 and
exclude the element from the rendered project.

Phase B's goal: replace the exclusion with a **round-2-style coarse-
genrule fallback** that still produces a buildable downstream
artifact, mirroring how `kind:cmake` Phase B handles unliftable
`execute_process` calls (`docs/design/cmake-execute-process-round2-fallback.md`).
Same architectural shape; the kind-specific bits are how the
placeholder enumerates its per-target stubs (install-plan-driven
instead of codemodel-driven) and the install genrule's command
shape.

This doc captures the architectural mismatch, the placeholder
shape, the kind-specific bits, and the staged-implementation
recipe. The kind-agnostic rendezvous mechanism is unchanged from
`docs/design/autotools-round2-rendezvous.md`; only the kind-specific
wiring is new.

## The architectural shape

`kind:meson` round-2 fallback uses the same A-converter + B-install
+ round-2-rendezvous split that kind:cmake's Phase B already
established:

- **Project A — converter genrule.** Runs `meson setup` + parses
  the intro-*.json bundle + lowers into IR. Decides per-element
  at action time whether to emit native cc rules
  (`convert-element-meson` Phase A) or the placeholder shape
  in `BUILD.bazel.out`. The decision is "what shape of
  `BUILD.bazel.out` to write," not "what build to run."
- **Project B — install genrule.** `build-tracer` wraps
  `meson setup --prefix=/ --libdir=lib` + `ninja` +
  `meson install --destdir=...`. Outputs `install_tree.tar`
  (single tar; per-tag split is a future optimisation). Inline
  `trace-publish` lands the AC entry keyed by
  `SyntheticActionDigest(srckey)`.
- **Round-2 rendezvous.** A's converter genrule consumes
  `@trace_<elem>//:trace` via load-time `_trace_repo` lookup
  against the REAPI ActionCache. Same mechanism + same helpers
  as kind:cmake / kind:autotools round-2.

write-a uniformly emits both genrules for every kind:meson
element when round-2 fallback is enabled. Bazel evaluates B's
install genrule lazily; it's only built when A's
`BUILD.bazel.out` references its outputs. Native-render elements
emit cc rules that don't reference `install_tree.tar`, so B's
install genrule sits unbuilt; fallback elements emit the
placeholder shape that does reference it, so B's install
genrule runs.

### Convergence: trace turns refusal into refinement

Same property as kind:cmake's Phase B: refused elements aren't
permanently coarse. The first build of a given srckey hits the
miss path (trace empty → placeholder shape); after pass-3
publishes the trace + AC entry, subsequent builds of the same
srckey *anywhere* (CI, dev laptop, fresh executor) see the trace
at A's load time and the converter can refine.

v1 of the converter doesn't do that refinement; it always emits
the placeholder shape when lowering refuses. The convergence
direction is queued as a research item (kind-agnostic — same
follow-on touches kind:cmake's converter too).

## The kind-specific bits

What's new for `kind:meson` Phase B (everything else reuses the
kind-agnostic infra):

- **`mesonSrckeyPatterns()`** (`cmd/write-a/handler_meson_round2.go`).
  Content-inclusion rules for srckey: `meson.build` + nested,
  `meson_options.txt` / `meson.options`, header families
  (`**/*.h`, `*.hpp`, `*.hxx`, `*.hh`). Compile sources are
  path-only (trace records the compile command regardless of source
  bytes). Mirrors `cmakeSrckeyPatterns` / `autotoolsSrckeyPatterns`
  in shape and default-deny semantics.

- **`wrapMesonPipelineCmds(cmds)`**. Sister of
  `wrapCmakePipelineCmds` / `wrapAutotoolsPipelineCmds`. Wraps
  meson's `setup / build / install` sequence under build-tracer
  with `--normalize-prefix` for byte-stable traces. The tracer-out
  path lives under `$$MESON_TRACE` so a pipeline that has
  kind:meson alongside kind:cmake / kind:autotools elements
  doesn't have the tracers stomp on each other's tmpfile.

- **Install genrule shape** (`mesonRound2InstallBuild`). Pins
  `meson setup --prefix=/ --libdir=lib` so the install destinations
  resolve to clean relative paths (`lib/libfoo.a`, `bin/foo`,
  `include/foo.h`) that match what the placeholder shape in
  A's BUILD.bazel.out references via `install_tree/<dest>`.
  Without the pin, multiarch hosts (debian / ubuntu) produce
  `lib/x86_64-linux-gnu/...` paths and the host's `/usr/local`
  prefix gets baked in, and A's computed paths drift from B's
  actual install bytes. `--parallel 1` is unnecessary for meson
  / ninja today (ninja is deterministic in its scheduling on a
  given tracer wrap) so we leave it out; if a future fixture
  surfaces trace drift, add a `--single-thread` ninja flag.

- **A render gate** at `scripts/meta-meson-round2-fallback.sh`,
  modelled on `meta-cmake-round2-fallback.sh`. Asserts the
  meson-specific shape (placeholder BUILD.bazel.out content,
  meson-flavoured install genrule cmd) and exercises the
  standalone converter against a refusal-triggering fixture
  to confirm strict mode refuses while the fallback emits
  the install-plan-driven placeholder.

### The "richer signal" — install_plan vs codemodel

cmake's Phase B placeholder reads `Target.Install.Destinations[0].Path`
+ `Target.NameOnDisk` and infers the artefact type from the
destination directory (lib → archive, bin → executable). The
inference is mostly right but fragile around shared libraries
(SONAMEs land under `lib` with `.so.N` suffixes; the inferrer has
to know about all the basename variants).

Meson's `intro-install_plan.json` carries the `tag` field
directly:

```json
{
  "targets": {
    "/bd/libfoo.a":   {"destination": "{libdir_static}/libfoo.a",    "tag": "devel"},
    "/bd/libfoo.so":  {"destination": "{libdir_shared}/libfoo.so",   "tag": "runtime"},
    "/bd/foo":        {"destination": "{bindir}/foo",                "tag": "runtime"}
  },
  "headers": {
    "/src/include/foo.h": {"destination": "{includedir}/foo.h",      "tag": "devel"}
  }
}
```

`tag` values are well-defined (`runtime`, `devel`, `man`, `i18n`,
`doc`, `tests`) and partition the install set unambiguously. The
fallback emitter (`converter/cmd/convert-element-meson/lower_fallback.go`)
classifies on (tag, basename) instead of on (destination prefix,
basename), which makes SONAME-versioned shared libraries
(`libfoo.so.1.2.3`) and macOS dylibs (`libfoo.dylib`) fall out as
clean shared-library cases without per-destination inference.

### Placeholder shape (per-target rules from install-plan)

```python
# One-time tar extract genrule. Outputs are enumerated from
# intro-install_plan's `targets` + `headers` sections with
# placeholders ({libdir_static}, {bindir}, {includedir}, ...)
# resolved against intro-buildoptions's `section: directory`
# rows.
genrule(
    name = "_install_tree_extract",
    srcs = ["install_tree.tar"],
    outs = [
        "install_tree/lib/libfoo.a",
        "install_tree/bin/foo-bin",
        "install_tree/include/foo.h",
    ],
    cmd = "mkdir -p $(RULEDIR)/install_tree && tar -C $(RULEDIR)/install_tree -xf $(location install_tree.tar)",
    tags = ["meson-codegen-target-fallback", "meson-codegen-target-fallback-extract"],
    visibility = ["//visibility:private"],
)

# Per-target stubs. Kind dispatch follows (tag, basename):
cc_import(
    name = "foo",                      # tag=devel + lib*.a
    static_library = "install_tree/lib/libfoo.a",
    hdrs = ["install_tree/include/foo.h"],
    tags = ["meson-codegen-target-fallback"],
    visibility = ["//visibility:public"],
)
sh_binary(
    name = "foo-bin",                  # tag=runtime, no lib prefix
    srcs = ["install_tree/bin/foo-bin"],
    tags = ["meson-codegen-target-fallback"],
    visibility = ["//visibility:public"],
)
```

Headers from the install-plan's `headers` section fold into every
library's `hdrs` (the v1 coarse fold). Per-library header attribution
would require meson to expose which target a header belongs to, which
the introspection schema doesn't carry today.

Subproject-tagged entries are filtered — native lowering refuses
subprojects entirely, and the fallback inherits the same filter.
Subproject artefacts aren't part of the consumer-visible install
contract.

Unresolved-placeholder entries are dropped silently — if
intro-buildoptions doesn't carry a value for `{weirdthing}`, the
emitter skips the row rather than emitting a stub with a literal
placeholder the install_tree.tar can't satisfy.

## Compile-flag fidelity

The round-2 install genrule builds the project with whatever
ninja/gcc default flags meson selected from the project's
`meson.build` + `default_options`. Downstream Bazel consumers can't
override `-O3` vs `-O2` etc. — they get whatever the install
genrule picked. Mitigation: round-2 is the *fallback*, not the
primary path. Native render (Phase A) still applies for projects
whose introspection survives the v1 lowering.

## Trade-offs and known gaps

- **Doubled build graph for fallback-enabled elements.** Each
  kind:meson element with fallback enabled gets two genrules
  (converter + install). The install genrule only builds when
  consumers pull on it, but the analysis-phase cost is doubled.
  Mitigation: the fallback flag is opt-in — projects whose
  introspection lowers cleanly shouldn't enable it.

- **Trace publish overhead at first build.** The round-2 install
  genrule runs meson setup + ninja + install every first-time
  srckey, even when the native render succeeded. Mitigation: the
  install genrule is only built when the placeholder BUILD.bazel.out
  actually points at it, which only happens on Phase-A refusal.

- **Coarse header fold.** Headers from `install_headers()` fold into
  every library's `hdrs` rather than the specific target that
  declared them. Downstream `#include <foo.h>` resolves through any
  library that gets folded, which is structurally correct but loses
  per-target attribution. The fold becomes precise once meson's
  introspection grows per-header target attribution (or once a real
  fixture forces a more sophisticated lift).

- **Multi-target SONAME variants.** A library declared as
  `both_libraries()` emits both `libfoo.a` (devel) and `libfoo.so`
  (runtime). Each gets its own cc_import stub (`foo` for the static,
  `foo` for the shared — collision!). v1's emitter only sees one
  install_plan entry per archive output, so this collision is rare
  in practice; when it surfaces, the fix is to use distinct target
  names in meson.build (`static_library('foo_static')` +
  `shared_library('foo_shared')`).

- **Fixture fragility.** Building the round-2 install genrule in
  tests requires real meson + ninja on the CI runner. The render-half
  acceptance gate (`scripts/meta-meson-round2-fallback.sh`) is
  render-only — it only invokes write-a and asserts the emitted
  BUILD shape, no bazel build half — so it stays runnable without
  bazel on the host. A live-AC gate (cmake's sister
  `tools/e2e-meta-autotools-round2-live.sh` covers the publish/lookup
  wire contract kind-agnostically) is the one that exercises a real
  `bazel build`; a kind:meson sibling lands when an FDSDK fixture
  forces it.

## Reference

| File | Role |
|---|---|
| `cmd/write-a/handler_meson.go` | `mesonHandler` — primary kind:meson render + dispatches into the fallback when `--meson-round2-fallback` is set |
| `cmd/write-a/handler_meson_round2.go` | `mesonSrckeyPatterns`, `wrapMesonPipelineCmds`, `mesonRound2InstallBuild`, `renderMesonRound2B` — kind-specific scaffolding |
| `cmd/write-a/handler_pipeline_round2.go` | kind-agnostic round-2 helpers (reused) |
| `cmd/write-a/handler_cmake_round2.go::wrapCmakePipelineCmds` | sibling pattern for `wrapMesonPipelineCmds` |
| `converter/cmd/convert-element-meson/lower_fallback.go` | install-plan-driven placeholder emitter |
| `converter/cmd/convert-element-meson/introspect.go` | `InstallPlan` + `BuildOption` parsing for intro-install_plan.json / intro-buildoptions.json |
| `converter/cmd/convert-element-meson/main.go` | `--unsupported-target-fallback` flag + the Tier-1 → placeholder dispatch in `run()` |
| `cmd/build-tracer/main.go` | already kind-agnostic; reused as-is |
| `cmd/trace-publish/main.go` | already kind-agnostic |
| `cmd/trace-lookup/main.go` | already kind-agnostic |
| `internal/tracenorm/synthkey.go` | `SyntheticActionDigest(srckey)` — already kind-agnostic |
| `scripts/meta-meson-round2-fallback.sh` | render gate |
