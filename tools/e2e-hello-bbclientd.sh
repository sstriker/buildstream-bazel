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
#  4. bb_clientd: bring up the daemon (`make bb-clientd-up`)
#     so its FUSE mount serves CAS Directories at
#     <mount>/cas/<digest>/ and its grpc.sock speaks
#     RemoteOutputService.
#  5. cmd/write-a --use-fuse-sources: generate project A
#     whose hello/BUILD.bazel references @src_<key>//:tree.
#     The repo rule reads BB_CLIENTD_MOUNT (set by this
#     script) and ctx.symlinks into the daemon's mount.
#  6. bazel build //elements/hello:hello_converted with
#     --remote_output_service pointing at the daemon's
#     grpc.sock. Bazel trusts the daemon's reported
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
    skip_reason "bazel $bazel_major < 9 (--remote_output_service not stable until 9.x)"
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

echo "== write-a (--use-fuse-sources, BB_CLIENTD_MOUNT) =="
# write-a's --use-fuse-sources path emits ctx.symlink into
# whatever path the consumer's CAS_FUSE_MOUNT env-var is set to.
# bb_clientd's mount happens to use the same digest-addressed
# subdir layout (blobs/directory/<digest>) under <mount>/cas/,
# so we point CAS_FUSE_MOUNT at <mount>/cas to match.
build/bin/write-a \
    --bst testdata/fuse-fixtures/hello.bst \
    --out "$PROJ_A" \
    --out-b "$PROJ_B" \
    --source-cache "$CACHE" \
    --convert-element build/bin/convert-element \
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
# Sanity: bb_clientd's mount serves the digest tree.
if ! test -d "$MOUNT/cas/$DIGEST" 2>/dev/null \
     && ! test -d "$MOUNT/blobs/directory/$DIGEST" 2>/dev/null; then
    echo "bb_clientd mount does not yet serve digest tree at \$MOUNT/cas/$DIGEST or \$MOUNT/blobs/directory/$DIGEST"
    echo "(bb_clientd config schema may have evolved; verify deploy/buildbarn/config/bb_clientd.jsonnet)"
    ls -la "$MOUNT" || true
    exit 1
fi

cd "$PROJ_A"
"$BAZEL" build \
    --repo_env=CAS_FUSE_MOUNT="$MOUNT/cas" \
    --remote_output_service="unix://$GRPC_SOCK" \
    //elements/hello:hello_converted
cd "$repo"

echo "== e2e-hello-bbclientd: PASS =="
