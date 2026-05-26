# Cross-package `$<TARGET_FILE:t>` resolution

This doc proposes how the file(GENERATE) lifter's (a)-shape
genex evaluator should handle `$<TARGET_FILE:t>` (and its
on-disk-path variants — `TARGET_FILE_DIR`, `TARGET_FILE_NAME`,
`TARGET_LINKER_FILE*`, `TARGET_SONAME_FILE`) when `t` lives in
a different Bazel package from the lift site.

Status: **shipped** — both PR 1 (the refusal-stub soundness
gate) and PR 2 (the resolved-lift via the imports manifest)
landed. See `ROADMAP.md`'s Done section for the per-PR
summaries. This doc remains the architectural reference for
the two-shape behaviour.

## Today's behaviour

The (a)-shape evaluator's `$<TARGET_FILE:t>` lift (PR #179)
emits a Bazel-time `--target-file=<t>=$(location :<t>)` flag on
the lifted `cmake-configure-file` action. The `:<t>` label is
**implicitly relative to the current Bazel package** — fine
when `t` is in the lifter's current codemodel (same BuildStream
element → same Bazel package), refused via `UnsupportedError`
when `t` isn't.

`buildGenexTargets(r, recordedBuildDir)` populates
`Context.Targets[t]` only for targets cmake's fileapi codemodel
lists as locally-defined (`!t.IsGeneratorProvided` and not
imported). Cross-package targets — surfaced via
`find_package(Foo CONFIG)` / `include(<dep>-targets.cmake)` —
arrive as **IMPORTED** in the codemodel and `buildGenexTargets`
skips them. The evaluator then refuses with `UnsupportedError`
("no target %q in Context.Targets"), the lifter falls through
to the (b) shape, and the (b) shape captures cmake's rendered
bytes — which for `$<TARGET_FILE:t>` is the **recording
machine's absolute path** to `t`'s artifact.

### The latent soundness bug

(b) shipping the recording-machine path is wrong: that path
won't exist on Bazel's executor, and embedding it in the lifted
cmd makes the resulting `BUILD.bazel` machine-bound. The legacy
shape has the same problem (it's literally cmake's rendered
output bytes). Today, both shapes for cross-package
`$<TARGET_FILE:t>` silently ship broken paths into Bazel.

For the same-package case, (a) always wins (the byte-equal check
passes because cmake's recorded path matches what the evaluator
returns when FileLocation is populated), so (b)/legacy never fire
and the bug is latent only on cross-package.

A correct cross-package resolution shouldn't just enable the
case — it should also **detect-and-refuse** cross-package
references in (b)/legacy so the silent breakage becomes a loud
audit-visible refusal until (a) handles it.

## What's already in place

The cross-element label resolver already exists:
`internal/manifest/imports.go`.

- `imports.json` (per-orchestration sidecar) carries, per
  element, a list of exported targets:
  `{cmake_target: "Foo::bar", bazel_label: "//elements/foo:bar"}`.
- `manifest.Load(path) → *Resolver` builds an indexed view
  (`byCMakeTarget`).
- `lower.go` already passes a `*manifest.Resolver` through to
  `lowerTarget`, used by the dep-resolution pass to map
  `find_package`-surfaced `Foo::bar` references to Bazel
  labels in `cc_library.deps`.

The file(GENERATE) lifter doesn't currently see this resolver
— `recoverFileGenerate` and `buildFileGenerateGenrule` aren't
in its argument plumbing.

## Design

Three changes, in order of independence:

### 1. Thread the resolver into the file(GENERATE) lifter

Lower-level plumbing. `recoverFileGenerate` /
`buildFileGenerateGenrule` accept an additional `imports
*manifest.Resolver` parameter (nil is fine — same convention
the dep-resolution pass uses). `lower.go`'s call site passes
the resolver it already has.

No behaviour change yet — the parameter is unused.

### 2. (a) shape: emit fully-qualified labels for cross-package targets

`extractTargetFileRefs(body)` already returns the unique target
names referenced. For each name, resolve to a Bazel label:

1. If `genexTargets[name]` exists (local codemodel) → label is
   `:<name>` (current behaviour).
2. Else if `imports.Lookup(name)` resolves → label is the
   resolved `bazel_label`.
3. Else → drop the target from the cmd's `--target-file` flag
   set; the evaluator will refuse and (b) / legacy fallback
   takes over (which then refuses sanely under step 3 below).

The resolver's lookup key needs to match cmake's surface form.
cmake target names in genex contexts come in two shapes:

- **Bare**: `$<TARGET_FILE:bar>` — the operator wrote a
  project-local target. Looking up `bar` in the imports
  manifest doesn't find it (manifest indexes by namespaced
  `Foo::bar`); this stays at step 3 (drop / refuse).
- **Namespaced**: `$<TARGET_FILE:Foo::bar>` — the imported
  target. Looks up cleanly in the manifest.

`extractTargetFileRefs` returns names as bytes — it doesn't
distinguish bare from namespaced. The downstream check in
`buildFileGenerateGenrule` is what filters: same-package wins
first, then manifest, then drop.

The lifted cmd's `--target-file=Foo::bar="$(location //elements/foo:bar)"`
flag carries the cross-package label. `cmake-configure-file`'s
existing `--target-file` flag handler already accepts any string
on either side of the `=`, so no tool-side change needed.

The convert-time byte-equal check: cmake's `$<TARGET_FILE:Foo::bar>`
expansion is the imported target's `IMPORTED_LOCATION_*` value —
typically a synth-prefix stub path (see
`docs/design/cross-element-config-rendezvous.md`). The
evaluator's at-convert-time `FileLocation` for the imported
target needs to be that same stub path for the byte-equal
check to pass.

The simplest way: when `buildGenexTargets` encounters an
imported target, look up its `IMPORTED_LOCATION` from the
codemodel (cmake's fileapi exposes this under
`Target.Artifacts[0].Path` for imported targets — needs
verification) and populate `FileLocation` with the
synth-prefix stub path. The marshaled wire still omits
`FileLocation` (json:"-"), so machine-specific stub paths
don't leak.

### 3. (b) shape and legacy: refuse cross-package TARGET_FILE

Soundness fix for the latent bug. When the (a) shape falls
through (target not in codemodel, not in imports manifest, no
resolution), the (b) shape currently captures cmake's rendered
bytes — which for TARGET_FILE is the recording-machine
absolute path. We need to detect this case and refuse.

Two detection options:

- **Op-aware refusal in (b)'s extractor**: parse the template
  for `$<TARGET_FILE:` (and variants) before running
  `extractGenexValues`. If any reference is unresolved by step
  2, refuse the (b) lift and fall to legacy. Legacy has the
  same bug, so layer the same check there — refuse to lift
  altogether and surface a clear audit tag
  (`cmake-codegen-file-generate-genex-target-file-cross-package-unresolved`).
- **Value-shape heuristic in (b)'s extractor**: if any captured
  value looks like a recording-machine absolute path
  (`strings.HasPrefix(val, recordedBuildDir)` or the synth-
  prefix sentinel), refuse. Less precise but op-agnostic — would
  catch future absolute-path-bearing genexes without per-op
  knowledge.

The op-aware approach is more specific and gives a better
audit-tag taxonomy; it's the recommended path. The heuristic
is a nice belt-and-suspenders second layer if we want it.

The refusal lands a placeholder in the recovered IR that emits
a `genrule` whose `cmd` is just `false; echo 'unresolvable
cross-package TARGET_FILE reference for <t>'` — so a downstream
`bazel build` fails loudly with the diagnostic, the audit tag
flags the case for the operator, and no broken bytes ship.

## Implementation sketch

The recommended path lands in two PRs:

### PR 1: detect-and-refuse

- Thread `*manifest.Resolver` into `recoverFileGenerate` /
  `buildFileGenerateGenrule`.
- Add `unresolvableCrossPackageTargetFile(template,
  genexTargets, imports)` helper in `file_generate.go` that
  scans for `$<TARGET_FILE*:>` references and returns the set
  unresolvable in both same-package + imports-manifest.
- If non-empty: skip the lift entirely (no (a), no (b)),
  emit a refusal stub `genrule` with the
  `cmake-codegen-file-generate-genex-target-file-cross-package-unresolved`
  tag.
- New unit test: an imports.json-less call with a
  `$<TARGET_FILE:Foo::bar>` template lands the refusal stub
  with the audit tag.

This PR fixes the soundness bug and adds the resolver
plumbing. No actual cross-package lifts happen yet — those
land in PR 2.

### PR 2: resolved cross-package lifts

- `extractTargetFileRefs` stays unchanged (still returns names).
- `buildFileGenerateGenrule`'s `--target-file=` flag emission
  branches per target on resolution:
  - same-package → `--target-file=<name>="$(location :<name>)"`
  - imports-resolved → `--target-file=<name>="$(location <bazel_label>)"`
  - else → drop from cmd; the (a) eval refuses; PR 1's refusal
    stub fires.
- `buildGenexTargets` extends to populate `FileLocation` for
  imported targets via their codemodel `IMPORTED_LOCATION`
  (needs fileapi research to confirm where to read).
- New unit test: imports.json with `Foo::bar →
  //elements/foo:bar`, template uses `$<TARGET_FILE:Foo::bar>`,
  lift produces the (a) shape with
  `--target-file=Foo::bar="$(location //elements/foo:bar)"`.
- New render gate test in
  `scripts/meta-file-generate-cross-package.sh` exercising the
  end-to-end shape with two BuildStream elements (one
  exporting a library, one with file(GENERATE) referencing it).

## Out of scope

- **`$<TARGET_PROPERTY:t,p>` for cross-package targets.**
  Same mechanism applies in principle (look up `t`, fetch the
  property), but the property values for imported targets are
  set differently from project-local ones (cmake's `INTERFACE_*`
  property model). Address in the queued `INTERFACE_*
  aggregation` work, not here.
- **`$<TARGET_OBJECTS:t>`.** Cross-package or not, this has an
  unclear Bazel mapping (`cc_library` doesn't expose individual
  `.o` files as labels). Separate design needed; queued under
  Later.
- **Imports manifest absent.** When `--imports-manifest` is
  unset (operator hasn't generated one yet), the resolver is
  nil. The lifter behaves as today: same-package lifts via (a),
  cross-package refuses via PR 1's stub. Operators get a clear
  audit signal pointing them at the resolver as the fix path.

## Open questions

1. **`IMPORTED_LOCATION` from fileapi codemodel**: cmake docs
   say imported targets have their `IMPORTED_LOCATION` set
   when `find_package` resolves; the synth-prefix bundle
   populates stub paths (per cross-element-config-rendezvous.md).
   Need to verify the codemodel actually surfaces these — if
   not, the recording-side `cmake-to-bazel.vars.dump` could
   capture them. Implementation work in PR 2.
2. **Multi-config builds**: `$<TARGET_FILE:t>` is per-config
   (`Release` vs `Debug` paths differ). The existing
   `FileLocation` shape is single-config. For cross-package
   lifts under multi-config generators, we'd need per-config
   FileLocation — same future-work consideration the
   same-package case already has.
3. **Generator-expression namespace operator (`::`) inside
   genex args**: cmake's grammar parses `Foo::bar` as a target
   name, but the v1 `extractTargetFileRefs` does a simple
   `bytes.IndexByte` scan for `>`. The `::` would parse fine
   (no `>` inside it), but worth a fixture to confirm and a
   test pinning the behaviour.
4. **Audit tag naming**: the proposed
   `cmake-codegen-file-generate-genex-target-file-cross-package-unresolved`
   is long. Consider
   `cmake-codegen-file-generate-genex-cross-package` instead
   — same precision in context, shorter to type / grep.
