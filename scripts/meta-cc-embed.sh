#!/bin/sh
# meta-cc-embed.sh — render+build gate for the rules_buildstream_bazel
# `cc_embed` rule + cc-embed tool (docs/research/codegen-idiom-coverage.md).
#
# The Bazel-native lowering of the "embed a file as a C array" cmake -P
# idiom (vtkEncodeString). This gate stages a workspace that:
#   - wraps the cc-embed Go binary as //tools:cc-embed (native_binary),
#   - runs cc_embed over a data file to produce a .h + .cxx,
#   - links them into a C++ binary that prints the embedded symbol,
# then bazel-runs it and asserts the embedded symbol's RUNTIME VALUE equals
# the input file's bytes (the faithfulness contract — no cmake involved).
#
# Pure Bazel rule (no cmake): the gate guards only on bazel >= 9.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "skip: bazel not on PATH"
    exit 0
fi
# `$BZL --version` can report the Bazelisk wrapper's own version (a date)
# rather than the underlying bazel; parse the `Build label:` line from
# `$BZL version` instead, which is the real bazel version for both bazel
# and bazelisk (same approach as scripts/meta-pyproject.sh).
bazel_version_label=$("$BZL" version 2>&1 | awk -F': ' '/^Build label:/{print $2; exit}')
bazel_major=$(printf '%s\n' "$bazel_version_label" | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "skip: bazel ${bazel_version_label:-unknown} < 9 (Bazel 9 floor: bzlmod + load() for cc_*)"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -C "$repo_root" -o "$bin_dir/cc-embed" ./cmd/cc-embed

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT
ws="$work_dir/ws"
mkdir -p "$ws/tools"

# Stage the cc-embed tool as //tools:cc-embed (native_binary, mirroring how
# write-a / run-fidelity stage //tools:cmake-configure-file).
cp "$bin_dir/cc-embed" "$ws/tools/cc-embed.bin"
chmod 0755 "$ws/tools/cc-embed.bin"
cat > "$ws/tools/BUILD.bazel" <<'EOF'
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "cc-embed",
    src = "cc-embed.bin",
    out = "cc-embed",
    visibility = ["//visibility:public"],
)
EOF

cat > "$ws/MODULE.bazel" <<EOF
module(name = "ccembedgate", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(module_name = "rules_buildstream_bazel", path = "$repo_root/rules_buildstream_bazel")
EOF

# Distinctive payload incl. characters the escaping must handle.
printf 'hello "embedded" world\nsecond line\tand a tab\n' > "$ws/data.txt"

cat > "$ws/check.cc" <<'EOF'
#include <cstring>
#include "embedded_data.h"

// The expected bytes, written independently of cc_embed's escaping.
static const char *expected =
    "hello \"embedded\" world\n"
    "second line\tand a tab\n";

int main() {
    return std::strcmp(embedded_data, expected) == 0 ? 0 : 1;
}
EOF

cat > "$ws/BUILD.bazel" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")
load("@rules_buildstream_bazel//rules:cc_embed.bzl", "cc_embed")

cc_embed(
    name = "embedded_data_gen",
    src = "data.txt",
    symbol = "embedded_data",
    out_header = "embedded_data.h",
    out_source = "embedded_data.cxx",
    tool = "//tools:cc-embed",
)

cc_library(
    name = "embedded_data",
    hdrs = ["embedded_data.h"],
    srcs = ["embedded_data.cxx"],
)

cc_binary(
    name = "check",
    srcs = ["check.cc"],
    deps = [":embedded_data"],
)
EOF

bzl_cache="$work_dir/.bazel"
# shellcheck disable=SC2086
if ! (cd "$ws" && "$BZL" --output_user_root="$bzl_cache" ${META_BAZEL_STARTUP_ARGS:-} \
        run ${META_BAZEL_BUILD_ARGS:-} //:check) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: cc_embed round-trip binary did not run clean (embedded value != input?)"
    sed 's/^/   /' "$work_dir/bazel.log"
    exit 1
fi

echo "ok  meta-cc-embed: cc_embed embeds the file; the linked binary's symbol value matches the input (no cmake)"
