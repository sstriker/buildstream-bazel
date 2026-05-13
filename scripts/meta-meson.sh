#!/bin/sh
# meta-meson.sh — render-half acceptance gate for the kind:meson
# native render. Mirrors meta-hello.sh's structure (the kind:cmake
# round-trip gate) but against testdata/meta-project/meson-greet/
# and using //tools:convert-element-meson.
#
# Drives:
#
#   1. cmd/write-a renders project A (with --convert-element-meson set)
#      and project B for the meson-greet fixture.
#   2. The per-element BUILD.bazel in project A invokes
#      //tools:convert-element-meson against the staged source tree,
#      producing BUILD.bazel.out + pkg-config-bundle.tar.
#   3. The driver stages project A's BUILD.bazel.out into project B.
#   4. A smoke target (smoke/BUILD.bazel + smoke/smoke.c) depends on
#      the converted cc_library; project B's bazel-build + bazel-run
#      verifies the binary prints "Hello from meson!".
#
# Bazel-availability gating: rendering checks always run; the bazel
# build phases self-skip when EITHER no bazel >= 7 is on PATH OR
# meson isn't on PATH. If meson isn't installed, write-a still
# renders (the meson converter binary is just a binary; it doesn't
# run during write-a), but project A's `bazel build` step depends
# on `meson setup` working inside the genrule so we self-skip there.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-meson" ./converter/cmd/convert-element-meson

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --bst testdata/meta-project/meson-greet.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element "$bin_dir/convert-element" \
    --convert-element-meson "$bin_dir/convert-element-meson"

# Render-phase checks. Always run; don't gate on bazel.
for f in MODULE.bazel BUILD.bazel \
        rules/zero_files.bzl rules/BUILD.bazel \
        tools/convert-element tools/convert-element-meson tools/BUILD.bazel \
        elements/meson-greet/BUILD.bazel \
        elements/meson-greet/sources/meson.build \
        elements/meson-greet/sources/src/greet.c; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-meson: missing rendered project A file $f" >&2
        exit 1
    fi
done
for marker in \
    '//tools:convert-element-meson' \
    '"BUILD.bazel.out"' \
    '"pkg-config-bundle.tar"'; do
    if ! grep -qF -- "$marker" "$A/elements/meson-greet/BUILD.bazel"; then
        echo "meta-meson: A-side BUILD missing marker: $marker" >&2
        cat "$A/elements/meson-greet/BUILD.bazel" >&2
        exit 1
    fi
done
for f in MODULE.bazel BUILD.bazel \
        elements/meson-greet/BUILD.bazel \
        elements/meson-greet/meson.build \
        elements/meson-greet/src/greet.c; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-meson: missing rendered project B file $f" >&2
        exit 1
    fi
done

# Standalone converter sanity check: run convert-element-meson against
# the fixture directly and verify cc_library / cc_binary land in the
# output. Skips when meson isn't on PATH (the binary itself runs but
# `meson setup` needs the host meson installation).
if command -v meson >/dev/null; then
    out_build="$work_dir/BUILD.bazel.out"
    "$bin_dir/convert-element-meson" \
        --source-root "$repo_root/testdata/meta-project/sources/meson-greet" \
        --out-build "$out_build" >/dev/null 2>&1
    for marker in \
        'cc_library' \
        'name = "greet"' \
        'srcs = ["src/greet.c"]' \
        'cc_binary' \
        'name = "greet-bin"' \
        'deps = [":greet"]'; do
        if ! grep -qF -- "$marker" "$out_build"; then
            echo "meta-meson: standalone convert-element-meson output missing $marker" >&2
            cat "$out_build" >&2
            exit 1
        fi
    done
    echo "meta-meson: standalone convert-element-meson ok (cc_library + cc_binary + deps)"
else
    echo "meta-meson: render OK; meson not on PATH, skipping standalone converter check"
fi

# Bazel phase. Both projects need bazel >= 7 (bzlmod). If only an
# older bazel (or none) is available, the rendering checks above are
# the only assertions. Even with bazel present, the genrule that runs
# convert-element-meson needs `meson` on the executor's PATH; if meson
# isn't installed locally, the genrule will fail. Self-skip in that
# case too.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-meson: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-meson: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi
if ! command -v meson >/dev/null; then
    echo "meta-meson: render OK; meson not on PATH, skipping bazel build phase (the genrule needs it on the executor)"
    exit 0
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"
sha_of() { sha256sum "$1" | cut -d' ' -f1; }

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

# run_bazel_tail captures bazel's combined stdout+stderr to a temp
# file, then prints the last N lines. Crucially, it preserves
# bazel's exit status — `run_bazel ... | tail` would have masked
# it because under /bin/sh (often dash) the pipeline's exit
# status is the last command's, and `tail` virtually always
# succeeds. With `set -e` active (this script's mode), masking
# bazel failures was making gate breakage hard to spot.
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

run_bazel_tail "$A" build 10 //elements/meson-greet:meson-greet_converted
build_out_a="$A/bazel-bin/elements/meson-greet/BUILD.bazel.out"
if [ ! -f "$build_out_a" ]; then
    echo "meta-meson: project A's BUILD.bazel.out not produced" >&2
    exit 1
fi
if ! grep -q '^cc_library' "$build_out_a"; then
    echo "meta-meson: project A's BUILD.bazel.out missing cc_library output" >&2
    head -20 "$build_out_a" >&2
    exit 1
fi
echo "meta-meson: project A built; BUILD.bazel.out sha=$(sha_of "$build_out_a")"

cp "$build_out_a" "$B/elements/meson-greet/BUILD.bazel"
if grep -q BUILD_NOT_YET_STAGED "$B/elements/meson-greet/BUILD.bazel"; then
    echo "meta-meson: stage step appears to have failed; placeholder still present" >&2
    exit 1
fi

mkdir -p "$B/smoke"
cat > "$B/smoke/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "greet_smoke",
    srcs = ["smoke.c"],
    deps = ["//elements/meson-greet:greet"],
)
EOF
cat > "$B/smoke/smoke.c" <<'EOF'
#include <stdio.h>
#include "greet.h"

int main(void) {
    printf("%s\n", greet_message());
    return 0;
}
EOF

run_bazel_tail "$B" build 10 //smoke:greet_smoke
# `bazel run` similarly: capture to a log, but disarm `set -e` for
# the call so a non-zero rc still lets us print the log tail
# (otherwise the script would exit silently and the smoke-target
# breakage would be hard to diagnose). Mirrors run_bazel_tail's
# pattern but keeps the captured stdout available for the smoke
# assertion below.
smoke_log="$work_dir/bazel-smoke-run.log"
set +e
run_bazel "$B" run //smoke:greet_smoke >"$smoke_log" 2>&1
smoke_rc=$?
set -e
if [ "$smoke_rc" -ne 0 ]; then
    echo "meta-meson: bazel run //smoke:greet_smoke failed (rc=$smoke_rc); last 20 lines:" >&2
    tail -20 "$smoke_log" >&2
    exit "$smoke_rc"
fi
smoke_out=$(tail -5 "$smoke_log")
echo "meta-meson: smoke output: $smoke_out"
if ! echo "$smoke_out" | grep -q "Hello from meson!"; then
    echo "meta-meson: smoke binary did not print expected message" >&2
    echo "$smoke_out" >&2
    exit 1
fi
echo "meta-meson: round-trip ok (project A built; staged into B; smoke binary linked + ran)"
