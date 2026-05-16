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
| `pyproject` | 115 | 10.5 % | **deep** | static analysis of pyproject.toml + a source-tree walk via convert-element-pyproject (no build-backend introspection — pyproject.toml is structurally rich enough on its own); per-backend dispatch covers flit / hatchling / setuptools / poetry-core; C-extension / dynamic-metadata / unknown-backend cases Tier-1 refuse — with `--convert-element-pyproject` set the per-element genrule fails at bazel-build time, so operators re-render without the flag to take the pipeline-shape default (per-element write-a-time dispatch that routes refused elements to the pipeline shape automatically is the queued Phase B install-plan fallback follow-up) |
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
| `collect_initial_scripts` | 15 | 1.4 % | placeholder | v1 stub (FDSDK-local initial-scripts collector) |
| `makemaker` | 14 | 1.3 % | **deep** | trace-driven via convert-element-trace (Perl ExtUtils::MakeMaker still goes through cc/ar) |
| `junction` | 8 | 0.7 % | orchestration | cross-project link, project-level concern |
| `snap_image` | 6 | 0.5 % | structural | install-tree manipulation |
| `bazel` | n/a | n/a | **passthrough** | source ships its own BUILD; verbatim staging |
| `collect_integration` | 2 | 0.2 % | placeholder | v1 stub (integration-script collector) |
| `check_forbidden` | 2 | 0.2 % | placeholder | v1 stub — assertion does NOT run yet |
| `flatpak_repo` | 1 | 0.1 % | placeholder | v1 stub (flatpak-repo packager) |
| `modulebuild` | 1 | 0.1 % | **deep** | trace-driven via convert-element-trace (Perl Module::Build) |

**Today: 25.1 % (autotools) + 12.3 % (meson) + 10.5 %
(pyproject) + 9.5 % (manual) + 6.9 % (cmake) + 5.4 % (make)
+ 4.9 % (script) + 1.3 % (makemaker) + 0.1 % (modulebuild) =
~76.0 % of FDSDK has deep conversion.** Adding the structural
kinds — whose quality follows their deps' — pushes the
effective figure higher (`stack`/`filter`/`compose`/`import`/
`flatpak_image`/`snap_image` together: ~19.8 %), but those
don't have a build of their own to convert.

## FDSDK-specific glue

`collect_initial_scripts` (15), `collect_integration` (2),
`check_forbidden` (2), `flatpak_repo` (1) — total ~20
elements (1.8% of FDSDK). Each is small and FDSDK-specific.
All four now have **v1 stub handlers** (same shape as the
pre-existing `collect_manifest` stub) so render of FDSDK
fixtures completes without these kinds breaking the graph.
The stubs emit an empty `install_tree.tar`; **real plugin
semantics are not yet ported** — see per-kind comments in
`cmd/write-a/handler_*.go` for what the real plugin does and
what would need to change for it to ride a bazel-build-time
contract.

Cost-to-port for the real semantics, per kind:

- **`collect_initial_scripts`** — walk deps' install trees
  for `%{install-root}/usr/lib/initial-scripts/*` and
  assemble. Could be a single genrule that tars the union;
  no introspection needed. ~1-2 hours.
- **`collect_integration`** — walk deps' public-domain
  `integration-commands` metadata into
  `%{install-root}/usr/share/integration/integrate.sh`. The
  public-domain metadata isn't currently captured by the
  converter — would need a kindHandler extension. ~half a
  day including the metadata-capture plumbing.
- **`check_forbidden`** — config-block-driven dep-tree walk +
  glob match, fail-with-diagnostic. Stub today succeeds
  unconditionally; porting needs the operator-declared
  forbidden-pattern parsing + a Bazel-time assertion shape
  (probably a sh_test that exits non-zero on match). ~half a
  day.
- **`flatpak_repo`** — ostree repo init + per-image commit +
  summary regen. Needs `ostree` available at bazel-build
  time (a system tool the converter doesn't currently
  assume). Bigger lift; deferred until an FDSDK release-
  pipeline fixture forces it.

## Recommendation

Tackle in order of impact-per-work-unit:

1. ~~**meson** — biggest single chunk (12.3 %).~~ Shipped Phase A
   (introspection-driven deep conversion); Phase B install-plan
   fallback queued in `ROADMAP.md` Next.
2. ~~**trace-driven for make / manual / script** (~19.8 %).~~
   Shipped: same `pipelineHandler` opt-in shape kind:autotools
   established. The kind-agnostic `convert-element-trace`
   binary serves all six trace-driven kinds today.
3. ~~**pyproject** — fresh translator (10.5 %).~~ Shipped Phase
   A (per-backend dispatch over flit / hatchling / setuptools /
   poetry-core; C-extension / dynamic-metadata / unknown-
   backend cases Tier-1 refuse — operators re-render without
   `--convert-element-pyproject` to take the pipeline-shape
   default; per-element write-a-time dispatch for refused
   elements is the queued Phase B install-plan fallback
   follow-up).
4. ~~**FDSDK glue** — last; small impact each.~~ All four
   (`collect_initial_scripts`, `collect_integration`,
   `check_forbidden`, `flatpak_repo`) now have v1 stub
   handlers so render of FDSDK fixtures reaches completion.
   Real plugin semantics deferred until an FDSDK fixture
   forces it; see "FDSDK-specific glue" above for per-kind
   port cost.

Coverage today: **~76 % of FDSDK has deep conversion** (vs.
~65 % before pyproject; ~32 % before the trace-driven
generalization). With the v1-stub FDSDK-glue kinds, **100 %
of FDSDK's element-kind catalog now has a handler** — even
if 1.8 % of FDSDK's elements (the glue kinds) get a
render-only placeholder rather than full bazel-build-time
correctness.
