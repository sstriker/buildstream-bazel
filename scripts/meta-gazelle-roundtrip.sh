#!/bin/sh
# meta-gazelle-roundtrip.sh — Phase 7c acceptance gate for the
# gazelle-roundtrip story.
#
# Drives the full pipeline against the hello-world fixture, then
# exercises the post-conversion gazelle-prep contract:
#
#   1. write-a + bazel-build pass (same as scripts/meta-hello.sh)
#      produces a staged project B.
#   2. build-cc-index walks project B's elements/ to populate
#      tools/cc_index.json + tools/python_modules.json from the
#      emitted cc_library hdrs (plus the .h-in-srcs cheap
#      mitigation) and any py_library / py_binary names.
#   3. Assertions on the populated indexes — the known hello-
#      world headers must resolve to the expected labels.
#   4. (Optional) Run buildifier --mode=fix against project B and
#      assert no diff. Skipped if buildifier isn't on PATH.
#   5. (Optional) Add a smoke source file with `#include
#      "hello.h"` to a new package in project B; run gazelle and
#      assert the resulting BUILD's deps resolves the dep to
#      //elements/hello-world. Skipped if gazelle isn't reachable
#      (requires both `bazel` and the gazelle_cc bzlmod
#      registration in the rendered MODULE.bazel — Phase 7c may
#      not yet have added that).
#
# Run from repo root: scripts/meta-gazelle-roundtrip.sh

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/build-cc-index" ./cmd/build-cc-index
CGO_ENABLED=0 go build -o "$bin_dir/relax-keeps" ./cmd/relax-keeps

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --bst testdata/meta-project/hello-world.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element"

# Validate the Phase 7b/8 stub files write-a emitted exist.
# - cc_index.json + python_modules.json start as `{}` (Phase 7c
#   rewrites them).
# - gazelle-rewritable.json starts as a comment + empty patterns
#   list (Phase 8 — operator-edited to declare rewritable
#   genrule patterns).
for f in tools/cc_index.json tools/python_modules.json tools/gazelle-rewritable.json; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-gazelle-roundtrip: Phase 7b/8 stub $f missing in project B" >&2
        exit 1
    fi
done
# overlay.MODULE.bazel at project B root (Phase 8 operator-owned
# overlay loaded by MODULE.bazel's include()).
if [ ! -f "$B/overlay.MODULE.bazel" ]; then
    echo "meta-gazelle-roundtrip: Phase 8 overlay.MODULE.bazel missing in project B" >&2
    exit 1
fi

# Bazel phase: same gating as meta-hello.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-gazelle-roundtrip: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
# Strict-numeric check: bazel --version sometimes prefixes
# its output with timestamps or wrapper banners on CI runners
# (bazelisk under `set -x` shows lines like `[08:30:42]`); the
# `awk $2` extraction can pick those up, and a `cut -d.` of a
# non-version string yields garbage. Require bazel_major to be
# all digits before doing arithmetic — otherwise treat as
# "version unknown, skip build phase" rather than letting the
# subsequent [ ... -lt 9 ] error out with "Illegal number".
case "$bazel_major" in
    ''|*[!0-9]*) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-gazelle-roundtrip: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# === Project A build (produces BUILD.bazel.out) ===
run_bazel "$A" build //elements/hello-world:hello-world_converted 2>&1 | tail -5
build_out_a="$A/bazel-bin/elements/hello-world/BUILD.bazel.out"
if [ ! -f "$build_out_a" ]; then
    echo "meta-gazelle-roundtrip: BUILD.bazel.out not produced" >&2
    exit 1
fi

# === Stage A's output into B ===
cp "$build_out_a" "$B/elements/hello-world/BUILD.bazel"

# === Populate cc_index.json + python_modules.json ===
"$bin_dir/build-cc-index" \
    --root "$B" \
    --out-cc-index tools/cc_index.json \
    --out-python-modules tools/python_modules.json

# Assertions: hello-world's exported header must map to the
# canonical label. Phase 3's label shortening (//pkg:pkg → //pkg)
# applies only when the cc_library's target name equals the
# package basename. The hello-world fixture's CMakeLists defines
# `add_library(hello ...)` — target "hello" in package
# "elements/hello-world" — so basename ("hello-world") differs
# from target name ("hello") and the canonical form keeps the
# `:hello` tail.
expected_header="elements/hello-world/include/hello.h"
expected_label="//elements/hello-world:hello"
if ! grep -qE "\"$expected_header\":[[:space:]]*\"$expected_label\"" "$B/tools/cc_index.json"; then
    echo "meta-gazelle-roundtrip: cc_index.json missing $expected_header → $expected_label mapping" >&2
    cat "$B/tools/cc_index.json" >&2
    exit 1
fi
echo "meta-gazelle-roundtrip: cc_index.json populated; hello.h → $expected_label"

# === relax-keeps with default empty patterns is a no-op ===
# Sanity check: continuous-conversion loops invoke relax-keeps
# unconditionally; the empty default in tools/gazelle-rewritable.json
# must produce no BUILD changes.
build_before=$(cat "$B/elements/hello-world/BUILD.bazel")
"$bin_dir/relax-keeps" --root "$B" elements/hello-world
build_after=$(cat "$B/elements/hello-world/BUILD.bazel")
if [ "$build_before" != "$build_after" ]; then
    echo "meta-gazelle-roundtrip: relax-keeps modified BUILD with default empty patterns:" >&2
    diff <(echo "$build_before") <(echo "$build_after") >&2 || true
    exit 1
fi
echo "meta-gazelle-roundtrip: relax-keeps (empty patterns) is a no-op as expected"

# === Optional: buildifier no-op contract ===
# Phase 3 promised buildifier --mode=fix is a no-op against our
# emit. Verify that's still true after Phase 7a's # keep markers
# + Phase 7b's directives + Phase 7c's populated indexes.
if command -v buildifier >/dev/null; then
    # Run against the per-element + tools BUILD.bazel files.
    # Capture diffs (--mode=diff is non-destructive; exit 4 = diffs
    # found, exit 0 = clean, other = bug).
    bf_out=$(buildifier --mode=diff -r "$B" 2>&1) || bf_rc=$?
    bf_rc=${bf_rc:-0}
    if [ "$bf_rc" = 4 ]; then
        echo "meta-gazelle-roundtrip: buildifier --mode=diff reports changes (Phase 3 no-op contract violated):" >&2
        echo "$bf_out" >&2
        exit 1
    fi
    if [ "$bf_rc" != 0 ]; then
        echo "meta-gazelle-roundtrip: buildifier failed with exit $bf_rc:" >&2
        echo "$bf_out" >&2
        exit 1
    fi
    echo "meta-gazelle-roundtrip: buildifier --mode=diff clean (Phase 3 contract preserved)"
else
    echo "meta-gazelle-roundtrip: buildifier not on PATH; skipping no-op assertion"
fi

# === Gazelle resolution smoke (deferred wiring) ===
# Running actual gazelle requires gazelle_cc to be declared in
# project B's MODULE.bazel (it isn't yet — Phase 7c ships the
# metadata files and the directives that reference them; landing
# the bazel_dep is left for the operator or a follow-up that
# bumps the bcr-pinned gazelle_cc version once it's published
# there). The cc_index.json content + MODULE.bazel directives
# above are the contract this gate enforces today.
echo "meta-gazelle-roundtrip: ok (write-a render + bazel-build + build-cc-index + buildifier no-op)"
