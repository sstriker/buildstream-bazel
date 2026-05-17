# Converter test coverage plan

Snapshot of coverage gaps in `converter/internal/lower/` and
sibling packages, drafted in response to issues #192 / #193 /
#194 (all three landed in the same area within the same hour
from one external operator). The shared shape: a helper with
partial unit-test coverage, an edge-case input the existing
render-gate fixtures didn't exercise, and a downstream
consumer (Bazel) rejecting the resulting output.

This doc identifies the next-most-likely-to-surface
candidates and proposes a small, prioritized batch of tests
to add before the bugs are reported.

## Baseline

`go test -cover` numbers as of `main` after #196/#197/#198
land (the three issue-fix PRs):

| Package | Coverage |
|---|---|
| `converter/internal/fileapi` | 80.4 % |
| `converter/emit/bazel` | 79.9 % |
| `converter/internal/ninja` | 76.7 % |
| `converter/internal/lower` | 72.8 % |
| `converter/internal/cmakerun` | 62.3 % |

The lowest-coverage package is `cmakerun` — but its missing
50%+ is `Configure()` (runs cmake) and `configureEnv()`
(builds the env passed to cmake). Both genuinely need an
actual cmake on PATH to exercise; covered via `e2e-*` gates
rather than unit tests. Honest gap, not a priority.

The actionable gap is **`converter/internal/lower/`**: 27.2 %
of statements untested, in code where three operator-
reported bugs just landed.

## What the recent bugs had in common

| # | Function | Pre-fix coverage | Pre-fix direct test? | Bug shape |
|---|---|---|---|---|
| #192 | `genruleNameFor` | 100 % (incidental via `recoverGenrule`) | **no** | Edge-case input (absolute path) untested |
| #193 | `recoverGenrule` empty-cmd path | 82.8 % overall | indirect only | Specific input shape (CommandFor returning `(cmd="", ok=true)`) untested |
| #194 | `lowerTarget` dep-loop | 62.9 % overall | indirect only | Duplicate input list untested |

The common pattern is not "helper has 0 % coverage" — all
three had some indirect coverage via render-gate fixtures —
but rather "the SPECIFIC edge-case input shape is never
constructed in any test." Render-gate fixtures use realistic
cmake projects, which don't naturally hit unusual shapes
(duplicate deps, absolute paths in build.ninja, empty
custom-command bindings).

**Conclusion:** the gap to close isn't statement coverage —
it's per-function table-driven tests that enumerate the
weird input shapes cmake CAN produce, even if our fixtures
don't.

## Next-most-likely candidates

### Tier 1: helpers adjacent to the just-reported bugs

These are in the same files (`lower.go` / `genrule.go`),
operate on the same inputs (codemodel deps, ninja builds,
cmake command strings), and have partial coverage with
visible edge cases.

| Function | File | Coverage | Hazard worth a test |
|---|---|---|---|
| `splitShellTokens` | genrule.go:329 | 51.9 % | shell parsing of cmake custom commands — quoted args, escapes, embedded `$VAR`. A wrong split corrupts the genrule cmd. |
| `extractDriver` | genrule.go:280 | 52.4 % | drives `cmake-codegen-driver=X` audit tag. Wrong driver classification means narrowing-audit allowlists don't match. |
| `normalizeInput` | genrule.go:232 | 44.4 % | path normalization for custom-command inputs. Same shape as #192's leak — if a sibling helper has the same bug, it'd ship wrong bytes to genrule srcs. |
| `depScopeIsPrivate` | lower.go:1213 | 55.6 % | directly adjacent to #194's bug. Trace-keyword recovery for `target_link_libraries` arms. |
| `stripIDHash` | lower.go:1238 | 66.7 % | fileapi codemodel `Id` parsing — `name::@hash` shape. Used by #194's call site as the lookup key for imports.LookupCMakeTarget. |
| `isPathPrefix` | lower.go:1284 | 33.3 % | path predicate. `genruleOuts` and others rely on `relativeIfInsideRelaxed`, which calls into this family. |
| `isTargetObjectsRef` | lower.go:1254 | 33.3 % | parser for `$<TARGET_OBJECTS:name>` references in trace fragments. |
| `recoverGenrule` (remaining branches) | genrule.go:72 | 82.8 % | the 17.2 % uncovered includes: ninja graph absent, BuildFor falls through to absolute form, "outside build dir" path. Each is a separate refusal mode that could miss a typed error. |

Each of these is a ~30-line table-driven test.

### Tier 2: configure_file lift's untested branches

`configure_file.go` has multiple 0%-coverage functions and
one 8.3%-coverage function. This is the same code shape as
the file(GENERATE) lifter that took 12 PRs to harden — and
operator-facing in exactly the same way.

| Function | Coverage | Why it matters |
|---|---|---|
| `recoverConfigureFilesFromCalls` | **8.3 %** | top-of-the-stack code path. The 91.7 % uncovered includes shadow.ConfigureFileCall decoding, OUTPUT-path resolution, the lift-vs-legacy gate. Same hazard area as #192/#193. |
| `buildConfigureFileGenrule` | 0 % | unit-test entry point matching `buildFileGenerateGenrule` (which IS extensively tested). Symmetric API; symmetric test coverage warranted. |
| `configureFileOptionsFromCall` | 0 % | parses configure_file's AT_ONLY / ESCAPE_QUOTES / NEWLINE_STYLE / COPYONLY flags. Same Options struct file(GENERATE) consumes. Wrong flag mapping ships wrong-bytes outputs. |
| `isConfigureFileKeyword` | 0 % | classifies configure_file argument tokens. Wrong classification routes through the wrong lift path. |
| `configureFileTags` | 0 % | mirrors `fileGenerateTags` — at 100 % per the just-landed `fileGenerateTagSet` refactor. Apply the same refactor + test treatment here. |

### Tier 3: ninja parser's untested entry points

`converter/internal/ninja` is 76.7 % covered, but the
uncovered slice includes `ParseFile` (the public entry the
converter uses) and `defaultFileResolver`. Direct tests
exercise `Parse` (the io.Reader variant); the file-level
wrapper just has its include-resolution logic untested.

| Function | Coverage | Why it matters |
|---|---|---|
| `ParseFile` | 0 % | top-of-stack. Tested incidentally via the converter's render gates but no unit coverage for path-resolution edge cases (relative includes, symlinks, missing files). |
| `defaultFileResolver` | 0 % | hands include paths to `Parse`. A wrong path returned here silently misses includes — would surface as missing build statements downstream. |
| `parsePoolStmt` | 0 % | rare ninja syntax, may not appear in our fixtures at all. Defensive coverage. |

### Tier 4: emit-side untested branches

`converter/emit/bazel` is at 79.9 %. The recent bugs all
landed in `lower/` (IR construction), but `emit/bazel` is
the last hop before BUILD.bazel hits disk — wrong emission
here ships wrong bytes too. Less likely to surface as a
correctness bug (the IR-to-emit mapping is mostly literal)
but worth a defensive pass.

(Coverage detail not enumerated here — emit/ has fewer
hazard-shaped helpers than lower/. Defer until a real bug
surfaces.)

## Proposed test-writing batches

Sized for ~one PR each, each landing in <1 day of work:

### Batch A: Tier-1 lower/genrule helpers (~5 tests, 1 PR)

Pick the five highest-hazard Tier-1 entries:
`splitShellTokens`, `extractDriver`, `normalizeInput`,
`depScopeIsPrivate`, `stripIDHash`. Table-driven, each with
a "happy path" row and at least 3 edge-case rows
(empty input, quoted/escaped variants, unusual cmake-id shapes).

Goal: bring lower/ coverage from 72.8 % → ~85 %, with the
gained coverage specifically targeting the edge-case input
shapes that match the recent-bug pattern.

### Batch B: recoverGenrule refusal branches (~4 tests, 1 PR)

Each of `recoverGenrule`'s 17.2 % uncovered statements is a
refusal branch:
1. `g == nil` (no ninja graph)
2. `b == nil` (no build statement produces output)
3. `b.Rule != "CUSTOM_COMMAND"` (object file etc.)
4. `relOut` outside buildDir

Each refusal returns a typed `failure.UnsupportedCustomCommand`
with a specific Reason. A test per branch asserts the typed
error fires AND no genrule is synthesized (same shape as
#193's test).

Goal: 100 % coverage of `recoverGenrule`'s refusal paths.
The refusal contract is operator-facing (drives audit-tag
taxonomy), so the regression-guard value is high.

### Batch C: configure_file lift symmetry (~6 tests, 1 PR)

Bring `configure_file.go`'s coverage up to match
`file_generate.go`'s (which is heavily tested by the genex
chain). Functions: `buildConfigureFileGenrule`,
`configureFileOptionsFromCall`, `isConfigureFileKeyword`,
`configureFileTags` (apply the `fileGenerateTagSet`-style
refactor while we're here), plus filling in
`pickValues`'s 50 %→100 % and
`recoverConfigureFilesFromCalls`'s 8.3 %→ goal-85 %.

Goal: configure_file lift hardened to the same standard as
the file(GENERATE) lift. configure_file is the older/more-
established lift; closing the test-coverage delta is
overdue.

### Batch D: ninja ParseFile + resolver (~3 tests, small PR)

Cover the file-level entry points that the unit tests
currently skip in favor of the Reader-based `Parse`. Mostly
defensive — would catch include-resolution edge cases
before they ship.

## What this plan deliberately doesn't do

- **Doesn't chase 100 % coverage as a metric.** The goal is
  "edge-case input shapes have a test that pins them," not
  "every line has a test that traverses it." `splitMultiLanguage`,
  `langSuffix`, `relForSource` (all at 0 %) are simple
  helpers that would lift the percentage but not the
  bug-catching power — skip them until they're on a
  bug-report path.
- **Doesn't introduce property-based testing.** Tempting
  for `splitShellTokens` / `normalizeInput` / `stripIDHash`,
  but adds a dependency + complexity the project doesn't
  currently carry. Table-driven tests with 5-10 rows are
  enough for the bug-shapes we know about.
- **Doesn't gate-promote anything.** No "coverage must stay
  above N %" CI assertion. The narrowing-audit gate has
  shown that promotion is gated on operator signal
  accumulation, not on test additions. Same logic here:
  ship the tests, let them prevent the bugs they prevent,
  don't manufacture additional CI pressure.

## Process change suggestion (open question)

The recent bug pattern — "operator hits an edge case our
fixtures don't" — would be partially caught by adding a
**hazard-shapes test fixture** alongside the realistic
project fixtures. The idea: a single `*.bst` element with a
deliberately weird shape (absolute paths in custom commands,
duplicate deps via two `target_link_libraries` calls, an
empty-cmd custom command, etc.) — exercising every Tier-1
helper's edge case in one render-gate run.

Pros: catches the pattern, not just the individual bugs.
Cons: fixture maintenance overhead; the "weird shape" surface
is open-ended.

This is more of a long-term direction than a near-term PR.
Worth thinking about if operator-reported bugs in this
package keep coming.

## Status

This is a planning doc. Batches A-D are not yet scoped into
PRs; they're proposed sequencing if the operator wants
defensive coverage shipped before the next bug report.
Pause-and-wait remains the honest alternative — these tests
prevent bugs we believe exist by analogy with #192/#193/#194,
but no specific bug report has yet pointed at any of them.
