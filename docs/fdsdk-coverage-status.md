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

All trace-driven kinds (autotools / make / manual / script /
makemaker / modulebuild) share the same deep-conversion shape
through `convert-element-trace`. The converter operates on
cc/ar execve events captured by `build-tracer` regardless of
which build driver actually invoked them — the per-kind
opt-in is one line on the kind's `pipelineHandler`
(`traceDrivenSrckeyPatterns`). Operators activate the path
by passing `--convert-element-trace` + `--build-tracer-bin` +
`--trace-publish-bin` + `--trace-lookup-bin` to write-a; without
those, the kinds fall back to their historical coarse
install_tree.tar shape.

| Kind | Count | % | Quality | Notes |
|---|---|---|---|---|
| `autotools` | 274 | 25.1 % | **deep** | trace-driven via build-tracer + convert-element-trace |
| `meson` | 134 | 12.3 % | **deep** | introspection-driven via convert-element-meson; Phase B install-plan fallback queued |
| `pyproject` | 115 | 10.5 % | coarse | py_library / py_binary mapping deferred (no shared trace-driven path; Python doesn't go through cc/ar) |
| `manual` | 104 | 9.5 % | **deep** | trace-driven via convert-element-trace (when the element's commands invoke cc/ar through any wrapper) |
| `stack` | 96 | 8.8 % | structural | filegroup composition over deps |
| `cmake` | 75 | 6.9 % | **deep** | File API + trace-expand |
| `make` | 59 | 5.4 % | **deep** | trace-driven via convert-element-trace (make-db hint additionally consumed when present) |
| `script` | 53 | 4.9 % | **deep** | trace-driven via convert-element-trace |
| `filter` | 42 | 3.8 % | structural | filegroup composition |
| `flatpak_image` | 26 | 2.4 % | structural | install-tree manipulation |
| `compose` | 25 | 2.3 % | structural | filegroup composition |
| `import` | 22 | 2.0 % | structural | filegroup-only |
| `collect_manifest` | 18 | 1.6 % | placeholder | v1 stub |
| `collect_initial_scripts` | 15 | 1.4 % | **missing** | FDSDK-specific glue |
| `makemaker` | 14 | 1.3 % | **deep** | trace-driven via convert-element-trace (Perl ExtUtils::MakeMaker still goes through cc/ar) |
| `junction` | 8 | 0.7 % | orchestration | cross-project link, project-level concern |
| `snap_image` | 6 | 0.5 % | structural | install-tree manipulation |
| `bazel` | n/a | n/a | **passthrough** | source ships its own BUILD; verbatim staging |
| `collect_integration` | 2 | 0.2 % | **missing** | FDSDK glue |
| `check_forbidden` | 2 | 0.2 % | **missing** | CI assertion |
| `flatpak_repo` | 1 | 0.1 % | **missing** | FDSDK glue |
| `modulebuild` | 1 | 0.1 % | **deep** | trace-driven via convert-element-trace (Perl Module::Build) |

**Today: 25.1 % (autotools) + 12.3 % (meson) + 9.5 % (manual)
+ 6.9 % (cmake) + 5.4 % (make) + 4.9 % (script) + 1.3 %
(makemaker) + 0.1 % (modulebuild) = ~65.5 % of FDSDK has
deep conversion.** Adding the structural kinds — whose
quality follows their deps' — pushes the effective figure
higher (`stack`/`filter`/`compose`/`import`/`flatpak_image`/
`snap_image` together: ~19.8 %), but those don't have a
build of their own to convert.

## Highest-impact next: pyproject

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

1. ~~**meson** — biggest single chunk (12.3 %).~~ Shipped Phase A
   (introspection-driven deep conversion); Phase B install-plan
   fallback queued in `ROADMAP.md` Next.
2. ~~**trace-driven for make / manual / script** (~19.8 %).~~
   Shipped: same `pipelineHandler` opt-in shape kind:autotools
   established. The kind-agnostic `convert-element-trace`
   binary serves all six trace-driven kinds today.
3. **pyproject** — fresh translator (10.5 %). Distinct shape
   from cc-based; Python doesn't go through cc/ar so the trace-
   driven path doesn't apply.
4. **FDSDK glue** — last; small impact each.

Net after these: **~76 % of FDSDK has deep conversion** (vs.
~65 % today; ~32 % before the trace-driven generalization).
