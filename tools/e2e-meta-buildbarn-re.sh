#!/usr/bin/env bash
# e2e-meta-buildbarn-re: remote-execution + build-without-the-bytes
# gate for the write-a + Bazel path against a real Buildbarn cluster.
#
# This gate proves the claim the orchestrator-absorption plan rests
# on (docs/design/orchestrator-absorption.md): with Bazel as the
# scheduler, the per-element converter genrule executes on a real
# Buildbarn worker via Bazel-native --remote_executor — no bespoke
# REAPI submission code — and the build stays
# build-without-the-bytes: the genrule's output is never materialised
# on local disk.
#
# It is the production-intended path — real `bazel build`, the real
# deploy/buildbarn/ stack, the operator's own .bazelrc + platform()
# wiring. It is explicitly NOT a Go test harness submitting Actions
# through orchestrator internals; that is what e2e-buildbarn /
# e2e-buildbarn-execute do today, and this gate is what replaces
# their coverage once orchestrator/ is deleted (absorption step 7).
#
# Scope (v1): pass A — project A's convert-element-cmake genrule.
# Pass B (project B's cc_* compiles on the remote worker) additionally
# needs a cc-toolchain-for-RBE configuration; it is a documented
# follow-up. Pass A already exercises the core thesis: a converter
# genrule running remotely, bytes staying remote.
#
# Self-skips cleanly when bazel >= 9 is not on PATH (consistent with
# the sibling meta gates) — but note this gate IS the bazel build, so
# a skip means no coverage. CI installs bazelisk so it actually runs.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# --- bazel availability -----------------------------------------------
BAZEL=""
if command -v bazel >/dev/null; then
    BAZEL=$(command -v bazel)
elif command -v bazelisk >/dev/null; then
    BAZEL=$(command -v bazelisk)
fi
bazel_major=0
if [ -n "$BAZEL" ]; then
    # Capture the version output first, parse it second. Piping a live
    # `bazelisk --version` straight through `head` can SIGPIPE the
    # still-running bazelisk (it downloads bazel on first use), which
    # `set -o pipefail` then turns into a script-killing failure
    # before we've printed anything. `|| true` keeps a genuine
    # bazelisk failure a clean skip rather than a crash.
    bazel_version_out=$("$BAZEL" --version 2>&1 || true)
    # Match the `bazel <version>` line specifically — bazelisk may
    # print download-progress lines ahead of it on a cold runner.
    bazel_major=$(printf '%s\n' "$bazel_version_out" \
        | awk '/^bazel [0-9]/{print $2; exit}' | cut -d. -f1)
    case "$bazel_major" in ''|*[!0-9]*) bazel_major=0 ;; esac
fi
if [ -z "$BAZEL" ] || [ "$bazel_major" -lt 9 ]; then
    msg="e2e-meta-buildbarn-re: bazel >= 9 not on PATH"
    # BST_RE_GATE_REQUIRE closes the one silent-skip hole. This gate
    # IS a `bazel build` against Buildbarn — every other path through
    # the script either succeeds or hard-fails — so without this, a
    # green CI job can't be distinguished from a quiet opt-out. CI
    # sets BST_RE_GATE_REQUIRE=1; a green buildbarn-e2e job then means
    # the gate actually ran the remote build and its assertions held.
    if [ -n "${BST_RE_GATE_REQUIRE:-}" ]; then
        echo "$msg — BST_RE_GATE_REQUIRE is set, so this is a hard failure, not a skip" >&2
        exit 1
    fi
    echo "$msg; skipping (this gate IS the bazel build)"
    exit 0
fi

# --- prereqs ----------------------------------------------------------
make -s converter >/dev/null
mkdir -p build/bin
CGO_ENABLED=0 go build -o build/bin/write-a ./cmd/write-a

# --- buildbarn stack --------------------------------------------------
# Build the custom worker image (cmake/ninja/bwrap layered on
# bb-runner-bare) then bring the stack up. CI builds the image as an
# explicit step too; building it here keeps the gate self-contained
# for local runs.
echo "e2e-meta-buildbarn-re: building bb-runner image + bringing up buildbarn"
docker compose -f deploy/buildbarn/docker-compose.yml build bb-runner-bare >/dev/null
make -s buildbarn-up

work_dir="$(mktemp -d)"
trap 'make -s buildbarn-down >/dev/null 2>&1 || true; rm -rf "$work_dir"' EXIT

A="$work_dir/A"

# --- render project A -------------------------------------------------
# hello-world is kind:cmake — the simplest converter genrule, and the
# one TestE2E_Buildbarn_ExecuteRealConvertElement already proves runs
# on this worker image.
build/bin/write-a \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/hello-world.bst \
    --out "$A" \
    --convert-element-cmake "$repo_root/build/bin/convert-element-cmake"

# --- inject the operator's remote config ------------------------------
# A real operator points their rendered project at their Buildbarn
# cluster via .bazelrc + a platform() carrying the worker's advertised
# properties. The gate does exactly that — no write-a changes, no
# CI-only code path. exec_properties MUST match
# deploy/buildbarn/config/worker.jsonnet or bb-scheduler never routes
# work to the worker pool.
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
cat > "$A/.bazelrc" <<'EOF'
# bb-storage exposes CAS + AC on :8980; bb-scheduler exposes the
# Execution service on :8983 (see deploy/buildbarn/docker-compose.yml).
build --remote_cache=grpc://localhost:8980
build --remote_executor=grpc://localhost:8983
build --extra_execution_platforms=//platforms:buildbarn
# Force the converter genrule remote: a local fallback would make the
# gate silently meaningless, so make "can't run remote" a hard failure.
build --strategy=Genrule=remote
# build-without-the-bytes: outputs stay in remote CAS, never downloaded.
build --remote_download_minimal
EOF

# --- pass A: converter genrule on a real Buildbarn worker -------------
# META_BAZEL_STARTUP_ARGS / META_BAZEL_BUILD_ARGS let restricted
# environments inject overrides — most usefully `--registry=...` when
# bcr.bazel.build isn't reachable (point it at the github-raw BCR
# mirror) and a `--host_jvm_args` truststore path. Empty by default;
# CI runners reach bcr directly and need nothing. Same knobs as
# scripts/meta-hello.sh.
META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

exec_log="$work_dir/exec-a.json"
build_log="$work_dir/build-a.log"
echo "e2e-meta-buildbarn-re: bazel build //elements/hello-world:hello-world_converted (remote)"
# shellcheck disable=SC2086 # META_BAZEL_*_ARGS are intentionally word-split.
( cd "$A" && "$BAZEL" --output_user_root="$work_dir/.bazel" \
    $META_BAZEL_STARTUP_ARGS \
    build //elements/hello-world:hello-world_converted \
    --execution_log_json_file="$exec_log" \
    $META_BAZEL_BUILD_ARGS ) 2>&1 | tee "$build_log" | tail -15

# Assertion (a): the converter genrule executed on a remote worker.
# --strategy=Genrule=remote already makes a non-remote genrule a build
# failure, so reaching here is most of the proof; the execution-log
# check is the explicit belt-and-suspenders confirmation.
if ! grep -Eq '"runner": *"remote"' "$exec_log"; then
    echo "e2e-meta-buildbarn-re: FAIL — execution log shows no spawn ran on a remote worker" >&2
    grep -Eo '"runner": *"[^"]*"' "$exec_log" | sort -u >&2 || true
    exit 1
fi
echo "e2e-meta-buildbarn-re: converter genrule executed on a Buildbarn worker"

# Assertion (b): build-without-the-bytes. Under --remote_download_minimal
# the genrule's output stays in remote CAS — it must NOT be a populated
# file on local disk.
build_out="$A/bazel-bin/elements/hello-world/BUILD.bazel.out"
if [ -s "$build_out" ]; then
    echo "e2e-meta-buildbarn-re: FAIL — BUILD.bazel.out was materialised locally" >&2
    echo "  $build_out is a non-empty file; --remote_download_minimal should keep it remote" >&2
    exit 1
fi
echo "e2e-meta-buildbarn-re: build stayed build-without-the-bytes (output not on local disk)"

echo "ok e2e-meta-buildbarn-re: remote execution + bwotb verified against real Buildbarn"
