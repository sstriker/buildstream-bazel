# Agent-actionable conversion TODOs (`conversion-todos.json`)

> **Lifespan.** Design doc for unbuilt work. **Delete it once the producer
> has landed** — the code + `ROADMAP.md` become the record (per `CLAUDE.md`:
> architecture docs describe how systems work today; they don't carry plans).

## Problem

Some cmake constructs have a perfectly good *Bazel* form but **no mechanical
translation** — the behavior lives in a script only an author can re-express.
The canonical case is `add_test(NAME … COMMAND cmake -P <runner>)` integration
harnesses (brotli's roundtrip/compatibility tests): the idiomatic target is an
`sh_test` / `bazel_skylib` `diff_test` driving the built CLI, but reaching it
means *re-authoring* the cmake-script harness, not translating an AST.

Today the converter **breadcrumbs** these to stderr warnings so the gap is
visible (the `#417` `add_test`-not-converted audit, the `#412`
cmake-internal-drop audit, and the `install(SCRIPT)`/`install(CODE)` surface).
A human reads them; an AI post-pass can't. Promote the breadcrumb to a
**structured, deterministic `conversion-todos.json`** a post-conversion AI pass
consumes to author the Bazel form.

## Scope of this design

The roadmap names a producer *and* implies a consumer; the discipline line —
"converter stays **deterministic**; the non-deterministic authoring is
quarantined to the separate post-pass" — draws the box:

- **In scope:** the deterministic **producer** (`conversion-todos.json` schema,
  the `todos.Collector`, the operator preamble, the per-construct entries), and
  the **consumer contract** the post-pass must honor (idempotency + the
  trust boundary).
- **Out of scope:** the AI post-pass itself. It is the nondeterministic
  consumer; we define its contract, we don't build it here.

This keeps the converter a pure, replayable function.

## v1 producers (all three existing breadcrumb sites)

Detection **reuses** the existing breadcrumb logic — only the payload format is
new. Each site already groups and sorts deterministically, so each gains one
`todos.Add` call alongside its (retained) stderr warning:

| producer | site | grouping (existing) |
|---|---|---|
| `cmake-p-test` (`add_test` COMMAND not converted, `#417`) | `lower.go` (`byCmd[tst.Target]`) | by COMMAND target — brotli's 28 tests are already one group |
| `cmake-internal-drop` (`#412`) | `lower.go` (filtered command edges) | by kind (install/uninstall/cpack/dashboard/create_symlink/…) |
| `install-script` / `install-code` | `install_script_surface.go` | by `(site, scriptFile)` |

Evidence is recoverable from data the converter already holds — e.g. for a
`cmake -P` test, `ctest.Test` carries `Name`, `Target` (the runner), `Args`
(the invocation), `Timeout`, `Env`, `Tags`, `Data`.

## Convention

`conversion-todos.json` mirrors `rejection.Collector`
(`converter/internal/rejection/rejection.go`): an opt-in `todos.Collector`
plumbed through `lower.Options`, written when `--conversion-todos-report=<path>`
is set, emitted per-element by `convert-element-cmake` and aggregated by the
survey alongside `rejections` / `bazel-idiom` / `coverage`.

## Schema

```jsonc
{
  "version": 1,
  "tool_version": "<converter version>",
  "preamble": { /* see "Operator preamble" */ },
  "todos": [
    {
      "id": "todo-<stable-hash>",          // hash(kind, group_key, anchors) — the idempotency key
      "kind": "cmake-p-test",              // | cmake-internal-drop | install-script | install-code
      "group_key": "tools/run_test.cmake", // the shared unit → ONE todo, N instances
      "anchors": [                          // every source site folded into this unit
        {"file": "tests/CMakeLists.txt", "line": 42, "construct": "add_test(NAME roundtrip_x COMMAND cmake -P run_test.cmake x)"}
      ],
      "evidence": {                         // recovered facts, kind-specific
        "runner": "tools/run_test.cmake",
        "exe_target": "//:brotli",
        "invocations": [["-P", "run_test.cmake", "testdata/x"]],
        "verification": "SHA512(input) == SHA512(decompress(compress(input)))"
      },
      "suggested_shape": "one reusable macro wrapping bazel_skylib diff_test over //:brotli, instantiated per input",
      "prompt": "Author <…> as plain Bazel: …"
    }
  ]
}
```

Determinism: each producer site already sorts/groups; the final `todos` slice is
explicitly sorted by `(kind, group_key, anchors[0].file, anchors[0].line)` before
marshaling (mirroring the coverage report). The preamble is static text. Same
input → byte-identical report.

## Operator preamble

The report carries a **preamble** block — the standing guidance the post-pass
reads before working the list. It ships a strong **default that matches this
repo's intent** (a *transition tool*: the success state is plain Bazel you no
longer need the converter for), and is **operator-overridable**
(`--conversion-todos-preamble=<file>`, else the built-in default).

Default preamble (intent + rules + a worked example):

> **Intent.** This project is being moved off cmake onto plain, idiomatic
> Bazel. Author the target a Bazel maintainer would write by hand — a native
> Bazel rule driving the *built artifact* — **not** a wrapper that re-invokes
> `cmake -P` or shells out to the cmake harness. Prefer `bazel_skylib`
> `diff_test` / `sh_test` over re-running cmake.
>
> **Rules.** (1) Author into the designated authored-output file, never the
> converter-owned `BUILD.bazel` (the converter regenerates it wholesale).
> (2) One reusable macro per shared unit, instantiated N times — not N
> near-duplicate targets. (3) Preserve the recovered `verification` as the
> test's assertion. (4) Your output crosses the same trust boundary as
> mechanical output: it must pass the render gates (`buildifier -mode=diff`
> no-op, gazelle roundtrip, `bazel build`/`test`) — it is not trusted on faith.
>
> **Example (brotli).** 28 `add_test(… COMMAND cmake -P run_test.cmake <input>)`
> share one runner whose contract is "compress then decompress `<input>` with
> the built `brotli` CLI and assert the result equals `<input>`." Author **one**
> `brotli_roundtrip_test` macro wrapping a `diff_test` (or `sh_test`) over
> `//:brotli`, then instantiate it over the input list — one prompt, one macro,
> 28 cheap call sites.

## Idempotency: stable `id` + file-ownership split (no in-BUILD marker)

The converter **regenerates its `BUILD.bazel.out` wholesale** every run (and
`stage-b` then derives project B's `elements/<name>/BUILD.bazel` from it), and it
emits nothing for these constructs today. So an in-BUILD marker can't carry
idempotency: if the post-pass authored into either the converter-owned
`BUILD.bazel.out` or the stage-b-written `BUILD.bazel`, the next
convert+stage-b would **clobber** it. Idempotency therefore comes from two
things, not a marker:

1. **The stable `id`** (a content hash of the construct), constant across runs.
2. **A file-ownership split.** The converter owns `BUILD.bazel.out` (purely
   mechanical, carries *no* todo placeholder), and `stage-b` owns the
   `BUILD.bazel` it copies from it. The post-pass authors into a **separate file
   outside that ownership chain** — one neither the converter's `BUILD.bazel.out`
   emit nor `stage-b` writes (e.g. its own `extra_tests/BUILD` package, or a
   `conversion_authored.bzl` that the authored package — not the converter-owned
   BUILD — loads), keyed by `id`. Re-running the post-pass is idempotent because
   it checks whether output for `id` already exists in *its own* file;
   convert+stage-b never touch that file.

This is decision **(c)**: no placeholder target, no comment marker — the JSON is
the sole worklist, the converter-owned `BUILD.bazel.out` (and the
stage-b-derived `BUILD.bazel`) stay clean and fully mechanical, and the human
stderr warnings are **retained** (humans + agents read the same breadcrumb from
different surfaces).

## Consumer contract (what the post-pass must honor)

- Read `preamble` + `todos`; author one unit per `id` into the authored-output
  file; skip any `id` already present there (idempotent).
- Turn `evidence.verification` into the authored test's assertion.
- Emit plain idiomatic Bazel per the preamble — no cmake re-invocation.
- Authored output goes through the **same render gates** as mechanical output;
  a failing gate is a failing authoring, not a converter regression.

## Acceptance

- `convert-element-cmake --conversion-todos-report=<p>` writes a deterministic
  (byte-identical across runs) `conversion-todos.json` with the preamble + one
  grouped entry per unit from all three producers; the survey aggregates them.
- The stderr breadcrumbs are unchanged (retained alongside the JSON).
- The converter-owned `BUILD.bazel.out` (and therefore the stage-b-derived
  project-B `BUILD.bazel`) is byte-identical to today — no placeholder targets,
  no markers.
- The operator can override the preamble; the default encodes the
  transition-to-plain-Bazel intent with the brotli worked example.
