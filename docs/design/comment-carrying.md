# Carrying CMakeLists comments into BUILD files

> **Lifespan.** Design doc for unbuilt work. **Delete it once the
> comment-carrying producer has landed** — the code + `ROADMAP.md` become the
> record (per `CLAUDE.md`: architecture docs describe how systems work today;
> they don't carry plans).

## Problem

The converter loses author comments: a `# this lib wraps the vendored zlib`
above `add_library(...)` in `CMakeLists.txt` never reaches the generated
`cc_library`. The root cause is structural — **cmake discards comments at lex
time** (there is no AST), so neither the File API codemodel nor
`--trace-expand`/`--trace` carry a single comment. The BUILD is *regenerated*
from comment-free inputs, not transformed from source. Carrying comments over is
therefore a **recovery** problem (read raw source), not a "stop dropping them"
fix.

## Substrate that already exists (so most pillars are done)

- **Per-target declaration site is already on the IR.** `ir.Target.Provenance
  {File, Line, Command}` is populated from the codemodel `backtraceGraph` at
  lowering (`lower.go` ~1714). Association of a target to its `CMakeLists:line`
  is **free**.
- **Roundtrip-safe leading-comment emission already ships.** `emit.Options.
  EmitProvenance` emits a leading `# Source: <file>:<line> (<command>)` comment
  above each target; the emitter is buildtools-AST-based and routes through the
  `build.Parse` → `build.Format` canonicalize pass (same one `buildifier
  --mode=fix` uses), and the code notes leading comments attached to rule calls
  survive it. So **Option D (a provenance breadcrumb) is already implemented**,
  and leading-comment emission is proven gate-safe.
- **Reading raw cmake source at a declaration site is a production pattern.**
  `cmakeargv.ReadCall(path, line, command)` opens `CMakeLists.txt` at a backtrace
  `file:line` and tokenizes the call (`backtrace_scope.go` already uses it to
  recover PUBLIC/PRIVATE keywords).
- **A file-level comment slot already emits.** `pkg.HeaderComments []string`
  (rendered at the top of the BUILD) — today fed from semantic sources
  (find_package / `option()` / `message(DEPRECATION)`), not raw comments.
- **Per-rule / per-attr / per-string comment attachment is in use** — the
  `# keep` markers attach via `call.Comment().Suffix` (`emit.go` ~645). The
  symmetric `.Before` field is what leading comments use.

The only genuinely **new** pillar is recovering the comment *text*.

## Scope (v1): A + B + C-trailing + D

- **A — file header.** Recover the leading comment block at the top of the
  top-level `CMakeLists.txt` (license / copyright / file doc) and feed it into
  the existing `pkg.HeaderComments` slot → top of BUILD. (`split.go` already
  routes `HeaderComments` to the root package only — correct.)
- **B — leading comment → target.** Per target, at `Provenance.{File, Line}`,
  read *upward* for the contiguous `#` comment block immediately preceding the
  command, and emit it as the rule's leading (`.Before`) comment via the proven
  EmitProvenance path.
- **C-trailing only.** A `#` comment trailing the command's last line
  (`add_library(foo ...)  # core lib`) → the rule's `.Suffix`. **Attr-level C
  is explicitly out** (comments next to individual arguments): args are
  reordered / canonicalized / split across the cmake→IR→emit transform, so
  attr-level attribution is fragile and risks the roundtrip gate.
- **D — provenance breadcrumb.** Already shipped (EmitProvenance); kept, and
  composable with B (when both fire: author comment first, then the `# Source:`
  ref).

### Synthesized targets carry their originating comment (every lift, not just custom commands)

The high-value B case the operator called out is *"comments before a codegen"*
(e.g. `# generate the parser tables` above a tablegen custom command). But
`add_custom_command` / `add_custom_target` is **only one** lift family — the
converter synthesizes codegen targets from several trace-recovered call shapes,
and a comment above any of them is just as worth keeping:

- `add_custom_command` / `add_custom_target` → `genrule`
  (`AddCustomCommandCall` / `AddCustomTargetCall`).
- **`execute_process` codegen** → `genrule` / `cmake_configure_file` / stamp
  lifts (`recoverExecuteProcess`, from `shadow.ExecuteProcessCall`, which carries
  `File`/`Line` — already used for the refusal records).
- `configure_file` / `file(GENERATE)` → `cmake_configure_file` / `genrule`
  (`ConfigureFileCall` / `FileGenerateCall`).
- `cmake -P` script lifts (cc-embed / cc-hash / script-bake) → the wrapping
  custom command's call.

None of these flow through the codemodel `backtraceGraph` (they're
trace-recovered, not codemodel targets), so they have **no `Provenance` today**.
The design therefore generalizes: **at each lift site, stamp the synthesized
target's `Provenance` from its originating trace call's `File`/`Line`**, and
**prefer the highest-level originating call** — the wrapping `add_custom_target`
over the inner `add_custom_command`; the `execute_process` / `configure_file` /
`file(GENERATE)` call itself otherwise — mirroring the existing genrule-naming
logic that already names a genrule after its wrapping `add_custom_target`. That
"highest-level CMakeLists line it came from" is the line whose leading comment we
want.

With `Provenance` populated uniformly across every lift, **B applies to all of
them with no per-lift comment logic** — the single upward-read at
`Provenance.{File, Line}` carries the comment for custom commands, lifted
`execute_process`, and `configure_file`/`file(GENERATE)` alike.

## Recovery (the new work)

Extend the `cmakeargv` source reader (or a sibling) so that, given a declaration
`(file, line)`:

- **Leading block:** scan upward from the command's start line over a contiguous
  run of comment lines (`#` line comments and `#[[ … ]]` / `#[=[ … ]=]` bracket
  comments), stopping at a blank line or a non-comment line. That run is the
  leading comment.
- **Trailing:** `ReadCall` already spans the (possibly multi-line) call; any `#`
  comment after the closing `)` on the call's last line is the trailing comment.
- **File header (A):** the leading block before the *first* command in the
  top-level `CMakeLists.txt`.

This is a *targeted upward/trailing read per declaration site*, not a whole-file
lexer — it reuses the `ReadCall` "open at line, tokenize" precedent.

## Emission

- New IR fields: `Target.LeadingComment []string`, `Target.TrailingComment
  string`; `Package.HeaderComments` already exists.
- Leading → `call.Comment().Before`; trailing → `call.Comment().Suffix`; header →
  `HeaderComments`. All survive `build.Format` (leading proven by EmitProvenance,
  suffix proven by `# keep`).
- **Collision with `# keep`:** for whole-rule-keep kinds (genrule, filegroup,
  cc_import, …) the rule already carries a `# keep` *suffix*; stacking an author
  *trailing* comment there renders two suffix comments ambiguously. Rule: on
  whole-rule-keep kinds, route the author comment to **leading** placement (B),
  never trailing — trailing is reserved for kinds without a whole-rule keep.

## Gating, determinism, caveats

- **Opt-in flag** (`emit.Options.EmitSourceComments`, sibling of
  EmitProvenance). Off by default keeps output byte-identical to today and avoids
  silently adding source-read inputs; operators opt in. (Whether to default it on
  later is a follow-up call.)
- **Determinism:** recovered comments are a pure function of source text; emitted
  in declaration order; buildtools Format keeps output buildifier-canonical so
  the gazelle-roundtrip gate stays green.
- **Source-read inputs:** recovery reads `CMakeLists.txt`/`*.cmake`, adding them
  to the converter's action inputs (already precedented via `cmakeargv` /
  `read_paths`); a comment edit re-renders, which is correct.
- **Function/macro caveat:** `Provenance` resolves to the outermost user frame,
  so a target/genrule declared *inside* a helper points at the helper body line —
  a comment recovered there is the helper's, not the call site's. Conservative
  rule: **skip comment recovery when the declaration's backtrace passes through a
  function/macro frame** (same bounded ambiguity as the function-forwarded stamp
  work). Revisit if a real project needs call-site recovery.
- **Synthesized-without-source targets** (cc_import facades, header libs, write_file
  bakes) have no originating comment → none emitted (fine).

## Acceptance

- With `--emit-source-comments`: leading comments above target-defining commands
  and above **every lifted codegen call** (`add_custom_target`/
  `add_custom_command`, `execute_process`, `configure_file`/`file(GENERATE)`,
  `cmake -P` lifts) carry to the synthesized rule's leading comment — each via a
  `Provenance` stamped from the originating trace call site; trailing inline
  comments carry to the rule suffix (except on
  whole-rule-keep kinds, where they route to leading); the top-of-CMakeLists
  header block carries to `HeaderComments`; the EmitProvenance `# Source:` ref is
  unchanged and composes.
- `buildifier --mode=diff` / gazelle-roundtrip stays a no-op; output is
  deterministic across runs.
- Comments sited inside functions/macros are skipped (not misattributed).
- Off by default → byte-identical to today.
