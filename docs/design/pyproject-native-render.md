# Pyproject native render — design

Architecture recipe for `converter/cmd/convert-element-pyproject` — the
static-analysis kind:pyproject translator (parses pyproject.toml
+ walks the source tree; doesn't run the build backend). Coverage
context: `docs/fdsdk-coverage-status.md` (pyproject is 10.5 %
of FDSDK, the largest remaining coarse kind after the trace-
driven generalization shipped). Roadmap state: `ROADMAP.md`
Done (Phase A) once this PR lands.

This doc captures the v1 architecture, the patterns covered,
and the patterns refused.

## What kind:pyproject does in BuildStream

The community plugin
(`buildstream_plugins_community/elements/pyproject.{py,yaml}`)
is a one-pager:

```yaml
build-commands:   [ "%{python} -mbuild %{build-args} ." ]
install-commands: [ "%{python} -minstaller %{dist-dir}/*.whl --destdir %{install-root}" ]
```

The element runs the project's PEP 517 build-backend to
produce a wheel, then `python -m installer` unpacks the wheel
under `%{install-root}` (typically `/usr/lib/python3/site-
packages/<pkg>/...` for FDSDK).

There is no dependency resolution at element time. FDSDK
declares one element per Python package and chains them via
`depends:`, mirroring the cc-shaped kinds.

## Why a native lift is worth it

Coarse install_tree.tar conversion works but produces opaque
filegroups: an edit to one .py file invalidates the whole
element's wheel-build action, and downstream py rules can't
import individual modules at fine grain.

`pyproject.toml` is structurally rich (PEP 621 + per-backend
config):

- `[project]` carries the package name, version,
  `dependencies`, `scripts`.
- `[build-system]` carries the build-backend identifier — the
  most useful single piece of information, since each backend
  has stable, documented package-discovery rules.
- `[tool.<backend>]` carries discovery overrides (e.g.
  `tool.setuptools.packages.find{where, include, exclude}`).

Combined with a walk of the source tree, that's enough to
emit native `py_library` / `py_binary` rules without running
the build backend.

## Pipeline

```
                   ┌─────────────────────────────┐
   source tree ──▶ │ parse pyproject.toml         │
                   │ + dispatch on build-backend  │
                   └─────────────────────────────┘
                                │
                                ▼
                   ┌──────────────────────────────┐
                   │ per-backend package discovery │
                   │ (flit / hatchling / setuptools│
                   │  / poetry-core)               │
                   └──────────────────────────────┘
                                │
                                ▼
                   ┌─────────────────────────────┐
                   │  Package + Script structs    │
                   └─────────────────────────────┘
                                │
                                ▼
                   ┌─────────────────────────────┐
                   │  emit BUILD.bazel.out        │
                   └─────────────────────────────┘
                                │
                                ▼
                       BUILD.bazel.out
```

`converter/cmd/convert-element-pyproject/main.go` is a sibling
of `converter/cmd/convert-element-meson/main.go` (and
`cmd/convert-element-trace/main.go`, which still lives under
top-level `cmd/` rather than `converter/cmd/` for historical
reasons). Unlike the cc-shaped
converters it does NOT lower into the shared
`converter/internal/ir` (which is cc-only) — py_* rules have
their own attribute set and a separate emit. The duplicated
emit code is small; the architectural alignment isn't worth
the abstraction overhead at v1 scope.

## Per-target lowering rules

| pyproject.toml shape                    | Emitted Bazel rule |
|-----------------------------------------|---------------------|
| Top-level package directory `<pkg>/` (any backend) | `py_library(name="<pkg>", srcs=[…enumerated .py paths…], imports=[<root>], deps=[…])`. One py_library per package directory (per the design call: not aggregated into a single project-wide rule). `srcs` is an explicit, sorted list of the .py files the discovery pass found — not a `glob()` — so future test files / scratch .py the backend wouldn't have shipped don't accidentally enter the rule. |
| `[project.scripts]` / `[project.gui-scripts]` `name = "module:func"` | `py_binary(name="<name>", srcs=[":<name>_entry"], main="<name>_entry.py", deps=[":<package>"])` plus a sibling `genrule(name="<name>_entry", outs=["<name>_entry.py"], cmd="…printf…")` that materialises a 3-line entry shim (`import sys`, `from <module> import <func>`, `sys.exit(<func>() or 0)`). We don't use `py_console_script_binary` (added in rules_python ≥ 0.21) because this repo doesn't pin rules_python's version in project B's MODULE.bazel — the hand-rolled shim is universally compatible. Per PEP 621 gui-scripts have identical syntax to scripts (the distinction is only that gui-scripts launch without a console window on Windows); v1 emits both as `py_binary` and refuses on key collision between the two tables. |
| `[project.dependencies] foo = "*"` resolving to another in-graph element | `deps += [<imports-manifest label>]`. Convention bind: `<dep>::<dep>` → `//elements/<dep>:<dep>`. |
| `[project.dependencies] foo` not in the manifest and not in the project's own packages | Refuse with `unresolved-pyproject-dependency`. |

`imports = [<root>]` is the prefix the package lives at. For
flat layouts (`<repo>/<pkg>/`) it's `["."]`. For src layouts
(`<repo>/src/<pkg>/`) it's `["src"]`. The discovery step
returns this verbatim per package.

## Backend coverage in v1

| Backend | Discovery shape we recognize |
|---------|------------------------------|
| `flit_core.buildapi` | `tool.flit.module.name` (explicit) or default-by-project-name. Single-distribution backend; v1 recognizes both shapes flit supports: a `<name>/__init__.py` package OR a single-file `<name>.py` module at the source root. The single-file shape emits a `py_library` with `srcs = ["<name>.py"]` and `imports = ["."]`. |
| `hatchling.build` | `tool.hatch.build.targets.wheel.packages` (explicit list). Auto-discovery via VCS-tracked files refused (we don't run git here). |
| `setuptools.build_meta` | `tool.setuptools.packages = [...]` (explicit) or `tool.setuptools.packages.find{where, include, exclude}`. Dynamic auto-discovery (no `packages` config; setuptools' default behaviour) refused. |
| `poetry.core.masonry.api` | `tool.poetry.packages = [{include = ...}]`. |

Anything else (`pdm.backend`, `setuptools.build_meta:__legacy__`,
`mesonpy`, `scikit-build-core`, custom backends) refuses with
`unsupported-pyproject-backend`. With `--convert-element-pyproject`
set, the per-element native genrule fails at bazel-build time
(Tier-1 surface). To route refused elements through the pipeline
shape automatically, operators need either the queued Phase B
install-plan fallback (per-element write-a-time dispatch) or to
re-render without `--convert-element-pyproject` so every
kind:pyproject element takes the pipeline default.

## Typed Tier-1 refusals

| Code | When | Operator action |
|------|------|-----------------|
| `unsupported-pyproject-backend` | `[build-system].build-backend` not in v1 allow-list, or `[build-system]` block missing entirely. | If the backend is one of the v1 set, declare it explicitly. Otherwise wait for the queued Phase B install-plan fallback (per-element write-a-time dispatch) or re-render without `--convert-element-pyproject` (pipeline shape is the no-flag default). |
| `unsupported-pyproject-c-extension` | Source tree contains `*.c`/`*.cpp`/`*.pyx`/`*.rs`/`Cargo.toml` (filename / extension scan only — v1 doesn't parse `setup.py`, so a `setup.py` declaring `ext_modules`/`cmdclass` without sibling extension-source files isn't detected here). | Pure-Python repackage (rare), or wait for the Phase B install-plan fallback queued in ROADMAP. |
| `unsupported-pyproject-dynamic-metadata` | `[project] dynamic = […]` referencing `version` / `dependencies` / `scripts` (which the backend would compute at build time, e.g. via setuptools_scm or hatch-vcs). | Pin the dynamic field statically in pyproject.toml, or accept the pipeline fallback. |
| `unsupported-pyproject-package-discovery` | A recognized backend whose discovery shape we couldn't statically resolve (e.g. setuptools without explicit packages config). | Add an explicit `tool.setuptools.packages = [...]` listing. |
| `unsupported-pyproject-entry-point` | A `[project.scripts]` / `[project.gui-scripts]` entry whose value isn't a valid `module:func` (missing `:`, empty module/func, identifier component that isn't a valid Python identifier — each dotted component must match `[A-Za-z_][A-Za-z0-9_]*`, so leading digits or empty components refuse; the strict subset prevents arbitrary shell syntax leaking into the generated entry-shim genrule), or a name declared in both tables (which would emit two `py_binary` rules with the same target name). | Fix the entry-point spec in pyproject.toml to a valid `module:func` form using Python identifier characters only, or remove the duplicate key. |
| `unresolved-pyproject-dependency` | `[project.dependencies]` entry that's not in this element's own packages and not in the imports manifest. | Add the providing element to the imports manifest, or pre-stage the dep on the executor. |

## Anti-patterns and how v1 handles them

- **`version = "from VCS"`** (setuptools_scm, hatch-vcs): listed under `[project] dynamic = ["version"]`. Refused — version comes from `git describe`, which the converter doesn't run. Operator action: pin a static version string.
- **`[tool.setuptools.dynamic.readme] file = ["README.md"]`** — accepted. Dynamic *metadata* that doesn't affect the dep graph or sources is benign; we ignore the dynamic-readme entry.
- **Console scripts pointing at deps**: `[project.scripts] foo = "external_pkg.cli:main"` — the entry shim still imports `external_pkg`, which must be a `deps` of the binary. If `external_pkg` doesn't resolve (not in our element's deps + not in the imports manifest), refuse with `unresolved-pyproject-dependency`.
- **PEP 420 namespace packages** (no `__init__.py` in the namespace dir): NOT covered in v1. The discovery walkers (flit / hatchling / setuptools / poetry-core) all key on `__init__.py` to identify package directories, and setuptools' `tool.setuptools.packages.find { namespaces = true }` mode is explicitly refused with `unsupported-pyproject-package-discovery`. Operator action: declare each leaf package explicitly (so each constituent has an `__init__.py`-bearing directory we recognize), or accept the pipeline-shape fallback. True PEP 420 lift requires walking `*.py` without an `__init__.py` anchor and would need a per-backend convention for naming the resulting py_library — deferred.
- **Optional dependencies** (`[project.optional-dependencies]` aka extras): NOT folded into the rendered `py_library.deps`. Bazel's analysis-phase model can't represent "user opts into extras at install time". Operators wanting the extras path stage the relevant elements explicitly. v1 just emits the base deps.
- **Multi-source elements and `Directory != ""`**: structurally incompatible with the native genrule's shadow-merge shape (the genrule invokes the converter with `--source-root=$SHADOW` and the converter expects exactly one `pyproject.toml` at the source root; multi-source merges and per-source `Directory` subpaths break both invariants). write-a's `pyprojectHandler` detects these shapes at render time and routes the element to the pipeline shape unconditionally — independent of `--convert-element-pyproject`. A one-line stderr diagnostic per element explains the routing. Operators see the pipeline-shape coarse install genrule for affected elements; the wheel-build + pip install pipeline handles multi-source and `Directory` natively, so the element still builds correctly.

## Cross-element dependency resolution

Same imports-manifest contract the cmake / meson sides use
(`internal/manifest`). The convert-element-pyproject binary
calls `LookupCMakeTarget(name)` against the consumer-side
imports.json that write-a renders next to the per-element
BUILD; resolution order:

1. `LookupCMakeTarget("<dep>")` — bare dep name.
2. `LookupCMakeTarget("<dep>::<dep>")` — convention bind
   write-a uses by default.
3. Otherwise: `unresolved-pyproject-dependency` refusal.

### Element-name facade target

The per-package py_library names from the discovery walk
don't necessarily match the .bst element name — setuptools
normalizes dist-name to package directory (`python-dateutil`
→ `dateutil`), the script-collision rename suffixes `_lib`,
and multi-package distributions have no single "primary"
target. The convention bind `//elements/<elem>:<elem>` would
dangle in any of those cases.

To keep the convention working, write-a passes
`--element-name=<elem.Name>` to the converter, and Lower
appends a stable facade target whenever the element name
doesn't already match an emitted target:

```
py_library(
    name = "<element-name>",
    deps = [
        ":<primary-package-1>",
        ":<primary-package-2>",
        …
    ],
    visibility = ["//visibility:public"],
)
```

The facade has no `srcs` of its own; the deps pull the
per-package libraries' files into the dependency graph
transitively. When element-name DOES match an existing
target (the common case: single-package element with
dist-name == package-name), no facade is emitted — the
primary py_library itself serves as the element's stable
label.

## Phase B install-plan fallback (option A: per-element auto-detection)

Shipped via `--pyproject-fallback`. When set (alongside
`--convert-element-pyproject`), write-a probes each
kind:pyproject element's pyproject.toml at render time by
invoking the converter with `--probe`. The probe runs the full
parse / discover / lower pipeline without writing output and
exits 0 on would-succeed; non-zero otherwise. A Tier-1 refusal
exits 1 with the typed failure on stderr; other non-zero exits
(64 = CLI usage error; 65 = untyped/Tier-2 error covering
anything that isn't a typed Tier-1 refusal — e.g. unreadable
source tree, malformed imports manifest, an unhandled converter
path) indicate probe / infrastructure problems rather than a
typed refusal. write-a treats any non-zero exit as "would
refuse" and routes to the pipeline-shape fallback. Per-element
dispatch:

- Probe exits 0 → render the native genrule
  (//tools:convert-element-pyproject) — Phase A path.
- Probe exits non-zero → render the pipeline-shape coarse
  install genrule — same shape as the no-`--convert-element-
  pyproject` default. Element still ships an install_tree.tar;
  downstream consumers reference `//elements/<elem>:<elem>_install`.
  Operator sees the per-element refusal reason on write-a's
  stderr (matching the diagnostic the converter would emit at
  action time).

Probe results are memoized by element name so re-renders of the
same graph don't re-spawn the binary. Keying by element name (not
by source-tree abspath) avoids cross-contamination between two
elements that share a source directory but declare different
deps: the probe outcome depends on `elem.Deps` via the temp
imports.json that write-a passes through `--imports-manifest`, so
source-path alone isn't a safe key. Per-element spawn cost: well
under a millisecond on a modest dev box; FDSDK's 115 kind:pyproject
elements add ~1s to write-a render time.

Operator UX: flip `--convert-element-pyproject +
--pyproject-fallback` once; every kind:pyproject element renders
correctly regardless of per-element backend / metadata shape.
Refused elements are still install_tree.tar-shaped (no per-target
Bazel labels — that's option B, queued separately), but the
element BUILDs without operator intervention.

Render gate: `scripts/meta-pyproject-fallback.sh` against a
two-element fixture (one Phase-A-friendly setuptools element +
one pdm-backend element that Phase A refuses).

## What's NOT covered (deferred follow-ups, tracked in ROADMAP Later)

- **Phase B option B: per-target py_library stubs** — emit
  stubs that reference the install_tree.tar's contents via a
  build-time extract genrule, so downstream consumers can
  `deps = ["//elements/<elem>:<package>"]` even for refused-
  by-Phase-A elements. Tricky because `py_library.srcs` wants
  concrete .py files at analysis time, not genrule outputs —
  needs a small Starlark wrapper rule. Option A above suffices
  for FDSDK's element-graph use case (downstream consumers are
  themselves elements that link the install tree); option B is
  for non-element downstream consumers that want fine-grained
  py_library deps.
- **C extension support** — needs rules_python's
  C-extension toolchain (or `cc_library` deps via the existing
  trace-driven path). Distinct work; separate roadmap entry.
  Phase B option A above handles C-extension elements coarsely
  via the pipeline shape today.
- **`pip_parse` integration for non-element deps** — for
  downstream non-FDSDK projects whose deps come from PyPI
  rather than from per-element converted graphs. FDSDK
  doesn't use this pattern (every dep is its own element).
- **Package data files** (`tool.setuptools.package-data`,
  `tool.hatch.build.targets.wheel.shared-data`,
  `[project] include` / equivalents). The parser captures
  `tool.setuptools.package-data` but the lift doesn't fold it
  into `py_library`'s `data` attribute yet. Operators whose
  packages bundle non-`.py` runtime resources (templates, JSON
  assets) accept the Phase B fallback for now; the lift
  treats their element as Python-only and the runtime
  resources won't appear in `bazel run`'s runfiles. Phase B
  (install_tree.tar fallback) sidesteps the issue by shipping
  the whole install tree as a filegroup.

## write-a integration

Mirrors the meson handler's opt-in shape: a global
`pyprojectConfig.convertBin` populated from
`--convert-element-pyproject <path>`. When unset, kind:pyproject
falls back to the historical pipeline-shape coarse install
genrule (the existing handler unchanged). When set, the
handler renders project A like kind:cmake / kind:meson do
(per-element genrule invoking `//tools:convert-element-
pyproject` against a staged source tree, producing
`BUILD.bazel.out` as its single declared output — no bundle
artifact in v1; cross-element resolution is purely via the
imports manifest); project B writes the `BUILD_NOT_YET_STAGED`
placeholder that the driver script overwrites.

## Tests and render gate

- Unit tests for the lowering pass + per-backend discovery
  (`converter/cmd/convert-element-pyproject/lower_test.go`):
  each recognized backend's discovery shape; py_library
  emission for flat + src layouts; py_binary + entry-shim
  genrule emission for `[project.scripts]` and
  `[project.gui-scripts]`; deps resolution via the imports
  manifest; the full refusal taxonomy (each Tier-1 code's
  trigger condition).
- Write-a-side handler tests in
  `cmd/write-a/handler_pyproject_test.go`: pipeline-shape
  fallback when convertBin is unset; native-render shape when
  set; tool staging into project A and project B.
- Render gate `scripts/meta-pyproject.sh`: drives write-a end-
  to-end + a standalone-converter assertion against
  `testdata/meta-project/pyproject-greet/`. The fixture is
  setuptools-based (representative of FDSDK's long tail) with
  `[tool.setuptools.packages.find]` discovery + a
  `[project.scripts]` console-script entry. The bazel-build
  half self-skips when bazel < 7 (no bzlmod) is on PATH or
  when bazel/python are missing entirely.
