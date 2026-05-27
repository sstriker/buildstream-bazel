#!/bin/sh
# run-fidelity.sh — drive a full convert-and-rebuild fidelity
# comparison for one fixture.
#
# Steps, all in a fresh scratch tree:
#   1. cmake configure + build the project (the oracle, project C in
#      docs/known-deltas.md's A-B-C harness terminology).
#   2. convert-element-cmake against the same build dir → produce
#      BUILD.bazel.
#   3. Stage a Bazel workspace: the project sources + the converted
#      BUILD.bazel + a minimal WORKSPACE that vendors rules_cc.
#      bazel build the same target.
#   4. Run cmd/fidelity-compare against the two artifacts. Report
#      benign / impactful deltas and exit non-zero on any impactful.
#
# Bazel-availability gating: the cmake configure + convert + classifier
# half always runs. The bazel build half self-skips when bazel is not
# on PATH OR when the rules_cc tarball isn't pre-staged at
# $RULES_CC_TARBALL (CI runners that vendor it expose the path;
# operators without it see a "skipped (no bazel)" line and exit 0
# on the configure + convert half).
#
# Usage:
#   scripts/run-fidelity.sh \
#       --project-name <name> \
#       --source-root <abs-path-to-cmake-source-root> \
#       --target <cmake-and-bazel-target-name> \
#       --artifact-pattern <e.g. libfoo.a> \
#       [--allowlist <abs-path>] \
#       [--cmake-flags '-DFOO=BAR ...'] \
#       [--bazel-target-label '//:foo']

set -eu

# CLI parsing.
project_name=""
source_root=""
target=""
artifact_pattern=""
allowlist=""
cmake_flags=""
bazel_target_label=""

usage() {
    echo "usage: $0 --project-name <name> --source-root <abs> --target <name>" >&2
    echo "          --artifact-pattern <libfoo.a> [--allowlist <abs>]" >&2
    echo "          [--cmake-flags '...'] [--bazel-target-label '//:foo']" >&2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --project-name) project_name="$2"; shift 2 ;;
        --source-root) source_root="$2"; shift 2 ;;
        --target) target="$2"; shift 2 ;;
        --artifact-pattern) artifact_pattern="$2"; shift 2 ;;
        --allowlist) allowlist="$2"; shift 2 ;;
        --cmake-flags) cmake_flags="$2"; shift 2 ;;
        --bazel-target-label) bazel_target_label="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 64 ;;
    esac
done

if [ -z "$project_name" ] || [ -z "$source_root" ] || [ -z "$target" ] || [ -z "$artifact_pattern" ]; then
    echo "missing required arg" >&2
    usage
    exit 64
fi
if [ -z "$bazel_target_label" ]; then
    bazel_target_label="//:$target"
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

echo "fidelity[$project_name]: building required converters + classifier" >&2
make -C "$repo_root" converter >/dev/null
CGO_ENABLED=0 go build -C "$repo_root" -o "$bin_dir/fidelity-compare" ./cmd/fidelity-compare

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT
cmake_build="$work_dir/cmake-build"
bazel_ws="$work_dir/bazel-ws"
mkdir -p "$cmake_build"

# --- Step 1: cmake configure + build (project C, the oracle).
echo "fidelity[$project_name]: cmake configure + build" >&2
# Stage cmake File API query so convert can read codemodel-v2.
mkdir -p "$cmake_build/.cmake/api/v1/query"
for k in codemodel-v2 toolchains-v1 cmakeFiles-v1 cache-v2; do
    touch "$cmake_build/.cmake/api/v1/query/$k"
done
# shellcheck disable=SC2086
cmake -G Ninja -DCMAKE_BUILD_TYPE=Release -B "$cmake_build" -S "$source_root" \
    $cmake_flags > "$work_dir/cmake-configure.log" 2>&1
cmake --build "$cmake_build" --target "$target" > "$work_dir/cmake-build.log" 2>&1

cmake_artifact="$(find "$cmake_build" -name "$artifact_pattern" -type f | head -1)"
if [ -z "$cmake_artifact" ]; then
    echo "fidelity[$project_name]: cmake build produced no artifact matching $artifact_pattern" >&2
    exit 1
fi
echo "fidelity[$project_name]:   cmake artifact: $cmake_artifact" >&2

# --- Step 2: convert.
echo "fidelity[$project_name]: convert-element-cmake" >&2
"$bin_dir/convert-element-cmake" \
    --cmake-build-dir "$cmake_build" \
    --out-build "$bazel_ws/BUILD.bazel" \
    > "$work_dir/convert.log" 2>&1 || {
        echo "fidelity[$project_name]: convert FAILED — see $work_dir/convert.log" >&2
        exit 1
    }

# --- Step 3: stage Bazel workspace + build (project B).
# Drop the `load("@rules_cc//...")` shim if present; built-in cc
# rules are available without it on the Bazel versions we test.
sed -i 's|load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")||' "$bazel_ws/BUILD.bazel"
# Copy the project sources into the bazel workspace.
cp -r "$source_root/." "$bazel_ws/"
# Overwrite any pre-existing project BUILD.bazel with the converted one.
"$bin_dir/convert-element-cmake" \
    --cmake-build-dir "$cmake_build" \
    --out-build "$bazel_ws/BUILD.bazel" \
    >> "$work_dir/convert.log" 2>&1
sed -i 's|load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")||' "$bazel_ws/BUILD.bazel"
cat > "$bazel_ws/WORKSPACE" <<EOF
workspace(name = "${project_name}_fidelity_ws")
EOF
if [ -n "${RULES_CC_TARBALL:-}" ]; then
    cat > "$bazel_ws/WORKSPACE" <<EOF
workspace(name = "${project_name}_fidelity_ws")
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
http_archive(name = "rules_cc", urls = ["file://${RULES_CC_TARBALL}"], strip_prefix = "rules_cc-0.0.16")
EOF
fi
cat > "$bazel_ws/.bazelrc" <<'EOF'
common --noenable_bzlmod
common --enable_workspace
EOF

if ! command -v bazel >/dev/null 2>&1; then
    echo "fidelity[$project_name]: bazel not on PATH; skipped — cmake + convert half ran clean" >&2
    exit 0
fi

echo "fidelity[$project_name]: bazel build $bazel_target_label" >&2
bazel_jvm_args=""
if [ -f /etc/ssl/certs/java/cacerts ]; then
    # On hosts where the JVM's embedded cacerts doesn't trust the
    # system CA bundle (Bazel-on-Debian-with-corporate-CA), point
    # the JVM at the system store.
    bazel_jvm_args="--host_jvm_args=-Djavax.net.ssl.trustStore=/etc/ssl/certs/java/cacerts --host_jvm_args=-Djavax.net.ssl.trustStorePassword=changeit"
fi
# shellcheck disable=SC2086
(cd "$bazel_ws" && bazel $bazel_jvm_args build "$bazel_target_label") > "$work_dir/bazel.log" 2>&1 || {
    echo "fidelity[$project_name]: bazel build FAILED — see $work_dir/bazel.log" >&2
    tail -20 "$work_dir/bazel.log" >&2
    exit 1
}

bazel_artifact="$(find "$bazel_ws/bazel-bin" -name "$artifact_pattern" -type f | head -1)"
if [ -z "$bazel_artifact" ]; then
    echo "fidelity[$project_name]: bazel build produced no artifact matching $artifact_pattern" >&2
    exit 1
fi
echo "fidelity[$project_name]:   bazel artifact: $bazel_artifact" >&2

# --- Step 4: classify deltas.
echo "fidelity[$project_name]: fidelity-compare" >&2
allowlist_arg=""
if [ -n "$allowlist" ] && [ -f "$allowlist" ]; then
    allowlist_arg="--allowlist $allowlist"
fi
# shellcheck disable=SC2086
"$bin_dir/fidelity-compare" \
    --cmake-artifact "$cmake_artifact" \
    --bazel-artifact "$bazel_artifact" \
    --report "$work_dir/fidelity.json" \
    $allowlist_arg
echo "fidelity[$project_name]: ok" >&2
