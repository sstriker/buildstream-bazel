package main

import "fmt"

// handler_cmake_round2.go: kind-specific scaffolding for the
// kind:cmake round-2 fallback.
//
// Phase A (PR #95) emits unsupported-execute-process Tier-1
// failures for execute_process calls the classifier can't lift
// natively. Phase B replaces the exclusion with a round-2
// coarse genrule fallback (build-tracer-wrapped cmake configure
// + ninja + install) so unliftable elements still produce a
// downstream artifact.
//
// This file holds the kind-specific scaffolding pieces (the
// srckey pattern set + the build-tracer wrapping helper) as
// pure functions with no call sites yet. Subsequent steps wire
// them into write-a's render path. See
// docs/design/cmake-execute-process-round2-fallback.md for the
// architectural shape and staged plan.
//
// What kind:cmake reuses from the existing round-2 plumbing:
//
//   - renderTraceDrivenRound2A in handler_pipeline_round2.go —
//     kind-agnostic; emits A's converter genrule wired against
//     @trace_<elem>//:trace.
//   - cmd/build-tracer / cmd/trace-publish / cmd/trace-lookup
//     and tracenorm.SyntheticActionDigest — the publish/lookup
//     wire contract is kind-agnostic.
//
// What kind:cmake does NOT reuse:
//
//   - pipelineTraceExtensionRound2 + pipelineTracePublishStep
//     hard-code autotools assumptions: they call
//     wrapAutotoolsPipelineCmds, write to $$AUTOTOOLS_TRACE,
//     and capture `make -np` for the make-db. kind:cmake's
//     install genrule needs its own equivalent that uses
//     wrapCmakePipelineCmds (defined below) and $$CMAKE_TRACE,
//     and synthesises an empty make-db (cmake doesn't have
//     one). Step 3 lands cmakeRound2InstallBuild; in this PR
//     wrapCmakePipelineCmds + the srckey patterns are the
//     standalone scaffolding it consumes.

// cmakeSrckeyPatterns returns the file-glob set that gates
// srckey content-inclusion for kind:cmake round-2.
//
// Content-included (rule.Include == true): paths whose **byte
// content** changes the trace cmake configure + ninja + install
// would record. Path-only (no rule, default-deny): everything
// else — adding/removing a path invalidates srckey via the
// universe, but editing an existing file's content does not.
//
// Per-pattern rationale:
//
//   - CMakeLists.txt + nested CMakeLists.txt — the canonical
//     declarative driver. Every directive (add_library,
//     target_compile_options, add_subdirectory, etc.) reshapes
//     the build graph cmake records.
//   - *.cmake (top-level + nested) — cmake module includes
//     (find_package logic, helper functions, generator-expr
//     macros). Same shape as CMakeLists.txt: edits change
//     command lines.
//   - *.cmake.in — templates substituted by configure_file at
//     cmake configure time. Output is consumed in cmake's own
//     reconfigure machinery (ProjectConfig.cmake.in etc.); the
//     resulting .cmake file feeds the build graph the same
//     way a hand-written .cmake would.
//   - **/*.h family + **/*.hpp / **/*.hxx / **/*.hh — header
//     edits shift include resolution and #include-driven
//     conditionals, which surface in compile commands the
//     trace records (added/removed -I dirs, distinct
//     dependency edges in ninja's deps DB).
//   - CMakePresets.json / CMakeUserPresets.json — alternative
//     configure entry points; their content reshapes the
//     configure command without going through CMakeLists.txt.
//
// **`.h.in` default is path-only.** cmake itself reads `.h.in`
// at configure time (configure_file substitutes them), so the
// `RERUN_CMAKE` oracle always flags them. The configure_file
// lift in PR #94 makes `.h.in` Bazel-srcs covered, removing
// the need for srckey content-inclusion. The kind default
// reflects that steady state: `.h.in` is path-only by default;
// elements without the lift staged surface undercoverage drift
// in `audit-narrowing` (the cmake oracle reports `.h.in`, the
// patterns don't cover it). Operators react by either staging
// the configure_file lift OR adding a per-element
// `include **/*.h.in` override to read-paths.txt. See
// `docs/design/narrowing-audit.md` "*.h.in and the
// configure_file lift" for the trade-off.
//
// Path-only (no rule) for: *.c / *.cc / *.cpp / *.cxx /
// *.C / *.s / *.S — compile sources. The trace records the
// compile command (`gcc -c foo.c -o foo.o`) regardless of
// foo.c's bytes; edits invalidate via Bazel's action cache,
// not via srckey.
//
// Mirrors makeSrckeyPatterns / autotoolsSrckeyPatterns shape
// — same Include semantics, same default-deny behaviour for
// unmatched paths.
func cmakeSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "CMakeLists.txt"},
			{Include: true, Pattern: "**/CMakeLists.txt"},
			{Include: true, Pattern: "*.cmake"},
			{Include: true, Pattern: "**/*.cmake"},
			{Include: true, Pattern: "*.cmake.in"},
			{Include: true, Pattern: "**/*.cmake.in"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
			{Include: true, Pattern: "**/*.hh"},
			// CMakePresets — alternative configure entry
			// points; their content reshapes the configure
			// command without going through CMakeLists.txt.
			//
			// TODO: investigate cmake kits (CMakeTools'
			// repo-local `cmake-kits.json`). Kits are a
			// workflow layer rather than a CMakeLists-
			// replacing input today, but they CAN influence
			// configure when the generator picks them up via
			// CMAKE_TOOLCHAIN_FILE / preset-style overrides.
			// User-global kits at
			// ~/.local/share/CMakeTools/cmake-tools-kits.json
			// are intentionally out of scope — srckey patterns
			// only match source-tree-relative paths, so
			// host-local config can't be captured by this
			// mechanism (treat it the same as host compiler
			// version: action-cache invalidation rather than
			// srckey). Decide per-fixture whether the repo-
			// local kit JSON needs a rule here once a real
			// project surfaces them.
			{Include: true, Pattern: "CMakePresets.json"},
			{Include: true, Pattern: "CMakeUserPresets.json"},
		},
	}
}

// cmakeRound2InstallBuild renders Project B's install genrule
// for the kind:cmake round-2 fallback shape. The genrule wraps
// cmake configure + ninja + install under build-tracer, tars
// the install tree, and inline-publishes the trace via
// trace-publish.
//
// Outputs:
//   - install_tree.tar — the installed artefacts the
//     placeholder's extract genrule untars (referenced via
//     same-package label "install_tree.tar" once A's
//     BUILD.bazel.out gets symlinked in).
//   - trace.log — canonical build-tracer output;
//     trace-publish reads it to land the AC entry.
//
// trace-publish runs only when the action environment supplies
// CAS_GRPC_ADDR (devs running locally without a remote cache
// leave it unset; the build still succeeds, no AC entry
// published).
//
// The source tree was staged into elemPkg by RenderB's
// stageAllSources call before this template runs; glob picks
// up every staged file under elemPkg as the genrule's srcs.
// srckey.txt (also rendered before this template) carries the
// per-element AC key trace-publish derives from.
//
// Multi-platform knobs: when plat.Name is non-empty (write-a's
// --platforms-json fan-out), the genrule is suffixed with the
// platform name, all outs are prefixed with `<plat>/`,
// exec_compatible_with carries the constraint set, and the
// trace-publish call bakes --platform=<plat> literally
// (vs the env-var fallback that single-platform mode uses).
// Empty plat.Name renders the single-platform legacy shape —
// functionally unchanged from the pre-fan-out behavior (same
// genrule name, same outs, same publish path), with two small
// cmd-comment refreshes that don't affect behavior or any
// existing render-gate / golden contract (the existing
// meta-cmake-round2-fallback gate keeps passing).
func cmakeRound2InstallBuild(elem *element, plat tracePlatform) string {
	nameSuffix := ""
	outputPrefix := ""
	execAttr := ""
	publishPlatform := `$${CMAKE_TO_BAZEL_PLATFORM:-}`
	if plat.Name != "" {
		nameSuffix = "_" + plat.Name
		outputPrefix = plat.Name + "/"
		// Reuse pipelineHandler's execCompatibleWithAttr —
		// sorts constraints for byte stability (matches
		// projecta/render.go precedent: exec_compatible_with
		// is set-typed in Bazel) and renders the empty-list
		// case as the empty string. Single source of truth
		// for the formatting prevents divergence between
		// kind:cmake and pipelineHandler handler outputs.
		execAttr = execCompatibleWithAttr(plat.Constraints)
		publishPlatform = plat.Name
	}
	return fmt.Sprintf(`# Generated by cmd/write-a. Do not edit by hand.
# kind:cmake round-2 fallback — Project B install genrule.
# Wraps cmake configure + ninja + install under build-tracer,
# tars the install tree, and publishes the trace to the REAPI
# ActionCache (when CAS_GRPC_ADDR is set).
#
# Pairs with Project A's converter genrule running
# convert-element-cmake with --unsupported-execute-process-fallback=true.
# When the classifier refuses, A's BUILD.bazel.out (symlinked
# into this package post-build) emits a cc_import / sh_binary
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

        # Stage the source tree into a fresh dir so cmake's
        # configure-time filesystem walks see exactly the user's
        # source tree without Bazel sandbox bookkeeping files.
        # cp -L dereferences symlinks so SRC_DIR ends up with
        # real bytes the cmake configure can read AND write to
        # (some cmake projects do file(WRITE ...) or run
        # generators that drop files in-tree at configure
        # time). Inside Bazel's sandbox $(SRCS) typically
        # resolve through the runfiles tree as read-only
        # symlinks; cp -P would preserve them and downstream
        # in-tree writes would fail with EROFS errors.
        # write-a's offline copyTree preserves user-source
        # symlinks at workspace-staging time, but once Bazel
        # has the byte set the bytes are what matter; cp -L
        # produces a writable, self-contained SRC_DIR. Dangling
        # symlinks land here as cp errors — a clear "missing
        # source" signal rather than the silent breakage cp -P
        # would produce when the symlink target leaves the
        # staged tree.
        export SRC_DIR="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                */srckey.txt) continue ;;
            esac
            # Use ## (greedy left-strip) so the rel computation is
            # robust to whatever path shape Bazel hands us:
            #   - Plain "elements/<name>/<rel>" (the typical case
            #     for sources colocated with the BUILD file).
            #   - "bazel-out/k8/bin/elements/<name>/<rel>" (when a
            #     generated source flows through this genrule).
            # The '#' (non-greedy) variant only strips the exact
            # leading prefix and would silently leave bazel-out/
            # paths un-stripped, miscopying them under a deep
            # SRC_DIR/bazel-out/.../<rel> tree.
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
        # the entries inside are <dest>/<artefact>
        # (matching cmake's install destinations + NameOnDisk).
        # --mtime=@0 / --sort=name / --owner=0 --group=0
        # --numeric-owner mirror the autotools install-genrule
        # tar in handler_pipeline.go: keep tar bytes
        # cross-machine stable so the AC entry that publishes
        # this artefact set hits on identical inputs across
        # different recording hosts.
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$$EXEC_ROOT/$(location %[4]sinstall_tree.tar)" \
            -C "$$INSTALL_ROOT" .

        # Surface the canonical trace.log as a declared output of
        # this genrule. trace-publish reads it from here.
        cp -L "$$CMAKE_TRACE" "$$EXEC_ROOT/$(location %[4]strace.log)"

        # Synthesize a cmake-config bundle from the install tree
        # (cross-element configure-step bootstrap rendezvous; see
        # docs/design/cross-element-config-rendezvous.md). Bundle
        # contents are pkg-config (.pc) files copied verbatim and
        # cmake-config files (lib/cmake/<Pkg>/) copied verbatim
        # when the element installs them. Elements that install
        # neither produce an empty bundle — downstream consumers
        # skip staging when the bundle's tar is empty. The bundle
        # is published to a separate AC keyspace partition
        # (SyntheticConfigDigest); a consumer's trace_load action
        # materializes it via --out-config-bundle.
        export CONFIG_BUNDLE_DIR="$$(mktemp -d)"
        if [ -d "$$INSTALL_ROOT/lib/pkgconfig" ]; then
            mkdir -p "$$CONFIG_BUNDLE_DIR/lib/pkgconfig"
            cp -r "$$INSTALL_ROOT/lib/pkgconfig"/. "$$CONFIG_BUNDLE_DIR/lib/pkgconfig/" 2>/dev/null || true
        fi
        if [ -d "$$INSTALL_ROOT/lib/cmake" ]; then
            mkdir -p "$$CONFIG_BUNDLE_DIR/lib/cmake"
            cp -r "$$INSTALL_ROOT/lib/cmake"/. "$$CONFIG_BUNDLE_DIR/lib/cmake/" 2>/dev/null || true
        fi
        export CONFIG_BUNDLE_TAR="$$(mktemp)"
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$$CONFIG_BUNDLE_TAR" -C "$$CONFIG_BUNDLE_DIR" .

        # Publish to the AC iff a remote is configured. Same
        # short-circuit pattern kind:autotools round-2 uses:
        # empty CAS_GRPC_ADDR ⇒ skip (e.g. local dev without
        # buildbarn); the build succeeds, no AC entry is
        # written, and the next render of project A sees a miss
        # → re-runs this install genrule. trace-publish itself
        # ALSO short-circuits on empty --cas; checking here
        # keeps the genrule output readable when debugging.
        if [ -n "$${CAS_GRPC_ADDR:-}" ]; then
            cd "$$EXEC_ROOT"
            # --platform= partitions the synthetic AC keyspace
            # per target platform. Multi-platform fan-out bakes
            # the platform tag literally into this argv so each
            # of N per-platform genrules publishes under its own
            # tag. Single-platform legacy mode reads
            # CMAKE_TO_BAZEL_PLATFORM from --action_env.
            # --config-bundle publishes the synthesized bundle
            # under SyntheticConfigDigest (distinct keyspace).
            $(location //tools:trace-publish) \\
                --cas="$${CAS_GRPC_ADDR}" \\
                --srckey="$$(cat $(location srckey.txt) | tr -d '[:space:]')" \\
                --platform="%[5]s" \\
                --trace="$(location %[4]strace.log)" \\
                --config-bundle="$$CONFIG_BUNDLE_TAR" >/dev/null
        fi
    """,
)
`, elem.Name, wrapCmakePipelineCmds(`        cmake -B "$$BUILD_ROOT" -G Ninja -S "$$SRC_DIR" -DCMAKE_INSTALL_PREFIX="$$INSTALL_ROOT"
        cmake --build "$$BUILD_ROOT" --parallel 1
        cmake --install "$$BUILD_ROOT" --prefix "$$INSTALL_ROOT"`),
		nameSuffix, outputPrefix, publishPlatform, execAttr)
}

// renderCmakeRound2B is project B's per-element render for the
// kind:cmake round-2 fallback shape. Single-platform legacy
// emits one install genrule via cmakeRound2InstallBuild (byte-
// stable with the pre-fan-out shape). Multi-platform mode
// (--platforms-json set on write-a) emits N per-platform
// install genrules + a top-level select()-filegroup at
// `:install_tree.tar` so downstream
// //elements/<dep>:install_tree.tar references resolve to the
// matching per-platform tarball.
func renderCmakeRound2B(elem *element) string {
	if len(traceConfig.platforms) == 0 {
		return cmakeRound2InstallBuild(elem, tracePlatform{})
	}
	bodies := make([]string, 0, len(traceConfig.platforms))
	for _, plat := range traceConfig.platforms {
		bodies = append(bodies, cmakeRound2InstallBuild(elem, plat))
	}
	return composeMultiPlatformInstallBuild(bodies, traceConfig.platforms)
}

// wrapCmakePipelineCmds wraps a cmake configure + build +
// install sequence under build-tracer so the trace artifact
// captures every execve under the build sandbox. Mirrors
// wrapAutotoolsPipelineCmds (handler_autotools_native.go) — same
// --normalize-prefix substitutions, same single-line-per-step
// shell shape, same trace tmpfile pattern.
//
// The cmds argument is the resolved configure / build / install
// shell snippet (already variable-substituted by the caller).
// For kind:cmake round-2 the canonical shape uses the same
// prelude-bound variable names that the surrounding install
// genrule prepares — $$BUILD_ROOT for the cmake build dir,
// $$INSTALL_ROOT for the install prefix, $$SRC_DIR for the
// staged source tree:
//
//	cmake -B "$$BUILD_ROOT" -G Ninja -S "$$SRC_DIR" \
//	    -DCMAKE_INSTALL_PREFIX="$$INSTALL_ROOT" [...]
//	cmake --build "$$BUILD_ROOT" --parallel 1
//	cmake --install "$$BUILD_ROOT" --prefix "$$INSTALL_ROOT"
//
// The variable names matter because --normalize-prefix below
// rewrites those exact prefixes into stable placeholders
// (`/BUILD_ROOT`, `/INSTALL_ROOT` — no trailing slash; the
// substitution covers any byte sequence that begins with the
// recording-machine value, slashed or otherwise) for byte-
// stable canonical traces. A configure step that bound a
// different variable (BUILD_DIR, DESTDIR, ...) would land
// mktemp paths the substitution can't match, breaking trace
// stability across machines.
//
// --parallel 1 mirrors `make -j1` in autotools round-2: serial
// execution keeps the trace's process-spawn order stable so
// canonicalization byte-equality holds across recordings on
// different machines.
//
// The tracer-out path lives under $$CMAKE_TRACE so the post-
// pipeline trace-publish step can find it; the tmpfile is
// machine-local and the canonical bytes are written to a
// genrule output. CMAKE_TRACE specifically (not the autotools
// AUTOTOOLS_TRACE) so an element with both kinds in its
// orchestrator pipeline doesn't have the two tracers stomp on
// each other's tmpfile.
//
// Path note: $(location //tools:build-tracer) resolves to an
// exec-root-relative path; the prelude already cd's into
// $$BUILD_ROOT before this runs, so we anchor explicitly to
// $$EXEC_ROOT to find the staged binary.
//
// Step 1 lands this as a pure function with no call sites.
// Step 3 wires it into the per-element round-2 install genrule
// emission.
func wrapCmakePipelineCmds(cmds string) string {
	return fmt.Sprintf(`        # Build-tracer wraps the entire cmake configure / build /
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
        # placeholder names (/INSTALL_ROOT, etc.) are stable
        # across machines and human-readable. DEP_PREFIX is only
        # set when the element has cmake deps — the
        # ${DEP_PREFIX:-/__unset_dep_prefix__} sentinel default
        # keeps the flag harmless when unset by substituting an
        # implausible path that no recorded trace line will
        # match, so the rewrite is a no-op rather than rewriting
        # an empty prefix (which would match every line).
        export CMAKE_TRACE="$$(mktemp)"
        "$$EXEC_ROOT/$(location //tools:build-tracer)" \
            --normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT" \
            --normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT" \
            --normalize-prefix="$${DEP_PREFIX:-/__unset_dep_prefix__}=/DEP_PREFIX" \
            --out="$$CMAKE_TRACE" -- sh -c '
%s
'
`, cmds)
}
