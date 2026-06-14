# Writing a codegen recognizer

When the converter recovers a code-generation `add_custom_command` from
`build.ninja`, its default move is to emit a hermetic `genrule` that re-runs
the generator. For generators that have a **native Bazel rule** — protoc
(`proto_library` + `cc_proto_library`), and in principle `flatc`, `thrift`,
Qt `moc`, … — that's a missed opportunity: the idiomatic rule is more
legible, gives Bazel the real dependency graph, and is what a `gazelle`
maintenance pass already understands.

A **codegen recognizer** maps one such generator invocation to its native
rule(s). Recognizers live in a registry; adding one for a new tool is a
self-contained change — no new `ir.Kind`, no bespoke emit path. There are two
ways to add one, both feeding the same registry:

- **In Go (first-party):** implement the `CodegenRecognizer` interface and
  register it in-tree. Covered first, below — it's also the substrate the
  Starlark path rides.
- **In Starlark (operator, no recompile):** drop a `*.star` file next to your
  project and point `--recognizers` at it. Covered in
  [Operator recognizers in Starlark](#operator-recognizers-in-starlark-no-recompile).
  This is the path for adding a generator your converter binary doesn't ship
  support for, without rebuilding it.

This doc is the how-to for both.

Behaviour today is gated behind the opt-in `--recognize-codegen` flag (off by
default; see [`ROADMAP.md`](../ROADMAP.md) for the rollout to default-on +
`--fidelity` gating). The mechanism below is stable regardless of the flag's
default.

## The moving parts

All of these are in `converter/internal/lower/codegen_recognizer.go` unless
noted.

- **`CodegenCommand`** — the recognizer's authoritative view of one recovered
  custom-command: the generator `Driver` (argv[0] basename, e.g. `protoc`),
  its `Args`, the recovered input `Srcs`, the cmake-recorded `Outs`, the Bazel
  `Pkg`, and any pre-resolved `ProtoDeps`. Built by the dispatch from the
  **rewritten** command (the `cd <build> &&` prefix and build-dir paths
  already stripped), so the driver basename and flags are clean.
- **`CodegenResult`** — what a recognizer returns: the native rule
  `Targets` and the `ConsumerDeps` (labels a `#include`r of the generated
  output should depend on).
- **`CodegenRecognizer`** — the interface you implement:

  ```go
  type CodegenRecognizer interface {
      Name() string                              // diagnostics
      Match(cmd CodegenCommand) bool             // claim by DRIVER + argv shape
      Lower(cmd CodegenCommand) (CodegenResult, error)
  }
  ```

- **`codegenRegistry`** — the ordered `[]CodegenRecognizer`. First `Match`
  wins; no match → the generic genrule path. Register your recognizer here.
- **`recognizeCodegen`** — walks the registry, returns
  `(result, matched, err)`. `matched && err == nil` → native rule;
  `matched && err != nil` → the tool was recognized but this invocation is
  non-standard (the caller refuses under `--fidelity=strict` / falls back to
  the genrule under best-effort); `!matched` → genrule.

The native rule itself rides a generic substrate (`converter/ir/types.go`):

- **`ir.KindNativeRule`** + **`ir.NativeRuleSpec`** (`Kind`, `LoadFrom`,
  `LoadSymbol`, `Attrs []NativeAttr`) + **`ir.NativeAttr`** (`Name` + `Str`
  *or* `List`). One `ir.Target` per emitted rule, `Kind: ir.KindNativeRule`,
  `NativeRule: &ir.NativeRuleSpec{...}`. The emitter renders the call and
  **auto-emits the `load()`** for every distinct non-empty `LoadFrom`
  (`emitNativeRuleLoads`, `converter/emit/bazel/emit.go`); `LoadSymbol`
  defaults to `Kind`. A built-in rule (no load) leaves `LoadFrom` empty.

## Two rules a recognizer must follow

### 1. Match on the TOOL, not the file extension

The converter has the decisive signal a source-file-dispatched tool like
gazelle lacks: the **actual argv** the build ran. Key `Match` on the driver
basename + a flag, e.g. protoc's recognizer:

```go
func (protocCppRecognizer) Match(cmd CodegenCommand) bool {
    if !strings.HasPrefix(filepath.Base(cmd.Driver), "protoc") {
        return false
    }
    return hasFlagPrefix(cmd.Args, "--cpp_out")
}
```

This is what disambiguates a common extension fed to *different* tools (a
`.xml` to `uic` vs `gdbus-codegen` vs `glib-compile-resources`): the argv
decides, not the suffix.

### 2. Be the OUTPUT AUTHORITY — derive, then cross-check

A recognized tool has a predictable output convention, so `Lower` should
**derive** the output set from the input(s) + flags (protoc: `foo.proto` +
`--cpp_out` → `foo.pb.{cc,h}`) and **cross-check it against what cmake
recorded** (`cmd.Outs`). A mismatch means a non-standard invocation — return
an error so the dispatch refuses/falls back rather than emitting a
non-faithful rule:

```go
derived := []string{base + ".pb.cc", base + ".pb.h"}
if err := derivedOutputsConsistent(cmd.Outs, derived); err != nil {
    return CodegenResult{}, fmt.Errorf("protoc-cpp: %w", err)
}
```

Corollary: a tool whose output **names depend on file content** (not
derivable from input + flags) gets **no recognizer** — it stays on the
genrule fallback.

## Consumer wiring is automatic — return `ConsumerDeps`

You do not wire consumers yourself. Return the consumer label(s) in
`CodegenResult.ConsumerDeps` (protoc: `:foo_cc_proto`). The dispatch records
each output → that label in `cc.OutToNativeConsumerDep`; a target that
`#include`s a generated header then gets a **direct `deps` edge** to your rule
(via `resolveCodegenHeaderConsumers` → `Package.NativeRuleConsumerLabels` →
the `--split-packages` transform). It is *not* routed through the
file-oriented `generated_includes` `textual_hdrs` wrapper (that model is for
genrule outputs, which exist as standalone files; a native rule produces its
headers internally). No `# keep` is added to the edge — the native rule is the
idiomatic target a `gazelle` pass resolves the `#include` to on its own.

> Consumer wiring runs in the `--split-packages` emit path (the
> corpus/production path), as the existing tablegen-consumer wiring does.

## Worked example: a `flatc` recognizer (sketch)

```go
// flatcCppRecognizer maps `flatc --cpp … schema.fbs` to ... the idiomatic
// FlatBuffers rule(s) for your environment. (Shown as a shape, not a drop-in:
// pick the rule + load label your MODULE provides.)
type flatcCppRecognizer struct{}

func (flatcCppRecognizer) Name() string { return "flatc-cpp" }

func (flatcCppRecognizer) Match(cmd CodegenCommand) bool {
    return strings.HasPrefix(filepath.Base(cmd.Driver), "flatc") &&
        hasFlagPrefix(cmd.Args, "--cpp")
}

func (r flatcCppRecognizer) Lower(cmd CodegenCommand) (CodegenResult, error) {
    fbs := soleInput(cmd.Srcs, ".fbs") // single-input convention
    if fbs == "" {
        return CodegenResult{}, fmt.Errorf("flatc-cpp: no single .fbs input in %v", cmd.Srcs)
    }
    base := strings.TrimSuffix(filepath.Base(fbs), ".fbs")
    // OUTPUT AUTHORITY: flatc --cpp's convention is schema.fbs -> schema_generated.h.
    derived := []string{base + "_generated.h"}
    if err := derivedOutputsConsistent(cmd.Outs, derived); err != nil {
        return CodegenResult{}, fmt.Errorf("flatc-cpp: %w", err)
    }
    name := base + "_fbs"
    rule := ir.Target{
        Name: name, Kind: ir.KindNativeRule,
        NativeRule: &ir.NativeRuleSpec{
            Kind:     "flatbuffer_cc_library",
            LoadFrom: "@flatbuffers//:build_defs.bzl",
            Attrs: []ir.NativeAttr{
                {Name: "srcs", List: []string{filepath.Base(fbs)}},
                {Name: "visibility", List: []string{"//visibility:public"}},
            },
        },
    }
    return CodegenResult{Targets: []ir.Target{rule}, ConsumerDeps: []string{":" + name}}, nil
}
```

Then register it (first match wins, so order by specificity):

```go
var codegenRegistry = []CodegenRecognizer{
    protocCppRecognizer{},
    flatcCppRecognizer{},
}
```

## Checklist for a new recognizer

1. **Implement** `CodegenRecognizer` in `codegen_recognizer.go` — `Match` on
   driver + flag, `Lower` deriving + cross-checking outputs and returning the
   native `Targets` + `ConsumerDeps`.
2. **Register** it in `codegenRegistry`.
3. **Unit tests** in `codegen_recognizer_test.go` — `Match` positives/negatives,
   `Lower` shape, the import/dep threading, and the output cross-check error.
4. **Fixture** under `converter/testdata/sample-projects/<tool>-recognize/`
   (producer only) and, if the tool feeds a consumer, a `<tool>-consumer/`
   fixture (a target that `#include`s the generated header).
5. **Gate** `scripts/meta-cmake-<tool>-recognize.sh` (model on
   `meta-cmake-protoc-recognize.sh` / `meta-cmake-protoc-consumer.sh`):
   a control half (no flag → genrule), a recognizer half (flag → native
   rule, genrule gone), and a `bazel build` half against the MODULE deps your
   rule's `LoadFrom` needs. The consumer gate runs under `--split-packages`.
6. **Run the dev loop** in [`CONTRIBUTING.md`](../CONTRIBUTING.md) — build /
   vet / gofmt / `make staticcheck` / `make lint-complexity` / `go test`,
   then your gate.

## Operator recognizers in Starlark (no recompile)

Everything above adds a recognizer *in the converter's source*. Operators who
can't (or don't want to) rebuild the binary can add one as a **Starlark file**
loaded at runtime — same registry, same contract, zero recompile:

```
convert-element-cmake --recognize-codegen --recognizers 'recognizers/*.star' …
```

`--recognizers` is a glob of `*.star` files; each is compiled at startup and
appended to the registry **after** the built-ins (so first-party recognizers
win; operator scripts extend). It requires `--recognize-codegen` to take effect
(the master switch is unchanged). A glob that matches nothing, or a file that
won't compile / is missing `match` or `lower`, is a hard startup error — a
broken `--recognizers` fails loudly, it doesn't silently no-op.

A recognizer script defines two top-level functions and uses two builtins
(`native_rule(...)`, `result(...)`) — that's the whole API:

```python
def match(cmd):            # cmd.driver, cmd.args, cmd.srcs, cmd.outs, cmd.pkg, cmd.proto_deps
    return cmd.driver.startswith("protoc") and \
           any([a.startswith("--cpp_out") for a in cmd.args])

def lower(cmd):
    base = cmd.srcs[0].rsplit("/", 1)[-1][:-len(".proto")]
    return result(
        targets = [
            native_rule("proto_library", base + "_proto",
                        load_from = "@protobuf//bazel:proto_library.bzl",
                        attrs = {"srcs": [base + ".proto"], "visibility": ["//visibility:public"]}),
            native_rule("cc_proto_library", base + "_cc_proto",
                        load_from = "@protobuf//bazel:cc_proto_library.bzl",
                        attrs = {"deps": [":" + base + "_proto"], "visibility": ["//visibility:public"]}),
        ],
        consumer_deps = [":" + base + "_cc_proto"],
        derived_outputs = [base + ".pb.cc", base + ".pb.h"],
    )
```

The complete, runnable version is [`recognizers/protoc.star`](../recognizers/protoc.star)
— copy it and change `match()` + the rule shape to teach the converter a new
generator.

The same rules apply as for Go recognizers, and a couple of properties make the
Starlark path safe by construction:

- **`native_rule(kind, name, load_from=, load_symbol=, attrs={})`** maps 1:1 to
  the `NativeRuleSpec` substrate; the `load()` is auto-emitted. `attrs` is a
  dict of attr-name → string *or* list-of-strings, emitted in insertion order.
- **Output authority stays first-party.** The script declares `derived_outputs`;
  the Go host cross-checks them against cmake's recorded outputs and falls back
  to the genrule (best-effort) / refuses (strict) on a mismatch — the soundness
  gate is *not* in the script.
- **Sandboxed + deterministic.** Starlark has no filesystem, clock, or network,
  so a recognizer can't break hermeticity; a buggy script can only decline (its
  command falls through to the next recognizer / the genrule), never corrupt the
  build.
- **Consumer wiring is automatic**, exactly as for Go recognizers: return
  `consumer_deps` and a `#include`r of a generated header gets a direct deps edge
  to the rule.

Gate: `scripts/meta-cmake-recognizer-starlark.sh` loads an operator `.star` for
a generator the built-ins don't know (`gen_pb`), asserts it fires + that the
template compiles, and bazel-builds the result.

## The complement: host codegen tools WITHOUT a native rule (the `tools` map)

A recognizer is for a generator that *has* a native Bazel rule. The other
half of fidelity is the generator that **doesn't** — a project's own
python/perl script, a `flatc`/`thrift` you have no rules for, an absolute
host-install binary. Those legitimately stay `genrule`s (the live-rerun rung
of the [fidelity ladder](design/codegen-fidelity-ladder.md)), but the genrule
must drive a **hermetic Bazel tool**, not the host binary cmake happened to
resolve at configure time. Left raw, the recovered command keeps a host PATH
name (`flatc …`) or an absolute host path (`/opt/host/bin/protoc …`) — which
is invisible under a sandboxed `/tmp` and breaks on a clean executor.

You hermeticize such a tool with a `tools` section in the imports manifest
(`--imports-manifest`). It maps a command token — by **driver basename** or
**absolute path** — onto the Bazel label that provides the tool:

```json
{
  "version": 1,
  "tools": [
    { "match": "flatc",                 "label": "@flatbuffers//:flatc" },
    { "match": "/opt/host/bin/gen.py",  "label": "//tools:gen" }
  ]
}
```

- A **bare basename** (`flatc`, `python3`, `perl`) matches any command token
  whose basename equals it — a PATH-resolved driver *or* an absolute host
  path to the same program.
- An **absolute path** matches that exact token (the in-tree-script-by-
  absolute-path shape).

A matched token is rewritten to `$(execpath <label>)` and the label is added
to the genrule's `tools`, so Bazel stages and runs the hermetic tool. This
flows through the **single tool-swap chokepoint** (`rewriteToolFromTarget`),
so it reaches *every* genrule recovery path — the standalone
`add_custom_command` path and the ninja build-dir-copy path alike — with no
per-path opt-in. It composes with the in-tree channel (a tool built *in* the
graph already lifts to `$(location :name)`) and the imported-library channel
(an `Export.LinkPaths` hit); the `tools` map is the explicit fallback for the
no-native-rule case those two don't cover.

A relative multi-component token (e.g. an in-tree output `build/gen/flatc`)
is **not** basename-matched — only its verbatim form — so a tool name never
accidentally rewrites a same-basenamed output.

**You don't have to find these by hand.** The converter can't *invent* a
label for a host tool, but it auto-**detects** the ones still un-hermeticized:
any recovered genrule whose driver is a host tool (not swapped to
`$(execpath)`/`$(location)`, not a benign `cmake -E`/shell builtin) emits a
`host-codegen-tool` entry in `conversion-todos.json`
(`--conversion-todos-report`), grouped per driver, with the exact `tools`
entry to paste in `suggested_shape` (keyed by the deterministic **basename**).
An absolute-path driver is `actionable` (it can't resolve on a clean
executor); a PATH-resolved basename is `improvement`. Add the manifest entry
and the todo disappears on the next convert.

The todo's `origin` evidence distinguishes where the tool came from, because
the remedy differs:
- **`host`** — a host-PATH name or host-install absolute path: add a `tools`
  entry mapping it to the providing label (a BCR module's tool, a wrapper
  rule).
- **`prefix`** — it resolved from the orchestrator's synth-prefix, so it's a
  **cross-element** tool. This is **auto-wired**: a producing element that
  *installs* the tool auto-exports it — `buildExportsDoc` emits a
  `Kind=executable` `Export` whose `LinkPaths` carry the `/opt/prefix/…`-anchored
  `bin/` path and whose label is the tool's converted target — and the
  consumer's tool-swap remaps the host prefix onto the anchor, matches that
  export, and rewrites the driver to `$(execpath <producing-element-label>)`
  with NO hand-authored entry. So a `prefix` todo means the producing element
  isn't installing/exporting the tool (or its `exports.json` wasn't wired into
  the consumer's deps); the basename `tools` entry is the stopgap. The recorded
  path is anchored to `/opt/prefix/…` (never the per-run-ephemeral synth-prefix
  path), so the report stays byte-identical across converts.

### Built-in conventions: auto-deriving the label (`--tool-conventions`)

For *well-known* generators the canonical Bazel label is a fixed convention,
so the converter ships a curated **tool→label registry**
(`tool_conventions.go`) — `protoc` → `@protobuf//:protoc`, `flatc` →
`@flatbuffers//:flatc`, `grpc_cpp_plugin` → `@grpc//src/compiler:grpc_cpp_plugin`
(each verified against the upstream BUILD + BCR). It's used two
ways:
- **Always on** — the `host-codegen-tool` todo for a known tool upgrades its
  `suggested_shape` to the REAL label (and names the `bazel_dep` to add)
  instead of a `//path/to:…` placeholder. Zero BUILD-output change.
- **`--tool-conventions`** (opt-in, off by default) — registers the
  conventions into the imports resolver's `tools` map, so a recovered genrule
  driving a known host tool **auto-hermeticizes** through the tool-swap with no
  hand-authored entry. An operator `tools` entry for the same tool wins (the
  convention is a fallback), and the swapped label's BCR module must be in the
  consumer's MODULE (the todo names it). Gated by
  `scripts/meta-cmake-tool-convention.sh`.

The registry is kept small and verified — each entry asserts a real BCR label.

Schema + resolver: `internal/manifest/imports.go` (`Tools` / `Tool`,
`Resolver.LookupTool`); swap: `rewriteToolFromTarget`
(`converter/internal/lower/genrule_tool_from_target.go`); auto-detect todo:
`converter/internal/lower/host_codegen_tool_todo.go`; gate:
`scripts/meta-cmake-host-codegen-tool.sh`.

## Where it sits

- Starlark host (loader, `match`/`lower` shim, builtins, the host-side output
  cross-check): `converter/internal/lower/starlark_recognizer.go`; the
  `--recognizers` flag (`converter/internal/cli/flags.go`) →
  `loadOperatorRecognizers` (`converter/cmd/convert-element-cmake/main.go`) →
  `Options.ExtraCodegenRecognizers`.
- Interface, registry, recognizers, dispatch helper:
  `converter/internal/lower/codegen_recognizer.go`,
  `converter/internal/lower/standalone_genrules.go`
  (`dispatchCodegenRecognizer`). Built-ins, in registry order:
  `grpcCppRecognizer` (a COMBINED `protoc --cpp_out --grpc_out` →
  proto_library + cc_proto_library + `cc_grpc_library(grpc_only = True)` from
  `@grpc//bazel:cc_grpc_library.bzl`; gated by
  `scripts/meta-cmake-protoc-grpc-recognize.sh`) then `protocCppRecognizer`
  (`--cpp_out` alone → proto_library + cc_proto_library). A grpc-ONLY call
  (services compiled separately from the messages) stays on the genrule path —
  emitting the native pair there would duplicate the sibling cpp call's
  proto_library.
- Consumer-dep wiring: `resolveCodegenHeaderConsumers`
  (`converter/internal/lower/lower.go`) → `Package.NativeRuleConsumerLabels`
  (`converter/ir/types.go`) → `generatedHeaderWrappers`
  (`converter/emit/bazel/split.go`).
- Native-rule substrate + auto-load: `converter/ir/types.go`,
  `converter/emit/bazel/emit.go` (`emitNativeRuleLoads`).

See [`codegen-tags.md`](codegen-tags.md) for the genrule-path tag taxonomy
(the fallback when no recognizer claims a command).
