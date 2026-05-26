# Project conventions for Claude Code

## Keeping `ROADMAP.md` current

`ROADMAP.md` (at the repo root) is the single source of truth for
what's shipped vs. queued. The architecture docs under `docs/`
describe how the systems work but **do not** restate roadmap state
— they point at `ROADMAP.md` instead. Don't add "What's shipped"
or "What's next" sections to architecture docs; that duplication
is what we deleted in PR #75.

When a change ships work that advances a `ROADMAP.md` item:

- If the change completes a `Now` or `Next` bullet, move it to
  `Done (high points)` in the same PR (or delete it if it's a
  small detail unworthy of the Done list).
- If the change makes meaningful progress without finishing the
  bullet, edit the bullet in place to reflect the new state.
- If the change reveals a follow-up that wasn't in `ROADMAP.md`,
  add it under `Now` / `Next` / `Later` as appropriate.

The goal: anyone reading `ROADMAP.md` on `main` should see an
honest current picture, not a snapshot from N PRs ago.

## Verifying code edits

Read `CONTRIBUTING.md` end-to-end before making changes that go beyond a
typo. It's the single source of truth for the dev-loop commands ("did my
change actually work?") and maps each touched file family to the unit
tests + render gates that exercise it.

The fast feedback loop in one paragraph: `go build ./...` then
`go vet ./...` then `gofmt -l .` (must print nothing) then
`go test ./...`. After that, run the render gate(s) under `scripts/`
matching what you touched (see `CONTRIBUTING.md`'s handler→gate table).
Render gates skip their bazel-build half cleanly when bazel ≥7 isn't on
`$PATH`, but the render half they always exercise is still the contract
`cmd/write-a` owes its consumers.

When something asks for the "correctness story" of a change — in a PR
description, a commit message, or your own design notes — show that you
ran these and what they printed. A green `go test ./...` plus the
relevant render gate's `ok` line is usually enough.

## When to open a PR

Open a PR proactively — no separate "should I open a PR?" check-in —
when the work is:

- A `ROADMAP.md` `Now` / `Next` item the user asked you to tackle.
- A specific GitHub issue the user pointed at (by number or URL).
- A direct fix-up follow-on to a PR already in flight (rebase, address
  review feedback, land a stacked PR's bottom layer).

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
6. **Push** the fixes. Use `--force-with-lease` for amends / squashes.
7. **Re-request review** (step 3 again).
8. **Loop** until the bot stops surfacing real bugs. Stop when the
   only open threads are doc-style suggestions you've considered and
   declined or that the bot will mark outdated on the next pass.

For stacked PRs:

- **All PRs target `main`.** Don't base one PR's branch on another
  PR's branch on GitHub — GitHub's stack-via-base-branch UI mostly
  fights the linear-history workflow this repo uses. Each PR is its
  own branch off main, carries its own commits, and is independently
  mergeable in principle. The "stack" lives in the operator's head
  (and the PR descriptions cross-referencing each other), not in the
  PR base setting.
- **Land the bottom PR first**, then rebase the upper branches onto
  `main` to drop the now-merged commits and pick up any review fixes.
  GitHub will narrow the upper PRs' diffs automatically after the
  rebase + force-push lands.
- Address the bottom PR's feedback first, push, then rebase the rest
  of the stack on top so each downstream PR picks up the fix before
  its own review pass.
- If the same kind of bug surfaces at multiple levels of the stack
  (e.g. a docstring claim landed at PR #N is still wrong at PR #N+2
  because the rebase brought it forward), fix at the lowest level
  that can hold the change cleanly; the chain rebase pulls it
  forward.
- Rebase conflicts on docs that have been edited at multiple stack
  levels are common — resolve in favour of the most-current text
  (the version closest to HEAD's intent), not the older snapshot
  the cherry-pick brought in.

Don't over-engineer the loop. Each iteration costs a re-review cycle
on the operator's Anthropic account; batch related fixes into one
push when the feedback is grouped, but a critical bug fix shouldn't
wait for a doc nit on a different file.
