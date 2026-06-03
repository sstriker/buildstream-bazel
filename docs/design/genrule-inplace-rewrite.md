# Genrule in-place rewrite (source-tree-input == build-tree-output)

Status: **design + reproduction landed; implementation queued** (the
remaining LLVM-frontier converter gap in `ROADMAP.md`).

## The shape

A cmake `add_custom_command` reads a file from the **source** tree and
writes its output to the **same relative path** in the **build** tree —
an in-place rewrite. LLVM's `Remarks.exports` is the canonical case (a
linker version-script massaged in place); the minimal reproduction is
`converter/testdata/sample-projects/genrule-inplace-rewrite/`:

```cmake
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/version.txt
    COMMAND ${CMAKE_COMMAND} -E copy
            ${CMAKE_CURRENT_SOURCE_DIR}/version.txt
            ${CMAKE_CURRENT_BINARY_DIR}/version.txt
    DEPENDS ${CMAKE_CURRENT_SOURCE_DIR}/version.txt VERBATIM)
```

## What the converter emits today (the bug — confirmed open)

```python
genrule(
    name = "custom_command_version_txt",
    srcs = ["version.txt"],
    outs = ["version.txt"],                                   # ① collides with the source file
    cmd  = "cp $(RULEDIR)/version.txt $(RULEDIR)/version.txt", # ② input mis-anchored to $(RULEDIR)
)
```

A real `bazel build` rejects it:

```
Error in genrule: rule 'custom_command_version_txt' has file
'version.txt' as both an input and an output
```

It is a **compound** bug:

1. **`outs` shadows the package source.** Bazel forbids a generated
   file whose name is also a source file in the same package.
2. **The cmd copies the output onto itself.** `rewriteGenruleCmd`
   anchors the cmakeSrc-rooted input path and the buildDir-rooted output
   path to the *same* package-relative token (`version.txt`), then
   `anchorGenruleOutputsToRuledir` rewrites **both** occurrences to
   `$(RULEDIR)/version.txt`. The reference to the real source input is
   lost.

Verified open on `main` (no existing in-place / output-vs-source
collision handling in `lower/`, `emit/bazel/`, or `bazelconstraints/`).

## Fix design

Remediate where the source vs. build paths are **still distinguishable
by their absolute prefixes** — i.e. inside / around `rewriteGenruleCmd`
+ `anchorGenruleOutputsToRuledir` (`converter/internal/lower/genrule.go`,
`standalone_genrules.go`), shared with the recovered-genrule path:

1. **Detect** the collision: a buildDir-rooted output path whose
   package-relative form equals a cmakeSrc-rooted input path's
   package-relative form.
2. **Rename the output** to a non-colliding sibling (e.g. a stable
   generated suffix / subdir) so it no longer shadows the source; record
   the rename so the `outs` list and the cmd's `$(RULEDIR)/<out>` anchor
   both use the new name.
3. **Disambiguate the cmd**: anchor the **input** occurrence to the
   source (`$(location <src>)`, resolving to the real source file) and
   the **output** occurrence to `$(RULEDIR)/<renamed>`. This must happen
   while the two are still separate absolute paths, before the bare-token
   collapse.
4. **Relabel same-package consumers** that referenced the original
   output path to the renamed label (a per-item `# keep` where gazelle
   can't resolve a generated file). For `Remarks.exports`-style outputs
   not consumed by a converted cc target, rename + audit-tag is enough to
   build.
5. **Audit tag** `cmake-codegen-genrule-inplace-rewrite` so the
   remediation is visible.

Cover both the standalone (`lowerStandaloneCustomCommands`) and recovered
(`recoverGenrule`) genrule paths via a shared helper. Guard against
regressions with the existing genrule golden/unit tests.

## Seam-level plan (code-grounded — turnkey for implementation)

Traced to the exact seam:

- `rewriteGenruleCmd` (`lower.go:4997`) strips the cmakeSrc-rooted
  **input** path and the buildDir-rooted **output** path *separately*
  (loop at `lower.go:5062`: anchors `{cmakeSrc → umbrella/rel}` and
  `{buildDir → rel}`), distinguishable there by absolute prefix. In the
  no-umbrella case both collapse to the same rel token (`version.txt`) —
  which is why this bites single-package projects hardest; under umbrella
  they already differ (`llvm/…` vs `…`).
- `anchorGenruleOutputsToRuledir` (`standalone_genrules.go:342`) then
  anchors *every* occurrence of each out token to `$(RULEDIR)/<out>`,
  including the input occurrence when it shares the token.

**Cleanest fix — rename the output first; the strip then disambiguates
itself.** Rename the colliding output (`version.txt` → `version.txt.gen`)
and have the buildDir-strip emit the renamed token: the cmd naturally
becomes `cp version.txt version.txt.gen`, then
`anchorGenruleOutputsToRuledir([version.txt.gen])` anchors only the
output → `cp version.txt $(RULEDIR)/version.txt.gen`. Input reads the
source (a srcs entry staged at its package-relative path), output writes
the renamed file. No `$(location)` wrapper or anchor-guard needed (which
the step-3 "$(location)" idea above would have required — superseded by
this approach).

Steps:
1. Detect: an out whose rel — modulo the umbrella prefix — equals a
   cmakeSrc-derived src rel (compare umbrella-normalized).
2. Build a `buildDir-rel → renamed` map; update `outs` to the renamed
   names.
3. Thread that map into `rewriteGenruleCmd` so only the **buildDir**
   strip emits the renamed token (cmakeSrc strip untouched → source token
   stays distinct). New optional param; both callers pass it (empty map =
   today's behavior, byte-identical → keeps all existing goldens stable).
4. `anchorGenruleOutputsToRuledir` runs on the renamed outs unchanged.
5. Relabel same-package consumers of the original output path to the
   renamed label (skip for outputs no converted target consumes, e.g.
   `Remarks.exports`).
6. Audit tag `cmake-codegen-genrule-inplace-rewrite`.

## Verification plan

Promote the fixture above to a render gate
(`scripts/meta-cmake-genrule-inplace-rewrite.sh`) that converts it,
asserts the output was renamed off `version.txt` and the cmd reads the
source input (not `$(RULEDIR)` for the input), and does a real
`bazel build` of the genrule (it currently fails with the
input==output error).

## Sequencing

Queued to land as its own PR after #399 (install granularity) merges,
per the operator's sequencing. Reproduction fixture + this design are
committed up front so the implementation is grounded and fast.
