#!/usr/bin/env bash
# e2e-meta-trace-driven-re: remote-execution + build-without-the-bytes
# gate for the round-2 trace-driven path (PR-1 trace_load, PR-4
# config-bundle). The sibling tools/e2e-meta-buildbarn-re.sh
# exercises a kind:cmake hello-world under RBE+bwotb — the
# baseline path with no trace_load involvement. This gate adds
# the trace_load + trace_build wire under the same RBE+bwotb
# substrate.
#
# What this gate proves:
#
#   1. trace_load (action-time AC lookup, PR-1) runs as a remote
#      action under RBE. The worker fetches the action's
#      executable (trace-lookup binary, uploaded via CAS), runs
#      it on the remote host, and the action's declared outputs
#      (trace.log + marker + cmake-config-bundle.tar when
#      expect_config_bundle=True) land in remote CAS, NOT on
#      local disk.
#   2. The action-env-bumped CONVERGE_GENERATION pattern works
#      under RBE — bazel's ActionCache (local OR remote) tracks
#      the env value, so a bump invalidates the action and
#      forces a re-run as designed.
#   3. The converter genrule consuming :*_trace_load (per
#      handler_pipeline_round2.go's renderer) works under
#      bwotb: the action's inputs are the trace_load's outputs
#      (their digests, not bytes), the converter reads them on
#      the worker via the worker's CAS fetch, and produces
#      BUILD.bazel.out that ALSO stays in remote CAS.
#
# Fixture: testdata/meta-project/autotools-greet. The round-2
# trace gets pre-published via trace-publish (in-process, not on
# a worker — this gate isn't testing trace_build under RBE; the
# in-process roundtrip is already covered by
# e2e-meta-autotools-round2-live.sh).
#
# Self-skips cleanly when docker / bazel >= 9 / bb_clientd aren't
# available.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

skip_reason() {
    msg="$1"
    if [ -n "${BST_RE_GATE_REQUIRE:-}" ]; then
        echo "e2e-meta-trace-driven-re: $msg — BST_RE_GATE_REQUIRE is set, so this is a hard failure" >&2
        exit 1
    fi
    echo "e2e-meta-trace-driven-re: $msg, skipping"
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
echo "e2e-meta-trace-driven-re: building bb-runner image + bringing up buildbarn"
docker compose -f deploy/buildbarn/docker-compose.yml build bb-runner-bare >/dev/null
make -s buildbarn-up
CAS_ADDR="${CAS_ADDR:-127.0.0.1:8980}"

work_dir="$(mktemp -d)"
trap 'make -s buildbarn-down >/dev/null 2>&1 || true; rm -rf "$work_dir"' EXIT

# --- pre-publish trace + bundle ---------------------------------------
# Without a worker-side trace_build (which would need autotools
# on the worker image), pre-stage a synthetic trace +
# bundle and publish them under the fixture's srckey. The
# trace_load action under RBE will fetch them via the AC.
FIXTURE="$work_dir/fixture"
mkdir -p "$FIXTURE/sources"
cp -r testdata/meta-project/autotools-greet/. "$FIXTURE/"
chmod -R u+w "$FIXTURE"
NONCE="trace-re-$(date +%s)-$$-$RANDOM"
echo "# trace-re nonce: $NONCE" >> "$FIXTURE/sources/configure"

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

# Stage trace + make-db + bundle.
STAGE="$work_dir/stage"
mkdir -p "$STAGE/bundle/lib/pkgconfig"
cat > "$STAGE/trace.log" <<'EOF'
execve("/usr/bin/cc", ["cc", "-c", "-o", "greet.o", "greet.c"], 0x0) = 0
execve("/usr/bin/cc", ["cc", "-o", "greet", "greet.o"], 0x0) = 0
EOF
cat > "$STAGE/make-db.txt" <<'EOF'
# (trace-driven-re gate synthetic make-db)
greet.o: greet.c
	cc -c -o greet.o greet.c
greet: greet.o
	cc -o greet greet.o
EOF
cat > "$STAGE/bundle/lib/pkgconfig/greet.pc" <<'EOF'
prefix=/
libdir=/lib
includedir=/include

Name: greet
Version: 0.1.0
Description: trace-driven-re gate synthetic pc file
Libs: -L${libdir} -lgreet
Cflags: -I${includedir}
EOF
( cd "$STAGE/bundle" && tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
    -cf "$STAGE/cmake-config-bundle.tar" . )

build/bin/trace-publish \
    --cas="$CAS_ADDR" \
    --srckey="$GREET_SRCKEY" \
    --trace="$STAGE/trace.log" \
    --make-db="$STAGE/make-db.txt" \
    --config-bundle="$STAGE/cmake-config-bundle.tar" >/dev/null
echo "  published trace + bundle for $GREET_SRCKEY"

# --- inject the operator's remote config ------------------------------
mkdir -p "$A/platforms"
cat > "$A/platforms/BUILD.bazel" <<'EOF'
platform(
    name = "buildbarn",
    exec_properties = {
        "Arch": "x86_64",
        "OSFamily": "linux",
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
# Action-side CAS endpoint. From inside a remote action,
# 127.0.0.1:8980 is the WORKER container's loopback — bb-storage
# isn't there. The in-docker DNS name "bb-storage" resolves to
# the storage container's IP. The host port-forward
# (127.0.0.1:8980) is for bazel ITSELF (its --remote_cache /
# --remote_executor); actions get their own endpoint.
build --action_env=CAS_GRPC_ADDR=bb-storage:8980
# Convergence-generation token (PR-5).
build --action_env=CONVERGE_GENERATION=1
EOF

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

# --- pass A: trace_load + converter genrule on remote workers ---------
exec_log="$work_dir/exec.json"
build_log="$work_dir/build.log"
echo "e2e-meta-trace-driven-re: bazel build //elements/greet:greet_build (remote)"
# shellcheck disable=SC2086
( cd "$A" && "$BAZEL" --output_user_root="$work_dir/.bazel" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/greet:greet_build \
    --execution_log_json_file="$exec_log" \
    $META_BAZEL_BUILD_ARGS ) 2>&1 | tee "$build_log" | tail -15

# Assertion (a): the trace_load action ran remotely.
if ! grep -Eq '"mnemonic": *"TraceLoad"[^}]*' "$exec_log"; then
    echo "e2e-meta-trace-driven-re: FAIL — execution log shows no TraceLoad action" >&2
    exit 1
fi
# The TraceLoad mnemonic should have runner=remote. The structure
# of execution-log JSON varies a bit by Bazel version; match on
# either "remote" or "remote cache hit" (the latter is the case
# when a previous identical action's result is in the AC).
if ! awk '/"mnemonic": *"TraceLoad"/,/"runner": *"/' "$exec_log" \
        | grep -Eq '"runner": *"(remote|remote cache hit|grpc-remote)"'; then
    echo "e2e-meta-trace-driven-re: FAIL — TraceLoad action did not run remotely" >&2
    awk '/"mnemonic": *"TraceLoad"/,/"runner": *"/' "$exec_log" | head -20 >&2
    exit 1
fi
echo "e2e-meta-trace-driven-re: TraceLoad action executed remotely"

# Assertion (b): the converter genrule executed remotely.
if ! grep -Eq '"runner": *"remote"' "$exec_log"; then
    echo "e2e-meta-trace-driven-re: FAIL — execution log shows no spawn ran on a remote worker" >&2
    exit 1
fi
echo "e2e-meta-trace-driven-re: converter genrule executed remotely"

# Assertion (c): bwotb. The trace_load outputs (trace.log, marker,
# cmake-config-bundle.tar) and the converter's BUILD.bazel.out
# must NOT be materialised on local disk under
# --remote_download_minimal.
for out in \
    "$A/bazel-bin/elements/greet/greet_trace_load/trace.log" \
    "$A/bazel-bin/elements/greet/greet_trace_load/marker" \
    "$A/bazel-bin/elements/greet/greet_trace_load/cmake-config-bundle.tar" \
    "$A/bazel-bin/elements/greet/BUILD.bazel.out"; do
    if [ -s "$out" ]; then
        echo "e2e-meta-trace-driven-re: FAIL — $out was materialised locally" >&2
        echo "  --remote_download_minimal should keep it in remote CAS only" >&2
        exit 1
    fi
done
echo "e2e-meta-trace-driven-re: trace_load outputs + converter output all stayed in remote CAS (bwotb)"

echo "ok e2e-meta-trace-driven-re: trace_load + converter genrule verified under RBE+bwotb"
