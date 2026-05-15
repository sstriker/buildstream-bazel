package main

import "fmt"

// handler_meson_round2.go: kind-specific scaffolding for the
// kind:meson round-2 fallback shape (Phase B).
//
// Phase A (convert-element-meson native render) lifts elements
// whose introspection survives the v1 lowering pass; refusals
// emit a typed Tier-1 failure (`unsupported-meson-subproject`,
// `unsupported-meson-target-type`, `unsupported-meson-custom-target`,
// `unsupported-meson-generated-sources`,
// `unsupported-meson-cross-compile`, `unresolved-meson-dependency`).
// Phase B replaces the exclusion with a round-2 coarse-genrule
// fallback: A's converter genrule emits an install-plan-driven
// placeholder shape (per-target cc_import / sh_binary / cc_library
// stubs referencing `install_tree.tar`); B's install genrule wraps
// `meson setup + ninja + meson install --destdir + tar` under
// build-tracer with inline trace-publish.
//
// Architectural shape mirrors kind:cmake's Phase B fallback
// (`docs/design/cmake-execute-process-round2-fallback.md`). The
// kind-agnostic plumbing (`renderTraceDrivenRound2A`, the
// build-tracer / trace-publish / trace-lookup wire contract,
// `SyntheticActionDigest(srckey)` AC keying) is reused as-is.
//
// What kind:meson does NOT reuse:
//
//   - pipelineTraceExtensionRound2 + pipelineTracePublishStep
//     hard-code autotools assumptions: they call
//     wrapAutotoolsPipelineCmds, write to $$AUTOTOOLS_TRACE, and
//     capture `make -np` for the make-db. kind:meson's install
//     genrule needs its own equivalent that uses
//     wrapMesonPipelineCmds (defined below) and $$MESON_TRACE,
//     and omits --make-db on the inline trace-publish call
//     (meson uses ninja directly — no `make` database to
//     capture; trace-publish handles missing --make-db
//     natively, publishing a trace.log-only Directory).

// mesonSrckeyPatterns returns the file-glob set that gates
// srckey content-inclusion for kind:meson round-2.
//
// Content-included (rule.Include == true): paths whose **byte
// content** changes the trace that meson setup + ninja + install
// would record. Path-only (no rule, default-deny): everything
// else — adding/removing a path invalidates srckey via the
// universe, but editing an existing file's content does not.
//
// Per-pattern rationale:
//
//   - meson.build + nested meson.build — the canonical
//     declarative driver. Every declaration (static_library,
//     executable, subdir, install_headers, etc.) reshapes the
//     build graph the trace records.
//   - meson_options.txt / meson.options — option declarations
//     consumed at configure time; values flow into compile
//     commands.
//   - *.cfg / *.toml under meson.build's ecosystem — pkg-config
//     templates, machine files, native files. Less common than
//     the above but content-affecting when present.
//   - **/*.h family + **/*.hpp / **/*.hxx / **/*.hh — header
//     edits shift include resolution and #include-driven
//     conditionals, which surface in compile commands the
//     trace records (added/removed -I dirs, distinct ninja
//     deps DB entries).
//
// Compile sources (*.c / *.cc / *.cpp / *.cxx / *.s / *.S) are
// **path-only**: the trace records compile commands, not source
// bytes; edits to .c files don't change the trace's structure.
//
// Mirrors cmakeSrckeyPatterns / autotoolsSrckeyPatterns shape —
// same Include semantics, same default-deny behaviour for
// unmatched paths.
func mesonSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "meson.build"},
			{Include: true, Pattern: "**/meson.build"},
			{Include: true, Pattern: "meson_options.txt"},
			{Include: true, Pattern: "**/meson_options.txt"},
			{Include: true, Pattern: "meson.options"},
			{Include: true, Pattern: "**/meson.options"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
			{Include: true, Pattern: "**/*.hh"},
		},
	}
}

// mesonRound2InstallBuild renders Project B's install genrule
// for the kind:meson round-2 fallback shape. The genrule wraps
// `meson setup` + `ninja` + `meson install --destdir` under
// build-tracer, tars the install tree, and inline-publishes the
// trace via trace-publish.
//
// The configure step pins `--prefix=/` + `--libdir=lib` so the
// install tree's relative layout (`lib/libfoo.a`, `bin/foo`,
// `include/foo.h`) matches what the placeholder shape in A's
// BUILD.bazel.out references via install_tree/<dest>. Both sides
// share the same expectation about install-path placeholders;
// without the pin, multiarch hosts produce `lib/x86_64-linux-gnu/...`
// or the host's `/usr/local` prefix gets baked in, and A's
// computed paths drift from B's actual install bytes.
//
// Outputs:
//   - install_tree.tar — the installed artefacts the
//     placeholder's extract genrule untars (referenced via
//     same-package label "install_tree.tar" once A's
//     BUILD.bazel.out gets symlinked in).
//   - trace.log — canonical build-tracer output; trace-publish
//     reads it to land the AC entry.
//
// trace-publish runs only when the action environment supplies
// CAS_GRPC_ADDR (devs running locally without a remote cache
// leave it unset; the build still succeeds, no AC entry
// published).
//
// Multi-platform knobs: when plat.Name is non-empty (write-a's
// --platforms-json fan-out), the genrule is suffixed with the
// platform name, all outs are prefixed with `<plat>/`,
// exec_compatible_with carries the constraint set, and the
// trace-publish call bakes --platform=<plat> literally. Empty
// plat.Name renders the single-platform legacy shape (byte-
// stable with no fan-out applied).
func mesonRound2InstallBuild(elem *element, plat tracePlatform) string {
	nameSuffix := ""
	outputPrefix := ""
	execAttr := ""
	publishPlatform := `$${CMAKE_TO_BAZEL_PLATFORM:-}`
	if plat.Name != "" {
		nameSuffix = "_" + plat.Name
		outputPrefix = plat.Name + "/"
		// Reuse pipelineHandler's execCompatibleWithAttr — sorts
		// constraints for byte stability (matches projecta/render.go
		// precedent: exec_compatible_with is set-typed in Bazel)
		// and renders the empty-list case as the empty string.
		execAttr = execCompatibleWithAttr(plat.Constraints)
		publishPlatform = plat.Name
	}
	return fmt.Sprintf(`# Generated by cmd/write-a. Do not edit by hand.
# kind:meson round-2 fallback — Project B install genrule.
# Wraps meson setup + ninja + meson install --destdir under
# build-tracer, tars the install tree, and publishes the trace to
# the REAPI ActionCache (when CAS_GRPC_ADDR is set).
#
# Pairs with Project A's converter genrule running
# convert-element-meson with --unsupported-target-fallback=true.
# When the native lowering refuses (subproject, custom_target,
# generated_sources, cross-compile, unresolved-dependency, or an
# unsupported target type), A's BUILD.bazel.out (symlinked into
# this package post-build) emits a cc_import / sh_binary
# placeholder shape whose extract genrule references
# install_tree.tar — produced here.

package(default_visibility = ["//visibility:public"])

genrule(
    name = "%[1]s_trace_build%[3]s",
    srcs = glob(["**"], exclude = ["BUILD.bazel", "BUILD.bazel.out", "srckey.txt", "srckey-breakdown.txt", "srckey-patterns.txt"]) + [
        "srckey.txt",
    ],
    outs = [
        "%[4]sinstall_tree.tar",
        "%[4]strace.log",
    ],
    tools = [
        "//tools:build-tracer",
        "//tools:trace-publish",
    ],
    tags = ["trace_build"],
%[6]s    cmd = """
        export EXEC_ROOT="$$PWD"

        # Stage the source tree into a fresh dir so meson's
        # configure-time filesystem walks see exactly the user's
        # source tree without Bazel sandbox bookkeeping files.
        # cp -L dereferences symlinks so SRC_DIR ends up with
        # real bytes meson can read AND write to (some meson
        # generators drop files in-tree at configure time).
        # Inside Bazel's sandbox $(SRCS) typically resolve through
        # the runfiles tree as read-only symlinks; cp -P would
        # preserve them and downstream in-tree writes would fail
        # with EROFS. Dangling symlinks land here as cp errors —
        # a clear "missing source" signal rather than the silent
        # breakage cp -P would produce when the symlink target
        # leaves the staged tree.
        export SRC_DIR="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                */srckey.txt) continue ;;
            esac
            # Use ## (greedy left-strip) so the rel computation is
            # robust to whatever path shape Bazel hands us — both
            # plain "elements/<name>/<rel>" (typical) and
            # "bazel-out/.../elements/<name>/<rel>" (when a generated
            # source flows through this genrule) reduce to <rel>.
            rel="$${src##*elements/%[1]s/}"
            mkdir -p "$$SRC_DIR/$$(dirname "$$rel")"
            cp -L "$$src" "$$SRC_DIR/$$rel"
        done

        export BUILD_ROOT="$$(mktemp -d)"
        export INSTALL_ROOT="$$(mktemp -d)"

%[2]s

        # Tar the installed artefacts. The extract genrule that
        # consumes install_tree.tar (in A's BUILD.bazel.out, once
        # symlinked into this package) untars under the
        # "install_tree/" prefix; we tar from $$INSTALL_ROOT so
        # the entries inside are <dest>/<artefact> (matching
        # meson's --prefix=/ install destinations).
        # --mtime=@0 / --sort=name / --owner=0 --group=0
        # --numeric-owner mirror the autotools/cmake install-
        # genrule tar: keep tar bytes cross-machine stable so the
        # AC entry that publishes this artefact set hits on
        # identical inputs across different recording hosts.
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$$EXEC_ROOT/$(location %[4]sinstall_tree.tar)" \
            -C "$$INSTALL_ROOT" .

        # Surface the canonical trace.log as a declared output of
        # this genrule. trace-publish reads it from here.
        cp -L "$$MESON_TRACE" "$$EXEC_ROOT/$(location %[4]strace.log)"

        # Publish to the AC iff a remote is configured. Same
        # short-circuit pattern kind:autotools / kind:cmake
        # round-2 use: empty CAS_GRPC_ADDR ⇒ skip (e.g. local dev
        # without buildbarn); the build succeeds, no AC entry is
        # written, and the next render of project A sees a miss
        # → re-runs this install genrule. trace-publish itself
        # ALSO short-circuits on empty --cas; checking here keeps
        # the genrule output readable when debugging.
        if [ -n "$${CAS_GRPC_ADDR:-}" ]; then
            cd "$$EXEC_ROOT"
            # --platform= partitions the synthetic AC keyspace per
            # target platform. Multi-platform fan-out bakes the
            # platform tag literally into this argv so each of N
            # per-platform genrules publishes under its own tag.
            # Single-platform legacy mode reads
            # CMAKE_TO_BAZEL_PLATFORM from --action_env (env-var
            # fallback can't differ across N parallel actions in
            # one Bazel build, hence the explicit literal bake under
            # multi-platform).
            $(location //tools:trace-publish) \\
                --cas="$${CAS_GRPC_ADDR}" \\
                --srckey="$$(cat $(location srckey.txt) | tr -d '[:space:]')" \\
                --platform="%[5]s" \\
                --trace="$(location %[4]strace.log)" >/dev/null
        fi
    """,
)
`, elem.Name, wrapMesonPipelineCmds(`        meson setup "$$BUILD_ROOT" "$$SRC_DIR" --prefix=/ --libdir=lib
        ninja -C "$$BUILD_ROOT"
        DESTDIR="$$INSTALL_ROOT" meson install -C "$$BUILD_ROOT"`),
		nameSuffix, outputPrefix, publishPlatform, execAttr)
}

// renderMesonRound2B is project B's per-element render for the
// kind:meson round-2 fallback shape. Single-platform legacy
// emits one install genrule via mesonRound2InstallBuild (byte-
// stable with the pre-fan-out shape). Multi-platform mode
// (--platforms-json set on write-a) emits N per-platform install
// genrules + a top-level select()-filegroup at
// `:install_tree.tar` so downstream
// //elements/<dep>:install_tree.tar references resolve to the
// matching per-platform tarball.
func renderMesonRound2B(elem *element) string {
	if len(traceConfig.platforms) == 0 {
		return mesonRound2InstallBuild(elem, tracePlatform{})
	}
	bodies := make([]string, 0, len(traceConfig.platforms))
	for _, plat := range traceConfig.platforms {
		bodies = append(bodies, mesonRound2InstallBuild(elem, plat))
	}
	return composeMultiPlatformInstallBuild(bodies, traceConfig.platforms)
}

// wrapMesonPipelineCmds wraps a meson setup + ninja + meson install
// sequence under build-tracer so the trace artifact captures every
// execve under the build sandbox. Mirrors wrapCmakePipelineCmds /
// wrapAutotoolsPipelineCmds — same --normalize-prefix substitutions,
// same single-line-per-step shell shape, same trace tmpfile pattern.
//
// The cmds argument is the resolved setup / build / install shell
// snippet (already variable-substituted by the caller). The
// canonical shape uses the same prelude-bound variable names that
// the surrounding install genrule prepares — $$BUILD_ROOT for the
// meson build dir, $$INSTALL_ROOT for the install prefix (passed
// to `meson install` via DESTDIR), $$SRC_DIR for the staged
// source tree:
//
//	meson setup "$$BUILD_ROOT" "$$SRC_DIR" --prefix=/ --libdir=lib
//	ninja -C "$$BUILD_ROOT"
//	DESTDIR="$$INSTALL_ROOT" meson install -C "$$BUILD_ROOT"
//
// The variable names matter because --normalize-prefix below
// rewrites those exact prefixes into stable placeholders
// (/BUILD_ROOT, /INSTALL_ROOT — no trailing slash; the
// substitution covers any byte sequence that begins with the
// recording-machine value) for byte-stable canonical traces. A
// caller binding different variable names would land mktemp paths
// the substitution can't match, breaking trace stability across
// machines.
//
// The tracer-out path lives under $$MESON_TRACE so the post-
// pipeline trace-publish step can find it; the tmpfile is
// machine-local and the canonical bytes are written to a genrule
// output. MESON_TRACE specifically (not CMAKE_TRACE /
// AUTOTOOLS_TRACE) so an element with multiple kinds in its
// pipeline doesn't have the tracers stomp on each other's tmpfile.
//
// Path note: $(location //tools:build-tracer) resolves to an
// exec-root-relative path; the caller's prelude has cd'd into
// $$BUILD_ROOT before this runs, so we anchor explicitly to
// $$EXEC_ROOT to find the staged binary.
func wrapMesonPipelineCmds(cmds string) string {
	return fmt.Sprintf(`        # Build-tracer wraps the entire meson setup / ninja /
        # install pipeline. The trace artifact captures every
        # execve under the build sandbox; pass-3's trace-publish
        # canonicalizes it and lands an AC entry keyed by
        # SyntheticActionDigest(srckey).
        #
        # --normalize-prefix substitutions neutralize action-time
        # mktemp paths (INSTALL_ROOT, BUILD_ROOT, DEP_PREFIX). Their
        # bytes vary across bazel invocations even when the build
        # is otherwise identical, so without normalization the
        # canonical trace would still drift run-to-run. The
        # placeholder names (/INSTALL_ROOT, etc.) are stable across
        # machines and human-readable. DEP_PREFIX is only set when
        # the element has cross-meson deps — the
        # ${DEP_PREFIX:-/__unset_dep_prefix__} sentinel default
        # keeps the flag harmless when unset by substituting an
        # implausible path that no recorded trace line will match,
        # so the rewrite is a no-op rather than rewriting an empty
        # prefix (which would match every line).
        export MESON_TRACE="$$(mktemp)"
        "$$EXEC_ROOT/$(location //tools:build-tracer)" \
            --normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT" \
            --normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT" \
            --normalize-prefix="$${DEP_PREFIX:-/__unset_dep_prefix__}=/DEP_PREFIX" \
            --out="$$MESON_TRACE" -- sh -c '
%s
'
`, cmds)
}
