#!/bin/sh
# run-survey.sh — drive the diagnostic-survey corpus through the cmake
# converter and emit a per-project rejection / bazel-idiom / coverage /
# conversion-todos report.
#
# This is the self-contained, in-repo version of the cross-project
# survey described in docs/codemodel-consumption-audit.md. It does NOT
# depend on any external harness: convert-element-cmake is self-driving
# in --source-root mode (it runs cmake itself with the File API + trace
# hooks), and --diagnostics collects every Tier-1 refusal and continues
# past it rather than aborting on the first.
#
# The corpus (which projects, where they're fetched from, and how to
# survey faithfully — e.g. survey llvm's `llvm/` subdir, exclude the
# benign missing-include-dir notices before comparing counts) is
# documented in docs/survey-corpus.md. Fetch the corpus with
# `make fetch-survey` (adds fetch-llvm / fetch-vtk for the big two).
#
# Each project is converted into a scratch out dir; the emitted
# BUILD.bazel is best-effort and NOT guaranteed to build — the point is
# enumerating the refusal + idiom surface in one pass, not producing
# usable output.
#
# Fourth lens (opt-in): SURVEY_BAZEL_BUILD turns on a `bazel build //...`
# pass/fail column — the end-to-end "does the Bazel-native output actually
# build, no cmake?" question the diagnostic lenses don't answer. It does its
# OWN clean (non-diagnostic) convert in the SAME faithful shape the survey
# diagnoses (multi-config + split) PLUS --out-config-settings, which emits the
# //config package the multi-config select() arms resolve against — the one
# piece write-a renders into project B that the bare converter otherwise
# leaves to the orchestrator. So the lens builds exactly project B's wiring,
# self-contained: it overlays the converted BUILD tree on a copy of the
# source, synthesizes a minimal MODULE.bazel, and builds the //... wildcard.
# See docs/survey-corpus.md "The build lens".
#
# Usage:
#   scripts/run-survey.sh [--out-dir <dir>] [name=<src-root> ...]
#
# With no project args it surveys the four corpus projects at their
# Makefile-pinned clone dirs (run `make fetch-survey` first):
#   abseil=$ABSEIL_DIR protobuf=$PROTOBUF_DIR
#   googletest=$GTEST_DIR eigen=$EIGEN_DIR
#
# Env / defaults mirror the Makefile vars; override to point elsewhere.
set -eu

out_dir="${SURVEY_OUT_DIR:-/tmp/survey-out}"
projects=""

# SURVEY_BUILD_TYPES controls multi-config surveying via --build-types
# (Ninja Multi-Config). Forms:
#   - unset / "auto" -> (DEFAULT) detect EACH project's own declared
#                       CMAKE_CONFIGURATION_TYPES and survey with exactly
#                       those, so no config's intent is dropped (a fixed
#                       subset like "Release,Debug" would silently drop
#                       RelWithDebInfo / MinSizeRel / custom configs).
#   - "single"/"none"-> single-config Release path (opt out of multi-config).
#   - explicit list  -> e.g. "Release,Debug" forces that subset (escape
#                       hatch; not faithful if the project declares more).
# Default is "auto" because a faithful survey shouldn't silently drop a
# project's non-default configs; opt out with "single" when you only need
# the Release surface.
build_types="${SURVEY_BUILD_TYPES:-auto}"
case "$build_types" in single|none|off) build_types="" ;; esac

# SURVEY_SPLIT_PACKAGES controls --split-packages (one BUILD per directory,
# the gazelle model) — the shape the converter ultimately targets, so
# split-mode findings are the most representative. Default ON; set to
# "0"/"no"/"off" to emit the single monolithic BUILD.bazel instead.
split_packages="${SURVEY_SPLIT_PACKAGES:-1}"
case "$split_packages" in 0|no|off|false) split_packages="" ;; esac

# SURVEY_BAZEL_BUILD controls the fourth lens — a `bazel build //...` pass/fail
# on the converted output (no cmake at build time). Forms:
#   - unset / "" / "off"   -> (DEFAULT) no build lens; the column shows "-".
#   - "auto" / "1" / "on"  -> build only the curated near-clean starter set
#                             ($build_lens_default) — the projects that already
#                             survey clean, so a FAIL is a real regression.
#   - "all"                -> attempt every surveyed project (most will FAIL on
#                             unresolved standalone find_package deps — honest).
#   - <name list>          -> exactly those (comma/space separated).
# The build half needs bazel/bazelisk on PATH; absent, the column shows skip.
#
# NOTE: the build lens only acts on projects actually being surveyed. The
# curated "auto" set (fmt/libxml2/brotli) is NOT in the no-args default corpus
# (abseil/protobuf/googletest/eigen), so `SURVEY_BAZEL_BUILD=auto
# scripts/run-survey.sh` shows "-" for all of them — pass those projects
# explicitly (e.g. `SURVEY_BAZEL_BUILD=auto scripts/run-survey.sh fmt=$FMT_DIR
# libxml2=$LIBXML2_DIR brotli=$BROTLI_DIR`) to exercise it.
# SURVEY_COMPILE_DB=1 turns on the fifth lens — compile-commands FIDELITY. For
# each surveyed project it diffs cmake's CMAKE_EXPORT_COMPILE_COMMANDS
# db against Bazel's `aquery 'mnemonic("CppCompile",//...)'` per translation unit
# (defines, -std, source includes), writing <out>/<name>/fidelity.json. Runs
# after the convert but BEFORE the build's compile (aquery needs only analysis),
# so it catches per-TU flag drift cheaply. It does NOT require SURVEY_BAZEL_BUILD:
# the lens triggers the converted-workspace setup on its own and skips the build
# (build-status column shows "skip" when only a workspace-lens selected the
# member). Report-only; see cmd/compile-commands-diff.
# SURVEY_INTENT=1 turns on the sixth lens — intent-capture, the agent-as-oracle
# "what did we miss?" pass. For each surveyed project (build lens not required —
# same workspace-only trigger as the compile-db lens) it hands the
# converted bundle (workspace BUILD/MODULE + the cmake sources) to a PLUGGABLE
# judge ($INTENT_LENS_JUDGE, e.g. 'claude -p'), then triages the findings against
# the element's own conversion-todos / rejections, writing <out>/<name>/
# intent-capture.json. Opt-in + cost-gated (skips silently with no judge);
# non-deterministic, so it's a triage queue, not a gate. See
# scripts/intent-capture-lens.sh + converter/cmd/intent-lens.
# SURVEY_SHARED=1 builds the FAITHFUL link model for build-lens members: the
# project's natural config (no forced BUILD_SHARED_LIBS=OFF) with real
# cc_shared_library .so's (--emit-shared-libraries) and consumers' dynamic_deps
# wired — i.e. what cmake would actually build. The default (off) keeps the
# forced-static alignment the green corpus is validated under; SURVEY_SHARED is
# the path to re-greening each member against its natural config.
# SURVEY_DERIVE_LINK_MODE=1 derives the bazel link mode from the (forced-static)
# codemodel for the compile-db lens: when the project declares no SHARED/MODULE
# library targets, the aquery + build link STATIC (--dynamic_mode=off) — the
# model cmake actually uses — instead of bazel's default dynamic cc_library
# .so's, which is what makes the project-archive link-ORDER comparison
# meaningful and drops the per-member --dynamic_mode=off knobs for all-static
# members. Opt-in: forcing static can surface ODR/duplicate-symbol issues a
# dynamic build tolerated, so it's gated pending a corpus re-green (then the
# default flips, folded with SURVEY_SHARED's flip). Skipped under SURVEY_SHARED
# and when a member already pins --dynamic_mode. Off → byte-identical surveys.
bazel_build="${SURVEY_BAZEL_BUILD:-}"
build_lens_default="fmt libxml2 brotli"

while [ $# -gt 0 ]; do
    case "$1" in
        --out-dir) out_dir="$2"; shift 2 ;;
        -h|--help)
            awk 'NR>=2 && /^#/{sub(/^# ?/, ""); print; next} NR>=2{exit}' "$0"
            exit 0 ;;
        *=*) projects="$projects $1"; shift ;;
        *) echo "run-survey.sh: unrecognized arg '$1'" >&2; exit 2 ;;
    esac
done

# Default corpus: the four pinned in the Makefile.
if [ -z "$projects" ]; then
    projects="
        abseil=${ABSEIL_DIR:-/tmp/abseil-cpp}
        protobuf=${PROTOBUF_DIR:-/tmp/protobuf}
        googletest=${GTEST_DIR:-/tmp/googletest}
        eigen=${EIGEN_DIR:-/tmp/eigen}
    "
fi

# Locate the converter. ALWAYS (re)build it from the CURRENT source before
# surveying — never trust a build/bin binary left lying about from an earlier
# checkout. A stale prebuilt converter silently runs without fixes the source
# already carries (e.g. after a session-recovery checkout moves HEAD), which
# produces wrong survey results that look like real regressions. `go build` is
# cheap (incremental + cached), so rebuilding every run is well worth removing
# that footgun. Fall back to an existing binary, then `go run`, only when
# `go build` can't run (no Go toolchain on PATH).
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
converter="$repo_root/build/bin/convert-element-cmake"
if command -v go >/dev/null 2>&1 && \
   ( cd "$repo_root" && go build -o "$converter" ./converter/cmd/convert-element-cmake ); then
    run_converter() { "$converter" "$@"; }
elif [ -x "$converter" ]; then
    echo "warning: 'go build' unavailable or failed; using existing (possibly stale) $converter" >&2
    run_converter() { "$converter" "$@"; }
else
    echo "note: $converter not built and 'go build' failed; using 'go run' (slower)." >&2
    run_converter() { ( cd "$repo_root" && go run ./converter/cmd/convert-element-cmake "$@" ); }
fi

# detect_configs <src> — echo the project's declared configuration types
# as a comma-separated list, for SURVEY_BUILD_TYPES=auto. Runs a throwaway
# Ninja Multi-Config configure WITHOUT forcing CMAKE_CONFIGURATION_TYPES,
# so cmake records whatever the project declares (or its default
# Debug;Release;MinSizeRel;RelWithDebInfo when the project doesn't set it),
# and reads it back from CMakeCache.txt. Echoes nothing on failure.
detect_configs() {
    _dc_bld="$(mktemp -d)"
    if cmake -S "$1" -B "$_dc_bld" -G "Ninja Multi-Config" >/dev/null 2>&1; then
        _dc_line="$(grep '^CMAKE_CONFIGURATION_TYPES' "$_dc_bld/CMakeCache.txt" 2>/dev/null | head -1)"
        printf '%s' "${_dc_line#*=}" | tr ';' ','
    fi
    rm -rf "$_dc_bld"
}

# Build-lens (4th lens) helpers. -------------------------------------------
bzl_bin=""
if command -v bazel >/dev/null 2>&1; then bzl_bin=bazel
elif command -v bazelisk >/dev/null 2>&1; then bzl_bin=bazelisk; fi

# build_lens_for <name> — true if SURVEY_BAZEL_BUILD selects this project.
build_lens_for() {
    case "$bazel_build" in
        ""|0|no|off|false) return 1 ;;
        all) return 0 ;;
        auto|1|on|yes|true) _bl_set="$build_lens_default" ;;
        *) _bl_set="$(printf '%s' "$bazel_build" | tr ',' ' ')" ;;
    esac
    for _bl_p in $_bl_set; do
        [ "$_bl_p" = "$1" ] && return 0
    done
    return 1
}

# ws_lens_wanted — true if any WORKSPACE-only lens is requested: the
# compile-db fidelity lens (SURVEY_COMPILE_DB), the intent-capture lens
# (SURVEY_INTENT + a judge), or the narrowing-compat lens
# (SURVEY_NARROWING_COMPAT). These all ride INSIDE try_bazel_build (they need
# the converted workspace / a clean convert) but DON'T need the bazel build —
# the compile-db lens runs `aquery` (analysis only), and the other two re-use
# the convert. They must therefore trigger try_bazel_build even when
# SURVEY_BAZEL_BUILD didn't select this member; the build step itself stays
# gated on build_lens_for (see try_bazel_build). Without this, SURVEY_COMPILE_DB=1
# on its own was a silent no-op — try_bazel_build was never entered.
ws_lens_wanted() {
    [ "${SURVEY_COMPILE_DB:-0}" != "0" ] && return 0
    [ "${SURVEY_NARROWING_COMPAT:-0}" != "0" ] && return 0
    [ "${SURVEY_INTENT:-0}" != "0" ] && [ -n "${INTENT_LENS_JUDGE:-}" ] && return 0
    return 1
}

# try_bazel_build <name> <src> <proj_out> <bt_args> <sp_args> — clean-convert
# the project in the faithful shape + --out-config-settings, overlay onto a
# copy of the source tree with a minimal MODULE.bazel, and `bazel build //...`.
# Echoes one summary token (ok / FAIL / skip(<why>)); detail in the proj_out
# logs. The convert here is deliberately WITHOUT --diagnostics: a clean build
# presupposes a clean convert. The caller already short-circuits a project that
# surveys with rejections to skip(rej) (so this convert isn't even attempted
# there); if a no-rejection project's clean convert still fails here, it's
# skip(convert) rather than building a partial tree.
try_bazel_build() {
    _bb_name="$1"; _bb_src="$2"; _bb_po="$3"; _bb_bt="$4"; _bb_sp="$5"
    [ -n "$bzl_bin" ] || { echo "skip(no-bazel)"; return; }
    _bb_ws="$_bb_po/build-ws"
    # Convert in the FAITHFUL project-B shape: the element lands under
    # elements/<name>/ (a real sub-package), NOT at the workspace root. This
    # mirrors how cmd/stage-b places a converted element in project B, and it
    # is load-bearing for correctness — a target at the workspace root can't
    # carry `includes = ["."]` (Bazel: "'.' resolves to the workspace root"),
    # so a project whose cmake puts a generated header on the build-dir-root
    # include path (libxml2's `$<BUILD_INTERFACE:${CMAKE_CURRENT_BINARY_DIR}>`
    # → `<libxml/xmlversion.h>`) could never go green when surveyed at "//".
    # Under elements/<name>/ that same include resolves to a valid sub-dir.
    # `--bazel-package-path` tells the converter the landing package so its
    # labels (and the `# gazelle:cc_search` directives) frame correctly.
    _bb_pkg="elements/$_bb_name"
    _bb_elt="$_bb_ws/$_bb_pkg"
    rm -rf "$_bb_ws"; mkdir -p "$_bb_elt"

    # Per-project build-lens config: ONE sourced file per project,
    # scripts/build-lens/<name>.conf, instead of inline `case "$_bb_name"`
    # statements here. A greening agent adds only its own <name>.conf, so two
    # agents greening different members never touch this shared file (the
    # Phase-0 enabler for conflict-free parallel greening — see
    # docs/corpus-green-campaign.md). Reset the knobs to their safe defaults
    # FIRST so one project's config can't leak into the next (try_bazel_build
    # runs once per project in the same shell). Sourced BEFORE the source copy
    # so ELEMENT_SOURCE_ROOT can redirect what gets overlaid. Schema (all
    # optional):
    #   CONVERT_FLAGS       extra args appended to the clean convert argv
    #                       (e.g. --cmake-define X=Y, --imports-manifest=...).
    #   BAZEL_FLAGS         extra args to `bazel build` (e.g. --dynamic_mode=off).
    #   EXTRA_BAZEL_DEPS    newline-separated MODULE.bazel lines injected into the
    #                       synthesized MODULE.bazel (bazel_dep / use_extension /
    #                       register_toolchains — e.g. the rules_cuda toolchain
    #                       block for CUDA projects; see scripts/build-lens/
    #                       cutlass.conf's note on the `.cu` path).
    #   EMIT_INSTALL_EXPORT 1 (default) emits --emit-install-export-config; 0 skips.
    #   BUILD_LENS_IGNORE_REJ  read in the main loop (NOT here): 1 bypasses the
    #                       skip(rej) gate when CONVERT_FLAGS disable the surface
    #                       producing the diagnostic rejection (cutlass's tools/).
    #   ELEMENT_SOURCE_ROOT absolute dir to OVERLAY into the element instead of
    #                       the surveyed cmake dir ($_bb_src) — for a subdir-cmake
    #                       project whose sources live OUTSIDE the cmake root. The
    #                       surveyed dir ($_bb_src) must be inside this dir; cmake
    #                       still configures at $_bb_src (the --source-root). zstd
    #                       is the canonical case: its cmake root is
    #                       <repo>/build/cmake but its library sources are at
    #                       <repo>/lib + <repo>/programs (siblings of build/), so
    #                       the converter — detecting the repo root as the
    #                       workspace root — emits labels like //elements/zstd/lib:…
    #                       that only resolve when the WHOLE repo is staged under
    #                       elements/zstd/. Default empty → overlay $_bb_src.
    #   CONFIGURE_PRUNE_SUBDIRS space-separated "<rel-cmakelists>:<subdir>" pairs —
    #                       consumed by the MAIN LOOP (not here): the whole
    #                       survey of the project (diagnostics, config
    #                       detection, AND this build lens) runs against a
    #                       PRUNED COPY of the source tree with each named
    #                       `add_subdirectory(<subdir>)` line commented out in
    #                       <rel-cmakelists> — for a tree whose one un-guarded
    #                       subdir can't configure on the provisioned toolchain
    #                       (cuda-samples' 9_CUDA_Tile: `find_program(tileiras
    #                       REQUIRED)` + Tile-IR-only nvcc flags, both
    #                       CUDA-13-only). The original clone is never modified.
    CONVERT_FLAGS=""
    BAZEL_FLAGS=""
    EXTRA_BAZEL_DEPS=""
    EMIT_INSTALL_EXPORT=1
    ELEMENT_SOURCE_ROOT=""
    CONFIGURE_PRUNE_SUBDIRS=""
    # Per-project override of the global SURVEY_SPLIT_PACKAGES (empty → inherit).
    # A member whose emit isn't split-package-clean yet (grpc's protoc `gens/`
    # codegen: the sub-package move would need $(RULEDIR)/<root> and cross-package
    # tool-label rewrites the converter doesn't do yet) can force monolithic.
    CONF_SPLIT_PACKAGES=""
    unset -f extra_ws_setup 2>/dev/null || true
    _bb_conf="$repo_root/scripts/build-lens/$_bb_name.conf"
    [ -f "$_bb_conf" ] && . "$_bb_conf"
    # A per-project CONF_SPLIT_PACKAGES=0 forces monolithic for THIS element,
    # overriding the global --split-packages arg passed in ($_bb_sp). Read here
    # (not in the main loop) because the loop sources the conf only in a subshell
    # for DIAG_CONVERT_FLAGS, so CONF_SPLIT_PACKAGES doesn't propagate out.
    case "${CONF_SPLIT_PACKAGES:-}" in 0 | no | off | false) _bb_sp="" ;; esac

    # The dir overlaid into the element. Defaults to the surveyed cmake dir;
    # ELEMENT_SOURCE_ROOT redirects it to an ancestor (the repo root) for a
    # subdir-cmake project whose sources sit outside the cmake root (zstd).
    _bb_overlay_src="${ELEMENT_SOURCE_ROOT:-$_bb_src}"
    if ! cp -a "$_bb_overlay_src/." "$_bb_elt/" 2>"$_bb_po/build.log"; then
        echo "skip(copy)"; return
    fi
    # Strip any Bazel files the project SHIPS (fmt's support/bazel/, etc.): the
    # lens tests the converter's output, not a project's hand-authored Bazel, so
    # a leftover foreign BUILD/MODULE would collide with what we emit, and a
    # shipped .bazelrc/.bazelversion would steer flags/toolchain away from
    # testing just our output. NOT BUILD.bzl — that's a Starlark library, a
    # legitimate source file, not a package marker. `|| true` so a stray find
    # error (perms) can't abort the whole survey under `set -e`.
    find "$_bb_elt" -type f \( -name BUILD.bazel -o -name BUILD \
        -o -name WORKSPACE -o -name WORKSPACE.bazel -o -name MODULE.bazel -o -name 'MODULE.bazel.lock' \
        -o -name .bazelrc -o -name .bazelversion \) -delete 2>/dev/null || true
    # Convert into the overlay: per-package BUILDs land alongside the sources
    # under elements/<name>/, the shared //config package stays at the
    # workspace root (the multi-config select() arms reference //config:<name>
    # absolutely, independent of the element's package path).

    # Build the converter argv in the positional params so every argument is
    # passed atomically: a --cmake-define value may carry spaces
    # (CMAKE_<LANG>_FLAGS commonly does), which an unquoted "$var" expansion
    # would word-split. (try_bazel_build saved its own args to _bb_* above, so
    # reusing "$@" here is safe; _bb_bt / _bb_sp are single tokens or empty.)
    set -- --source-root "$_bb_src"
    [ -n "$_bb_bt" ] && set -- "$@" "$_bb_bt"
    [ -n "$_bb_sp" ] && set -- "$@" "$_bb_sp"
    # When the element is overlaid at an ancestor of the surveyed cmake dir
    # (ELEMENT_SOURCE_ROOT, e.g. cuda-samples' whole-repo overlay with a
    # per-sample cmake configure), tell the converter to anchor labels at that
    # overlay root so the sample's sources/includes carry the subdir prefix that
    # matches where the overlay staged them.
    [ -n "$ELEMENT_SOURCE_ROOT" ] && set -- "$@" --element-source-root "$ELEMENT_SOURCE_ROOT"
    # Global build-lens default: configure cmake STATIC. Bazel's cc_library is
    # always static-linked into a cc_binary, so the build lens (which lowers
    # SHARED_LIBRARY → cc_library today — see ROADMAP's cc_shared_library item)
    # must configure cmake static for its model to match what Bazel actually
    # links. A SHARED configure silently diverges: the converter collapses the
    # .so to static, and a project that compiles differently for shared vs static
    # (curl's tests recompile the curlx utility sources only under SHARED, then
    # Bazel ALSO static-links libcurl → duplicate objects → the test binary
    # SIGSEGVs) builds wrong. Forcing BUILD_SHARED_LIBS=OFF makes cmake's own
    # static/shared conditionals fire the way Bazel links. Placed BEFORE
    # CONVERT_FLAGS so a project can still override (a later cmake -D wins) if it
    # genuinely needs the shared configure. See docs/survey-corpus.md.
    # SURVEY_SHARED=1 opts into the FAITHFUL link model: build the project's
    # NATURAL config (no forced static) and emit real cc_shared_library .so's
    # (--emit-shared-libraries) with consumers' dynamic_deps wired. This is the
    # fidelity target — match what cmake would actually build; the forced-static
    # default below is the deviation. Off by default (the green corpus is
    # validated under forced-static today; flipping is a per-member re-green).
    if [ "${SURVEY_SHARED:-0}" != "0" ]; then
        set -- "$@" --emit-shared-libraries
    else
        set -- "$@" --cmake-define BUILD_SHARED_LIBS=OFF
    fi
    # CONVERT_FLAGS from the per-project .conf (word-split intentional: it's a
    # flag list authored in the conf, e.g. `--cmake-define CMAKE_CXX_FLAGS=-w`).
    # shellcheck disable=SC2086
    [ -n "$CONVERT_FLAGS" ] && set -- "$@" $CONVERT_FLAGS
    # --emit-install-export-config: the build lens is the one place that opts in
    # to generating the install(EXPORT) config-mode bundle (the real
    # <Pkg>Targets.cmake + cmake_config_bundle filegroup). Default converts omit
    # it — the orchestrated graph wires its own synthprefix-synthesized bundle —
    # but the lens generates the real file so `bazel build //...` exercises the
    # bundle end-to-end rather than choking on a filegroup over not-on-disk files.
    # A project can opt out via EMIT_INSTALL_EXPORT=0 in its .conf.
    [ "$EMIT_INSTALL_EXPORT" = "0" ] || set -- "$@" --emit-install-export-config
    # SURVEY_HOIST_COMMON_COPTS=1: lens-validate the common-copts hoist. Emit the
    # self-contained common_compile_flags.bzl + COMMON_COPTS-referencing copts
    # (mode B — no cc_toolchain wiring, so the build lens can build it directly),
    # then `bazel build //...` proves the hoisted tree still compiles on real
    # corpus members. Off by default (the inline emission is what the green
    # corpus is validated under).
    [ "${SURVEY_HOIST_COMMON_COPTS:-0}" != "0" ] && set -- "$@" --emit-common-compile-flags-bzl
    set -- "$@" \
        --bazel-package-path "$_bb_pkg" \
        --out-build "$_bb_elt/BUILD.bazel" \
        --out-config-settings "$_bb_ws/config/BUILD.bazel"
    if ! run_converter "$@" >> "$_bb_po/build.log" 2>&1
    then
        echo "skip(convert)"; return
    fi
    cat > "$_bb_ws/MODULE.bazel" <<EOF
module(name = "survey_$(printf '%s' "$_bb_name" | tr -c 'a-z0-9_' '_')", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "rules_pkg", version = "1.0.1")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
bazel_dep(name = "platforms", version = "0.0.11")
bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(module_name = "rules_buildstream_bazel", path = "$repo_root/rules_buildstream_bazel")
EOF
    # EXTRA_BAZEL_DEPS from the per-project .conf: newline-separated
    # bazel_dep(...) lines a project needs beyond the base set above (e.g. a
    # find_package dep remapped to a @bcr module). Appended verbatim.
    [ -n "$EXTRA_BAZEL_DEPS" ] && printf '%s\n' "$EXTRA_BAZEL_DEPS" >> "$_bb_ws/MODULE.bazel"
    # extra_ws_setup: optional shell function a .conf may define to write
    # additional packages into the synthesized workspace AFTER MODULE.bazel
    # exists — e.g. protobuf's //absl_umbrella package, the cc_library that
    # re-exports abseil's full public header surface to model
    # find_package(absl)'s whole-include-tree behavior (see protobuf.conf and
    # the manifest's umbrella_label). Called with the workspace root; reset
    # below between projects so one project's setup can't leak into the next.
    command -v extra_ws_setup >/dev/null 2>&1 && extra_ws_setup "$_bb_ws"
    # Per-project `bazel build` flags come from BAZEL_FLAGS in the .conf sourced
    # above (e.g. glog's --dynamic_mode=off, because its tests reference glog's
    # internal -fvisibility=hidden symbols that a default-dynamic .so won't
    # export — see scripts/build-lens/glog.conf). Empty for projects with none.
    _bb_bzlflags="$BAZEL_FLAGS"
    _bb_to=""
    command -v timeout >/dev/null 2>&1 && _bb_to="timeout ${SURVEY_BAZEL_BUILD_TIMEOUT:-900}"
    # Shared Bazel repository cache, deliberately OUTSIDE the per-member
    # --output_user_root ($_bb_po/.bzcache, which a marathon driver typically
    # rm -rf's after each member to reclaim disk). Without a shared cache every
    # member (and every retry) re-downloads the BCR dep tarballs from the GitHub
    # release CDN, so a single transient 504 on any one of them (platforms,
    # bazel_features, protobuf, …) silently drops that member's fidelity row. A
    # content-addressable cache fetched once is reused by all members + retries,
    # insulating the lens from CDN flakiness. Default to a stable home-dir path
    # (survives across marathon runs); override with SURVEY_REPO_CACHE.
    _bb_repocache="${SURVEY_REPO_CACHE:-${HOME:-/tmp}/.cache/bsb-survey-repos}"
    mkdir -p "$_bb_repocache" 2>/dev/null || true

    # Fifth lens — compile-commands FIDELITY (SURVEY_COMPILE_DB=1). Runs HERE,
    # after the convert + MODULE/workspace are in place but BEFORE the build's
    # compile: `bazel aquery` needs only ANALYSIS, so we catch per-TU flag drift
    # (defines / -std / source includes) without paying for the full build. cmake
    # ground truth comes from a quick EXPORT_COMPILE_COMMANDS configure of
    # $_bb_src with the SAME cmake-defines the convert used (BUILD_SHARED_LIBS=OFF
    # + the .conf's --cmake-define pairs). Report-only (writes fidelity.json); it
    # does not gate the build. See cmd/compile-commands-diff + docs.
    if [ "${SURVEY_COMPILE_DB:-0}" != "0" ]; then
        # CMAKE_POLICY_VERSION_MINIMUM=3.5: cmake 4.x removed compat with
        # cmake_minimum_required(<3.5); the converter auto-retries with this on
        # the policy-floor error (cmakerun/run.go), so the fidelity configure
        # must pass it too or it fails for old-floor projects (libevent) and the
        # fidelity row is silently absent. Harmless for projects that don't need it.
        _cc_defs="-DBUILD_SHARED_LIBS=OFF -DCMAKE_POLICY_VERSION_MINIMUM=3.5"
        # shellcheck disable=SC2086
        set -- $CONVERT_FLAGS
        while [ $# -gt 0 ]; do
            if [ "$1" = "--cmake-define" ] && [ $# -ge 2 ]; then
                _cc_defs="$_cc_defs -D$2"; shift 2
            else shift; fi
        done
        _cc_cm="$_bb_po/cc-cmake"
        rm -rf "$_cc_cm"; mkdir -p "$_cc_cm/.cmake/api/v1/query"
        : > "$_cc_cm/.cmake/api/v1/query/codemodel-v2"  # for the link-ORDER check
        # Match the converter's build type. cmakerun defaults a SINGLE-config
        # convert to CMAKE_BUILD_TYPE=Release, so the reference cmake must
        # configure Release too — else a project that declares no default of its
        # own (OpenBLAS) resolves an EMPTY build type here (no -DNDEBUG / -O*)
        # while the converted bazel carries the Release flags, a spurious per-TU
        # drift on EVERY TU (OpenBLAS's 6277-TU NDEBUG delta). A project that DOES
        # default Release (fmt/spdlog) is unchanged (already Release). Multi-config
        # members ($_bb_bt set) keep the cache-read alignment below instead — their
        # convert uses --build-types select() arms, not a single Release.
        [ -z "$_bb_bt" ] && _cc_defs="$_cc_defs -DCMAKE_BUILD_TYPE=Release"
        # shellcheck disable=SC2086
        if cmake -S "$_bb_src" -B "$_cc_cm" -G Ninja -DCMAKE_EXPORT_COMPILE_COMMANDS=ON $_cc_defs \
                >> "$_bb_po/fidelity.log" 2>&1 && [ -f "$_cc_cm/compile_commands.json" ]; then
            # Derive the build's link mode from this (forced-static) codemodel
            # (opt-in SURVEY_DERIVE_LINK_MODE; skipped under SURVEY_SHARED and
            # when the member already pins --dynamic_mode). When the project
            # declares NO SHARED/MODULE library targets, link the bazel
            # aquery + build STATIC (--dynamic_mode=off) — the model cmake
            # actually uses here — instead of bazel's default dynamic cc_library
            # .so's. This is what makes the project-archive link-ORDER check
            # below meaningful (dynamic linking is order-independent), and drops
            # the per-member --dynamic_mode=off knobs for all-static members.
            # Rides the compile-db lens's codemodel (the link-order lens is where
            # static linking matters); generalizing to the pure-build path is a
            # follow-up. Forcing static can surface ODR/duplicate-symbol issues a
            # dynamic build tolerated, so it stays opt-in pending a corpus re-green.
            if [ "${SURVEY_DERIVE_LINK_MODE:-0}" != "0" ] && [ "${SURVEY_SHARED:-0}" = "0" ]; then
                case " $_bb_bzlflags " in
                    *" --dynamic_mode"*) ;;  # explicit per-member override wins
                    *)
                        if ! grep -rlq '"SHARED_LIBRARY"\|"MODULE_LIBRARY"' "$_cc_cm/.cmake/api/v1/reply" 2>/dev/null; then
                            _bb_bzlflags="$_bb_bzlflags --dynamic_mode=off"
                            echo "  $_bb_name: derive-link-mode -> --dynamic_mode=off (all-static codemodel)" >&2
                        fi ;;
                esac
            fi
            # Align the aquery's config with cmake's EFFECTIVE build type. cmake's
            # single-config configure resolves a CMAKE_BUILD_TYPE — often the
            # project's own default (fmt/spdlog default to Release) — so its
            # compile_commands carry that config's flags (-DNDEBUG, -O3). The
            # converted MULTI-config BUILD gates those behind //config:<type>
            # select() arms whose driving flag (//config:build_type) DEFAULTS to
            # debug. Aquery without selecting cmake's config and every Release-only
            # define (NDEBUG …) reads as "missing in bazel" — a spurious per-TU
            # diff (the fmt NDEBUG drift). Read the build type cmake chose, lower-
            # case it, and select it when the //config package offers that value.
            _cc_cfg=""
            _cc_bt="$(sed -n 's/^CMAKE_BUILD_TYPE:[^=]*=//p' "$_cc_cm/CMakeCache.txt" 2>/dev/null | head -1 | tr 'A-Z' 'a-z')"
            if [ -n "$_cc_bt" ] && [ -f "$_bb_ws/config/BUILD.bazel" ] && grep -q "\"$_cc_bt\"" "$_bb_ws/config/BUILD.bazel"; then
                _cc_cfg="--//config:build_type=$_cc_bt"
            fi
            # Thread the conf's BAZEL_FLAGS ($_bb_bzlflags, e.g.
            # --incompatible_autoload_externally) into the aquery: ANALYSIS still
            # loads the BCR deps' BUILD files, so a member whose deps need the
            # autoload shim (re2/abseil/protobuf cc_library on bazel 9) fails the
            # aquery without it — exactly as the build at the bottom does.
            # shellcheck disable=SC2086
            if ( cd "$_bb_ws" && $bzl_bin --output_user_root="$_bb_po/.bzcache" --noworkspace_rc \
                    ${META_BAZEL_STARTUP_ARGS:-} aquery --repository_cache="$_bb_repocache" $_bb_bzlflags $_cc_cfg --output=jsonproto 'mnemonic("CppCompile", //...)' ) \
                    > "$_bb_po/cc-aquery.json" 2>> "$_bb_po/fidelity.log"; then
                # CppLink aquery for the link-order check (best-effort).
                # shellcheck disable=SC2086
                ( cd "$_bb_ws" && $bzl_bin --output_user_root="$_bb_po/.bzcache" --noworkspace_rc \
                    ${META_BAZEL_STARTUP_ARGS:-} aquery --repository_cache="$_bb_repocache" $_bb_bzlflags $_cc_cfg --output=jsonproto 'mnemonic("CppLink", //...)' ) \
                    > "$_bb_po/cc-aquery-link.json" 2>> "$_bb_po/fidelity.log" || true
                _cc_diff="$repo_root/build/bin/compile-commands-diff"
                ( cd "$repo_root" && go build -o "$_cc_diff" ./converter/cmd/compile-commands-diff ) 2>>"$_bb_po/fidelity.log" || _cc_diff="go run $repo_root/converter/cmd/compile-commands-diff"
                # shellcheck disable=SC2086
                $_cc_diff --cmake "$_cc_cm/compile_commands.json" --aquery "$_bb_po/cc-aquery.json" \
                    --json "$_bb_po/fidelity.json" --cmake-src "$_bb_src" --cmake-build "$_cc_cm" \
                    --bazel-package "$_bb_pkg" \
                    --cmake-codemodel "$_cc_cm/.cmake/api/v1/reply" --aquery-link "$_bb_po/cc-aquery-link.json" \
                    >> "$_bb_po/fidelity.log" 2>&1 || true
                echo "  $_bb_name: compile-db fidelity -> $_bb_po/fidelity.json" >&2
            fi
        fi
    fi
    # Sixth lens — intent-capture (SURVEY_INTENT=1 + $INTENT_LENS_JUDGE). The
    # agent-as-oracle "what did we miss?" pass over the converted bundle (this
    # workspace's BUILD/MODULE + the cmake sources), triaged against the
    # conversion-todos / rejections this element already wrote to $_bb_po. Runs
    # here, after convert + MODULE are in place; needs no build. Best-effort.
    if [ "${SURVEY_INTENT:-0}" != "0" ] && [ -n "${INTENT_LENS_JUDGE:-}" ]; then
        if sh "$repo_root/scripts/intent-capture-lens.sh" \
                "$_bb_ws" "$_bb_src" "$_bb_po" "$_bb_name" >> "$_bb_po/intent.log" 2>&1; then
            echo "  $_bb_name: intent-capture -> $_bb_po/intent-capture.json" >&2
        else
            echo "  $_bb_name: intent-capture lens failed (see $_bb_po/intent.log)" >&2
        fi
    fi
    # Source-narrowing-compatibility lens (SURVEY_NARROWING_COMPAT=1). STRUCTURAL
    # (cmake-only, no Bazel build) — runs here, BEFORE the build, with the other
    # structural lenses, per the pipeline ordering (structural → build → symbol-
    # fidelity). Proves the convert is a pure function of build-system files +
    # codemodel + trace, not source/header BYTES, by re-converting a copy with
    # every non-read-set source/header zeroed and asserting a byte-identical
    # BUILD. Report-only (writes narrowing-compat.json). See the lens script.
    if [ "${SURVEY_NARROWING_COMPAT:-0}" != "0" ]; then
        _nc_split=0; [ -n "$_bb_sp" ] && _nc_split=1
        # shellcheck disable=SC2086
        _nc_res="$(sh "$repo_root/scripts/narrowing-compat-lens.sh" \
            --src "$_bb_src" --name "$_bb_name" --out "$_bb_po" --split "$_nc_split" \
            --bazel-package-path "$_bb_pkg" -- $CONVERT_FLAGS 2>>"$_bb_po/narrowing-compat.log" || echo "skip(error)")"
        echo "  $_bb_name: narrowing-compat -> $_nc_res ($_bb_po/narrowing-compat.json)" >&2
    fi
    # --noworkspace_rc: the lens measures whether OUR emitted module/build graph
    # builds, so ignore any workspace .bazelrc (matches the repo's other survey
    # scripts, and is belt-and-suspenders with the .bazelrc strip above). Thread
    # both startup-arg and build-arg passthrough too (META_BAZEL_STARTUP_ARGS
    # goes before the subcommand — registry tweaks for sandboxed/offline runs).
    # Run the actual `bazel build //...` only when the build lens selected THIS
    # member. try_bazel_build is also entered purely for the workspace-only
    # lenses above (compile-db / intent / narrowing — see ws_lens_wanted) when
    # SURVEY_BAZEL_BUILD did NOT select this member; in that case the convert +
    # those lenses have run and we skip the (unrequested) build. SURVEY_SKIP_BUILD=1
    # forces the same skip even for a selected member — for refreshing the
    # compile-db + intent rows across an already-green corpus without paying for
    # the (redundant) full rebuild. Either way the build-status column shows "skip".
    if [ "${SURVEY_SKIP_BUILD:-0}" != "0" ] || ! build_lens_for "$_bb_name"; then
        echo "skip"
        return
    fi
    if ( cd "$_bb_ws" && $_bb_to "$bzl_bin" --output_user_root="$_bb_po/.bzcache" \
            --noworkspace_rc ${META_BAZEL_STARTUP_ARGS:-} build --repository_cache="$_bb_repocache" ${META_BAZEL_BUILD_ARGS:-} $_bb_bzlflags //... ) >> "$_bb_po/build.log" 2>&1; then
        echo "ok"
    else
        echo "FAIL"
    fi
}
# --------------------------------------------------------------------------

mkdir -p "$out_dir"
summary="$out_dir/summary.txt"
: > "$summary"

# The `missed` column is the intent-capture lens's net-new finding count (6th
# lens; "-" unless SURVEY_INTENT ran). UNLIKE every other column it is
# NON-DETERMINISTIC (an LLM judge produced it) — a triage pointer to
# producer-gap candidates, NOT a count comparable across runs. See the
# intent-capture lens section in docs/survey-corpus.md.
printf '%-14s %10s %10s %10s %6s %7s %8s %s\n' project rejections idioms coverage todos missed build status | tee "$summary"
printf '%-14s %10s %10s %10s %6s %7s %8s %s\n' ------- ---------- ------ -------- ----- ------ ----- ------ | tee -a "$summary"

for entry in $projects; do
    name="${entry%%=*}"
    src="${entry#*=}"
    proj_out="$out_dir/$name"
    mkdir -p "$proj_out"

    if [ ! -d "$src" ]; then
        printf '%-14s %10s %10s %10s %6s %7s %s\n' "$name" - - - - - "MISSING ($src) — run 'make fetch-$name'" | tee -a "$summary"
        continue
    fi

    rej="$proj_out/rejections.json"
    idiom="$proj_out/bazel-idiom.json"
    cov="$proj_out/coverage.json"
    todo="$proj_out/conversion-todos.json"
    status="ok"

    # DIAG_CONVERT_FLAGS (opt-in, per-project): extra --cmake-define / convert
    # args applied to THIS project's *diagnostics* convert below — i.e. the
    # convert whose rejections.json drives the build-lens skip(rej) gate. It is
    # SEPARATE from the build-lens CONVERT_FLAGS (sourced inside try_bazel_build):
    # those shape only the build, and run AFTER the gate, so they can't lift a
    # rejection that makes the gate skip in the first place. Default empty → the
    # diagnostics survey stays the faithful default-configure for every member
    # that doesn't set it (no change to existing survey numbers). A project sets
    # it when its faithful default configure surfaces a not-Bazel-modelable
    # rejection that an equally-faithful ALTERNATE configure of the same project
    # avoids — e.g. OpenBLAS: its native configure runs the getarch host-CPU
    # probes (4 execute_process(OUTPUT_VARIABLE) → unsupported-execute-process),
    # but its first-class deterministic-arch (cross-compile) configure branch
    # writes the arch config from a static per-core table and needs no probe;
    # pinning the core (CORE/TARGET=HASWELL + CMAKE_SYSTEM_NAME) is what makes
    # the arch-conditional source selection static, which is exactly what a
    # Bazel graph requires. See scripts/build-lens/openblas.conf. Sourced in a
    # subshell so only this one var crosses back — the .conf's other knobs
    # (build CONVERT_FLAGS/BAZEL_FLAGS/...) stay scoped to try_bazel_build.
    diag_convert_flags=""
    _diag_conf="$repo_root/scripts/build-lens/$name.conf"
    if [ -f "$_diag_conf" ]; then
        diag_convert_flags="$(
            DIAG_CONVERT_FLAGS=""
            . "$_diag_conf" >/dev/null 2>&1 || true
            printf '%s' "$DIAG_CONVERT_FLAGS"
        )"
    fi

    # CONFIGURE_PRUNE_SUBDIRS (per-project conf knob, "<rel-cmakelists>:<subdir>"
    # pairs): survey a PRUNED COPY of the source tree with each named
    # `add_subdirectory(<subdir>)` line commented out — for a tree whose one
    # un-guarded subdir can't configure on the provisioned toolchain
    # (cuda-samples' 9_CUDA_Tile: `find_program(tileiras REQUIRED)` + Tile-IR
    # nvcc flags, both CUDA-13-only). Staged HERE, before any pass runs, so the
    # diagnostics convert, detect_configs, and the build lens all see the same
    # pruned tree through $src; the original clone is never modified. Each
    # pair's <rel-cmakelists> anchors at the nearest ancestor of $src that
    # carries it (a scoped survey of one sample subdir prunes the same
    # repo-root file the whole-tree survey does); entries whose file resolves
    # nowhere are skipped — the prune target isn't in this survey's view.
    prune_subdirs="$(
        CONFIGURE_PRUNE_SUBDIRS=""
        [ -f "$_diag_conf" ] && { . "$_diag_conf" >/dev/null 2>&1 || true; }
        printf '%s' "$CONFIGURE_PRUNE_SUBDIRS"
    )"
    for _pp in $prune_subdirs; do
        _pp_file="${_pp%%:*}"
        _pp_dir="${_pp#*:}"
        _pp_base="$src"
        while [ "$_pp_base" != "/" ] && [ ! -f "$_pp_base/$_pp_file" ]; do
            _pp_base="$(dirname "$_pp_base")"
        done
        [ -f "$_pp_base/$_pp_file" ] || continue
        case "$src" in
        "$proj_out"/*) ;; # later pair: already staged, prune in place
        *)
            _pp_root="$proj_out/pruned-src"
            rm -rf "$_pp_root"
            mkdir -p "$_pp_root"
            cp -a "$_pp_base/." "$_pp_root/"
            src="$_pp_root${src#"$_pp_base"}"
            _pp_base="$_pp_root"
            ;;
        esac
        sed -i "s|^\\([[:space:]]*\\)add_subdirectory(${_pp_dir})|\\1# pruned by run-survey (CONFIGURE_PRUNE_SUBDIRS): add_subdirectory(${_pp_dir})|" \
            "$_pp_base/$_pp_file"
    done

    # The survey runs each project standalone, so out-of-tree
    # find_package(...) deps (e.g. protobuf's find_package(ZLIB))
    # surface as honest find-package-dep-unresolved findings. In a
    # real .bst element graph these resolve through the orchestrated
    # producer→consumer export channel (write-a stages each kind:cmake
    # dep's exports.json + cmake-config bundle into the consumer's
    # convert genrule); the standalone survey deliberately doesn't
    # paper over the gap with a hand-authored imports manifest.

    # --diagnostics implies --ignore-rejections-for-diagnostics; the run
    # continues past refusals. A non-zero exit here means cmake configure
    # itself failed (itself a survey datapoint), not a refusal.
    #
    # Optional multi-config (--build-types) + split-packages modes, per
    # SURVEY_BUILD_TYPES / SURVEY_SPLIT_PACKAGES. "auto" detects this
    # project's own declared configuration types so ALL of them are
    # surveyed (faithful — no config's intent dropped).
    bt_args=""
    if [ "$build_types" = "auto" ]; then
        bt_detected="$(detect_configs "$src")"
        if [ -n "$bt_detected" ]; then
            bt_args="--build-types=$bt_detected"
            echo "  $name: configuration types: $bt_detected" >&2
        else
            echo "  $name: config detection failed; single-config" >&2
        fi
    elif [ -n "$build_types" ]; then
        bt_args="--build-types=$build_types"
    fi
    sp_args=""
    _eff_split="${CONF_SPLIT_PACKAGES:-$split_packages}"
    case "$_eff_split" in 0|no|off|false) _eff_split="" ;; esac
    if [ -n "$_eff_split" ]; then
        sp_args="--split-packages"
    fi

    # $diag_convert_flags word-splits intentionally (a flag list authored in the
    # .conf, e.g. `--cmake-define CORE=HASWELL`); empty for every project that
    # doesn't opt in, so the expansion adds nothing there.
    # shellcheck disable=SC2086
    if ! run_converter \
        --source-root "$src" \
        --diagnostics \
        $bt_args \
        $sp_args \
        $diag_convert_flags \
        --rejections-report "$rej" \
        --audit-bazel-idiom-report "$idiom" \
        --audit-coverage-report "$cov" \
        --conversion-todos-report "$todo" \
        --out-build "$proj_out/BUILD.bazel" \
        --out-failure "$proj_out/failure.json" \
        > "$proj_out/convert.log" 2>&1
    then
        status="CONFIGURE/CONVERT FAILED — see $proj_out/convert.log"
    fi

    # Count records. rejections.json is a JSON array of {code,...};
    # bazel-idiom.json and coverage.json are arrays of {Code,...}
    # (capitalised — distinct Go structs). The coverage column is the
    # lens-3 dependency-coverage count (silent dropped link edges); 0
    # is the healthy state.
    rej_n=$( [ -f "$rej" ]   && grep -o '"code"' "$rej"  2>/dev/null | wc -l | tr -d ' ' || echo "-" )
    idi_n=$( [ -f "$idiom" ] && grep -o '"Code"' "$idiom" 2>/dev/null | wc -l | tr -d ' ' || echo "-" )
    cov_n=$( [ -f "$cov" ]   && grep -o '"Code"' "$cov"   2>/dev/null | wc -l | tr -d ' ' || echo "-" )
    # conversion-todos.json is {version, preamble, todos:[{id,...}]}; the
    # todos column counts the no-mechanical-form units. Match the "id" KEY
    # (`"id":`) rather than the bare token so a string value equal to "id"
    # can't overcount.
    todo_n=$( [ -f "$todo" ] && grep -oE '"id"[[:space:]]*:' "$todo" 2>/dev/null | wc -l | tr -d ' ' || echo "-" )

    # The build-lens skip(rej) decision must ignore BENIGN diagnostics that
    # don't actually block a clean (strict-mode) convert: the
    # "...doesn't exist on disk; treated as empty" unsupported-source-path
    # notice (forward-declared / genex include dirs) is recorded only in
    # diagnostics mode and is a no-op in strict mode -- it never aborts the
    # convert, so counting it falsely skipped projects whose clean convert
    # succeeds (glog). rej_blocking subtracts that benign class; rej_n stays
    # the raw diagnostic count shown in the column.
    # `|| true` so grep -c's exit-1-on-zero-matches doesn't trip `set -e` for
    # the common case (rejections.json present, no benign notice); the "0" it
    # printed is still captured. ${:-0} covers the no-file case (empty).
    rej_benign=$( [ -f "$rej" ] && grep -c 'treated as empty' "$rej" 2>/dev/null || true )
    rej_benign=${rej_benign:-0}
    if [ "$rej_n" != "-" ]; then
        rej_blocking=$((rej_n - rej_benign))
    else
        rej_blocking="-"
    fi

    # A project whose build-lens .conf disables the very surface that produces
    # its diagnostic rejection(s) can opt the skip(rej) gate OFF with
    # BUILD_LENS_IGNORE_REJ=1 in scripts/build-lens/<name>.conf. The diagnostic
    # pass above surveys the FULL tree (no CONVERT_FLAGS) so its rejection count
    # — the reported `rejections` column — stays honest; but the build lens
    # converts the CONFIGURED surface (CONVERT_FLAGS applied), which may not hit
    # those rejections at all. cutlass is the case: its core is a header-only
    # template library, but its tools/ DEV SURFACE has a `cmake -E env`
    # execute_process the full-tree diagnostic flags as 1 rejection; the
    # build-lens conf disables tools/ (CUTLASS_ENABLE_TOOLS=OFF, …), so the
    # configured convert never sees it. The gate is only a cost optimization
    # ("don't pay for a second convert on a tree that would abort"); the
    # build-lens's OWN clean convert is the real test and returns skip(convert)
    # if it actually fails — so honoring this opt-out can't mask a broken
    # convert. Read just this knob from the .conf here (try_bazel_build sources
    # the full .conf again, resetting all knobs, so nothing leaks between
    # projects). Empty/0 (the default, no .conf or knob unset) keeps the gate.
    bl_ignore_rej=0
    _bl_conf="$repo_root/scripts/build-lens/$name.conf"
    if [ -f "$_bl_conf" ]; then
        BUILD_LENS_IGNORE_REJ=0
        # shellcheck disable=SC1090
        . "$_bl_conf"
        bl_ignore_rej="${BUILD_LENS_IGNORE_REJ:-0}"
        unset BUILD_LENS_IGNORE_REJ
    fi

    # 4th lens: `bazel build //...` of the faithful (project-B-shaped) output,
    # when this project is selected by SURVEY_BAZEL_BUILD — OR when a
    # workspace-only lens (compile-db / intent / narrowing, see ws_lens_wanted)
    # needs try_bazel_build's converted workspace WITHOUT the build (the build
    # step inside is gated on build_lens_for, so a lens-only entry skips it).
    # Short-circuit the cases that can't (or shouldn't) proceed before paying
    # for a clean convert: a hard-failed diagnostic convert (configure must work
    # first), and a project that surveys with rejections (the lens contract is to
    # skip refusals — the clean convert would just abort on the first one, and
    # the aquery/intent lenses need that clean convert too), unless the project
    # opted out of the rejection gate (BUILD_LENS_IGNORE_REJ, above).
    build_status="-"
    if build_lens_for "$name" || ws_lens_wanted; then
        if [ "$status" != "ok" ]; then
            build_status="skip(convert)"
        elif [ "$bl_ignore_rej" != "1" ] && [ -n "$rej_blocking" ] && [ "$rej_blocking" != "0" ] && [ "$rej_blocking" != "-" ]; then
            build_status="skip(rej)"
        else
            build_status="$(try_bazel_build "$name" "$src" "$proj_out" "$bt_args" "$sp_args")"
            [ "$build_status" = "FAIL" ] && status="bazel build //... FAILED — see $proj_out/build.log"
        fi
    fi

    # 7th lens — SYMBOL FIDELITY (SURVEY_SYMBOL_FIDELITY=1). Runs LAST, after the
    # build, and only when the build lens passed — per the pipeline ordering
    # (structural → build → symbol-fidelity). SPLIT-consistent with the rest of
    # the survey: it does NOT re-convert via run-fidelity (that's the CI/mono
    # path). Instead it (1) reuses the bazel archive the build lens ABOVE already
    # built split (so generated configure_file headers etc. are wired exactly as
    # the corpus builds them — libxml2's <libxml/xmlversion.h> resolves via the
    # synthesized root_headers, which mono never gets), (2) builds the cmake-side
    # archive natively (cmake owns its configure_file generation), and (3) diffs
    # the two exported-symbol sets with cmd/fidelity-compare. Driven by the
    # member's scripts/build-lens/<name>.symfidelity (SYMFID_TARGET + SYMFID_
    # ARTIFACT or SYMFID_{CMAKE,BAZEL}_ARTIFACT [+ SYMFID_CMAKE_FLAGS]) and its
    # testdata/fidelity/<name>.allowlist.txt. Report-only
    # (<out>/<name>/symbol-fidelity.json); members without a config self-skip.
    if [ "${SURVEY_SYMBOL_FIDELITY:-0}" != "0" ] && [ "$build_status" = "ok" ]; then
        _sf_conf="$repo_root/scripts/build-lens/$name.symfidelity"
        if [ -f "$_sf_conf" ]; then
            (
                SYMFID_TARGET=""; SYMFID_ARTIFACT=""; SYMFID_CMAKE_ARTIFACT=""
                SYMFID_BAZEL_ARTIFACT=""; SYMFID_CMAKE_FLAGS=""; SYMFID_CONVERT_FLAGS=""
                # shellcheck disable=SC1090
                . "$_sf_conf"
                _sf_log="$proj_out/symbol-fidelity.log"; : > "$_sf_log"
                _sf_cmke="${SYMFID_CMAKE_ARTIFACT:-$SYMFID_ARTIFACT}"
                _sf_bzl="${SYMFID_BAZEL_ARTIFACT:-$SYMFID_ARTIFACT}"
                [ -z "$_sf_bzl" ] && _sf_bzl="lib${SYMFID_TARGET}.a"

                # (1) Bazel side: reuse the build lens's CONVERTED SPLIT workspace
                # (so generated configure_file headers wire as the corpus builds
                # them — libxml2's <libxml/xmlversion.h>), but rebuild //... at the
                # RELEASE config arm. Exported-symbol sets are optimization-
                # sensitive: a debug build emits weak template instantiations that
                # an optimized build inlines away, so the bazel side must match the
                # cmake Release build below. Deps are cached in the build lens's
                # bzcache, so this is an incremental release recompile.
                _sf_rc="${SURVEY_REPO_CACHE:-${HOME:-/tmp}/.cache/bsb-survey-repos}"
                # Thread the member's .conf BAZEL_FLAGS (e.g. glog's
                # --dynamic_mode=off, which the build lens also uses) so this
                # release rebuild matches what the build lens built — the loop
                # sources confs only in subshells, so read it here.
                _sf_bzlflags="$(BAZEL_FLAGS=""; [ -f "$repo_root/scripts/build-lens/$name.conf" ] && . "$repo_root/scripts/build-lens/$name.conf" >/dev/null 2>&1 || true; printf '%s' "$BAZEL_FLAGS")"
                # shellcheck disable=SC2086
                ( cd "$proj_out/build-ws" && $bzl_bin --output_user_root="$proj_out/.bzcache" \
                    --noworkspace_rc ${META_BAZEL_STARTUP_ARGS:-} build --repository_cache="$_sf_rc" \
                    $_sf_bzlflags --//config:build_type=release //... ) >>"$_sf_log" 2>&1 || true
                _sf_bzlart="$(find -L "$proj_out/build-ws/bazel-bin" -name "$_sf_bzl" -type f 2>/dev/null | head -1)"

                # (2) Cmake side: cmake configures+builds the target natively at
                # Release (config-aligned with the bazel side above). Defines mirror
                # the build-lens convert so the symbol set matches: BUILD_SHARED_LIBS
                # =OFF + the .conf's --cmake-define pairs (sourced here; the loop
                # sources confs only in subshells) + SYMFID_CMAKE_FLAGS.
                _sf_defs="-DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DCMAKE_POLICY_VERSION_MINIMUM=3.5 $SYMFID_CMAKE_FLAGS"
                _sf_cflags="$(
                    CONVERT_FLAGS=""
                    [ -f "$repo_root/scripts/build-lens/$name.conf" ] && . "$repo_root/scripts/build-lens/$name.conf" >/dev/null 2>&1 || true
                    printf '%s' "$CONVERT_FLAGS"
                )"
                # shellcheck disable=SC2086
                set -- $_sf_cflags
                while [ $# -gt 0 ]; do
                    if [ "$1" = "--cmake-define" ] && [ $# -ge 2 ]; then _sf_defs="$_sf_defs -D$2"; shift 2; else shift; fi
                done
                _sf_cm="$proj_out/symfid-cmake"; rm -rf "$_sf_cm"
                _sf_cmart=""
                # shellcheck disable=SC2086
                if cmake -S "$src" -B "$_sf_cm" -G Ninja $_sf_defs >>"$_sf_log" 2>&1 \
                        && cmake --build "$_sf_cm" --target "$SYMFID_TARGET" >>"$_sf_log" 2>&1; then
                    _sf_cmart="$(find "$_sf_cm" -name "$_sf_cmke" -type f 2>/dev/null | head -1)"
                fi

                # (3) Compare exported-symbol sets.
                _sf_cmp="$repo_root/build/bin/fidelity-compare"
                ( cd "$repo_root" && go build -o "$_sf_cmp" ./cmd/fidelity-compare ) 2>>"$_sf_log" || _sf_cmp="go run $repo_root/cmd/fidelity-compare"
                _sf_al=""
                [ -f "$repo_root/testdata/fidelity/$name.allowlist.txt" ] && _sf_al="--allowlist $repo_root/testdata/fidelity/$name.allowlist.txt"
                if [ -n "$_sf_bzlart" ] && [ -n "$_sf_cmart" ]; then
                    # shellcheck disable=SC2086
                    if $_sf_cmp --cmake-artifact "$_sf_cmart" --bazel-artifact "$_sf_bzlart" \
                            --report "$proj_out/symbol-fidelity.json" $_sf_al >>"$_sf_log" 2>&1; then
                        echo "  $name: symbol-fidelity -> ok" >&2
                    else
                        echo "  $name: symbol-fidelity -> FAIL (see $_sf_log)" >&2
                    fi
                else
                    printf '{"member":"%s","ok":false,"error":"missing-artifact","cmake":"%s","bazel":"%s"}\n' \
                        "$name" "${_sf_cmart:-NONE}" "${_sf_bzlart:-NONE}" > "$proj_out/symbol-fidelity.json"
                    echo "  $name: symbol-fidelity -> skip(no-artifact: cmake=$_sf_cmke bazel=$_sf_bzl)" >&2
                fi
            )
        else
            echo "  $name: symbol-fidelity -> skip(no-config)" >&2
        fi
    fi

    # 8th lens — ELF DYNAMIC-SECTION FIDELITY (SURVEY_ELF_FIDELITY=1). Runs LAST,
    # after the symbol lens — the pipeline-last ordering (structural -> build ->
    # symbol -> elf). Compares the cmake-built vs Bazel-built SHARED artifact's
    # dynamic section (SONAME / DT_NEEDED / .gnu.version_d / DT_RPATH-RUNPATH) with
    # cmd/elf-fidelity-compare — the ABI facts an exported-symbol-SET compare
    # can't express. Needs SHARED artifacts on both sides, so it pairs with
    # SURVEY_SHARED=1 (the build lens then converted with --emit-shared-libraries
    # and built the cc_shared_library .so) and builds the cmake side with
    # BUILD_SHARED_LIBS=ON. Dynamic metadata is config-invariant, so it REUSES the
    # build-ws .so the build lens already produced (no release rebuild, unlike the
    # symbol lens). Driven by scripts/build-lens/<name>.elffidelity (ELFID_TARGET +
    # ELFID_ARTIFACT or ELFID_{CMAKE,BAZEL}_ARTIFACT [+ ELFID_CMAKE_FLAGS]) and its
    # testdata/fidelity/<name>.elf-allowlist.txt. Report-only
    # (<out>/<name>/elf-fidelity.json); members without a config self-skip.
    if [ "${SURVEY_ELF_FIDELITY:-0}" != "0" ] && [ "$build_status" = "ok" ]; then
        _ef_conf="$repo_root/scripts/build-lens/$name.elffidelity"
        if [ -f "$_ef_conf" ]; then
            if [ "${SURVEY_SHARED:-0}" = "0" ]; then
                # Without SURVEY_SHARED the converted workspace has no
                # cc_shared_library, so there's no .so to compare — skip loudly
                # rather than silently emit nothing.
                echo "  $name: elf-fidelity -> skip(needs SURVEY_SHARED=1 for the .so)" >&2
            else
            (
                ELFID_TARGET=""; ELFID_ARTIFACT=""; ELFID_CMAKE_ARTIFACT=""
                ELFID_BAZEL_ARTIFACT=""; ELFID_CMAKE_FLAGS=""
                # shellcheck disable=SC1090
                . "$_ef_conf"
                _ef_log="$proj_out/elf-fidelity.log"; : > "$_ef_log"
                _ef_cmke="${ELFID_CMAKE_ARTIFACT:-$ELFID_ARTIFACT}"
                _ef_bzl="${ELFID_BAZEL_ARTIFACT:-$ELFID_ARTIFACT}"
                [ -z "$_ef_bzl" ] && _ef_bzl="lib${ELFID_TARGET}.so"
                [ -z "$_ef_cmke" ] && _ef_cmke="lib${ELFID_TARGET}.so"

                # (1) Bazel side: reuse the SHARED .so the build lens already built
                # in the converted split workspace (dynamic metadata is config-
                # invariant, so no rebuild needed). A `.so.N` versioned name is
                # matched by globbing the base.
                _ef_bzlart="$(find -L "$proj_out/build-ws/bazel-bin" -name "$_ef_bzl" -type f 2>/dev/null | head -1)"
                [ -z "$_ef_bzlart" ] && _ef_bzlart="$(find -L "$proj_out/build-ws/bazel-bin" -name "$_ef_bzl.*" -type f 2>/dev/null | head -1)"

                # (2) Cmake side: configure+build the target natively with SHARED
                # libraries. Defines mirror the build-lens convert (the .conf's
                # --cmake-define pairs, sourced here) but with BUILD_SHARED_LIBS=ON.
                _ef_defs="-DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=ON -DCMAKE_POLICY_VERSION_MINIMUM=3.5 $ELFID_CMAKE_FLAGS"
                _ef_cflags="$(
                    CONVERT_FLAGS=""
                    [ -f "$repo_root/scripts/build-lens/$name.conf" ] && . "$repo_root/scripts/build-lens/$name.conf" >/dev/null 2>&1 || true
                    printf '%s' "$CONVERT_FLAGS"
                )"
                # shellcheck disable=SC2086
                set -- $_ef_cflags
                while [ $# -gt 0 ]; do
                    if [ "$1" = "--cmake-define" ] && [ $# -ge 2 ]; then _ef_defs="$_ef_defs -D$2"; shift 2; else shift; fi
                done
                _ef_cm="$proj_out/elffid-cmake"; rm -rf "$_ef_cm"
                _ef_cmart=""
                # shellcheck disable=SC2086
                if cmake -S "$src" -B "$_ef_cm" -G Ninja $_ef_defs >>"$_ef_log" 2>&1 \
                        && cmake --build "$_ef_cm" --target "$ELFID_TARGET" >>"$_ef_log" 2>&1; then
                    _ef_cmart="$(find "$_ef_cm" -name "$_ef_cmke" -type f 2>/dev/null | head -1)"
                    [ -z "$_ef_cmart" ] && _ef_cmart="$(find "$_ef_cm" -name "$_ef_cmke.*" -type f 2>/dev/null | head -1)"
                fi

                # (3) Compare dynamic sections.
                _ef_cmp="$repo_root/build/bin/elf-fidelity-compare"
                ( cd "$repo_root" && go build -o "$_ef_cmp" ./cmd/elf-fidelity-compare ) 2>>"$_ef_log" || _ef_cmp="go run $repo_root/cmd/elf-fidelity-compare"
                _ef_al=""
                [ -f "$repo_root/testdata/fidelity/$name.elf-allowlist.txt" ] && _ef_al="--allowlist $repo_root/testdata/fidelity/$name.elf-allowlist.txt"
                if [ -n "$_ef_bzlart" ] && [ -n "$_ef_cmart" ]; then
                    # shellcheck disable=SC2086
                    if $_ef_cmp --cmake-artifact "$_ef_cmart" --bazel-artifact "$_ef_bzlart" \
                            --report "$proj_out/elf-fidelity.json" $_ef_al >>"$_ef_log" 2>&1; then
                        echo "  $name: elf-fidelity -> ok" >&2
                    else
                        echo "  $name: elf-fidelity -> FAIL (see $_ef_log)" >&2
                    fi
                else
                    printf '{"member":"%s","ok":false,"error":"missing-artifact","cmake":"%s","bazel":"%s"}\n' \
                        "$name" "${_ef_cmart:-NONE}" "${_ef_bzlart:-NONE}" > "$proj_out/elf-fidelity.json"
                    echo "  $name: elf-fidelity -> skip(no-artifact: cmake=$_ef_cmke bazel=$_ef_bzl)" >&2
                fi
            )
            fi
        else
            echo "  $name: elf-fidelity -> skip(no-config)" >&2
        fi
    fi

    # 6th lens: intent-capture net-new count (written by try_bazel_build via
    # intent-capture-lens.sh when SURVEY_INTENT ran). "-" when the lens didn't
    # run. NON-DETERMINISTIC (LLM judge) — a triage pointer, not a comparable
    # metric; the per-finding detail lives in $proj_out/intent-capture.json.
    intent="$proj_out/intent-capture.json"
    missed_n=$( [ -f "$intent" ] && grep -oE '"net_new"[[:space:]]*:[[:space:]]*[0-9]+' "$intent" 2>/dev/null | grep -oE '[0-9]+' | head -1 || echo "-" )
    missed_n="${missed_n:--}"

    printf '%-14s %10s %10s %10s %6s %7s %8s %s\n' "$name" "${rej_n:--}" "${idi_n:--}" "${cov_n:--}" "${todo_n:--}" "$missed_n" "$build_status" "$status" | tee -a "$summary"
done

echo ""
echo "Reports under $out_dir/<project>/{rejections,bazel-idiom,coverage,conversion-todos,intent-capture}.json; summary at $summary"
