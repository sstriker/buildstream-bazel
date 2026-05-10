# Pyproject native render — design

Architecture recipe for `cmd/convert-element-pyproject` — the
introspection-driven kind:pyproject translator. Coverage
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

`cmd/convert-element-pyproject/main.go` is a sibling of
`converter/cmd/convert-element-meson/main.go` and
`cmd/convert-element-trace/main.go`. Unlike the cc-shaped
converters it does NOT lower into the shared
`converter/internal/ir` (which is cc-only) — py_* rules have
their own attribute set and a separate emit. The duplicated
emit code is small; the architectural alignment isn't worth
the abstraction overhead at v1 scope.

## Per-target lowering rules

| pyproject.toml shape                    | Emitted Bazel rule |
|-----------------------------------------|---------------------|
| Top-level package directory `<pkg>/` (any backend) | `py_library(name="<pkg>", srcs=glob(["<pkg>/**/*.py"]), imports=[<root>], deps=[…], data=[…])`. One py_library per package directory (per the design call: not aggregated into a single project-wide rule). |
| `[project.scripts]` `name = "module:func"` | `py_console_script_binary(name="<name>", pkg=":<package>", script="<name>")` when the consumer's Bazel ≥ rules_python's entry-points support; otherwise a hand-rolled `py_binary` whose `srcs` contains a generated entry-shim and whose `main` points at it. |
| `[project.dependencies] foo = "*"` resolving to another in-graph element | `deps += [<imports-manifest label>]`. Convention bind: `<dep>::<dep>` → `//elements/<dep>:<dep>`. |
| `[project.dependencies] foo` not in the manifest and not in the project's own packages | Refuse with `unresolved-pyproject-dependency`. |
| Package data files (e.g. `tool.setuptools.package-data`) | Folded into the `py_library`'s `data` attribute as a globbed filegroup. |

`imports = [<root>]` is the prefix the package lives at. For
flat layouts (`<repo>/<pkg>/`) it's `["."]`. For src layouts
(`<repo>/src/<pkg>/`) it's `["src"]`. The discovery step
returns this verbatim per package.

## Backend coverage in v1

| Backend | Discovery shape we recognize |
|---------|------------------------------|
| `flit_core.buildapi` | `tool.flit.module.name` (explicit) or default-by-project-name. Single-package backend. |
| `hatchling.build` | `tool.hatch.build.targets.wheel.packages` (explicit list). Auto-discovery via VCS-tracked files refused (we don't run git here). |
| `setuptools.build_meta` | `tool.setuptools.packages = [...]` (explicit) or `tool.setuptools.packages.find{where, include, exclude}`. Dynamic auto-discovery (no `packages` config; setuptools' default behaviour) refused. |
| `poetry.core.masonry.api` | `tool.poetry.packages = [{include = ...}]`. |

Anything else (`pdm.backend`, `setuptools.build_meta:__legacy__`,
`mesonpy`, `scikit-build-core`, custom backends) refuses with
`unsupported-pyproject-backend` and falls back to the existing
pipeline-shape coarse install genrule.

## Typed Tier-1 refusals

| Code | When | Operator action |
|------|------|-----------------|
| `unsupported-pyproject-backend` | `[build-system].build-backend` not in v1 allow-list, or `[build-system]` block missing entirely. | If the backend is one of the v1 set, declare it explicitly. Otherwise this element falls back to the pipeline shape; works, just not Bazel-incremental. |
| `unsupported-pyproject-c-extension` | Source tree contains `*.c`/`*.cpp`/`*.pyx`/`*.rs`/`Cargo.toml`/`setup.py:cmdclass`/`setup.py:ext_modules`. | Pure-Python repackage (rare), or wait for the Phase B install-plan fallback queued in ROADMAP. |
| `unsupported-pyproject-dynamic-metadata` | `[project] dynamic = […]` referencing `version` / `dependencies` / `scripts` (which the backend would compute at build time, e.g. via setuptools_scm or hatch-vcs). | Pin the dynamic field statically in pyproject.toml, or accept the pipeline fallback. |
| `unsupported-pyproject-package-discovery` | A recognized backend whose discovery shape we couldn't statically resolve (e.g. setuptools without explicit packages config). | Add an explicit `tool.setuptools.packages = [...]` listing. |
| `unresolved-pyproject-dependency` | `[project.dependencies]` entry that's not in this element's own packages and not in the imports manifest. | Add the providing element to the imports manifest, or pre-stage the dep on the executor. |

## Anti-patterns and how v1 handles them

- **`version = "from VCS"`** (setuptools_scm, hatch-vcs): listed under `[project] dynamic = ["version"]`. Refused — version comes from `git describe`, which the converter doesn't run. Operator action: pin a static version string.
- **`[tool.setuptools.dynamic.readme] file = ["README.md"]`** — accepted. Dynamic *metadata* that doesn't affect the dep graph or sources is benign; we ignore the dynamic-readme entry.
- **Console scripts pointing at deps**: `[project.scripts] foo = "external_pkg.cli:main"` — the entry shim still imports `external_pkg`, which must be a `deps` of the binary. If `external_pkg` doesn't resolve (not in our element's deps + not in the imports manifest), refuse with `unresolved-pyproject-dependency`.
- **PEP 420 namespace packages** (no `__init__.py` in the namespace dir): accepted. Each constituent package gets its own `py_library`; consumers link them all to assemble the namespace.
- **Optional dependencies** (`[project.optional-dependencies]` aka extras): NOT folded into the rendered `py_library.deps`. Bazel's analysis-phase model can't represent "user opts into extras at install time". Operators wanting the extras path stage the relevant elements explicitly. v1 just emits the base deps.

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

## What's NOT covered (deferred follow-ups, tracked in ROADMAP Next/Later)

- **Phase B install-plan fallback** — analog of meson's queued
  Phase B. For elements that refuse v1 (C extensions, unknown
  backends, dynamic metadata), emit per-target `py_library`
  stubs against an install_tree.tar that comes from project
  B's install genrule (the existing pipeline shape's output).
  Keeps Bazel labels resolvable for downstream consumers even
  when the per-file lift fails.
- **C extension support** — needs rules_python's
  C-extension toolchain (or `cc_library` deps via the existing
  trace-driven path). Distinct work; separate roadmap entry.
- **`pip_parse` integration for non-element deps** — for
  downstream non-FDSDK projects whose deps come from PyPI
  rather than from per-element converted graphs. FDSDK
  doesn't use this pattern (every dep is its own element).

## write-a integration

Mirrors the meson handler's opt-in shape: a global
`pyprojectConfig.convertBin` populated from
`--convert-element-pyproject <path>`. When unset, kind:pyproject
falls back to the historical pipeline-shape coarse install
genrule (the existing handler unchanged). When set, the
handler renders project A like kind:cmake / kind:meson do
(per-element genrule invoking `//tools:convert-element-
pyproject` against a staged source tree, producing
BUILD.bazel.out + a placeholder pkg-bundle.tar); project B
writes the `BUILD_NOT_YET_STAGED` placeholder that the driver
script overwrites.

## Tests and render gate

- Unit tests for the pyproject parser
  (`cmd/convert-element-pyproject/parse_test.go`): each
  recognized backend's discovery shape; each refusal code's
  trigger condition.
- Unit tests for the lowering pass
  (`cmd/convert-element-pyproject/lower_test.go`): py_library
  emission for flat + src layouts, py_console_script_binary
  emission, deps resolution via the imports manifest, the
  full refusal taxonomy.
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
