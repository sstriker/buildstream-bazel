# Meta-project test fixtures

End-to-end fixtures for the Bazel-as-orchestrator shape described
in `docs/architecture.md`. Eleven fixtures so far:

- **`hello-world.bst`** + **`sources/hello-world/`** — single cmake
  element. Phase 1 acceptance gate (`make e2e-meta-hello`).
- **`two-libs/`** — multi-element graph: two `kind: cmake`
  elements (`lib-a.bst`, `lib-b.bst`) plus one `kind: stack`
  (`runtime.bst`) bundling them. Phase 2 acceptance gate
  (`make e2e-meta-stack`).
- **`cross-cmake/`** — two `kind: cmake` elements where the
  consumer (`cons.bst`) `depends:` on the producer
  (`prod.bst`) and uses `find_package(prod CONFIG REQUIRED)`
  to resolve `prod::prod` from the producer's synthesized
  cmake-config bundle. write-a's cmake handler stages the
  producer's `cmake_config_bundle` filegroup as an action
  input, extracts it under `$PREFIX/lib/cmake/<dep>/` at
  genrule time, and passes `--prefix-dir=$PREFIX` to
  convert-element-cmake. Render-only Go test in
  `cmd/write-a/main_test.go`
  (`TestWriter_CmakeElementStagesDepBundles`); end-to-end
  bazel-build acceptance is gated on bzlmod-capable bazel
  and lands as a follow-up shell script.
- **`manual-greet/`** — single `kind: manual` element with a
  trivial install pipeline. Phase 3 acceptance gate
  (`make e2e-meta-manual`).
- **`make-greet/`** — single `kind: make` element with a Makefile
  that builds a tiny binary and a `make install` target. Phase 3
  sibling-kind acceptance gate (`make e2e-meta-make`).
- **`vars-greet/`** — single `kind: manual` element exercising the
  full layered variable scope: `project.conf` sets `prefix=/usr`,
  the `.bst`'s `variables:` block overrides it again to
  `/opt/freedesktop-sdk`, and a custom `%{greeting-dir}` composes
  onto derived defaults. Variable-resolver acceptance gate
  (`make e2e-meta-vars`).
- **`compose-greet/`** — multi-element graph: two `kind: cmake`
  elements (`greet-a`, `greet-b`) plus one `kind: compose`
  (`bundle.bst`) bundling them. Phase 2 sibling-kind acceptance
  gate (`make e2e-meta-compose`); rendering-equivalent to
  `two-libs/`'s stack but using `kind: compose`.
- **`filter-greet/`** — multi-element graph: 1 `kind: cmake` parent
  (`greet`) + 1 `kind: filter` (`greet-headers.bst`) intending to
  keep only the `public-headers` domain. Domain-based slicing is
  deferred (the filter currently passes the parent through); the
  `.bst`'s `config: include / exclude / include-orphans` is
  recorded as comments in the rendered BUILD. Phase 2 acceptance
  gate (`make e2e-meta-filter`).
- **`import-greet/`** — single `kind: import` element with a
  `kind: local` source tree of two files (`greeting.txt`,
  `manifest.json`). write-a stages the tree verbatim into project
  B and renders a filegroup over `glob(["**/*"])`. Phase 2
  acceptance gate (`make e2e-meta-import`).
- **`conditional-greet/`** — single `kind: manual` element with a
  `(?):` per-arch variable-override block. write-a lowers the
  block into a project-B `cmd = select({...})` over
  `@platforms//cpu:*`; the conditional-lowering acceptance gate
  (`make e2e-meta-conditional`) asserts the per-arch resolved
  paths flow through.
- **`script-greet/`** — single `kind: script` element with a flat
  `config: commands:` list. write-a maps the list onto the
  pipelineHandler's install-commands slot at render time.
  Acceptance gate (`make e2e-meta-script`).
- **`autotools-greet/`** — single `kind: autotools` element with a
  minimal `./configure` script (honors `--prefix`, accepts and
  ignores the rest of the canonical autoconf flag set), a
  `Makefile.in` template, and a `greet.c` source. Exercises the
  BuildStream autotools plugin's `%{autogen}` / `%{configure}` /
  `%{make}` / `%{make-install}` chain end-to-end under the
  project.conf prefix override. Phase 3 sibling-kind acceptance
  gate (`make e2e-meta-autotools`).

Each fixture that uses the variable resolver also ships a tiny
`project.conf` overriding `prefix=/usr` (matches FDSDK's
project-conf overlay of BuildStream stock `/usr/local`).

## hello-world fixture (Phase 1)

```
testdata/meta-project/
  hello-world.bst             # the sole .bst input
  sources/hello-world/        # the element's source tree (cmake)
    CMakeLists.txt
    hello.c
    include/hello.h
```

`scripts/meta-hello.sh` drives the pipeline:

1. `cmd/write-a` parses `hello-world.bst`, resolves the `kind: local`
   source, and renders **two** workspaces:
   - **Project A** (the meta workspace): `MODULE.bazel`,
     `tools/convert-element-cmake` (staged binary), `rules/zero_files.bzl`,
     and `elements/hello-world/BUILD.bazel` containing one `genrule`
     that invokes `convert-element-cmake` plus a `zero_files` target for
     any source paths the converter doesn't read.
   - **Project B** (the consumer workspace): `MODULE.bazel`
     (`bazel_dep` on `rules_cc`), the user's source tree under
     `elements/hello-world/`, and a `BUILD_NOT_YET_STAGED`
     placeholder waiting for project A's converted output.
2. `bazel build` in project A runs the genrule; convert-element-cmake
   emits `BUILD.bazel.out` (a `cc_library` declaration) plus
   `cmake-config-bundle.tar` and `read_paths.json`.
3. The driver overwrites project B's per-element `BUILD.bazel`
   with project A's `BUILD.bazel.out`. The placeholder is gone;
   project B's element package is now well-formed.
4. The driver writes a hand-authored smoke target into
   `project-B/smoke/` (`cc_binary` linking against the converted
   `cc_library`).
5. `bazel build` + `bazel run` in project B compiles + executes the
   smoke binary; the gate asserts the output contains
   "Hello, World!".

After the round-trip succeeds, two cache-stability assertions
through both projects:

- **Scenario A** (`hello.c` content edit): convert-element-cmake cache-hits
  in project A (zero-stub-backed input merkle is content-stable
  across edits to non-read source files); project B's smoke binary
  still prints "Hello, World!".
- **Scenario B** (CMakeLists.txt comment edit): convert-element-cmake
  re-runs in project A (CMakeLists is real) but produces a
  byte-identical `BUILD.bazel.out` (cmake's parser strips comments
  before the codemodel); project B's smoke binary sha is unchanged
  (no rebuild).

### Why this fixture

The element source tree is a verbatim copy of
`converter/testdata/sample-projects/hello-world/` — the smallest
cmake project that exercises the full convert-element-cmake pipeline
(cc_library + install + cmake-config export). Reusing it keeps
the gate's correctness anchored to existing fixture coverage.

## two-libs fixture (Phase 2)

```
testdata/meta-project/two-libs/
  lib-a.bst                # kind:cmake, kind:local source under sources/lib-a/
  lib-b.bst                # kind:cmake, kind:local source under sources/lib-b/
  runtime.bst              # kind:stack, depends on [lib-a, lib-b]
  sources/
    lib-a/{CMakeLists.txt, lib-a.c, include/lib-a.h}
    lib-b/{CMakeLists.txt, lib-b.c, include/lib-b.h}
```

Each lib defines `lib_<x>_message()` returning `"lib-<x> says hi"`
and declares `add_library(lib-<x>)` so the converter emits
`cc_library(name="lib-<x>")` in project A — matching the Phase 2
convention where `kind: stack` references its deps as
`//elements/<dep>:<dep>`.

`scripts/meta-stack.sh` drives the pipeline:

1. `cmd/write-a` parses all three .bst files, builds the dep DAG
   (lib-a → mid, lib-b → mid, runtime → both), renders project A
   (per-element genrules for the two cmake elements + a no-target
   marker package for `runtime`) and project B (cc_library
   placeholders + the runtime filegroup composing dep labels).
2. `bazel build` in project A runs convert-element-cmake on each cmake
   element.
3. The driver stages each cmake element's `BUILD.bazel.out` into
   project B's `elements/<name>/BUILD.bazel`.
4. `bazel build //elements/runtime:runtime` in project B validates
   the stack's filegroup resolves all dep labels (the multi-element
   graph composes correctly).
5. The driver writes a smoke target (`cc_binary` depending on both
   `lib-a` and `lib-b`) and `bazel run`s it; output must contain
   both libs' messages.

## manual-greet fixture (Phase 3)

```
testdata/meta-project/manual-greet/
  greet.bst                   # kind:manual + kind:local source
  sources/greeting.txt        # "Hello from kind:manual!"
```

Smallest viable kind:manual fixture: an element whose only
phase-command list is `install-commands`, copying the staged
`greeting.txt` to `%{install-root}%{prefix}/share/greeting.txt`.
Exercises both variable substitutions Phase 3's manual handler
supports (`%{install-root}` / `%{prefix}`) and the install-tree-
tarball output shape.

`scripts/meta-manual.sh` drives the pipeline:

1. `cmd/write-a` parses `greet.bst`, renders project A (a per-
   element genrule that runs the install-commands and tars
   `$INSTALL_ROOT` as `install_tree.tar`) and project B (a
   placeholder package; the typed-filegroup wrapper for
   downstream consumers lands in a follow-up).
2. `bazel build //elements/greet:greet_install` in project A.
3. The driver extracts `bazel-bin/elements/greet/install_tree.tar`
   and asserts:
   - `usr/share/greeting.txt` exists.
   - Its content is `"Hello from kind:manual!"`.

## make-greet fixture (Phase 3 sibling)

```
testdata/meta-project/make-greet/
  greet.bst                   # kind:make + kind:local source; no config:
  sources/
    Makefile                  # all: greet ; install: install -D greet ...
    greet.c                   # prints "greet from kind:make"
```

Smallest viable `kind: make` fixture: an empty `config:` block
exercises the kind's defaults (`make` for build, `make -j1
DESTDIR="%{install-root}" install` for install). The Makefile's
`all` target compiles `greet.c` via `cc`; `install` places the
binary at `%{install-root}%{prefix}/bin/greet`.

`scripts/meta-make.sh` drives the same render → bazel-build →
extract pipeline as `meta-manual.sh`, then runs the extracted
`usr/bin/greet` binary and asserts its output is
`"greet from kind:make"`. End-to-end proof that kind:make's
defaults compose correctly with the shared pipelineHandler shape.

## vars-greet fixture (variable resolver)

```
testdata/meta-project/vars-greet/
  greet.bst                   # kind:manual; variables: { prefix, greeting-dir }
  sources/greeting.txt        # "Hello from a custom prefix!"
```

Exercises the variable resolver (`cmd/write-a/variables.go`) end-
to-end. The `.bst`'s `variables:` block:

- Overrides `%{prefix}` from the project default `/usr` to
  `/opt/freedesktop-sdk`. Every derived default (`%{datadir}`,
  `%{libdir}`, `%{bindir}`, ...) follows the override automatically
  via the resolver's recursive expansion.
- Defines a fresh user variable `%{greeting-dir}` whose RHS
  references `%{datadir}` — so `%{greeting-dir}` resolves to
  `/opt/freedesktop-sdk/share/greetings` purely through layered
  expansion.

The single `install-commands` line writes the staged
`greeting.txt` to `%{install-root}%{greeting-dir}/hello.txt`. After
resolution, the rendered genrule cmd reads:

```
install -D greeting.txt $INSTALL_ROOT/opt/freedesktop-sdk/share/greetings/hello.txt
```

`scripts/meta-vars.sh` drives the same render → bazel-build →
extract pipeline as the other Phase 3 gates, plus extra render-
phase grep checks asserting no `%{...}` reference leaks through
unsubstituted (a typo'd variable in a `.bst` would surface as a
`variable %q referenced but not defined` error from
`cmd/write-a` rather than a silent literal in the rendered BUILD).
