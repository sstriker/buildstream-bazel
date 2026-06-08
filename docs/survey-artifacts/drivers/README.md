# Survey-marathon drivers (2026-06-08 full-corpus run)

These are the **orchestration scripts** that drove the 2026-06-08 full-corpus
run captured in `../` (the per-member fidelity + intent artifacts). They are
session-specific (hardcoded `/tmp` clone paths, per-member timeouts) and are
kept here for **reproducibility / provenance**, not as a reusable tool — the
reusable entry point is `scripts/run-survey.sh`.

Each driver loops `run-survey.sh` over a set of members with
`SURVEY_SKIP_BUILD=1 SURVEY_COMPILE_DB=1 SURVEY_INTENT=1` and the judge
`INTENT_LENS_JUDGE='env -u CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES claude -p --add-dir /tmp'`
(the remote-env recipe documented in `../../survey-corpus.md`), then copies
each member's `fidelity.json` + `intent-capture.json` out and reclaims the
per-member build dir.

| Driver | What it ran |
| --- | --- |
| `marathon1-light-and-heavies.sh` | first pass: glog, libpng, libevent, mbedtls, abseil, curl, sdl, zstd, protobuf, cuda-samples, then cutlass/vtk/llvm |
| `marathon2-redos-and-heavies.sh` | libevent + zstd redos, vtk, llvm (generous budgets) |
| `marathon3-remaining-members.sh` | the Status-at-a-glance members not yet covered: fmt, libxml2, brotli, googletest, zlib, boost-core, spdlog, catch2, openblas |
| `marathon4-fidelity-504-retries.sh` | re-run members whose fidelity was dropped by the transient GitHub release-CDN 504 outage (googletest, brotli, vtk, llvm), cumulative-retry against the shared repo cache |
| `marathon5-openblas-redo.sh` | openblas re-run under the correct `openblas` conf token |

Note: `marathon4` reflects the pre-fix workaround (per-member retries); the
durable fix — a shared `--repository_cache` so a tarball fetched once survives
per-member cleanup — is in `scripts/run-survey.sh` itself.
