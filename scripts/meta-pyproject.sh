#!/bin/sh
# meta-pyproject.sh — render-half acceptance gate for the
# kind:pyproject native render. Mirrors meta-meson.sh's
# structure (single-element fixture + standalone-converter
# assertion + bazel-build half against
# testdata/meta-project/pyproject-greet/).
#
# Drives:
#
#   1. cmd/write-a renders project A (with --convert-element-
#      pyproject set) and project B for the pyproject-greet
#      fixture.
#   2. The per-element BUILD.bazel in project A invokes
#      //tools:convert-element-pyproject against the staged
#      source tree, producing BUILD.bazel.out with py_library
#      + py_binary rules.
#   3. The driver stages project A's BUILD.bazel.out into
#      project B (overwriting the BUILD_NOT_YET_STAGED
#      placeholder).
#   4. project B's bazel-run :greet — verifies the binary
#      prints "Hello from pyproject!".
#
# Bazel-availability gating: rendering checks always run; the
# bazel build phases self-skip when EITHER no bazel >= 7 is on
# PATH OR python3 isn't on PATH.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-pyproject" ./converter/cmd/convert-element-pyproject

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --bst testdata/meta-project/pyproject-greet.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-pyproject "$bin_dir/convert-element-pyproject"

# Render-phase checks. Always run; don't gate on bazel.
for f in MODULE.bazel BUILD.bazel \
        rules/zero_files.bzl rules/BUILD.bazel \
        tools/convert-element-cmake tools/convert-element-pyproject tools/BUILD.bazel \
        elements/pyproject-greet/BUILD.bazel \
        elements/pyproject-greet/sources/pyproject.toml \
        elements/pyproject-greet/sources/src/greet/cli.py; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-pyproject: missing rendered project A file $f" >&2
        exit 1
    fi
done
for marker in \
    '//tools:convert-element-pyproject' \
    '"BUILD.bazel.out"'; do
    if ! grep -qF -- "$marker" "$A/elements/pyproject-greet/BUILD.bazel"; then
        echo "meta-pyproject: A-side BUILD missing marker: $marker" >&2
        cat "$A/elements/pyproject-greet/BUILD.bazel" >&2
        exit 1
    fi
done
# Project B's MODULE.bazel must declare rules_python so the
# emitted py_library / py_binary load() lines resolve.
if ! grep -qF 'bazel_dep(name = "rules_python"' "$B/MODULE.bazel"; then
    echo "meta-pyproject: B-side MODULE.bazel missing rules_python bazel_dep" >&2
    cat "$B/MODULE.bazel" >&2
    exit 1
fi
for f in MODULE.bazel BUILD.bazel \
        elements/pyproject-greet/BUILD.bazel \
        elements/pyproject-greet/pyproject.toml \
        elements/pyproject-greet/src/greet/cli.py; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-pyproject: missing rendered project B file $f" >&2
        exit 1
    fi
done

# Standalone-converter sanity check: invoke convert-element-
# pyproject against the fixture directly and verify py_library
# + py_binary land in the output.
out_build="$work_dir/BUILD.bazel.out"
# Keep stderr on the script's stderr (don't redirect to
# /dev/null) — with `set -e` a failure here exits the gate, and
# the converter's Tier-1 refusal message is the only operator-
# visible signal in the CI log. stdout is suppressed because
# the assertions below grep the on-disk BUILD.bazel.out, not
# the converter's stdout.
"$bin_dir/convert-element-pyproject" \
    --source-root "$repo_root/testdata/meta-project/sources/pyproject-greet" \
    --out-build "$out_build" >/dev/null
for marker in \
    'py_library' \
    'name = "greet_lib"' \
    '"src/greet/__init__.py"' \
    'py_binary' \
    'name = "greet"' \
    'deps = [":greet_lib"]'; do
    if ! grep -qF -- "$marker" "$out_build"; then
        echo "meta-pyproject: standalone convert-element-pyproject output missing $marker" >&2
        cat "$out_build" >&2
        exit 1
    fi
done
echo "meta-pyproject: standalone convert-element-pyproject ok (py_library + py_binary + deps)"

# Bazel phase. Both projects need bazel >= 7 (bzlmod). The
# emitted py_binary needs python3 on the executor; if either is
# missing, the assertion below would either fail to resolve or
# fail to run — self-skip instead.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-pyproject: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
# `$BZL --version` reports the Bazelisk wrapper version (a date)
# when BZL=bazelisk, not the underlying Bazel — `awk '{print $2}'`
# would parse it as non-numeric and force a spurious skip. Use
# `$BZL version` and grep the `Build label:` line, which both
# Bazel and Bazelisk emit in the same shape regardless of the
# fronting wrapper.
bazel_version_label=$("$BZL" version 2>&1 | awk -F': ' '/^Build label:/{print $2; exit}')
bazel_major=$(printf '%s\n' "$bazel_version_label" | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-pyproject: render OK; bazel ${bazel_version_label:-unknown} is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi
if ! command -v python3 >/dev/null; then
    echo "meta-pyproject: render OK; python3 not on PATH, skipping bazel build phase (rules_python's runtime needs it)"
    exit 0
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"

run_bazel() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    # shellcheck disable=SC2086
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

run_bazel_tail() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    n="$1"
    shift
    log="$work_dir/bazel-$cmd-$$.log"
    if run_bazel "$workspace" "$cmd" "$@" >"$log" 2>&1; then
        tail -"$n" "$log"
        return 0
    fi
    rc=$?
    tail -"$n" "$log" >&2
    return "$rc"
}

run_bazel_tail "$A" build 10 //elements/pyproject-greet:pyproject-greet_converted
build_out_a="$A/bazel-bin/elements/pyproject-greet/BUILD.bazel.out"
if [ ! -f "$build_out_a" ]; then
    echo "meta-pyproject: project A's BUILD.bazel.out not produced" >&2
    exit 1
fi
if ! grep -q '^py_library' "$build_out_a"; then
    echo "meta-pyproject: project A's BUILD.bazel.out missing py_library output" >&2
    head -20 "$build_out_a" >&2
    exit 1
fi

cp "$build_out_a" "$B/elements/pyproject-greet/BUILD.bazel"
if grep -q BUILD_NOT_YET_STAGED "$B/elements/pyproject-greet/BUILD.bazel"; then
    echo "meta-pyproject: stage step appears to have failed; placeholder still present" >&2
    exit 1
fi

run_bazel_tail "$B" build 10 //elements/pyproject-greet:greet
# The fixture's :greet py_binary runs `greet.cli:main` which
# prints "Hello from pyproject!". Capture combined output and
# disarm `set -e` around the call so a non-zero rc still lets
# us print the log tail (matches meta-meson.sh's pattern).
greet_log="$work_dir/bazel-greet-run.log"
set +e
run_bazel "$B" run //elements/pyproject-greet:greet >"$greet_log" 2>&1
greet_rc=$?
set -e
if [ "$greet_rc" -ne 0 ]; then
    echo "meta-pyproject: bazel run :greet failed (rc=$greet_rc); last 20 lines:" >&2
    tail -20 "$greet_log" >&2
    exit "$greet_rc"
fi
greet_out=$(tail -5 "$greet_log")
if ! echo "$greet_out" | grep -q "Hello from pyproject!"; then
    echo "meta-pyproject: greet binary did not print expected message" >&2
    echo "$greet_out" >&2
    exit 1
fi
echo "meta-pyproject: round-trip ok (project A built; staged into B; greet binary ran + printed expected message)"
