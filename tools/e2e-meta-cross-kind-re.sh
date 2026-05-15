#!/usr/bin/env bash
# e2e-meta-cross-kind-re: the full cross-element configure-step
# bootstrap proof under RBE + build-without-the-bytes. Combines:
#
#   - the cross-kind fixture (cons/kind:cmake depending on
#     auto-prod/kind:autotools, cons doing find_package(autoprod
#     CONFIG REQUIRED)) from e2e-meta-cross-kind-live.sh
#   - the RBE + bwotb substrate (--remote_executor +
#     --remote_download_minimal + the cmake-equipped bb-runner
#     image) from e2e-meta-buildbarn-re.sh +
#     e2e-meta-trace-driven-re.sh
#
# What this gate proves:
#
#   1. The trace_load action runs on a remote worker (PR-1's
#      action-time rule survives RBE — execution log mnemonic
#      "TraceLoad", runner=remote). The bundle bytes the action
#      materializes via gRPC stay in remote CAS under
#      --remote_download_minimal.
#   2. The consumer's converter genrule runs on a remote worker.
#      Its action inputs include the bundle bytes' DIGEST (via
#      trace_load's output) — not the bundle bytes themselves on
#      local disk. The worker fetches the bytes via CAS on demand
#      when the genrule's cmd extracts the bundle.
#   3. cmake configure runs on the remote worker (the bb-runner
#      image has cmake 3.28.3 + ninja). It resolves
#      find_package(autoprod CONFIG REQUIRED) against the
#      bundle's autoprodConfig.cmake — proving the bundle bytes
#      flowed correctly through the AC → remote action input
#      tree → cmake.
#   4. The consumer's BUILD.bazel.out (the converter's output)
#      contains cc_library — proving find_package succeeded —
#      AND stays in remote CAS post-build (bwotb).
#
# This is the load-bearing proof for the entire 6-PR stack:
# kind:cmake + kind:autotools-dep ACTUALLY WORKS, end-to-end,
# under the bwotb production path.
#
# Skips with BST_RE_GATE_REQUIRE awareness — green CI run under
# BST_RE_GATE_REQUIRE means the full path exercised.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

skip_reason() {
    msg="$1"
    if [ -n "${BST_RE_GATE_REQUIRE:-}" ]; then
        echo "e2e-meta-cross-kind-re: $msg — BST_RE_GATE_REQUIRE is set, so this is a hard failure" >&2
        exit 1
    fi
    echo "e2e-meta-cross-kind-re: $msg, skipping"
    exit 0
}

# --- bazel availability -----------------------------------------------
BAZEL=""
if command -v bazel >/dev/null; then
    BAZEL=$(command -v bazel)
elif command -v bazelisk >/dev/null; then
    BAZEL=$(command -v bazelisk)
fi
bazel_major=0
if [ -n "$BAZEL" ]; then
    bazel_version_out=$("$BAZEL" --version 2>&1 || true)
    bazel_major=$(printf '%s\n' "$bazel_version_out" \
        | awk '/^bazel [0-9]/{print $2; exit}' | cut -d. -f1)
    case "$bazel_major" in ''|*[!0-9]*) bazel_major=0 ;; esac
fi
if [ -z "$BAZEL" ] || [ "$bazel_major" -lt 9 ]; then
    skip_reason "bazel >= 9 not on PATH"
fi
if ! command -v docker >/dev/null; then
    skip_reason "docker not on PATH"
fi
if ! docker compose version >/dev/null 2>&1; then
    skip_reason "docker compose plugin missing"
fi
if ! docker info >/dev/null 2>&1; then
    skip_reason "docker daemon not reachable"
fi

# --- build binaries ---------------------------------------------------
make -s converter >/dev/null
mkdir -p build/bin
CGO_ENABLED=0 go build -o build/bin/write-a ./cmd/write-a
CGO_ENABLED=0 go build -o build/bin/build-tracer ./cmd/build-tracer
CGO_ENABLED=0 go build -o build/bin/convert-element-trace ./cmd/convert-element-trace
CGO_ENABLED=0 go build -o build/bin/trace-publish ./cmd/trace-publish
CGO_ENABLED=0 go build -o build/bin/trace-lookup ./cmd/trace-lookup

# --- buildbarn stack --------------------------------------------------
# bb-runner-bare carries cmake + ninja + bwrap (see
# deploy/buildbarn/Dockerfile.bb-runner-bare). The cmake configure
# step inside the cons converter genrule's REAPI action runs on
# this image.
echo "e2e-meta-cross-kind-re: building bb-runner image + bringing up buildbarn"
docker compose -f deploy/buildbarn/docker-compose.yml build bb-runner-bare >/dev/null
make -s buildbarn-up
CAS_ADDR="${CAS_ADDR:-127.0.0.1:8980}"

work_dir="$(mktemp -d)"
trap 'make -s buildbarn-down >/dev/null 2>&1 || true; rm -rf "$work_dir"' EXIT

# --- fixture: cross-kind with per-run nonce --------------------------
FIXTURE_SRC="$repo_root/testdata/meta-project/cross-kind"
FIXTURE="$work_dir/fixture"
mkdir -p "$FIXTURE/sources"
cp "$FIXTURE_SRC/cons.bst" "$FIXTURE/"
cp "$FIXTURE_SRC/auto-prod.bst" "$FIXTURE/"
cp -r "$FIXTURE_SRC/sources/." "$FIXTURE/sources/"
chmod -R u+w "$FIXTURE"
NONCE="cross-kind-re-$(date +%s)-$$-$RANDOM"
echo "# cross-kind-re nonce: $NONCE" >> "$FIXTURE/sources/auto-prod/configure"
echo "  per-run nonce: $NONCE"

# --- write-a render --------------------------------------------------
A="$work_dir/A"
B="$work_dir/B"
build/bin/write-a \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$FIXTURE/cons.bst" \
    --bst "$FIXTURE/auto-prod.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$repo_root/build/bin/convert-element-cmake" \
    --convert-element-trace "$repo_root/build/bin/convert-element-trace" \
    --build-tracer-bin "$repo_root/build/bin/build-tracer" \
    --trace-publish-bin "$repo_root/build/bin/trace-publish" \
    --trace-lookup-bin "$repo_root/build/bin/trace-lookup"

# --- pre-stage auto-prod's config bundle -----------------------------
AUTOPROD_SRCKEY=$(tr -d '[:space:]' < "$A/elements/auto-prod/srckey.txt")
echo "  auto-prod srckey = $AUTOPROD_SRCKEY"

BUNDLE_STAGE="$work_dir/bundle-stage"
mkdir -p "$BUNDLE_STAGE/lib/cmake/autoprod"
cat > "$BUNDLE_STAGE/lib/cmake/autoprod/autoprodConfig.cmake" <<'EOF'
# Synthesized by e2e-meta-cross-kind-re. The cons element's
# converter genrule runs cmake configure inside its REAPI action
# on the bb-runner worker; find_package(autoprod CONFIG REQUIRED)
# below resolves against this file. cmake's `if(NOT EXISTS)`
# import-check loop in DepTargets.cmake passes against zero-byte
# stubs at every IMPORTED_LOCATION_<CONFIG> path; this
# INTERFACE IMPORTED target has no IMPORTED_LOCATION so no
# stubs are needed.
if(NOT TARGET autoprod::autoprod)
    add_library(autoprod::autoprod INTERFACE IMPORTED)
endif()
EOF
BUNDLE_TAR="$work_dir/cmake-config-bundle.tar"
( cd "$BUNDLE_STAGE" && tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
    -cf "$BUNDLE_TAR" . )

TRACE_STAGE="$work_dir/trace.log"
printf 'execve("/bin/true", ["true"], 0x0) = 0\n' > "$TRACE_STAGE"
build/bin/trace-publish \
    --cas="$CAS_ADDR" \
    --srckey="$AUTOPROD_SRCKEY" \
    --trace="$TRACE_STAGE" \
    --config-bundle="$BUNDLE_TAR" >/dev/null
echo "  published trace + bundle for auto-prod's srckey"

# --- inject the operator's remote config -----------------------------
mkdir -p "$A/platforms"
cat > "$A/platforms/BUILD.bazel" <<'EOF'
platform(
    name = "buildbarn",
    exec_properties = {
        "Arch": "x86_64",
        "OSFamily": "linux",
        "bwrap-version": "0.8.0",
        "cmake-version": "3.28.3",
        "ninja-version": "1.11.1",
    },
    visibility = ["//visibility:public"],
)
EOF
cat > "$A/.bazelrc" <<EOF
build --remote_cache=grpc://localhost:8980
build --remote_executor=grpc://localhost:8983
build --extra_execution_platforms=//platforms:buildbarn
# Force every action remote so a local fallback can't quietly
# satisfy the gate.
build --strategy=Genrule=remote
# bwotb: outputs stay in remote CAS.
build --remote_download_minimal
# Action-side CAS endpoint. The worker container is on the
# same docker network as bb-storage; the in-network DNS name
# resolves there. From the host (where bazel runs) the same
# endpoint is 127.0.0.1:8980 via the docker compose port
# forward, but actions can't see the host's loopback — they
# see the WORKER's loopback. The two endpoints MUST be
# threaded distinctly.
build --action_env=CAS_GRPC_ADDR=bb-storage:8980
# Convergence-generation token (PR-5). Set to 1 for this gate.
build --action_env=CONVERGE_GENERATION=1
EOF

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

# --- bazel build the consumer's converter genrule (RBE+bwotb) -------
exec_log="$work_dir/exec.json"
build_log="$work_dir/build.log"
echo "e2e-meta-cross-kind-re: bazel build //elements/cons:cons_converted (remote + bwotb)"
# shellcheck disable=SC2086
( cd "$A" && "$BAZEL" --output_user_root="$work_dir/.bazel" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/cons:cons_converted \
    --execution_log_json_file="$exec_log" \
    $META_BAZEL_BUILD_ARGS ) 2>&1 | tee "$build_log" | tail -20

# --- assertions ------------------------------------------------------
# (1) TraceLoad action ran remotely.
if ! awk '/"mnemonic": *"TraceLoad"/,/"runner": *"/' "$exec_log" \
        | grep -Eq '"runner": *"(remote|remote cache hit|grpc-remote)"'; then
    echo "e2e-meta-cross-kind-re: FAIL — TraceLoad action did not run remotely" >&2
    awk '/"mnemonic": *"TraceLoad"/,/"runner": *"/' "$exec_log" | head -20 >&2
    exit 1
fi
echo "  (1) TraceLoad action executed remotely"

# (2) Genrule (the converter) ran remotely.
if ! grep -Eq '"runner": *"remote"' "$exec_log"; then
    echo "e2e-meta-cross-kind-re: FAIL — converter genrule did not run remotely" >&2
    exit 1
fi
echo "  (2) cons converter genrule executed remotely"

# (3) bwotb: trace_load outputs + converter output stayed remote.
for out in \
    "$A/bazel-bin/elements/auto-prod/auto-prod_trace_load/trace.log" \
    "$A/bazel-bin/elements/auto-prod/auto-prod_trace_load/marker" \
    "$A/bazel-bin/elements/auto-prod/auto-prod_trace_load/cmake-config-bundle.tar"; do
    if [ -s "$out" ]; then
        echo "e2e-meta-cross-kind-re: FAIL — $out was materialised locally (bwotb broken)" >&2
        exit 1
    fi
done
CONS_OUT="$A/bazel-bin/elements/cons/BUILD.bazel.out"
if [ -s "$CONS_OUT" ]; then
    echo "e2e-meta-cross-kind-re: FAIL — cons BUILD.bazel.out was materialised locally (bwotb broken)" >&2
    exit 1
fi
echo "  (3) trace_load outputs + cons converter output stayed in remote CAS (bwotb)"

# (4) cons BUILD.bazel.out has cc_library (find_package resolved).
# Under --remote_download_minimal the output isn't on disk; fetch
# it specifically for inspection.
echo "  fetching cons BUILD.bazel.out via --remote_download_outputs=all"
# shellcheck disable=SC2086
( cd "$A" && "$BAZEL" --output_user_root="$work_dir/.bazel" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/cons:cons_converted \
    --remote_download_outputs=all \
    $META_BAZEL_BUILD_ARGS ) > /dev/null 2>&1
if [ ! -s "$CONS_OUT" ]; then
    echo "e2e-meta-cross-kind-re: FAIL — cons BUILD.bazel.out not fetched even with --remote_download_outputs=all" >&2
    exit 1
fi
if ! grep -qE '^cc_library' "$CONS_OUT"; then
    echo "e2e-meta-cross-kind-re: FAIL — cons BUILD.bazel.out missing cc_library" >&2
    echo "this means cmake's find_package(autoprod CONFIG) didn't resolve on the remote worker:" >&2
    head -50 "$CONS_OUT" >&2
    exit 1
fi
if ! grep -qF 'name = "cons"' "$CONS_OUT"; then
    echo "e2e-meta-cross-kind-re: FAIL — cons BUILD.bazel.out missing cons target" >&2
    head -50 "$CONS_OUT" >&2
    exit 1
fi
echo "  (4) cons BUILD.bazel.out has cc_library — find_package(autoprod) resolved through:"
echo "      AC → remote trace_load action → remote converter genrule's input tree →"
echo "      cmake configure on the bb-runner worker → fine cc_library"

echo "ok e2e-meta-cross-kind-re: cross-element configure-step bootstrap verified end-to-end under RBE+bwotb"
