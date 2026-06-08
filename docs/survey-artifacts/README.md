# Survey lens artifacts — full-corpus run (2026-06-08)

This directory is the **captured per-member output** of the survey's two
opt-in qualitative lenses, run over the whole corpus
(`make fetch-*` roster, 26 members) so the findings aren't lost to the
ephemeral run environment. It is the durable companion to the curated
summary + per-row counts in [`../survey-corpus.md`](../survey-corpus.md).

The **intent-capture** findings here are the high-value payload: each
`net-new` finding is a **producer / lowering gap candidate** — something the
converter silently dropped that compiles fine and is flagged nowhere else.
Treat this directory as a converter-improvement backlog, not just a record.

## How it was produced

```sh
export INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'
SURVEY_BAZEL_BUILD=<member> SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1 \
  scripts/run-survey.sh --out-dir <out> <member>=<cmake-src>
```

- **Judge:** `claude -p` (capable agent, filesystem access to the converted
  bundle + cmake sources). The exact prompt each judge saw is preserved as
  `intent-prompt.txt` per member.
- **Lenses:** 5th = compile-db fidelity (`SURVEY_COMPILE_DB`), 6th =
  intent-capture (`SURVEY_INTENT`). The build lens was skipped
  (`SURVEY_SKIP_BUILD=1`) — the corpus is already build-green; this run
  refreshes only the fidelity + intent rows. See `../survey-corpus.md`
  "The intent-capture lens" + "compile-commands fidelity" for the full
  framing.

## Files per member

| File | What it is |
| --- | --- |
| `intent-capture.json` | **Triaged** judge output — every finding with full `evidence` / `cmake_ref` / `severity`, plus a triage `status` (`net-new` vs `dup-todo` / `dup-rejection`) and a `summary` (`net_new` = the `missed` count in `survey-corpus.md`). Triage annotates; it drops nothing. |
| `intent-findings.json` | **Raw** pre-triage judge output (the findings exactly as emitted, before dedup against this element's `conversion-todos.json` / `rejections.json`). |
| `intent-prompt.txt` | The exact prompt the judge read (project-derived context + the converted BUILD/MODULE + cmake-source file list). |
| `fidelity.json` | compile-db fidelity diff (per-TU defines / `-std` / includes / copts drift; `gen_root_missing`; link order), stored as a **signature-grouped summary** — each per-TU `*_mismatch` map is collapsed into a `{signature, tu_count, examples}` histogram (`_compacted: true`), so the distinct drift classes + their TU counts + example TUs are kept without the per-TU repetition. The deterministic fidelity output is the lower-value half here; the full per-TU file is reproducible via the recipe above. Compactor: `compact-fidelity.py`. **Absent** for members with no `CppCompile` TUs. |

## Fidelity coverage caveats (members with no `fidelity.json`)

- **Header-only** (no compiled TUs): `boost-core`, `cutlass`, `eigen`,
  `nlohmann-json`. (`glm` emits a single TU, so it has a minimal one.)
- **`cuda-samples`**: `.cu` sources lower to `CudaCompile`, which the lens's
  `CppCompile` aquery doesn't see (0 TUs).
- **`zstd`**: blocked by a real converter regression in split-emit — the
  cross-package relabel emits an invalid subpackage label
  (`//elements/zstd:lib/libzstd.so` where `elements/zstd/lib` is a
  subpackage). zstd is otherwise docs-green, so this is a main-drift
  regression worth fixing (tracked in `../survey-corpus.md`). NOT a transient
  infra failure.

## Reading the intent findings

`intent-capture.json` is a **triage queue, not a pass/fail gate** — the judge
is non-deterministic, so `net_new` counts are **not comparable across runs**.
Use a finding as a pointer to investigate, weighted by `severity` and whether
`status == "net-new"`. The curated high-signal findings (the producer-gap
shortlist) are written up per member in `../survey-corpus.md`.
