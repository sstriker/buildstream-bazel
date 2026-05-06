# FDSDK kind coverage — what's left, what's next

A snapshot of which FreeDesktop SDK element kinds have
**deep** (introspection-driven, native-Bazel-rule-emitting)
conversion vs. **coarse** (run the build in a genrule, output
an opaque install_tree.tar) conversion. The full kind catalog +
counts is in
[docs/research/fdsdk-element-survey.md](research/fdsdk-element-survey.md).

## Conversion-quality levels

- **Deep** — introspection of the build's actual structure
  produces native cc_library / cc_binary rules. Bazel's
  incremental build, remote cache, and consumer-side cc deps
  see the element at fine grain. Needed for project B's
  `bazel build //...` to be incremental at scale.
- **Coarse** — element renders as a single genrule that runs
  the upstream build in a sandbox and tars the install dir.
  Downstream consumers see install_tree.tar as a single
  filegroup. Edits to one .c file invalidate the whole
  element's build action. Acceptable for transitive deps;
  inadequate for elements consumers Bazel-build against.
- **Structural** — kind doesn't run a build; renders as
  Bazel filegroup composition over deps' install trees.
  Quality is "as good as the deps' quality."
- **Passthrough** — source already declares Bazel rules;
  staged verbatim.

## Coverage today

| Kind | Count | % | Quality | Notes |
|---|---|---|---|---|
| `autotools` | 274 | 25.1 % | **deep (NEW)** | trace-driven via build-tracer + convert-element-autotools |
| `meson` | 134 | 12.3 % | coarse | introspection available but not wired (next high-impact) |
| `pyproject` | 115 | 10.5 % | coarse | py_library / py_binary mapping deferred |
| `manual` | 104 | 9.5 % | coarse | command-list driven; trace-driven path applicable |
| `stack` | 96 | 8.8 % | structural | filegroup composition over deps |
| `cmake` | 75 | 6.9 % | **deep** | File API + trace-expand |
| `make` | 59 | 5.4 % | coarse | command-list driven; trace-driven path applicable |
| `script` | 53 | 4.9 % | coarse | command-list driven; trace-driven path applicable |
| `filter` | 42 | 3.8 % | structural | filegroup composition |
| `flatpak_image` | 26 | 2.4 % | structural | install-tree manipulation |
| `compose` | 25 | 2.3 % | structural | filegroup composition |
| `import` | 22 | 2.0 % | structural | filegroup-only |
| `collect_manifest` | 18 | 1.6 % | placeholder | v1 stub |
| `collect_initial_scripts` | 15 | 1.4 % | **missing** | FDSDK-specific glue |
| `makemaker` | 14 | 1.3 % | coarse | Perl ExtUtils::MakeMaker |
| `junction` | 8 | 0.7 % | orchestration | cross-project link, project-level concern |
| `snap_image` | 6 | 0.5 % | structural | install-tree manipulation |
| `bazel` | n/a | n/a | **passthrough (NEW)** | source ships its own BUILD; verbatim staging |
| `collect_integration` | 2 | 0.2 % | **missing** | FDSDK glue |
| `check_forbidden` | 2 | 0.2 % | **missing** | CI assertion |
| `flatpak_repo` | 1 | 0.1 % | **missing** | FDSDK glue |
| `modulebuild` | 1 | 0.1 % | coarse | Perl Module::Build |

**Today: 25.1% (autotools) + 6.9% (cmake) = 32.0% of FDSDK has
deep conversion. With meson on top: ~44.3%.**

## Highest-impact next: meson

134 elements (12.3% of FDSDK). meson exposes its build graph
via `meson introspect --buildoptions / --targets / --installed`
— a JSON dump analogous to cmake's File API. The meson
introspection is structurally rich enough for native
conversion:

- `--targets` lists every executable / static_library /
  shared_library with its source files, dependencies, and
  per-target compile args.
- `--installed` lists install destinations.
- `--buildoptions` lists build-time options (analog of
  cmake cache values).

A `convert-element-meson` translator would:

1. Run `meson setup` + `meson introspect` against the source.
2. Parse the introspection JSON.
3. Emit native `cc_library` / `cc_binary` rules with proper
   `srcs` / `copts` / `deps`, mirroring the cmake handler's
   shape.
4. Emit a synth cmake-config-bundle equivalent (meson
   pkg-config files via `meson dependency('foo').generate_pc()`)
   for cross-element dep resolution.

Estimate: similar scope to the cmake converter (~2 weeks of
focused work). Reuses everything else (lower IR, bazel
emit, imports manifest, cross-element bundle plumbing).

## After meson: trace-driven for kind:make / kind:manual / kind:script

Combined: 216 elements (19.8% of FDSDK). All are
command-list-driven (no introspection surface). The
trace-driven autotools converter is largely a *trace consumer*
— `convert-element-autotools` cares about cc / ar execve
events, not about how the build was driven. A small
generalization would let it apply to any cc-based build that
goes through a make / shell-script wrapper:

- Rename `convert-element-autotools` → `convert-element-trace`.
- Generalize the make-db hint integration: when the build
  uses `make`, capture make-db; otherwise, skip.
- Wire the trace-driven path into kind:make / kind:manual /
  kind:script handlers (same `pipelineExtension` shape used
  by the autotools handler today).

Estimate: ~1-2 days. Each kind's handler becomes a 30-line
extension wiring.

## After that: pyproject

115 elements (10.5%). Python-shaped — Bazel's native rules
are rules_python's `py_library` / `py_binary` /
`py_console_script_binary`. pyproject.toml has structured
metadata
([PEP 621](https://peps.python.org/pep-0621/)) listing
dependencies, scripts, and entry points.

A `convert-element-pyproject` translator would:

1. Parse `pyproject.toml` (stdlib `encoding/toml` not yet —
   need a Go TOML parser; `github.com/pelletier/go-toml/v2`
   is the standard).
2. For each `[project.scripts]` entry, emit
   `py_console_script_binary`.
3. For each package directory under `[tool.<backend>.packages]`
   (or auto-discovered), emit `py_library`.
4. For `[project.dependencies]`, emit
   `requirement("<name>")` references via rules_python's pip
   integration (assumes a workspace pip lockfile).

Different conversion shape than cc-based; a fresh translator.
Estimate: ~1 week.

## Lowest priority: FDSDK-specific glue

`collect_initial_scripts` (15), `collect_integration` (2),
`check_forbidden` (2), `flatpak_repo` (1) — total ~20
elements (1.8% of FDSDK). Each is small and FDSDK-specific.
A v1 stub handler for each (similar to `collect_manifest`
today) takes about an hour each. Plumb in after the
high-impact items above.

## Recommendation

Tackle in order of impact-per-work-unit:

1. **meson** — biggest single chunk (12.3%), structurally
   rich introspection available. Highest ROI.
2. **trace-driven for make / manual / script** — quick win
   (1-2 days, 19.8% of FDSDK becomes deep instead of coarse).
3. **pyproject** — fresh translator (10.5%). Distinct shape
   from cc-based.
4. **FDSDK glue** — last; small impact each.

Net after these: **~75% of FDSDK has deep conversion** (vs.
32% today).
