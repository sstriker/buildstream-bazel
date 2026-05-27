# FreeDesktop SDK — kind coverage

FDSDK is the working target: 1,092 elements across 21 kinds, the
corpus that drives every fidelity decision in this repo. This doc
captures which kinds have **deep** (introspection-driven,
native-cc-rule-emitting) conversion vs. **coarse** (run-the-build-
in-a-genrule, opaque `install_tree.tar`) conversion.

Snapshot source: `gitlab.com/freedesktop-sdk/freedesktop-sdk` @
`ba490d000` (2026-04-28). Re-run the survey and update this doc when
chasing a newer FDSDK release.

## Conversion-quality levels

- **Deep** — introspection of the build's actual structure produces
  native `cc_library` / `cc_binary` rules. Bazel's incremental build,
  remote cache, and consumer-side cc deps see the element at fine
  grain. Needed for project B's `bazel build //...` to be incremental
  at scale.
- **Coarse** — element renders as a single genrule that runs the
  upstream build in a sandbox and tars the install dir. Downstream
  consumers see `install_tree.tar` as a single filegroup. Edits to
  one `.c` file invalidate the whole element's build action.
  Acceptable for transitive deps; inadequate for elements consumers
  Bazel-build against.
- **Structural** — kind doesn't run a build; renders as Bazel
  filegroup composition over deps' install trees. Quality is "as
  good as the deps' quality."
- **Passthrough** — source already declares Bazel rules; staged
  verbatim.

## Coverage today

All trace-driven kinds (`autotools` / `make` / `manual` / `script` /
`makemaker` / `modulebuild`) share the same deep-conversion shape
through `cmd/convert-element-trace`. The converter operates on cc/ar
execve events captured by `build-tracer` regardless of which build
driver actually invoked them — the per-kind opt-in is one line on
the kind's `pipelineHandler` (`traceDrivenSrckeyPatterns`). Operators
activate the path by passing `--convert-element-trace` +
`--build-tracer-bin` + `--trace-publish-bin` + `--trace-lookup-bin`
to write-a; without those, the kinds fall back to their historical
coarse `install_tree.tar` shape.

| Kind | Count | % | Plugin source | Quality | Notes |
|---|---:|---:|---|---|---|
| `autotools` | 274 | 25.1 % | buildstream-plugins | **deep** | trace-driven via build-tracer + convert-element-trace |
| `meson` | 134 | 12.3 % | buildstream-plugins | **deep** | introspection via convert-element-meson; Phase B install-plan fallback queued |
| `pyproject` | 115 | 10.5 % | community | **deep** | static analysis (no build-backend introspection); per-backend dispatch covers flit / hatchling / setuptools / poetry-core; C-extension / dynamic-metadata / unknown-backend cases Tier-1 refuse and fall back to the pipeline shape |
| `manual` | 104 | 9.5 % | core | **deep** | trace-driven via convert-element-trace |
| `stack` | 96 | 8.8 % | core | structural | filegroup composition over deps |
| `cmake` | 75 | 6.9 % | buildstream-plugins | **deep** | File API + trace-expand |
| `make` | 59 | 5.4 % | buildstream-plugins | **deep** | trace-driven; make-db hint additionally consumed when present |
| `script` | 53 | 4.9 % | core | **deep** | trace-driven via convert-element-trace |
| `filter` | 42 | 3.8 % | core | structural | filegroup composition |
| `flatpak_image` | 26 | 2.4 % | community | structural | install-tree manipulation |
| `compose` | 25 | 2.3 % | core | structural | filegroup composition |
| `import` | 22 | 2.0 % | core | structural | filegroup-only |
| `collect_manifest` | 18 | 1.6 % | community | placeholder | v1 stub |
| `collect_initial_scripts` | 15 | 1.4 % | local | placeholder | v1 stub (FDSDK-local initial-scripts collector) |
| `makemaker` | 14 | 1.3 % | community | **deep** | trace-driven (Perl ExtUtils::MakeMaker still goes through cc/ar) |
| `junction` | 8 | 0.7 % | core | orchestration | cross-project link, project-level concern |
| `snap_image` | 6 | 0.5 % | community | structural | install-tree manipulation |
| `bazel` | n/a | n/a | local | **passthrough** | source ships its own BUILD; verbatim staging |
| `collect_integration` | 2 | 0.2 % | community | placeholder | v1 stub (integration-script collector) |
| `check_forbidden` | 2 | 0.2 % | community | placeholder | v1 stub — assertion does NOT run yet |
| `flatpak_repo` | 1 | 0.1 % | community | placeholder | v1 stub (flatpak-repo packager) |
| `modulebuild` | 1 | 0.1 % | community | **deep** | trace-driven (Perl Module::Build) |

**Today: ~76 % of FDSDK has deep conversion** (25.1 % autotools +
12.3 % meson + 10.5 % pyproject + 9.5 % manual + 6.9 % cmake + 5.4 %
make + 4.9 % script + 1.3 % makemaker + 0.1 % modulebuild). Adding
the structural kinds — whose quality follows their deps' — pushes the
effective figure higher (`stack` / `filter` / `compose` / `import` /
`flatpak_image` / `snap_image` together: ~19.8 %), but those don't
have a build of their own to convert.

**100 % of FDSDK's element-kind catalog now has a handler** — even
if 1.8 % of FDSDK's elements (the glue kinds) get a render-only
placeholder rather than full bazel-build-time correctness.

## FDSDK-specific glue kinds

`collect_initial_scripts` (15), `collect_integration` (2),
`check_forbidden` (2), `flatpak_repo` (1) — total ~20 elements
(1.8 % of FDSDK). Each is small and FDSDK-specific. All four have
v1 stub handlers (same shape as `collect_manifest`) so render of
FDSDK fixtures completes without these kinds breaking the graph. The
stubs emit an empty `install_tree.tar`; real plugin semantics are
not yet ported — see per-kind comments in
`cmd/write-a/handler_*.go`.

Cost-to-port for the real semantics:

- **`collect_initial_scripts`** — walk deps' install trees for
  `%{install-root}/usr/lib/initial-scripts/*` and assemble. Single
  genrule that tars the union; no introspection needed. ~1–2 hours.
- **`collect_integration`** — walk deps' public-domain
  `integration-commands` metadata into
  `%{install-root}/usr/share/integration/integrate.sh`. The
  public-domain metadata isn't currently captured by the converter
  — would need a `kindHandler` extension. ~half a day including
  metadata-capture plumbing.
- **`check_forbidden`** — config-block-driven dep-tree walk + glob
  match, fail-with-diagnostic. Stub today succeeds unconditionally;
  porting needs operator-declared forbidden-pattern parsing + a
  Bazel-time assertion shape (probably a `sh_test` that exits
  non-zero on match). ~half a day.
- **`flatpak_repo`** — ostree repo init + per-image commit + summary
  regen. Needs `ostree` available at bazel-build time (a system tool
  the converter doesn't currently assume). Bigger lift; deferred
  until an FDSDK release-pipeline fixture forces it.

## Source-kind breakdown (1 092 elements)

Sources referenced by FDSDK elements:

| Source kind | Count | Notes |
|---|---:|---|
| `git_tag` | 525 | Tag-pinned git checkouts; trivially mapped to `kind:remote-asset` via `bst source push`. |
| `local` | 388 | In-repo subtree; staged directly by write-a. |
| `tar` | 142 | Pre-fetched tarballs; map to `kind:remote-asset` after a checksum-stable rehost. |
| `patch` | 35 | Composes over the upstream source via a sibling source entry. |
| `zip` | 2 | Same shape as `tar`. |

`bst-translate` (`cmd/bst-translate`) rewrites the non-local sources
to the `kind:remote-asset` shape that the FUSE-served sources mount
expects.

## Empirical takeaway

The two-workspace + three-pass shape (with trace-driven round-2 for
the no-introspection kinds) covers the dominant 76 % of FDSDK at
fine grain. The structural kinds compose over those deeply-converted
elements, so the effective downstream-build correctness for
consumers of FDSDK's `cc_library` graph is higher still. The
remaining gap is the small set of FDSDK-specific glue kinds, which
have v1 stubs today and well-bounded port costs above.
