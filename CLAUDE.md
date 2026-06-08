# Project conventions for Claude Code

## Keeping `ROADMAP.md` current

`ROADMAP.md` (at the repo root) is the single source of truth for
what's **planned** — the `Now` / `Next` / `Later` work. It is **not**
a changelog: git history and the code are the record of what shipped.
The architecture docs under `docs/` describe how the systems work but
**do not** restate roadmap state — they point at `ROADMAP.md` instead.
Don't add "What's shipped" or "What's next" sections to architecture
docs; that duplication is what we deleted in PR #75.

When a change ships work that advances a `ROADMAP.md` item:

- If the change completes a `Now` or `Next` bullet, **delete the
  bullet** — don't archive it. (We removed the old `Done (high
  points)` section precisely to stop using the roadmap as a changelog.)
- If the change makes meaningful progress without finishing the
  bullet, edit the bullet in place to reflect the new state.
- If the change reveals a follow-up that wasn't in `ROADMAP.md`,
  add it under `Now` / `Next` / `Later` as appropriate.

The goal: anyone reading `ROADMAP.md` on `main` should see an honest
picture of what's *left*, not a snapshot from N PRs ago and not an
archive of work already done.

## Verifying code edits

Read `CONTRIBUTING.md` end-to-end before making changes that go beyond a
typo. It's the single source of truth for the dev-loop commands ("did my
change actually work?") and maps each touched file family to the unit
tests + render gates that exercise it.

The fast feedback loop in one paragraph: `go build ./...` then
`go vet ./...` then `gofmt -l .` (must print nothing) then
`make staticcheck` (catches what `vet` doesn't — unused code / U1000,
simplifications) then `make lint-complexity` (the complexity lens — now a
blocking gate) then `go test ./...`. All of these gate in CI, so a green
`go test` alone isn't enough: a stray U1000 or a new complexity regression
still fails the build. After that, run the render gate(s) under `scripts/`
matching what you touched (see `CONTRIBUTING.md`'s handler→gate table).
Render gates skip their bazel-build half cleanly when bazel ≥9 isn't on
`$PATH` (most `meta-*` gates check `bazel_major < 9`), but the render half
they always exercise is still the contract `cmd/write-a` owes its consumers.

When something asks for the "correctness story" of a change — in a PR
description, a commit message, or your own design notes — show that you
ran these and what they printed. A green `go test ./...` plus the
relevant render gate's `ok` line is usually enough.

## Environment toolchain (Claude Code on the web)

The `SessionStart` hook (`.claude/hooks/session-start.sh`) provisions the
toolchain the gates + survey corpus need. On web sessions assume these are
on `$PATH` — don't burn a turn rediscovering them with `which`:

- `bazel` / `bazelisk` (repo-pinned launcher; `BAZELISK_BASE_URL` points at
  GitHub releases because `releases.bazel.build` 403s here), `buildifier`,
  `cmake` (bumped to the Makefile `CMAKE_VERSION` pin — cmake 4.x — so web
  sessions survey on the same cmake as production; the base image's system
  cmake is older, and modern projects' `>=3.29` floors would fail on it),
  `ninja`, `go` (host SDK), `gofmt`, `gfortran`.
- BCR modules resolve: `~/.bazelrc` repoints `--registry` at the GitHub BCR
  mirror (`bcr.bazel.build` — and every `*.bazel.build` host — 403s in this
  sandbox) and hands bazel's JVM a truststore for the egress CA. So
  `bazel_dep`s (rules_go, gazelle, gazelle_cc, …) fetch fine.

Two network caveats worth not rediscovering:

- **The Go SDK download from `go.dev` is blocked (403).** gazelle_cc needs a
  Go SDK, so use the **host Go** via `go_sdk.host()` in the MODULE — for the
  gazelle gate that's `META_GAZELLE_USE_HOST_GO=1 sh
  scripts/meta-cmake-split-gazelle.sh`. Without it, `bazel run //:gazelle`
  dies fetching the SDK (`go_sdk.download` → 403).
- Heavy/optional toolchains are opt-in env flags the hook honors:
  `BSB_PROVISION_CUDA=1` (CUDA toolkit), `BSB_WARM_GAZELLE=1` (pre-build
  gazelle_cc into the persistent survey cache so the first gazelle run is
  fast).

## Staying current with `main` (don't drift)

Long-lived branches drift, and features land on `main` *separately* while
you work — during the VTK push, comment-carrying (#452/#454/#455) and the
`conversion-todos.json` producer (#450/#451) both landed on `main`, but the
branch had drifted ~40 commits behind and didn't have them. Two concrete
costs: you can **answer wrong** ("comments aren't carried" — true for the
stale branch, false for `main`), and you can **re-implement work that
already shipped**.

So **`git fetch origin main` and check divergence
(`git rev-list --left-right --count origin/main...HEAD`) at reasoning
points** — at minimum:

- **Before starting each new corpus member** (grpc, cuda-samples, …). Skim
  `git log origin/main ^HEAD --oneline` for anything touching the converter
  area you're about to work in, so you build on the latest mechanisms
  instead of a stale snapshot.
- **Before claiming a capability is missing** — confirm against `origin/main`,
  not just the working branch, before saying "the converter doesn't do X."
- **When a fix recurs or feels like it should already exist** — check whether
  it landed on `main` first.

Picking the changes up (merge `origin/main` in vs. land your branch and let
GitHub's 3-way merge reconcile at PR-merge time) is a per-situation call —
ask when the conflict surface is large. The non-negotiable part is *looking*,
not silently diverging.

## When to open a PR

Open a PR proactively — no separate "should I open a PR?" check-in —
when the work is:

- A `ROADMAP.md` `Now` / `Next` item the user asked you to tackle.
- A specific GitHub issue the user pointed at (by number or URL).
- A direct fix-up follow-on to a PR already in flight (address review
  feedback, land a stacked PR's bottom layer).

For everything else — ad-hoc cleanups, refactors not on the roadmap,
experimental spikes — keep the default: commit + push, then ask
before opening a PR. The line is "is this work the user already
sanctioned the *outcome* of?" If yes, the PR is the natural delivery
vehicle. If no, surface the work first.

## PR review iteration

When you create a PR (or are asked to land a stack of PRs), the default
loop is:

1. **Open the PR** with `mcp__github__create_pull_request`.
2. **Subscribe** to PR activity for that PR with
   `mcp__github__subscribe_pr_activity`. This delivers review comments
   and CI status changes as `<github-webhook-activity>` messages that
   wake the session — no polling, no `sleep` loops.
3. **Request a Copilot review** with
   `mcp__github__request_copilot_review`. Don't wait for someone to
   trigger it manually.
4. **End the turn**. Webhook events will wake you when the bot
   responds.
5. **When feedback lands**, fetch the full state with
   `mcp__github__pull_request_read` (`get_review_comments` +
   `get_check_runs`) and triage each thread.

   **Whose feedback to weight, and how:**
   - **Copilot review comments** — the default automated reviewer.
     Triage per the rules below.
   - **Comments from other Claude sessions** (review or issue
     comments authored under the Claude/Anthropic identity, from a
     session other than the one driving this PR) — respect these as
     genuine review feedback. They aren't first-party reviewers but
     they've often seen the diff with a fresh perspective and catch
     issues a Copilot pass misses. Apply the same triage rules.
   - **Human reviewer comments** — the highest weight; address before
     re-requesting any automated review.

   Triage rules for each thread:
   - **Real bugs** (broken behavior, logic errors, unresolved merge
     conflict markers, CI failures): fix.
   - **Doc / comment accuracy** (claim doesn't match implementation):
     fix — comments rot; getting them right while the code is fresh is
     cheap.
   - **Architectural questions** (significant scope changes, new
     dispatch shapes): ask the user before acting.
   - **Already-resolved-by-an-earlier-fix** threads: skip; the bot's
     auto-outdate heuristic catches up on the next review pass.
6. **Push** the fixes as follow-up commits on the PR branch (a plain
   push — no amend/force-push or rebase, since we land with merge
   commits, not a squashed replay).
7. **Re-request review** (step 3 again).
8. **Loop** until the bot stops surfacing real bugs. Stop when the
   only open threads are doc-style suggestions you've considered and
   declined or that the bot will mark outdated on the next pass.

**Landing PRs — merge commits only.** Squash and rebase-merge are both
disabled on this repo, so **merge with a merge commit**
(`merge_method=merge`) and **never rebase a branch onto `main` just to
land it** — GitHub's 3-way merge reconciles any already-landed commits.
Merge once required checks are green; the auto-merge tool can't always
arm while checks are still pending, so in that case just merge when they
pass.

For stacked PRs:

- **All PRs target `main`.** Don't base one PR's branch on another
  PR's branch on GitHub — GitHub's stack-via-base-branch UI mostly
  fights the one-PR-per-branch workflow this repo uses. Each PR is its
  own branch off main, carries its own commits, and is independently
  mergeable in principle. The "stack" lives in the operator's head
  (and the PR descriptions cross-referencing each other), not in the
  PR base setting.
- **Land the bottom PR first**, then merge the upper PRs — also with
  merge commits, *without* rebasing them onto `main` first. GitHub
  reconciles the bottom PR's now-landed commits and narrows the upper
  PRs' diffs automatically.
- Address review feedback with follow-up commits on the relevant PR's
  branch (plain pushes). If the same bug spans multiple levels of the
  stack, fix it on the lowest branch that can hold the change; the
  upper PRs pick it up when they merge after the lower one lands.

Don't over-engineer the loop. Each iteration costs a re-review cycle
on the operator's Anthropic account; batch related fixes into one
push when the feedback is grouped, but a critical bug fix shouldn't
wait for a doc nit on a different file.
