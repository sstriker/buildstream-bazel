#!/usr/bin/env bash
# e2e-hello-bbclientd: Bazel-9 companion-daemon variant of
# e2e-hello-fuse. Exercises the same pipeline (CAS upload via
# source-push → mounted source serving → write-a render →
# bazel build) but uses bb_clientd's RemoteOutputService in
# place of cmd/cas-fuse + the dropped
# --unix_digest_hash_attribute_name fast-path.
#
# Pipeline:
#
#  1. Pack testdata/fuse-fixtures/hello-src/ under
#     <cache>/<sourceKey>/.
#  2. Stand up buildbarn (docker compose) — the CAS endpoint.
#  3. cmd/source-push graph: PushBlob each source tree's
#     bytes into bb-storage's CAS.
#  4. bb_clientd: bring up the daemon (`make bb-clientd-up`).
#     Its FUSE mount serves CAS Directories under the canonical
#     bb_clientd layout
#       <mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/
#     (with this deploy's defaults — empty instance, sha256 — that
#     collapses to <mount>/cas/blobs/sha256/directory/<digest>/).
#     Its grpc.sock speaks RemoteOutputService.
#  5. cmd/write-a --use-fuse-sources: generate project A
#     whose hello/BUILD.bazel references @src_<key>//:tree.
#     The _src_repo rule reads CAS_FUSE_MOUNT + CAS_DIRECTORY_PREFIX
#     and ctx.symlinks into the daemon's mount; the prefix env var
#     parameterises bb_clientd vs the legacy flat layout (see
#     docs/design/sources.md and rules/sources.bzl).
#  6. bazel build //elements/hello:hello_converted with
#     --experimental_remote_output_service pointing at the
#     daemon's grpc.sock. Bazel trusts the daemon's reported
#     digests; no re-hash on the input side.
#
# Skip cleanly when bb_clientd or bazel >= 9 isn't on PATH.
# This gate is the proof point for the xattr-replacement
# direction; it's NOT a default required gate yet (bb_clientd
# install is a per-host setup step).
#
# Non-zero exit on any wired step's failure. Daemons / mounts /
# compose stack torn down via trap on exit. Run from repo root.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"

TMP="$(mktemp -d -t e2e-hello-bbclientd.XXXXXX)"
CACHE="$TMP/source-cache"
PROJ_A="$TMP/project-A"
PROJ_B="$TMP/project-B"
BB_CLIENTD_ROOT="${BB_CLIENTD_ROOT:-$TMP/bb_clientd}"
BUILDBARN_COMPOSE="${BUILDBARN_COMPOSE:-deploy/buildbarn/docker-compose.yml}"
CAS_ADDR="${CAS_ADDR:-127.0.0.1:8980}"

mkdir -p "$CACHE"

cleanup() {
    set +e
    if [[ -n "${BB_CLIENTD_UP:-}" ]]; then
        BB_CLIENTD_ROOT="$BB_CLIENTD_ROOT" make bb-clientd-down 2>/dev/null || true
    fi
    if [[ -n "${BUILDBARN_UP:-}" ]]; then
        docker compose -f "$BUILDBARN_COMPOSE" down -v 2>/dev/null || true
    fi
    rm -rf "$TMP"
}
trap cleanup EXIT

# --- early gates: skip cleanly when prereqs missing -------------

skip_reason() {
    echo "== e2e-hello-bbclientd: $1, skipping =="
    exit 0
}

if ! command -v bb_clientd >/dev/null && [[ -z "${BB_CLIENTD_BIN:-}" ]]; then
    skip_reason "bb_clientd not on PATH (set BB_CLIENTD_BIN or install from buildbarn-storage)"
fi

if ! command -v bazel >/dev/null && ! command -v bazelisk >/dev/null; then
    skip_reason "bazel/bazelisk not on PATH"
fi
BAZEL=$(command -v bazel || command -v bazelisk)
bazel_major=$("$BAZEL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [[ "$bazel_major" -lt 9 ]]; then
    skip_reason "bazel $bazel_major < 9 (--experimental_remote_output_service requires 9.x)"
fi

# --- pipeline ---------------------------------------------------

echo "== build binaries =="
make converter source-push write-a >/dev/null

echo "== compute source key for hello fixture =="
SRC_KEY=$(printf 'tar\0https://example.org/hello-world-1.0.tar.gz\0stable' | sha256sum | awk '{print $1}')
echo "  source-key = $SRC_KEY"
mkdir -p "$CACHE/$SRC_KEY"
cp -r testdata/fuse-fixtures/hello-src/. "$CACHE/$SRC_KEY/"

echo "== buildbarn-up =="
make buildbarn-up >/dev/null
BUILDBARN_UP=1

echo "== source-push graph =="
build/bin/source-push graph --cas="$CAS_ADDR" --source-cache="$CACHE" >/dev/null

echo "== bb-clientd-up =="
BB_CLIENTD_ROOT="$BB_CLIENTD_ROOT" BB_CLIENTD_CAS="$CAS_ADDR" make bb-clientd-up
BB_CLIENTD_UP=1
MOUNT="$BB_CLIENTD_ROOT/mount"
GRPC_SOCK="$BB_CLIENTD_ROOT/grpc.sock"

echo "== write-a (--use-fuse-sources) =="
# write-a's --use-fuse-sources path emits ctx.symlink into
# <CAS_FUSE_MOUNT>/<CAS_DIRECTORY_PREFIX>/directory/<digest>;
# both env vars are read by the _src_repo rule at bazel-build
# time, set below as --repo_env flags.
build/bin/write-a \
    --rules-package-path "$repo/rules_buildstream_bazel" \
    --bst testdata/fuse-fixtures/hello.bst \
    --out "$PROJ_A" \
    --out-b "$PROJ_B" \
    --source-cache "$CACHE" \
    --convert-element-cmake build/bin/convert-element-cmake \
    --use-fuse-sources

echo "== verify generated structure =="
test -f "$PROJ_A/elements/hello/BUILD.bazel" || { echo "missing BUILD.bazel"; exit 1; }
grep -q "@src_${SRC_KEY}//:tree" "$PROJ_A/elements/hello/BUILD.bazel" || {
    echo "BUILD.bazel does not reference @src_${SRC_KEY}//:tree"
    cat "$PROJ_A/elements/hello/BUILD.bazel"
    exit 1
}
test -f "$PROJ_A/tools/sources.json" || { echo "missing sources.json"; exit 1; }
echo "  structure OK"

echo "== bazel build //elements/hello:hello_converted (Bazel 9 + RemoteOutputService) =="
DIGEST=$(grep '"digest"' "$PROJ_A/tools/sources.json" | head -1 | sed 's/.*"\([^"]*\)".*/\1/')
echo "  digest = $DIGEST"
# Sanity: bb_clientd's mount serves the digest tree. The
# canonical bb_clientd path is
# `<mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>`;
# our deploy/buildbarn/config/bb_clientd.jsonnet uses the
# defaults (empty instance, sha256), so the path collapses to
# `<mount>/cas//blobs/sha256/directory/<digest>` (which OS path
# resolution normalises to `<mount>/cas/blobs/sha256/directory/<digest>`).
# Probe that shape; if it's missing fall back to the older
# probes (older bb_clientd versions / different configs).
if ! test -d "$MOUNT/cas/blobs/sha256/directory/$DIGEST" 2>/dev/null \
     && ! test -d "$MOUNT/cas/$DIGEST" 2>/dev/null \
     && ! test -d "$MOUNT/blobs/directory/$DIGEST" 2>/dev/null; then
    echo "bb_clientd mount does not serve digest tree at expected paths under \$MOUNT=$MOUNT"
    echo "(bb_clientd config schema may have evolved; verify deploy/buildbarn/config/bb_clientd.jsonnet)"
    ls -la "$MOUNT" || true
    exit 1
fi

cd "$PROJ_A"
# Two ways to point the repo rule at bb_clientd:
#   (1) Legacy hack — set CAS_FUSE_MOUNT to "$MOUNT/cas" so the
#       hardcoded /blobs/directory/<digest> template under cas-fuse
#       happens to resolve under bb_clientd's empty-instance,
#       sha256 default (path collapses to .../cas//blobs/sha256/...).
#   (2) Canonical — set CAS_FUSE_MOUNT to "$MOUNT" and pass
#       CAS_DIRECTORY_PREFIX=cas//blobs/sha256 so the rule builds
#       the bb_clientd-canonical path explicitly.
# We use (2): exercises the parameterised path-template support
# rules/sources.bzl ships with so non-default bb_clientd configs
# (non-empty instance, non-sha256 digest fn) work without further
# rule changes.
"$BAZEL" build \
    --repo_env=CAS_FUSE_MOUNT="$MOUNT" \
    --repo_env=CAS_DIRECTORY_PREFIX="cas//blobs/sha256" \
    --experimental_remote_output_service="unix://$GRPC_SOCK" \
    //elements/hello:hello_converted
cd "$repo"

echo "== e2e-hello-bbclientd: PASS =="
