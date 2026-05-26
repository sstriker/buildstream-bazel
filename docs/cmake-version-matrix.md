# Multi-version cmake compatibility shakeout

The `e2e-cmake-matrix` CI job runs the converter's e2e surface
against four cmake releases in parallel. Its job is to surface
upstream-cmake drift (File API schema additions, build.ninja
shape changes, policy-removal regressions in the trace decoder)
before that drift propagates down to the pinned-cmake path the
production worker image (`deploy/buildbarn/runner/Dockerfile`)
and the `e2e` job both ship.

The matrix is **non-blocking** today: `continue-on-error: true`
on the job header applies to each matrix instantiation so a red
entry doesn't fail the workflow, and `strategy.fail-fast: false`
keeps one entry's failure from cancelling the others. Promotion
to blocking is queued — see "Promotion criteria" below.

## The matrix

| Version | Why this one |
|---------|--------------|
| `3.22.6` | Ubuntu 22.04 LTS default; the operator floor we expect downstream BuildStream projects to still ship against. |
| `3.28.6` | Ubuntu 24.04 LTS default; LLVM 23's floor; the modern stable reference. |
| `4.0.7`  | The major-bump that dropped pre-3.5 `cmake_minimum_required` compat; most likely to surface drift in fixtures + File API consumers + the build.ninja parser. |
| `4.3.3`  | Latest stable cmake release as of May 2026; catches new-release bugs before they hit distro defaults. |

The pinned `e2e` job (Makefile `CMAKE_VERSION`, currently 3.28.3)
remains the blocking gate — the matrix is the early-warning
system, not the contract.

## Per-version environment tweaks

| Knob | Where set | What it does |
|------|-----------|--------------|
| `CMAKE_POLICY_VERSION_MINIMUM=3.5` | matrix `env`, 4.x entries only | Gives projects whose `cmake_minimum_required(VERSION X)` declares X < 3.5 a one-version policy bump so cmake 4.x configures them instead of fatal-erroring. All in-tree fixtures already declare ≥ 3.20, but try_compile sub-projects cmake generates internally + future fixtures might not, so we set it defensively. The variable is only consulted by cmake 3.24+ and only acts on 4.x. |

In-tree fixture floors at the time the matrix landed: every
`converter/testdata/sample-projects/*/CMakeLists.txt` declares
`cmake_minimum_required(VERSION 3.20)` (or 3.23 for one fixture).
None trip the cmake 4.x pre-3.5 fatal — but the env var is set
anyway as forward insurance.

## Known per-version notes

Add a row here as soon as a matrix entry surfaces a real
converter bug; link the follow-up issue or `ROADMAP.md` bullet.
The shakeout's value is visibility — empty rows here are fine,
they just mean we haven't observed drift on that entry yet.

| Version | Status | Notes |
|---------|--------|-------|
| 3.22.6 | green | First run (PR #243 commit `aaff068`) clean across the e2e surface. |
| 3.28.6 | green | First run clean. |
| 4.0.7  | green | First run clean; matches the pre-matrix `e2e-latest-cmake` history at `CMAKE_LATEST_VERSION=4.0.3`. |
| 4.3.3  | green | First-run red caught a real cmake 4.3 drift: `fileapi.EventFindPackageFound`'s YAML decoder only accepted the legacy struct shape, but cmake 4.3 introduced a sibling `find-v1` event (firing on every `find_program` / `find_file` / `find_library`, including from cmake's own internal modules during compiler discovery) and made `found` polymorphic — string path / `false` bool / `null` / legacy struct. Fixed in PR #244 via a custom `UnmarshalYAML` accepting all four shapes; a trimmed cmake-4.3.3 configureLog under `converter/internal/fileapi/testdata/configurelog/` pins the parser against future drift. |

Per the matrix's `ROADMAP.md` rationale, the **default** response
to a surfaced bug is to file a follow-up `ROADMAP.md` bullet and
move on (visibility is the win). Trivial one-line fixes can land
opportunistically.

## Promotion criteria

The matrix promotes from non-blocking to blocking once:

1. **Three consecutive merges into `main` show all four entries
   green** (no `continue-on-error` rescues). This proves the
   shakeout's signal-to-noise ratio is acceptable for blocking
   PRs.
2. **Any "Known per-version notes" rows above have either been
   fixed in-tree or moved to an explicit `ROADMAP.md` `Later`
   bullet** with a documented why-we're-not-fixing-this stance.
   Promotion shouldn't lock in known-broken entries.
3. **The promotion itself is a one-line YAML change**: flip
   `continue-on-error: true` to `continue-on-error: false` on
   the job header in `.github/workflows/ci.yml`. (The
   `strategy.fail-fast: false` flag stays — it controls
   intra-matrix isolation, not block / non-block.)

Until those criteria are met, every red entry is treated as a
signal-to-investigate, not a signal-to-revert. The pinned `e2e`
job remains authoritative for "does this PR break the build."

## Adding a new version to the matrix

When a new cmake major or LTS-relevant minor ships, extend the
matrix entry list in `.github/workflows/ci.yml`:

```yaml
strategy:
  fail-fast: false
  matrix:
    cmake_version: ["3.22.6", "3.28.6", "4.0.7", "4.3.3", "<new>"]
```

The `install-cmake-toolchain` composite action takes
`cmake_version` as an input and downloads the matching
Kitware-released tarball — no other plumbing change needed.

When retiring an old entry (e.g. an LTS distro reaches EOL),
delete the entry and add a row to "Known per-version notes"
documenting the retirement so future readers know why coverage
narrowed.
