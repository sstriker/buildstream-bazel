#!/bin/sh
# meta-hello.sh — Phase 1 acceptance gate for the meta-project shape.
#
# Drives the full two-pass pipeline against the hello-world fixture:
#
#   1. cmd/write-a renders project A (the meta workspace) and project B
#      (the consumer workspace).
#   2. bazel build in project A invokes convert-element-cmake via the per-
#      element genrule, producing BUILD.bazel.out + cmake-config-bundle.
#   3. The driver stages project A's BUILD.bazel.out into project B's
#      elements/<name>/BUILD.bazel (overwriting the
#      BUILD_NOT_YET_STAGED placeholder write-a rendered).
#   4. The driver writes a hand-authored smoke target into project B
#      (smoke/BUILD.bazel + smoke/smoke.c) that depends on the
#      converted cc_library and prints the library's hello message.
#   5. bazel build + bazel run in project B compiles and executes the
#      smoke binary; output is asserted to contain "Hello, World!".
#
# Cache-stability scenarios A and B are exercised inline after the
# initial round-trip succeeds, asserting both project A's conversion
# stability AND project B's downstream rebuild behavior.
#
# Bazel-availability gating: rendering checks always run; bazel build
# phases self-skip when no bazel >= 7 is on PATH (via META_BAZEL_*_ARGS
# documented below).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

# Build prereqs.
make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/hello-world.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks. Always run; don't gate on bazel.
for f in MODULE.bazel BUILD.bazel \
        tools/convert-element-cmake tools/BUILD.bazel \
        elements/hello-world/BUILD.bazel \
        elements/hello-world/sources/CMakeLists.txt; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-hello: missing rendered project A file $f" >&2
        exit 1
    fi
done
# rules/ no longer renders into the project — loads from
# @rules_buildstream_bazel//rules via bazel_dep + local_path_override.
for f in rules/zero_files.bzl rules/sources.bzl rules/BUILD.bazel; do
    if [ -f "$A/$f" ]; then
        echo "meta-hello: project A unexpectedly rendered $f (loads from @rules_buildstream_bazel//rules)" >&2
        exit 1
    fi
done
for f in MODULE.bazel BUILD.bazel \
        elements/hello-world/BUILD.bazel \
        elements/hello-world/CMakeLists.txt \
        elements/hello-world/hello.c \
        elements/hello-world/include/hello.h; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-hello: missing rendered project B file $f" >&2
        exit 1
    fi
done

# Bazel phase. Both projects need bazel >= 7 (bzlmod). If only an
# older bazel (or none) is available, the rendering check above is
# the only assertion.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-hello: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-hello: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi

# Bazel-build half execs convert-element-cmake which shells to
# cmake + ninja under bwrap. Skip cleanly if any are missing —
# the render assertions above are still meaningful in isolation.
for tool in cmake ninja bwrap; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-hello: render OK; $tool not on PATH, skipping build phase"
        exit 0
    fi
done

# META_BAZEL_STARTUP_ARGS / META_BAZEL_BUILD_ARGS let sandboxed dev
# environments inject overrides for bcr.bazel.build access (proxy
# whitelists, JVM truststore paths, alternative registries). Empty by
# default; on a normal dev machine bazel reaches bcr fine and needs
# nothing extra.
#
# Bazel splits flags into startup (between `bazel` and the
# subcommand) and command-time (after the subcommand) — they're
# rejected in the wrong position, so the script accepts both
# separately. Example for dev containers without bcr egress but with
# github:
#
#   export META_BAZEL_STARTUP_ARGS="\
#     --host_jvm_args=-Djavax.net.ssl.trustStore=/etc/ssl/certs/java/cacerts \
#     --host_jvm_args=-Djavax.net.ssl.trustStorePassword=changeit"
#   export META_BAZEL_BUILD_ARGS="\
#     --registry=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main"
META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"
sha_of() { sha256sum "$1" | cut -d' ' -f1; }

# Run a bazel subcommand inside the chosen workspace. Both projects
# share an output_user_root so module deps resolve once across the
# two passes.
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

# === Pass 1: bazel build project A ===
run_bazel "$A" build //elements/hello-world:hello-world_converted 2>&1 | tail -10
build_out_a="$A/bazel-bin/elements/hello-world/BUILD.bazel.out"
if [ ! -f "$build_out_a" ]; then
    echo "meta-hello: project A's BUILD.bazel.out not produced" >&2
    exit 1
fi
if ! grep -q '^cc_library' "$build_out_a"; then
    echo "meta-hello: project A's BUILD.bazel.out missing cc_library output" >&2
    head -20 "$build_out_a" >&2
    exit 1
fi
sha_a1=$(sha_of "$build_out_a")
echo "meta-hello: project A built; BUILD.bazel.out sha=$sha_a1"

# === Stage A's outputs into B ===
# Replace the BUILD_NOT_YET_STAGED placeholder with the converter's
# real BUILD.bazel.out. After this step, project B's element package
# is well-formed and Bazel can resolve cc_library against the staged
# sources.
cp "$build_out_a" "$B/elements/hello-world/BUILD.bazel"
if grep -q BUILD_NOT_YET_STAGED "$B/elements/hello-world/BUILD.bazel"; then
    echo "meta-hello: stage step appears to have failed; placeholder still present" >&2
    exit 1
fi

# === Smoke target: cc_binary linking against the converted cc_library ===
# Hand-written, fixture-specific. Production graphs have their own
# consumers; this is the Phase 1 acceptance gate's notion of "project
# B actually compiles project A's converted output".
mkdir -p "$B/smoke"
cat > "$B/smoke/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "hello_smoke",
    srcs = ["smoke.c"],
    deps = ["//elements/hello-world:hello"],
)
EOF
cat > "$B/smoke/smoke.c" <<'EOF'
#include <stdio.h>
#include "hello.h"

int main(void) {
    printf("%s\n", hello_message());
    return 0;
}
EOF

# === Pass 2: bazel build + run project B ===
run_bazel "$B" build //smoke:hello_smoke 2>&1 | tail -10
smoke_out=$(run_bazel "$B" run //smoke:hello_smoke 2>&1 | tail -5)
echo "meta-hello: smoke output: $smoke_out"
if ! echo "$smoke_out" | grep -q "Hello, World!"; then
    echo "meta-hello: smoke binary did not print 'Hello, World!'" >&2
    echo "$smoke_out" >&2
    exit 1
fi
echo "meta-hello: round-trip ok (project A built; staged into B; smoke binary linked + ran)"
sha_smoke_initial=$(sha_of "$B/bazel-bin/smoke/hello_smoke")

# === Cache-stability scenarios through both projects ===
#
# The meta-project shape's two cache-stability claims:
#
#   Scenario A (edit hello.c — NOT in cmake's read set):
#     - Project A: convert-element-cmake cache-hits (zero_files makes the
#       genrule's input merkle stable across hello.c content changes).
#     - Project B: cc_library recompiles, since hello.c is in its
#       srcs. Smoke binary recompiles. This is expected; the win is
#       that A's conversion didn't re-run, so any sibling elements
#       that depend on hello-world's exports also wouldn't rebuild
#       (no sibling here, so the check is just A's stability).
#
#   Scenario B (edit CMakeLists.txt comment — IS in the read set):
#     - Project A: convert-element-cmake re-runs (CMakeLists is real),
#       but produces byte-identical output (cmake's parser strips
#       comments before the codemodel).
#     - Project B: cc_library + smoke do NOT recompile. CMakeLists
#       isn't in any cc_* rule's srcs, and the staged BUILD.bazel
#       (from A) is byte-identical, so the action keys downstream
#       are unchanged. Strictly stronger than today's orchestrator
#       semantics where any source-tree edit re-ran convert-element-cmake
#       AND triggered downstream rebuilds.
echo
echo "=== cache-stability: setting up writable source tree ==="

feedback="$work_dir/feedback-read-paths.json"
cp "$A/bazel-bin/elements/hello-world/read_paths.json" "$feedback"

# Editable source tree the scenarios will mutate.
edit_src="$work_dir/edit-src"
cp -r testdata/meta-project/sources/hello-world/. "$edit_src"
edit_bst="$work_dir/edit.bst"
cat > "$edit_bst" <<EOF
kind: cmake
sources:
- kind: local
  path: $edit_src
EOF

# Each scenario re-renders both projects from the editable tree.
# write-a wipes project B's elements/<name>/ on each run; the smoke
# target lives outside that path and survives, but the per-element
# BUILD must be re-staged from project A's bazel-bin output.
restage_b() {
    cp "$build_out_a" "$B/elements/hello-world/BUILD.bazel"
    if grep -q BUILD_NOT_YET_STAGED "$B/elements/hello-world/BUILD.bazel"; then
        echo "meta-hello: re-stage failed; placeholder still present" >&2
        exit 1
    fi
}

rerender_with_feedback() {
    "$bin_dir/write-a" \
        --rules-package-path "$repo_root/rules_buildstream_bazel" \
        --bst "$edit_bst" \
        --out "$A" \
        --out-b "$B" \
        --convert-element-cmake "$bin_dir/convert-element-cmake" \
        --read-paths-feedback "$feedback" >/dev/null
}

# Narrowing transition: re-render with feedback and confirm project A
# still produces the same BUILD.bazel.out (zero_files-based shape
# doesn't shift the converter's view).
rerender_with_feedback
run_bazel "$A" build //elements/hello-world:hello-world_converted 2>&1 | tail -3
sha_a_narrowed=$(sha_of "$build_out_a")
if [ "$sha_a_narrowed" != "$sha_a1" ]; then
    echo "meta-hello: narrowing transition shifted A's BUILD.bazel.out" >&2
    echo "  pre-narrow: $sha_a1" >&2
    echo "  narrowed:   $sha_a_narrowed" >&2
    exit 1
fi
restage_b
echo "meta-hello: narrowed-mode A build sha matches initial run"

echo
echo "=== Scenario A: edit hello.c (NOT in read set) ==="
echo "// scenario-A test edit" >> "$edit_src/hello.c"
rerender_with_feedback
scen_a_log=$(run_bazel "$A" build //elements/hello-world:hello-world_converted 2>&1 | tail -3)
sha_a_scen_a=$(sha_of "$build_out_a")
if [ "$sha_a_scen_a" != "$sha_a_narrowed" ]; then
    echo "Scenario A FAILED: A's BUILD.bazel.out shifted after hello.c edit" >&2
    exit 1
fi
echo "Scenario A: A's BUILD.bazel.out byte-identical"
# Soft check: bazel's "X processes" line on a cache-only run reports
# only internals. Different bazel versions format slightly; print a
# diagnostic but don't fail on shape mismatch.
if echo "$scen_a_log" | grep -q '1 process: 1 internal'; then
    echo "Scenario A: project A reports cache-only (no action ran)"
else
    echo "Scenario A: project A bazel summary: $scen_a_log"
fi
restage_b
# Project B's behavior on Scenario A is incidental to the gate: the
# *appended C-comment* doesn't change the compiled output regardless
# of whether bazel decides to rebuild. The check here is the
# functional invariant — the smoke binary still links + still prints
# "Hello, World!". The win for the meta-project shape is the A-side
# claim above (conversion didn't re-run).
run_bazel "$B" build //smoke:hello_smoke 2>&1 | tail -3
smoke_out=$(run_bazel "$B" run //smoke:hello_smoke 2>&1 | tail -3)
if ! echo "$smoke_out" | grep -q "Hello, World!"; then
    echo "Scenario A FAILED: B's smoke output broken after hello.c edit" >&2
    echo "$smoke_out" >&2
    exit 1
fi
echo "Scenario A: B's smoke binary still prints Hello, World!"

echo
echo "=== Scenario B: edit CMakeLists.txt comment (IS in read set) ==="
echo "# scenario-B comment $(date +%s)" >> "$edit_src/CMakeLists.txt"
rerender_with_feedback
sha_smoke_before_aprime=$(sha_of "$B/bazel-bin/smoke/hello_smoke")
run_bazel "$A" build //elements/hello-world:hello-world_converted 2>&1 | tail -3
sha_a_aprime=$(sha_of "$build_out_a")
if [ "$sha_a_aprime" != "$sha_a_narrowed" ]; then
    echo "Scenario B FAILED: A's BUILD.bazel.out shifted after comment edit" >&2
    echo "  expected: $sha_a_narrowed" >&2
    echo "  got:      $sha_a_aprime" >&2
    exit 1
fi
echo "Scenario B: A's BUILD.bazel.out byte-identical (cmake parser strips comments)"
restage_b
# Project B should NOT rebuild: nothing in any cc_* rule's srcs/deps
# changed, and the staged BUILD.bazel is byte-identical.
run_bazel "$B" build //smoke:hello_smoke 2>&1 | tail -3
sha_smoke_after_aprime=$(sha_of "$B/bazel-bin/smoke/hello_smoke")
if [ "$sha_smoke_before_aprime" != "$sha_smoke_after_aprime" ]; then
    echo "Scenario B FAILED: B's smoke binary changed after comment edit" >&2
    echo "  before: $sha_smoke_before_aprime" >&2
    echo "  after:  $sha_smoke_after_aprime" >&2
    exit 1
fi
echo "Scenario B: B's smoke binary sha unchanged (no rebuild)"

echo
echo "meta-hello: ok (round-trip + scenarios A and B validated through both projects)"
