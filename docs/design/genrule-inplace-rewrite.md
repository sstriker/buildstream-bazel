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
