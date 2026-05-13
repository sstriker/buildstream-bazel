# Build-output conventions — normalizing to Bazel / Gazelle

This repo is a **transition tool**. Its success state, per
`ROADMAP.md`, is "you don't need it anymore — your downstream builds
are plain Bazel." Project A is intermediate scaffolding; the
generated **project B is the post-conversion artifact**, owned and
maintained by operators as a Bazel/Gazelle project from then on.

Two consequences shape every emission decision in this repo:

1. **CMake/Meson/trace/pyproject are sources we extract from, not
   styles we emit in.** Once project B exists, those upstream
   formats are gone. The BUILD shape should look like what a human
   maintaining the project under Bazel/Gazelle would have written.
2. **Operator-owned tooling — buildifier and gazelle — must accept
   our output as-is.** Specifically:
   - `buildifier --mode=fix` against project B is a **no-op**.
   - `gazelle fix` against project B preserves what we emit (no
     attribute churn) AND adds missing deps for new sources
     operators write later, using metadata we ship.

This document fixes the conventions, names the references they
target, calls out deliberate divergences with reasons, and indexes
the phases that realize the contract (tracked in `ROADMAP.md`).

## References

Gazelle's per-language emission shape — the target.

- **C/C++**: [`EngFlow/gazelle_cc`](https://github.com/EngFlow/gazelle_cc).
  bazelbuild/bazel-gazelle ships no cc plugin; gazelle_cc is the
  canonical extension. Emits `cc_library` / `cc_binary` / `cc_test`
  (and declares but doesn't generate `cc_import` /
  `cc_shared_library` / `cc_static_library`, so existing rules of
  those kinds round-trip cleanly).
- **Python**:
  [`bazel-contrib/rules_python/gazelle/python`](https://github.com/bazel-contrib/rules_python/tree/main/gazelle/python).
  Emits `py_library` / `py_binary` / `py_test` with `pyi_srcs`,
  `imports`, and resolves deps via a JSON modules mapping plus
  `pip_parse`-generated requirement repos.
- **Attribute ordering** is buildifier's `tables.NamePriority` —
  the same table `buildifier --mode=fix` enforces. Adopting it
  means our output is byte-equal to what buildifier would have
  produced.

## Reading conventions

File paths and CLI flag names in this doc are stable
references anchored on `main`. The PR that introduces this
doc stacks on an upstream branch authored before the
kind:pyproject converter merged; on that stack base, paths
like `converter/cmd/convert-element-pyproject/...` and flags
like `--convert-element-pyproject` resolve only after the
stack rebases onto `main` (which it will before merge).
Reviewers reading the stack-base view should treat those
references as forward-looking; once the stack lands, the
referenced files and flags exist as named.

## Per-rule-kind shape

### `cc_library`

```
load("@rules_cc//cc:defs.bzl", "cc_library")

package(default_visibility = ["//visibility:public"])

cc_library(
    name = "foo",
    srcs = ["foo.cc", "foo_internal.cc"],
    hdrs = ["include/foo.h"],
    copts = ["-Wno-deprecated"],
    defines = ["FOO_ENABLED=1"],
    include_prefix = "foo",
    strip_include_prefix = "include",
    deps = ["//bar:bar"],
    implementation_deps = ["//baz:baz"],
)
```

Attribute rules:

- `srcs` / `hdrs` are explicit, sorted, set-deduplicated lists.
  Headers are split into `hdrs` (cc_library) or folded into `srcs`
  (cc_binary / cc_test — Bazel 9 doesn't accept `hdrs` on
  executables).
- `deps` holds dependencies whose headers are reachable through
  this library's public headers (`PUBLIC` / `INTERFACE` in CMake's
  `target_link_libraries`). `implementation_deps` holds deps used
  only in this library's `.cc` files (`PRIVATE` in CMake). This
  matches gazelle_cc's split exactly.
- `linkstatic` / `alwayslink` emit when the upstream input
  explicitly indicates them. "Explicit" means: CMake codemodel
  reports the target as `STATIC_LIBRARY` (→ `linkstatic = True`)
  or `OBJECT_LIBRARY` (→ `alwayslink = True`), or the trace
  driver observed an `ar` invocation producing a `.a` archive
  (→ `linkstatic = True` because the recovered target IS a
  static library by construction). In every other case the
  attributes are omitted — gazelle_cc's convention, which we
  match for hand-authored Bazel cc_library compatibility.
- `copts` / `defines` / `linkopts` / `includes` emit when the
  upstream has values for them. gazelle_cc treats these as
  operator-owned because it has no input source; we have rich
  CMake / trace input, so we emit them. They land in shape
  consistent with `gazelle fix` (no rewrite) and `# keep` markers
  signal "this attribute came from extraction; don't synthesize a
  different value."
- `tags` carries `cmake-codegen-*` provenance markers when the
  rule resulted from a codegen lift. Load-bearing for the
  converter audit + verify paths; documented divergence from
  gazelle_cc.
- `visibility` emitted per-rule only when overriding the
  package-level default (rare — typically only for rules consumed
  cross-element or by the element-name facade).

### `cc_binary`

Same attribute set as `cc_library` minus `hdrs` (folded into
`srcs`). `linkstatic` / `alwayslink` are meaningless on binaries
and never emitted.

### `cc_test`

Same as `cc_binary` plus CTest-derived metadata:

- `args` / `env` / `timeout` / `data` carry through from
  `set_tests_properties()` and similar CTest hooks.
- `tags` may include CTest labels.

gazelle_cc doesn't auto-generate any of these (no input source);
we have a richer source and emit them.

Naming convention divergence: gazelle_cc names tests
`<library>_test` after the sibling library. We carry the CTest
registration name (`add_test()` NAME), because operator-visible
test identity matters more than directory-basename alignment in
the post-conversion world. Documented divergence; `# keep` on
`name`.

### `cc_import`

Used only on the round-2 fallback path (prebuilt artifacts from
the install_tree.tar) and the multi-platform fold (per-platform
`static_library` / `shared_library` paths via `select()`).
gazelle_cc declares this kind in `Kinds()` so existing rules
round-trip but never generates one — there's no input shape that
maps. We do, because our round-2 fallback is exactly that input
shape.

Attributes: `static_library` and/or `shared_library` (string or
`select({})`), `hdrs`, `includes` (Phase 2 — pending), per-rule
`visibility` only when overriding.

### `sh_binary`

Used by the cmake round-2 fallback for EXECUTABLE prebuilt
artifacts. Single attribute (`srcs` pointing at the
install_tree.tar-relative artifact path). No gazelle equivalent;
out of scope for cross-checking.

### `py_library`

```
load("@rules_python//python:defs.bzl", "py_library")

package(default_visibility = ["//visibility:public"])

py_library(
    name = "demo",
    srcs = ["demo/__init__.py", "demo/cli.py"],
    pyi_srcs = ["demo/cli.pyi"],
    imports = ["."],
    deps = ["//elements/other:other"],
)
```

Attribute rules:

- `srcs` is sorted, explicit, depth-1 within the package
  directory; never `glob()`. Test files (`*_test.py`,
  `test_*.py`, files under `test*/` directories) are *not*
  included here — they go in the sibling `py_test`.
- `pyi_srcs` carries depth-1 `.pyi` files. Omitted when empty.
- `imports` set on per-package libraries so the package root
  resolves at runtime. Convention follows rules_python.
- `deps` resolves via the imports-manifest mapping (see "Roundtrip
  artifacts" below).
- `conftest.py` is lifted into its own `py_library(name =
  "<pkg>_conftest", testonly = True)` and wired as a `deps`
  entry of the sibling `py_test`. The `<pkg>_` namespace
  prefix differs from rules_python gazelle plugin's bare
  `conftest` name; we use the namespaced form to avoid
  cross-package target-name collisions when multiple
  packages in the same BUILD ship conftests
  (`converter/cmd/convert-element-pyproject/lower.go`).

### `py_binary`

Two paths:

1. **Strict-mode (default for `[project.scripts]` entries whose
   target module self-invokes)**: emit `py_binary` pointing
   directly at the module file, no shim:
   ```
   py_binary(
       name = "greet",
       srcs = ["src/demo/cli.py"],
       main = "src/demo/cli.py",
       deps = [":demo_lib"],
   )
   ```
2. **Shim mode (fallback when the entry module doesn't
   self-invoke, and the operator opt-in
   `--always-emit-entry-shim`)**: emit a genrule that
   materializes a 3-line entry shim plus a `py_binary` consuming
   it. The historical shape.

Also: `__main__.py` in a package directory automatically emits
`py_binary(name = "<pkg>_bin", srcs = ["<pkg>/__main__.py"],
main = "<pkg>/__main__.py", deps = [":<pkg>"])`, matching
gazelle's package-bin convention.

### `py_test`

```
py_test(
    name = "demo_test",
    srcs = ["test_demo.py"],
    deps = [":demo", ":demo_conftest"],
)
```

Test detection follows the rules_python Gazelle plugin's
filename-stem convention: `HasPrefix(stem, "test_") ||
HasSuffix(stem, "_test")`, plus files under `test*/`
directories. The same filename-stem shape (without the
`*_test.py` extension) is what gazelle_cc uses for its
`*_test.cc` detection on the C++ side — both extensions
converge on the same idiom.

### `genrule`

We emit genrules for:

- Project A's per-element converter wrapper (write-a).
- Project B's coarse install_tree.tar build (pipeline-shape
  fallback, kind:autotools / make / manual / script / makemaker
  / modulebuild / pyproject when refused).
- `configure_file` lift (cmake → `//tools:cmake-configure-file`
  Bazel-time invocation).
- Custom commands lowered from cmake `add_custom_command`.
- Entry-shim genrule for `[project.scripts]` in shim mode.

Gazelle never auto-generates genrules. Each of ours carries a
`# keep` marker after the closing `)` so that `gazelle fix` won't
attempt to rewrite or delete them.

### `filegroup`

Used by kind:stack composition, `<elem>_sources` and
`<elem>_real` glob filegroups, project B's
`:install_tree.tar` top-level select-filegroup (multi-platform
fan-out), and the per-element BUILDs that need a stable label for
hand-off between project A and project B.

`# keep` marker on every one — they're load-bearing for the
two-pass pipeline shape, not auto-regenerable from input.

### `package(default_visibility = ...)`

Emitted at the top of every generated BUILD, right after the
`load()` lines, before any rule. Default value:
`["//visibility:public"]`. Per-rule `visibility` only when
overriding (typically `//visibility:private` for internal
helpers).

Matches gazelle_cc canon. The visibility-unification work
(Phase 1) is what turns this from "two conflicting conventions
inside the same codebase" into the single shared convention.

### `load()` lines

- One `load()` per `.bzl` source.
- Symbols sorted alphabetically within each `load()`.
- Loads elided when no rule emits the corresponding kind.
- `load()` lines appear at the top of the file, before
  `package(...)` and before any rule, sorted by `.bzl` path.

### `MODULE.bazel`

Pinned `bazel_dep` per used ruleset:

- `rules_cc` always (every kind:cmake / kind:meson / kind:trace
  element emits cc_* rules).
- `rules_python` conditionally — only when the graph contains a
  kind:pyproject element AND `--convert-element-pyproject` is
  set.
- `bazel_skylib` conditionally — when project options/`string_flag`
  rules are emitted.

Plus `use_extension` blocks for the sources and traces repository
rules.

For post-conversion roundtrip (Phase 7), `MODULE.bazel` also
carries gazelle-config directives at file head:

```
# gazelle:cc_indexfile tools/cc_index.json
# gazelle:cc_use_builtin_bzlmod_index true
# gazelle:python_module_mapping tools/python_modules.json
```

## Roundtrip artifacts (Phase 7)

The conventions above are about the shape of what we emit. For
roundtrip to work — operator adds a new source, runs `gazelle
fix`, deps update correctly — gazelle needs metadata our
extraction process already has. Project B ships these alongside
the BUILD files:

### `tools/cc_index.json`

Header → label map. Sources:

- Every `cc_library` we emit, walked over `hdrs × name`.
- Imports manifest cross-element deps (each external dep's
  exported headers → its Bazel label).
- Supplemented by gazelle_cc's built-in bzlmod registry index
  (`# gazelle:cc_use_builtin_bzlmod_index true`).

Format example:

```json
{
  "absl/strings/str_cat.h": "@abseil-cpp//absl/strings:str_cat",
  "elements/myelem/include/myelem/api.h": "//elements/myelem:myelem"
}
```

### `tools/python_modules.json`

Dist-name → label map, matching rules_python gazelle plugin's
`modules_mapping.json` shape. Sources:

- Imports-manifest entries with their Bazel labels.
- pyproject.toml `[project.scripts]` entries (script-name →
  py_binary label).

### `# keep` markers

Emitted on:

- Every `genrule` rule (closing `)` line).
- Every `filegroup` rule.
- `cc_library.copts` / `defines` / `linkopts` / `includes` /
  `tags` attribute lines (extracted from CMake/trace; gazelle
  shouldn't synthesize different values).
- `cc_*.linkstatic` / `alwayslink` attribute lines when present.
- `cc_test.args` / `env` / `timeout` / `data` / `tags`.
- `py_library.imports` / `pyi_srcs` attributes.
- `package(default_visibility = ...)` line.

### `# gazelle:resolve` directives

For dep edges gazelle can't resolve from `#include` /
`import` lines alone (cross-element via the imports manifest,
script entry points pointing at modules in other elements). Emit
per-element in the element's BUILD when needed:

```
# gazelle:resolve cc external/myelem/api.h //elements/myelem:myelem
# gazelle:resolve py demo_other //elements/other:other
```

### `# gazelle:cc_search`

Mirrors our `cc_library.includes` attribute values so gazelle's
header-scan resolver finds in-tree headers at the right paths.

### Conformance gate

`scripts/meta-gazelle-roundtrip.sh` exercises the contract:

1. Run the converter against a representative fixture.
2. `buildifier --mode=fix` against project B → must be no-op.
3. `gazelle fix` against project B → must be no-op.
4. Add a new source file with an `#include` of an existing
   header; run `gazelle fix` → asserts the resulting `deps`
   includes the right label.

## Lossy paths (documented divergences from gazelle_cc/rules_python)

Some inputs don't carry the full Gazelle-target shape. Project B
emits the most we can extract; the rest is operator-managed.

| Input | Lossy attribute | Effect |
| --- | --- | --- |
| CMake codemodel-v2 (alone) | `target_link_libraries` PUBLIC/PRIVATE/INTERFACE scope | codemodel-v2 in isolation exposes only a flat `Target.Dependencies` list (no per-dep scope) and the rendered link `commandFragments`. The cmake handler runs codemodel and shadow trace together, however — the trace records each `target_link_libraries` call with its keyword arm, and `internal/shadow/trace_commands.go` decodes the arms into per-target lib→keyword maps. Phase 4 wires those maps through `lower/lower.go:lowerTarget` to route PRIVATE deps onto `ImplementationDeps`. So end-to-end, kind:cmake with trace enabled DOES populate the split; a hypothetical codemodel-only invocation (no trace decoded) would fold everything into `Deps`. |
| Meson introspection | `target_link_libraries` PUBLIC/PRIVATE/INTERFACE scope | Same shape as CMake — meson's `meson introspect` likewise reports no per-dep scope. Folds everything into `Deps`. |
| Trace-driven path | Link scope (PUBLIC vs PRIVATE) | The trace-driven converter recovers cc rules from `cc/ar` execve events; the link command-line carries `-lfoo` flags but not the PUBLIC/PRIVATE keyword that drove the cmake `target_link_libraries()` call. Folds into `Deps`. |
| pyproject entry shims (shim mode) | Pure-Bazel `py_binary` shape | `py_binary` references a genrule-materialized shim instead of the module file directly. Operators can opt into strict-mode (default) to get the cleaner shape. |

## Won't-do — architectural mismatches, not deferrals

These are off the table because they conflict with our input
shape, not because they're not worth doing:

- **`cc_proto_library` / `cc_grpc_library` / `cc_shared_library`
  / `cc_static_library` / `objc_library` generation.** We don't
  extract protobuf / gRPC / Objective-C info from CMake codemodel.
  Different input pipeline; out of scope.
- **Header-scan dep resolution (`cc_indexfile` consumption, not
  emission; `cc_search` as a converter input).** Our deps come
  from the CMake graph, which is canonical for our inputs. The
  `cc_indexfile` we emit (Phase 7) is for *gazelle's
  post-conversion use*, not for the converter's own resolution.
- **SCC unit-mode cc grouping.** Our cc-target boundaries come
  from CMake `add_library()` calls, which is the right answer for
  our inputs. gazelle_cc's `groupSourcesByUnits` is a useful
  feature for hand-written codebases that conflate libraries; not
  applicable.
- **One-rule-per-directory cc naming** (gazelle_cc's default).
  Breaks multi-target codemodels (e.g. one `CMakeLists.txt`
  declaring `multibin`, `multishared`, `multistatic` — see
  `testdata/.../multi-target/BUILD.bazel.golden`). Semantic CMake
  target names are load-bearing.
- **Drop `cmake-codegen-*` provenance tags.** Load-bearing for
  the converter's audit + verify paths; the trade-off (slight
  divergence from gazelle_cc which has no provenance signal) is
  worth it.

## Phase index

The contract above is realized in seven phases, tracked in
`ROADMAP.md`'s "Next" section. Phase numbers in this doc refer
to those entries; each is independently shippable.

- **Phase 1** — internal consistency: visibility unification
  across `bazel.Emit` and `converter/cmd/convert-element-pyproject/emit.go`;
  fold trace's inline renderer into `bazel.Emit(toIR(rules))`;
  load-line canonicalization.
- **Phase 2** — attribute completeness: `include_prefix` /
  `strip_include_prefix`; `cc_import.includes`; `py_test`
  emission; `pyi_srcs` discovery; `conftest.py` lift.
- **Phase 3** — buildtools-AST migration of all three current
  renderers (cc, py, write-a format-string handlers) onto
  `bazel.build/buildtools/build`. Result: `buildifier --mode=fix`
  is a no-op.
- **Phase 4** — `implementation_deps` split IR + emit
  plumbing + trace-driven populate path.
  `ir.Target.ImplementationDeps` is the new field; `bazel.Emit`
  renders `implementation_deps = [...]` when populated, in the
  priority-0 alpha block before `deps` per buildifier's
  `NamePriority`. **Populate path**: the cmake-side lowering
  consults `shadow.Decode`'s `target_link_libraries` keyword
  arm (PUBLIC / PRIVATE / INTERFACE / "" for the legacy
  positional shape) and routes PRIVATE deps to
  `ImplementationDeps`. When no trace is available
  (codemodel-only path) or the dep wasn't named in any
  keyword-scoped call, the dep falls through to `Deps` —
  strictly safe (matches pre-Phase-4 behavior). Meson
  introspection and pyproject paths leave the field unset.
- **Phase 5** — entry-shim strict mode (`if __name__ ==
  "__main__":` detection) and `__main__.py` package-bin
  detection.
- **Phase 6** — conventions doc + ROADMAP updates (this doc + the
  phase-index entries).
- **Phase 7** — gazelle roundtrip. Split across two PRs for
  reviewable scope:
  - **Phase 7a** — `# keep` markers on load-bearing attribute
    lines (cc_* copts/defines/linkopts/includes/tags/
    linkstatic/alwayslink/include_prefix/strip_include_prefix,
    cc_test args/env/timeout/data, cc_import-shape attrs,
    py_library/py_test imports/pyi_srcs/testonly, py_binary
    strict-shape main/srcs, package(...) and genrule /
    filegroup whole-rule keeps). Implemented as a buildtools-
    AST post-pass in the cc + py emitters' canonicalize step.
    Markers are inert without gazelle (a # comment any tool
    ignores), so the change is decoupled from Phase 7b.
  - **Phase 7b** — gazelle metadata foundation. Project B's
    MODULE.bazel ships the three `# gazelle:cc_indexfile` /
    `# gazelle:cc_use_builtin_bzlmod_index` /
    `# gazelle:python_module_mapping` directives;
    `tools/cc_index.json` + `tools/python_modules.json` ship
    alongside `tools/sources.json` as `exports_files`-
    declared paths the directives reference. Both index
    files start as empty `{}` content; Phase 7c populates
    them.
  - **Phase 7c** — populate the index files from per-element
    exports via a new `cmd/build-cc-index` Go binary that
    walks project B's staged BUILD.bazel files post-conversion
    and writes `tools/cc_index.json` (header path → label,
    sourced from each `cc_library`'s `hdrs` slice plus the
    `.h`/`.hpp`/`.hxx` subset of its `srcs` — the codemodel
    sometimes lists private headers in `srcs`, so widening the
    index entries from srcs-side captures pre-existing
    under-reporting cheaply) + `tools/python_modules.json`
    (`py_binary` / `py_library` name → label). Bundled with
    `scripts/meta-gazelle-roundtrip.sh` as a conformance gate
    that asserts: the populated index has the expected
    header→label mappings, and (when `buildifier` is on PATH)
    Phase 3's no-op contract still holds post-Phase-7a/b/c.
    The `# gazelle:resolve` directives for cross-element deps
    are queued as a Phase 7d follow-up once `gazelle_cc` is
    wired in.

The phases progress strictly: Phase 1 unifies the renderers,
Phase 2 closes attribute gaps, Phase 3 makes buildifier happy,
Phase 4 closes the deps-shape gap with gazelle_cc, Phase 5
closes the py-binary-shape gap with rules_python's plugin,
Phase 6 freezes the contract in writing, and Phase 7 lights up
the roundtrip story end-to-end.
