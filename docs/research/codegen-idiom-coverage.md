# Codegen-idiom coverage for the cmake converter

This repo is a **transition tool** (see `ROADMAP.md`). For the
generated-source ("codegen") slice of a cmake project — the custom
commands that run a tool to *write a source file the build then
compiles* — "transition" means: the converted project should end up
needing **neither cmake nor the converter** at build time. This doc
catalogs the codegen idioms cmake projects use, and the three routes
the converter has for each, ordered by how close they get to that
end-state.

## The three routes (cheap → end-state)

For any `add_custom_command` that runs a tool to produce a source:

1. **Runner bridge (`--cmake-script-runner`).** Emit a `genrule` that
   invokes an operator-staged cmake-like runner (`<runner> -P
   <script> [-D args]`) at Bazel build time. **Cheapest to reach** —
   no per-idiom recognizer, works for any `cmake -P` script whose
   inputs arrive via `-D` args and whose paths anchor under the source
   tree. **But it keeps cmake in the build**: the converted project
   still needs a runner. This is the EARLY-transition route — get the
   project building under Bazel *today*, defer the cmake-removal.

2. **Convert-time bake (`--cmake-script-bake`).** Run the script once
   at convert time and freeze its output bytes into a self-contained
   genrule (base64, no runner). Removes cmake from the build, but
   **couples the BUILD to the frozen output** — it goes stale if the
   script's inputs change, and convert-time execution is a hermeticity
   cost. A pragmatic middle for scripts with no clean native analog.

3. **Native recognizer → Bazel rule.** Recognize a *specific, common*
   idiom and lower it to a purpose-built Bazel rule that reproduces the
   idiom's contract with a small hermetic tool. **The END-state**: no
   cmake, no frozen bytes, no convert-time execution, and the BUILD
   re-renders from the original inputs through Bazel. This is where a
   given idiom's edges should land once the idiom is common enough to
   be worth a recognizer.

### The metric

The health number for this slice is the **runner-served edge count →
zero**. During a transition the runner carries the long tail cheaply;
as the common idioms get recognizers (route 3), their edges move off
the runner. A project is "done" with codegen when no edge needs the
runner (or the bake) — every codegen command is either a native rule
or a plain `genrule` over a hermetic tool.

### The North Star (bound on route 3)

The strategic end-state is **`cmake -G Bazel` / cmake-as-oracle**: a
generator running inside cmake's own generation pass would resolve the
residue for free, by virtue of *being cmake* (see `ROADMAP.md` "Now").
That bounds how far route 3 should go: **recognize the genuinely common
idioms** — the handful that recur across many projects and so pay back
a dedicated rule — and **don't gold-plate per-idiom recognizers** for
the long tail. The long tail is the runner's job until the North Star
subsumes it.

## Idiom catalog

What cmake projects do in custom-command codegen, and where each lands:

| Idiom | Example | Route today |
|---|---|---|
| **configure_file / file(GENERATE)** | `config.h.in` → `config.h` | Native: `cmake_configure_file` rule (`--cmake-configure-file-bin`) |
| **Embed a file as a C array** | VTK `vtkEncodeString` (shaders) | Native: `cc_embed` rule (`--lift-cc-embed`) — **this doc's focus** |
| **Hash a file → C constant** | VTK `vtkHashSource` | Runner (recognizer queued — low value, ~3 sites) |
| **tablegen-style generators** | LLVM `*.td` → `.inc` | `genrule` over the built tool (LLVM split-packages work, `ROADMAP.md`) |
| **Arbitrary script codegen** | project-specific `*.cmake` | Runner / bake (long tail; North Star subsumes) |

The first two are the common, recognizer-worthy idioms; both have
native rules. The rest are either niche (hash), already a plain
`genrule` over a real tool (tablegen), or genuinely arbitrary (the
runner's tail).

## The embed-file-as-C-array idiom → `cc_embed`

The dominant recognizer-worthy idiom after configure_file. A custom
command runs an encoder script that reads a file and writes a `.h` +
`.cxx` exposing its bytes as a named C symbol — VTK compiles GLSL
shaders into the library this way (`vtkEncodeString.cmake`); the same
pattern recurs in LLVM, Qt, and game engines.

**Native lowering** (`rules_buildstream_bazel/rules/cc_embed.bzl` +
`cmd/cc-embed`):

- `recognizeCcEmbed` (`converter/internal/lower/cc_embed_recognize.go`)
  fires under `--lift-cc-embed` when the script basename is a known
  encoder (`knownCcEmbedEncoders` — `vtkEncodeString.cmake` today; new
  encoders sharing the `-D source_file/output_name/binary/nul_terminate/
  export_symbol/export_header` contract drop in here).
- It parses the `-D` map and **declines** (falls through to
  runner/bake/refuse) on any shape the rule would reject: export
  symbol/header set without its pair, `nul_terminate` without `binary`,
  header and source in different directories, or a source file outside
  the source tree. Declining rather than emitting a deterministically-
  failing rule keeps the fallback honest.
- It emits a `cc_embed` target (predeclared `out_header` / `out_source`
  outputs, `tool = "//tools:cc-embed"`) and records the
  source→header pairing (`CcEmbedSourceToHeader`) so the consuming
  library gets BOTH the generated `.cxx` in `srcs` and the sibling
  `.h` in `hdrs` — a `#include`d generated header must be a *declared
  input* under Bazel, not just an `-I` path, or the consumer fails to
  compile.

**Faithfulness.** The emitted symbol NAME is the `symbol` attr
verbatim and the symbol's runtime value equals the input file's bytes,
so a consumer that `#include`s the header and references the symbol is
unchanged. The generated-source *formatting* is the tool's own (valid,
deterministic C) — only the symbol set and runtime value are
load-bearing. Control bytes (`<0x20`) and high bytes (`0x7f`–`0xff`)
are octal-escaped for cross-toolchain determinism.

**Operator plumbing.** `cmd/write-a --cc-embed-bin <path>` stages the
`cc-embed` binary into project A + project B `tools/` and threads
`--lift-cc-embed=true` into every kind:cmake converter genrule (both
the single-BUILD and `--split-packages` paths), mirroring
`--cmake-configure-file-bin`. Off by default; byte-stable when unset.

**Gate.** `scripts/meta-cc-embed-recognize.sh` converts a fixture that
calls `vtk_encode_string` over a `.glsl` (VTK's real
`vtkEncodeString.cmake`), asserts the rendered `cc_embed` rule + no
refusal + the consuming library's `srcs` carries the generated `.cxx`,
then (bazel ≥ 9) builds a `cc_binary` that LINKS the embedded symbol —
proving the full consumer wiring with no cmake involved.
