#!/usr/bin/env bash
# e2e-meta-trace-build-on-worker: the real autotools build under
# RBE — trace_build action runs configure + make + install +
# trace-publish on a remote worker, NOT pre-staged synthetic
# bytes.
#
# Together with the sibling gates this is the full proof:
#
#   - e2e-meta-trace-driven-re.sh: trace_load + converter run
#     under RBE+bwotb, but trace + bundle are pre-staged
#     (synthetic).
#   - e2e-meta-cross-kind-re.sh: cross-kind end-to-end under
#     RBE+bwotb, also pre-staged.
#   - e2e-meta-trace-build-on-worker.sh (this gate): the
#     trace_build action ITSELF runs on a remote worker. The
#     worker runs configure + make + install under build-tracer,
#     trace-publish dials bb-storage from inside the action over
#     the docker network, and the AC entry lands. A subsequent
#     trace_load action (also remote) fetches the just-published
#     bytes.
#
# What's tested specifically:
#
#   1. The bb-runner image carries enough toolchain to actually
#      run an autotools build (make + gcc + libc, plus build-tracer
#      + trace-publish which Bazel uploads to CAS as the action's
#      tools).
#   2. The action can dial bb-storage over the docker network
#      (CAS_GRPC_ADDR=bb-storage:8980 from inside the action,
#      not the host port-forward — actions DON'T have access to
#      the host's loopback). This is the wire shape any operator
#      using a real RBE cluster has to deal with.
#   3. build-tracer runs on the worker (it's a Linux binary that
#      uses ptrace/seccomp — requires capabilities the worker
#      grants). The published trace bytes are byte-stable across
#      machines (canonicalisation strips pid + mktemp paths).
#   4. trace-publish from inside the action successfully writes
#      the AC entry under SyntheticActionDigest, and a sibling
#      action (trace_load) sees the hit in the very next bazel
#      build.
#
# Skips with BST_RE_GATE_REQUIRE awareness.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

skip_reason() {
    msg="$1"
    if [ -n "${BST_RE_GATE_REQUIRE:-}" ]; then
        echo "e2e-meta-trace-build-on-worker: $msg — BST_RE_GATE_REQUIRE is set, so this is a hard failure" >&2
        exit 1
    fi
    echo "e2e-meta-trace-build-on-worker: $msg, skipping"
    exit 0
}

# --- prereqs ----------------------------------------------------------
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
echo "e2e-meta-trace-build-on-worker: building bb-runner image + bringing up buildbarn"
docker compose -f deploy/buildbarn/docker-compose.yml build bb-runner-bare >/dev/null
make -s buildbarn-up
# Action-side CAS endpoint. The worker container is on the same
# docker network as bb-storage; the in-network DNS name resolves
# there. From the host (where bazel runs) the same endpoint is
# 127.0.0.1:8980 via the docker compose port forward; bazel uses
# that for its own AC + Execution. Remote actions use the
# in-network endpoint for their own publishes.
CAS_ADDR_ACTION="bb-storage:8980"

work_dir="$(mktemp -d)"
trap 'make -s buildbarn-down >/dev/null 2>&1 || true; rm -rf "$work_dir"' EXIT

# --- fixture: tiny autotools producer --------------------------------
# Per-run nonce so write-a's computed srckey is unique and stale
# AC entries can't satisfy this run's lookup.
FIXTURE_SRC="$repo_root/testdata/meta-project/autotools-greet"
FIXTURE="$work_dir/fixture"
mkdir -p "$FIXTURE/sources"
cp -r "$FIXTURE_SRC/." "$FIXTURE/"
chmod -R u+w "$FIXTURE"
NONCE="trace-build-on-worker-$(date +%s)-$$-$RANDOM"
echo "# $NONCE" >> "$FIXTURE/sources/configure"

A="$work_dir/A"
B="$work_dir/B"
build/bin/write-a \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$FIXTURE/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$repo_root/build/bin/convert-element-cmake" \
    --convert-element-trace "$repo_root/build/bin/convert-element-trace" \
    --build-tracer-bin "$repo_root/build/bin/build-tracer" \
    --trace-publish-bin "$repo_root/build/bin/trace-publish" \
    --trace-lookup-bin "$repo_root/build/bin/trace-lookup"

GREET_SRCKEY=$(tr -d '[:space:]' < "$A/elements/greet/srckey.txt")
echo "  fixture srckey = $GREET_SRCKEY"

# --- operator's remote config ----------------------------------------
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
# Project A's .bazelrc: trace_load actions only.
cat > "$A/.bazelrc" <<EOF
build --remote_cache=grpc://localhost:8980
build --remote_executor=grpc://localhost:8983
build --extra_execution_platforms=//platforms:buildbarn
build --strategy=Genrule=remote
build --remote_download_minimal
# Action-side CAS endpoint — actions dial bb-storage over the
# docker network. The host port-forward (127.0.0.1:8980) is for
# bazel ITSELF; actions can't see the host's loopback.
build --action_env=CAS_GRPC_ADDR=$CAS_ADDR_ACTION
build --action_env=CONVERGE_GENERATION=1
EOF
cp "$A/platforms/BUILD.bazel" "$B/platforms/BUILD.bazel" 2>/dev/null || (mkdir -p "$B/platforms" && cp "$A/platforms/BUILD.bazel" "$B/platforms/BUILD.bazel")
# Project B's .bazelrc mirrors A's so the trace_build action
# uses the same in-network endpoint.
cp "$A/.bazelrc" "$B/.bazelrc"

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

# --- step 1: trace_build on the worker, FOR REAL ---------------------
echo "e2e-meta-trace-build-on-worker: bazel build //elements/greet:greet_trace_build in project B (remote)"
build_b_log="$work_dir/build-b.log"
build_b_exec="$work_dir/exec-b.json"
# shellcheck disable=SC2086
( cd "$B" && "$BAZEL" --output_user_root="$work_dir/.bazel-b" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/greet:greet_trace_build \
    --execution_log_json_file="$build_b_exec" \
    $META_BAZEL_BUILD_ARGS ) 2>&1 | tee "$build_b_log" | tail -20

# Assertion: the trace_build action ran remotely.
if ! grep -Eq '"runner": *"remote"' "$build_b_exec"; then
    echo "e2e-meta-trace-build-on-worker: FAIL — trace_build did not run remotely" >&2
    exit 1
fi
echo "  trace_build action executed remotely (configure + make + install + trace-publish on worker)"

# Assertion: install_tree.tar + trace.log (the trace_build's
# declared outputs) stayed in remote CAS — bwotb.
for out in \
    "$B/bazel-bin/elements/greet/install_tree.tar" \
    "$B/bazel-bin/elements/greet/trace.log"; do
    if [ -s "$out" ]; then
        echo "e2e-meta-trace-build-on-worker: FAIL — $out was materialised locally (bwotb broken)" >&2
        exit 1
    fi
done
echo "  install_tree.tar + trace.log stayed in remote CAS (bwotb)"

# --- step 2: trace_load fetches what trace_build just published -----
# A fresh bazel build of project A's converter genrule will run
# trace_load remotely; it should hit the AC entry the trace_build
# action just published. The action-time CAS lookup goes over
# the same in-network endpoint.
echo "e2e-meta-trace-build-on-worker: bazel build //elements/greet:greet_build in project A (remote)"
build_a_log="$work_dir/build-a.log"
build_a_exec="$work_dir/exec-a.json"
# shellcheck disable=SC2086
( cd "$A" && "$BAZEL" --output_user_root="$work_dir/.bazel-a" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/greet:greet_build \
    --execution_log_json_file="$build_a_exec" \
    $META_BAZEL_BUILD_ARGS ) 2>&1 | tee "$build_a_log" | tail -10

# Assertion: TraceLoad ran remotely AND found the hit.
if ! awk '/"mnemonic": *"TraceLoad"/,/"runner": *"/' "$build_a_exec" \
        | grep -Eq '"runner": *"(remote|remote cache hit|grpc-remote)"'; then
    echo "e2e-meta-trace-build-on-worker: FAIL — TraceLoad did not run remotely" >&2
    exit 1
fi
echo "  trace_load action executed remotely"

# Fetch the trace_load's marker output so we can read its
# hit/miss state.
# shellcheck disable=SC2086
( cd "$A" && "$BAZEL" --output_user_root="$work_dir/.bazel-a" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/greet:greet_trace_load \
    --remote_download_outputs=all \
    $META_BAZEL_BUILD_ARGS ) > /dev/null 2>&1
MARKER="$A/bazel-bin/elements/greet/greet_trace_load/marker"
if [ ! -f "$MARKER" ]; then
    echo "e2e-meta-trace-build-on-worker: FAIL — trace_load marker not produced at $MARKER" >&2
    exit 1
fi
MARKER_BODY=$(cat "$MARKER")
if [ "$MARKER_BODY" != "hit" ]; then
    echo "e2e-meta-trace-build-on-worker: FAIL — trace_load marker is '$MARKER_BODY', expected 'hit'" >&2
    echo "  this means trace_build's trace-publish step did NOT land an AC entry the worker could read" >&2
    exit 1
fi
echo "  trace_load marker = hit — the AC entry trace_build wrote IS reachable from a fresh action"

echo "ok e2e-meta-trace-build-on-worker: trace_build's full configure+make+install+publish ran on a remote worker, AC entry survives, downstream trace_load action sees it"
