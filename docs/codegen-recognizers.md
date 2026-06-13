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
self-contained change — implement an interface, register it, add a fixture +
gate. No new `ir.Kind`, no bespoke emit path. This doc is the how-to.

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

## Where it sits

- Interface, registry, recognizers, dispatch helper:
  `converter/internal/lower/codegen_recognizer.go`,
  `converter/internal/lower/standalone_genrules.go`
  (`dispatchCodegenRecognizer`).
- Consumer-dep wiring: `resolveCodegenHeaderConsumers`
  (`converter/internal/lower/lower.go`) → `Package.NativeRuleConsumerLabels`
  (`converter/ir/types.go`) → `generatedHeaderWrappers`
  (`converter/emit/bazel/split.go`).
- Native-rule substrate + auto-load: `converter/ir/types.go`,
  `converter/emit/bazel/emit.go` (`emitNativeRuleLoads`).

See [`codegen-tags.md`](codegen-tags.md) for the genrule-path tag taxonomy
(the fallback when no recognizer claims a command).
