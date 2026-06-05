# Corpus-green campaign — workflow + live board (target: Sunday night)

## Coordination rules (operator, Fri PM) — READ BEFORE LAUNCHING/LANDING
**Collective pickup — no double-work, no breaking each other** (curl + OpenBLAS
both independently re-fixed the same `split.go` cross-package bug — exactly what
this prevents):
1. **Land general converter fixes to `main` IMMEDIATELY**, separate from
   member-specific `.conf` (e.g. the split.go cross-package fixes landed as #446
   independent of OpenBLAS's green). The shared foundation grows continuously.
2. **Every greening-agent prompt MUST say:** "`git merge origin/main` before you
   start and periodically; before writing any *general* converter fix, check if
   it's already in `main` — don't re-implement landed work."
3. **Always branch agents off the LATEST `main`** (all landed fixes), never a
   stale base. (The pre-Phase-0 base bug came from launching off the campaign
   branch — `git checkout main && git pull` first.)
4. **At land: reconcile + DROP dups.** `git merge origin/main` into the agent
   branch; if it re-implemented a fix already in `main`, keep main's version.

**Code-review gate (quality) — before merging ANY agent branch:**
- Run a **review pass** on the diff — a dedicated review agent, or the
  `/code-review` skill — checking correctness, regressions to the green set,
  test coverage, and idiom. Weight changes to shared emit paths (`split.go`,
  `lower.go`) heavily — they affect every green member. Address findings, then
  merge. (Copilot's PR review is a second, async pass.)

## Operator directives — EXPANDED SCOPE (Fri PM, authoritative)
"Full corpus green" now explicitly **includes the CUDA tier**, and OpenBLAS's
**final** form is the Fortran build:
- **CUDA tier IN (cutlass + cuda-samples).** Needs `nvcc` (provision via
  `BSB_PROVISION_CUDA=1` / the SessionStart hook's CUDA path) + `rules_cuda`
  wired into the build-lens synthesized MODULE + a `.cu`→`cuda_library` mapping.
  Agent `claude/green-cutlass` is building the CUDA infra + greening cutlass;
  cuda-samples reuses the infra (then a `cuda-samples.conf`). Toolchain INFRA —
  multi-day, but IN scope.
- **OpenBLAS final form = Fortran (operator's explicit end-goal).** The C-only
  (`NOFORTRAN=1 C_LAPACK=1`) green from agent `claude/green-openblas` is
  **INTERIM**. The deliverable the operator wants is the **Fortran** OpenBLAS —
  reference BLAS + LAPACK compiled via **edbaunton's `fortran_rules`** Bazel
  ruleset — on its own branch (`claude/green-openblas-fortran`), as the FINAL
  item after the rest of the corpus is green. That agent: base the build-lens
  MODULE on edbaunton/fortran_rules (`bazel_dep` + git_override), compile
  OpenBLAS's `.f`/LAPACK with it, verify `openblas` green WITH Fortran.

## Live state — Fri (triage in; wave 1 running)
- **Phase 0** ✅ landed (#442) — data-driven `build-lens/<m>.conf`.
- **KEY (triage):** curl, **grpc, and SDL share ONE blocker** — the dangling
  install-export `cc_import` seam (`converter/internal/exportshape/emit.go:378`
  emits a `cc_import` at non-existent install-tree `lib/*.{a,so}` paths). The
  **curl agent's fix unblocks all three**; then grpc + SDL green with just a
  `.conf` (grpc: `--cmake-define gRPC_BUILD_TESTS=OFF`; SDL: `--cmake-define
  SDL_REVISION=<str>` short-circuits its git_describe → both its rejections
  vanish).
- **Landed greens this wave:** eigen (#443 → #7), abseil (#444 → #8, 639/639),
  zstd (#445 → #9 — survey-scope `ELEMENT_SOURCE_ROOT` knob, NO converter change).
- **In flight (4 agents):** curl (`claude/green-curl` — 3-for-1 cc_import fix,
  unblocks grpc+SDL); VTK (`claude/green-vtk` — cc-embed/cc-hash);
  **protobuf** (`claude/green-protobuf` — DEEP); **llvm** (`claude/green-llvm` —
  DEEPEST). Their branches are NOT yet pushed = still grinding.
- **Land procedure (agents branched pre-Phase-0 → reconcile onto main):**
  checkout the agent branch, `git merge origin/main` (picks up Phase-0 + prior
  greens; drop any *rebuilt* config loader in favor of main's #442), then
  `go build ./...` + `gofmt -l` + `go test ./converter/internal/...`, push, PR,
  merge-commit. (eigen needed file-by-file extraction since its merge collided
  on the rebuilt loader; abseil merged clean.)
- **Queued:** grpc + SDL (`.conf`-only, the moment curl's cc_import fix lands).
- **Deep-grind (IN scope per operator — iterative, multi-pass agents):**
  - **protobuf** — (A) general `--fetchcontent-remap` converter slice
    (detect+drop `_deps/<dep>-src` targets, route edges via imports;
    `lower.go:2817`/`5909`, `split.go:1177`) + (B) the abseil CMake→Bazel label
    table (~75 targets; the ~36 fine-grained CMake-only ones → coarse
    `@com_google_absl` owner, or drop-if-transitive). Agent reports progress +
    branch each pass.
  - **LLVM** — (A) `llvm/$(RULEDIR)` prefix-doubling in per-target genrule cmds;
    (B) per-target tablegen `.td` include-closure (278 genrules); (C) 601 empty
    split-header libs; (D) ZLIB imports; (E) tblgen-as-genrule-tool. Iterative.
- **zstd: GREEN (#445)** — the earlier "structural won't-green" flag was WRONG.
  The converter already anchors subdir-cmake labels to the repo root (#303
  workspace-root path); the gap was the build-lens harness overlaying only
  `build/cmake`. Fixed with the `ELEMENT_SOURCE_ROOT` overlay knob — no
  converter change. Lesson: re-test a "structural" verdict before accepting it.
- **Greened (9):** fmt, libxml2, brotli, glm, googletest, glog, eigen, abseil,
  zstd. **Remaining (6):** curl, VTK, protobuf, llvm (agents grinding), grpc,
  SDL (queued behind curl's cc_import fix).

This doc is the **single source of truth** for greening the whole survey
corpus on the build lens (`build = ok` for every member). It is the
campaign's *memory*: it survives context-window summarization and session
restarts. Any session resuming the campaign reads this first.

"Green" = `SURVEY_BAZEL_BUILD=<m> scripts/run-survey.sh <m>=<dir>` prints
`ok` in the build column.

## Workflow — optimized for context / parallelism / memory / conflicts

**Lean orchestrator + parallel worktree sub-agents + this board + data-driven
build-lens config.** The glog/curl pushes proved each green is a deep
*reproduce → fix → bazel-build-verify → debug* cycle that is **context-heavy**.
So the deep work is pushed into sub-agents (fresh budget, discarded after); the
orchestrator stays lean.

- **Context.** The orchestrator (main session) NEVER does the deep
  reproduce-verify inline. Per member it only: (1) launches one sub-agent,
  (2) reads that agent's *final report* (never its transcript — that overflows),
  (3) lands a verified-green branch (merge commit), (4) updates this board.
  All diagnosis/fix/build-debug happens in the sub-agent.
- **Parallelism.** One **background** sub-agent per un-green member, each in an
  **isolated git worktree** on its own branch off `main`. Independent members
  run concurrently. Re-survey/build verification runs on the agent's budget.
- **Memory.** THIS doc. Per-member row: status, blocker diagnosis, owner
  agent/branch, notes. Updated as members move. Committed + pushed so it's
  durable. The per-member *fix recipe* is captured so a member never needs
  re-diagnosing.
- **Conflicts.**
  1. **Worktree isolation** per agent (branch off `main`).
  2. **Data-driven build-lens config (Phase 0).** Per-project build-lens config
     (extra `--cmake-define`s, `--imports-manifest`, extra `bazel_dep`s, bazel
     build flags like `--dynamic_mode=off`, whether to `--emit-install-export-config`)
     moves OUT of `run-survey.sh`'s `case` statements into **one file per
     project** (`scripts/build-lens/<member>.conf`). Each greening agent adds
     ONLY its own `<member>.conf` → **zero shared-file conflict**. (Today glm's
     `-w` and glog's `--dynamic_mode=off` live inline in `run-survey.sh` and
     would collide if N agents edited them.)
  3. **Converter fixes** are usually in member-distinct files (glog→`genrule.go`,
     curl→`split.go`); cross-member converter-file collisions are rare and
     resolved at land time, lowest-branch-first.
  4. **`docs/survey-corpus.md` status table + this board** are the only other
     shared edits — single-row append/edit, low conflict; the **orchestrator
     owns** the final table/board writes (agents report; orchestrator records).
- **Landing.** Merge commits only, never rebase-to-land (per CLAUDE.md). Land
  each verified green as its own PR off `main`.

## Phases (Fri → Sun)

- **Phase 0 — enablers (must land first, serially):**
  (a) data-driven `scripts/build-lens/<member>.conf` refactor of `run-survey.sh`
  (migrate glm + glog); (b) this board. Unlocks conflict-free parallelism.
- **Phase 1 — parallel, tractable greens:** eigen (verified recipe below),
  plus any member whose only blocker is a localized converter fix.
- **Phase 2 — parallel, deep converter work:** curl (shared-lib `cc_import`
  seam), protobuf (FetchContent→`@bcr` remap), abseil (`testonly`
  classification).
- **Phase 3 — triage-then-attempt the large/unknown:** SDL, grpc, LLVM, VTK —
  diagnose blockers first (time-boxed; LLVM/VTK may not make Sunday — flag
  honestly rather than burn the weekend).
- **Won't-green-standalone (flag, don't chase):** zstd (sources live in
  `repo/lib`, outside the surveyed `build/cmake` root — structural scope, not
  converter debt).

## Per-member sub-agent contract

Each greening agent gets: "Green member `<m>` on the build lens. Work in your
worktree off `main`. Reproduce → fix → verify → land. Add config ONLY via
`scripts/build-lens/<m>.conf` (never edit `run-survey.sh`'s body). Converter
fixes go in the relevant package + MUST add a unit/golden test. Verify with
`SURVEY_BAZEL_BUILD=<m> scripts/run-survey.sh <m>=<dir>` → do NOT claim green
unless it prints `ok`. `go build/vet/gofmt/go test ./...` must stay green.
Commit on `claude/green-<m>`, push, report: verified ok? + branch + recipe +
any surprises. If not green, report the exact build.log blocker — don't push a
green claim."

## Live board

| Member | Status | Blocker / fix recipe | Owner branch |
| --- | --- | --- | --- |
| fmt | ✅ ok | — | merged |
| libxml2 | ✅ ok | — | merged |
| brotli | ✅ ok | — | merged |
| glm | ✅ ok | tests `-Werror` → build-lens `--cmake-define CMAKE_CXX_FLAGS=-w` | merged |
| googletest | ✅ ok | fused-source `textual_hdrs` + `--emit-install-export-config` | merged |
| glog | ✅ ok | genrule subdir `$(RULEDIR)`-anchor (#439) + benign-rej skip (#440) + `--dynamic_mode=off` (#441) | merged |
| eigen | 🟡 phase-1 | **header-only**; `--cmake-define EIGEN_BUILD_TESTING=OFF EIGEN_BUILD_BLAS=OFF EIGEN_BUILD_LAPACK=OFF` → single `cc_library`, 0/0/0 (verified). Fortran reference BLAS/LAPACK = no Bazel ruleset (deferred, honest). | needs `eigen.conf` (post Phase 0) |
| curl | 🟠 phase-2 | find_package(ZLIB)→`@zlib` lever PROVEN (imports-manifest). Blocker: converter emits `cc_import(shared_library="lib/libcurl-d.so")` for a from-source shared lib — invalid sub-package label + non-existent install-tree `.so`. Needs converter fix to shared-lib install(EXPORT) emission. **3 general converter fixes already on `claude/curl-find-package-green`** (exports_files/genrule collision, cross-pkg gen-output visibility, genrule `$`-escaping) — need tests, then land. | `claude/curl-find-package-green` |
| protobuf | 🟠 phase-2 | only external find_package = ZLIB (trivial). Real blocker: mandatory abseil **FetchContent** → 486+ dead `_deps/absl-src/...` temp labels. Needs converter FetchContent→`@bcr`(or in-corpus) remap. | — |
| abseil | 🟠 phase-2 | 637/639; 2 ext test-deps (`GTest::gmock`, testing-off `absl::test_instance_tracker`). Needs `testonly` classification (or imports for gmock). | — |
| SDL | 🔵 triage | git_describe stamp FIXED (#436). Remaining: include-revision rejection + platform build (X11/Wayland/objc). | — |
| grpc | 🔵 triage | not yet diagnosed (likely heavy find_package: abseil/protobuf/ssl/re2/c-ares). | — |
| LLVM | 🔵 triage | huge; not build-lens-exercised. Time-box; may not make Sunday. | — |
| VTK | 🔵 triage | huge; not build-lens-exercised. Time-box; may not make Sunday. | — |
| zstd | ⛔ won't-green | sources in `repo/lib` outside surveyed `build/cmake` root — structural scope, not converter debt. | — |

## Orchestrator loop (each turn / each fresh window)

1. Read this board.
2. For any 🟡/🟠/🔵 with a pushed verified-green branch → land it (merge commit),
   flip to ✅, record the PR.
3. For any member with no owner + no running agent → launch its sub-agent
   (background, worktree) per the contract; set owner.
4. On an agent's completion notification → record its recipe + branch here;
   land if verified.
5. Keep ≤ ~4 agents in flight (avoid host contention). Never read agent
   transcripts. Update this doc, commit, push.
