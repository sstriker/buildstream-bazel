#!/bin/sh
# run-survey.sh — drive the diagnostic-survey corpus through the cmake
# converter and emit a per-project rejection + bazel-idiom report.
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

# Locate the converter: prefer the Makefile-built binary, fall back to
# `go run` so the script works in a bare checkout.
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
converter="$repo_root/build/bin/convert-element-cmake"
if [ -x "$converter" ]; then
    run_converter() { "$converter" "$@"; }
else
    echo "note: $converter not built; using 'go run' (slower). Run 'make converter' to speed up." >&2
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
    if ! cp -a "$_bb_src/." "$_bb_elt/" 2>"$_bb_po/build.log"; then
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
    # Per-project cmake configure options that drive the project's OWN build
    # flags at configure time (so the codemodel — and thus the emitted BUILD —
    # reflects them). A project's warnings-as-errors policy is noise for a "does
    # it build" check — the lens tests buildability, and it already opts in to
    # install-export config generation in the same spirit.
    #
    # glm: its test/CMakeLists.txt adds `-Werror` (gcc) / `-Werror -Weverything`
    # (clang), and under GCC 13 that trips on -Wclass-memaccess in glm's own
    # gtc/packing.inl (memcpy over its packed vector types — intentional, not a
    # bug). We can't reach for glm's GLM_DISABLE_AUTO_DETECTION knob: it does
    # drop the test -Werror, but it also forces GLM_FORCE_CXX_UNKNOWN, which
    # zeroes GLM_LANG and so disables glm's C++11 std::hash specializations —
    # breaking the gtx_hash tests with deleted-function errors. Instead pass
    # CMAKE_CXX_FLAGS=-w: it inhibits all warnings (so -Werror has nothing to
    # promote, order-independently) while leaving C++ auto-detection intact, so
    # the full test suite — hash included — compiles.
    # Build the converter argv in the positional params so every argument is
    # passed atomically: a --cmake-define value may carry spaces
    # (CMAKE_<LANG>_FLAGS commonly does), which an unquoted "$var" expansion
    # would word-split. (try_bazel_build saved its own args to _bb_* above, so
    # reusing "$@" here is safe; _bb_bt / _bb_sp are single tokens or empty.)
    set -- --source-root "$_bb_src"
    [ -n "$_bb_bt" ] && set -- "$@" "$_bb_bt"
    [ -n "$_bb_sp" ] && set -- "$@" "$_bb_sp"
    case "$_bb_name" in
        glm) set -- "$@" --cmake-define "CMAKE_CXX_FLAGS=-w" ;;
    esac
    # --emit-install-export-config: the build lens is the one place that opts in
    # to generating the install(EXPORT) config-mode bundle (the real
    # <Pkg>Targets.cmake + cmake_config_bundle filegroup). Default converts omit
    # it — the orchestrated graph wires its own synthprefix-synthesized bundle —
    # but the lens generates the real file so `bazel build //...` exercises the
    # bundle end-to-end rather than choking on a filegroup over not-on-disk files.
    set -- "$@" \
        --bazel-package-path "$_bb_pkg" \
        --emit-install-export-config \
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
bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(module_name = "rules_buildstream_bazel", path = "$repo_root/rules_buildstream_bazel")
EOF
    _bb_to=""
    command -v timeout >/dev/null 2>&1 && _bb_to="timeout ${SURVEY_BAZEL_BUILD_TIMEOUT:-900}"
    # --noworkspace_rc: the lens measures whether OUR emitted module/build graph
    # builds, so ignore any workspace .bazelrc (matches the repo's other survey
    # scripts, and is belt-and-suspenders with the .bazelrc strip above). Thread
    # both startup-arg and build-arg passthrough too (META_BAZEL_STARTUP_ARGS
    # goes before the subcommand — registry tweaks for sandboxed/offline runs).
    if ( cd "$_bb_ws" && $_bb_to "$bzl_bin" --output_user_root="$_bb_po/.bzcache" \
            --noworkspace_rc ${META_BAZEL_STARTUP_ARGS:-} build ${META_BAZEL_BUILD_ARGS:-} //... ) >> "$_bb_po/build.log" 2>&1; then
        echo "ok"
    else
        echo "FAIL"
    fi
}
# --------------------------------------------------------------------------

mkdir -p "$out_dir"
summary="$out_dir/summary.txt"
: > "$summary"

printf '%-14s %10s %10s %10s %8s %s\n' project rejections idioms coverage build status | tee "$summary"
printf '%-14s %10s %10s %10s %8s %s\n' ------- ---------- ------ -------- ----- ------ | tee -a "$summary"

for entry in $projects; do
    name="${entry%%=*}"
    src="${entry#*=}"
    proj_out="$out_dir/$name"
    mkdir -p "$proj_out"

    if [ ! -d "$src" ]; then
        printf '%-14s %10s %10s %10s %8s %s\n' "$name" - - - - "MISSING ($src) — run 'make fetch-$name'" | tee -a "$summary"
        continue
    fi

    rej="$proj_out/rejections.json"
    idiom="$proj_out/bazel-idiom.json"
    cov="$proj_out/coverage.json"
    status="ok"

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
    if [ -n "$split_packages" ]; then
        sp_args="--split-packages"
    fi

    if ! run_converter \
        --source-root "$src" \
        --diagnostics \
        $bt_args \
        $sp_args \
        --rejections-report "$rej" \
        --audit-bazel-idiom-report "$idiom" \
        --audit-coverage-report "$cov" \
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

    # The build-lens skip(rej) decision must ignore BENIGN diagnostics that
    # don't actually block a clean (strict-mode) convert: the
    # "...doesn't exist on disk; treated as empty" unsupported-source-path
    # notice (forward-declared / genex include dirs) is recorded only in
    # diagnostics mode and is a no-op in strict mode -- it never aborts the
    # convert, so counting it falsely skipped projects whose clean convert
    # succeeds (glog). rej_blocking subtracts that benign class; rej_n stays
    # the raw diagnostic count shown in the column.
    rej_benign=$( [ -f "$rej" ] && grep -c 'treated as empty' "$rej" 2>/dev/null )
    rej_benign=${rej_benign:-0}
    if [ "$rej_n" != "-" ]; then
        rej_blocking=$((rej_n - rej_benign))
    else
        rej_blocking="-"
    fi

    # 4th lens: `bazel build //...` of the faithful (project-B-shaped) output,
    # only when this project is selected by SURVEY_BAZEL_BUILD. Short-circuit
    # the cases that can't (or shouldn't) build before paying for a clean
    # convert: a hard-failed diagnostic convert (configure must work first),
    # and a project that surveys with rejections (the lens contract is to skip
    # refusals — the clean convert would just abort on the first one).
    build_status="-"
    if build_lens_for "$name"; then
        if [ "$status" != "ok" ]; then
            build_status="skip(convert)"
        elif [ -n "$rej_blocking" ] && [ "$rej_blocking" != "0" ] && [ "$rej_blocking" != "-" ]; then
            build_status="skip(rej)"
        else
            build_status="$(try_bazel_build "$name" "$src" "$proj_out" "$bt_args" "$sp_args")"
            [ "$build_status" = "FAIL" ] && status="bazel build //... FAILED — see $proj_out/build.log"
        fi
    fi

    printf '%-14s %10s %10s %10s %8s %s\n' "$name" "${rej_n:--}" "${idi_n:--}" "${cov_n:--}" "$build_status" "$status" | tee -a "$summary"
done

echo ""
echo "Reports under $out_dir/<project>/{rejections,bazel-idiom,coverage}.json; summary at $summary"
