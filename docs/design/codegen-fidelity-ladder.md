# Codegen recovery: the fidelity ladder and its dials

When the converter recovers a code-generation or configure-time call, it
has a choice of shapes. They are not equal. This doc states the
preference order as doctrine, maps the operator dials onto it, and
records the performance reasoning (so nobody "optimizes" in a way that
breaks the model).

## The preference ladder

Descending fidelity, for any recovered codegen/configure-time call:

1. **Native rule** — the idiomatic Bazel rule (`proto_library` +
   `cc_proto_library`, via the codegen-recognizer registry). Live,
   legible, and *maintained by gazelle*. Highest fidelity.
2. **Live genrule** — re-runs the tool at build time; regenerates on
   every relevant input change. Opaque (a shell action, not an
   idiomatic rule) but **faithful to what the build does**.
3. **Bake** (`write_file` / base64 genrule) — the bytes the tool
   produced *at convert time*, frozen into the BUILD. **Unfaithful by
   construction**: it does not re-run, so it silently drifts when an
   input changes until the next `convert`.
4. **Refusal** — no faithful Bazel form; surfaced **loudly** (a Tier-1
   failure, or a structured conversion-todo). Honest non-conversion.

**Prefer up the ladder.** A live rule (1/2) beats a frozen bake (3)
because correctness over time is the point of a build conversion; a loud
refusal (4) beats a silently-wrong emission. The recovery dispatch is
ordered to climb as high as it can (recognizer → argv genrule →
dir-operand genrule → derived re-run → bake → refusal); see
`docs/design/execute-process-recovery.md` for the per-call flow.

## The dials, and how they map onto the ladder

Four operator dials touch this, threaded from `cmd/write-a`:

- **`--fidelity`** (`strict` default / `best-effort`) — how an
  *unfaithful* recovery is handled. Today it (a) governs refusal
  handling (strict exits non-zero; best-effort lowers to stubs), and (b)
  **gates the recognizer's output cross-check**: a recognizer that
  matches the tool but whose derived outputs disagree with cmake's
  recorded ones REFUSES under strict (a loud build-time stub —
  `cmake-codegen-recognizer-strict-refusal`) and FALLS BACK to the
  generic genrule under best-effort. "Faithful native rule, or a loud
  failure — never a genrule whose output set we couldn't validate."
- **`--bake-in`** (`warn` default / `allow` / `reject`) — tolerance for
  rung 3. `reject` makes any bake-shaped emission exit non-zero, i.e.
  "refuse rather than freeze."
- **`--recognize-codegen`** — enables the recognizer registry (rung 1).
- **`--lift-derived-codegen`** — upgrades the derived-name stem-match
  bake (rung 3) to a live genrule re-run (rung 2) when placement is
  sound.

## `--fidelity` as the master dial (SHIPPED, explicit-engaged)

`--fidelity` is the *master* dial: setting it EXPLICITLY drives the combo so
one flag replaces several. `strict` = "I'm sure: be faithful or fail";
`best-effort` = "I'm not sure: faithful where sound, else fall back."

| Behavior / lift | flag (default) | unset (legacy) | explicit `strict` | explicit `best-effort` |
|---|---|---|---|---|
| Tier-1 refusal handling | `--fidelity` | refuse | refuse (exit ≠0) | lower to stubs |
| Missing cmake trace | `--strict-trace` (derived) | on (refuse) | on (refuse) | off (warn) |
| execute_process refusal | `--unsupported-execute-process-fallback` (derived) | off | off | on (stubs) |
| Recognizer output cross-check mismatch | `--fidelity` gate | n/a | refusal stub | genrule fallback |
| Codegen recognizer (rung 1) | `--recognize-codegen` (off) | off | **on** | **on** |
| Derived-codegen live genrule (rung 2 vs 3) | `--lift-derived-codegen` (off) | off | **on** | **on** |
| Host-tool hermeticize | `--tool-conventions` (off) | off | **on** | **on** |
| Bake tolerance | `--bake-in` (warn) | warn | **reject** | warn |

Rules: an explicit individual flag always wins over the dial-derived value
(e.g. `--fidelity=strict --recognize-codegen=false`). The **unset** default
stays conservative (no lift/bake combo) so existing converts are
byte-identical. Only the **staging-free** lifts are auto-enabled —
`--lift-configure-file`, `--lift-download`, `--lift-cc-embed`/`-cc-hash`,
`--cmake-script-*` need the downstream envelope to stage their tool, so they
stay explicit. `--bake-in` is otherwise orthogonal (the dial only derives
`reject` under strict; you can still set it independently).

Ordering note: the recognizer dispatch keys on the PRE-tool-swap driver, so
`--recognize-codegen` + `--tool-conventions` together still lower to the
native rule (the higher rung) rather than the swapped genrule.

**Remaining (corpus-gated):** flipping the UNSET default to engage the strict
combo corpus-wide needs a survey byte-sweep (lifts on / `--bake-in=reject`
change output for every member) — tracked in `ROADMAP.md`.

## Performance

### Convert time: verify by evidence, not re-execution

The converter **does not re-run the tool to verify its outputs**. The
cmake *configure already ran* every `execute_process` / custom-command
once, so the outputs are on disk; the recognizer corroborates its
predicted output set against that on-disk evidence plus the codemodel's
demand (a `stat`/read — effectively free). Re-executing in the converter
to "verify" would:

- **double the codegen cost on every convert** (the configure already
  paid it once),
- require **every tool present and hermetic at convert time** (the
  converter runs outside the build sandbox), and
- introduce **nondeterminism** (a second run can differ),

for **zero gain** over the evidence already in hand. So corroboration —
not re-execution — is the convert-time verification, by design.

### Build time: Bazel parallelizes; caching amortizes

A live genrule (rung 2) re-runs the tool at *build* time, but the cost is
not what it looks like:

- **Parallelism is inherent.** Each genrule / proto action is a node in
  Bazel's action graph, scheduled concurrently up to `--jobs`, subject
  only to data deps. We don't parallelize; Bazel does. No converter-side
  work is needed or wanted.
- **Caching amortizes the re-run.** An action re-runs **only on input
  change or cache miss** (local action cache; shared via remote cache),
  so steady-state incremental builds pay ~nothing. The bake's
  "zero build cost" edge exists only on a cold, cacheless build — the
  regime where its frozen-bytes correctness liability matters least.
- **Multi-output tools already batch.** protoc's `.pb.{cc,h}` are one
  action; `liftDerivedOutputsRerun` declares all of a call's build-root
  orphans as one genrule's `outs` → one invocation. We do not spawn the
  tool per output.
- **Do not batch *across* calls.** Coalescing N inputs into one
  mega-action would cut invocations but destroy incremental granularity
  (one input edit rebuilds everything). Per-action granularity +
  Bazel's scheduling is the right trade — keep it.

Net: rung 2 ≻ rung 3 holds on performance too, once a cache is in play.
The only knob worth turning is *which rung* a call lands on (the dials
above), not manual parallelization or batching.
