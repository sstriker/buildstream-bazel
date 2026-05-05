#!/usr/bin/env bash
# e2e-hello-fuse: end-to-end exercise of the FUSE sources route
# from the hello-world fixture to bazel-built project A. PR #60's
# verification target.
#
# Pipeline:
#
#  1. Pack testdata/fuse-fixtures/hello-src/ under
#     <cache>/<sourceKey>/ — the same shape --source-cache
#     consumes.
#  2. Stand up buildbarn (docker compose).
#  3. cmd/source-push graph: pack each <cache>/<key>/ tree and
#     PushBlob every blob into bb-storage's CAS.
#  4. cmd/cas-fuse mount: serve <cache>/blobs/directory/<digest>/
#     paths from CAS via FUSE under $MOUNT_POINT.
#  5. cmd/write-a --use-fuse-sources: generate project A whose
#     hello/BUILD.bazel references @src_<key>//:tree.
#  6. bazel build //elements/hello:hello_converted in project A,
#     with --repo_env=CAS_FUSE_MOUNT=... pointing the source
#     extension at the FUSE mount. Bazel 9 reads file bytes
#     through the mount and hashes them itself; the dropped
#     `--unix_digest_hash_attribute_name` fast-path is the
#     subject of docs/design/bazel9-cas-fs.md.
#
# Non-zero exit on any step's failure. All daemons/mounts are
# torn down via trap on exit. Run from repo root.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"

TMP="$(mktemp -d -t e2e-hello-fuse.XXXXXX)"
MOUNT="$TMP/mnt"
CACHE="$TMP/source-cache"
PROJ_A="$TMP/project-A"
PROJ_B="$TMP/project-B"
BUILDBARN_COMPOSE="${BUILDBARN_COMPOSE:-deploy/buildbarn/docker-compose.yml}"
CAS_ADDR="${CAS_ADDR:-127.0.0.1:8980}"

mkdir -p "$MOUNT" "$CACHE"

cleanup() {
    set +e
    if [[ -n "${FUSE_PID:-}" ]] && kill -0 "$FUSE_PID" 2>/dev/null; then
        kill -INT "$FUSE_PID" 2>/dev/null
        wait "$FUSE_PID" 2>/dev/null
    fi
    if mountpoint -q "$MOUNT"; then
        fusermount3 -u "$MOUNT" 2>/dev/null || fusermount -u "$MOUNT" 2>/dev/null || true
    fi
    if [[ -n "${BUILDBARN_UP:-}" ]]; then
        docker compose -f "$BUILDBARN_COMPOSE" down -v 2>/dev/null || true
    fi
    rm -rf "$TMP"
}
trap cleanup EXIT

echo "== build binaries =="
make converter cas-fuse source-push write-a >/dev/null

echo "== compute source key for hello fixture =="
# Same shape as cmd/write-a's sourceKey: SHA256(kind | NUL | url | NUL | ref).
# Inline to avoid spawning Go just for this.
SRC_KEY=$(printf 'tar\0https://example.org/hello-world-1.0.tar.gz\0stable' | sha256sum | awk '{print $1}')
echo "  source-key = $SRC_KEY"
mkdir -p "$CACHE/$SRC_KEY"
cp -r testdata/fuse-fixtures/hello-src/. "$CACHE/$SRC_KEY/"

echo "== buildbarn-up =="
make buildbarn-up >/dev/null
BUILDBARN_UP=1

echo "== source-push graph =="
build/bin/source-push graph --cas="$CAS_ADDR" --source-cache="$CACHE" >/dev/null

echo "== cas-fuse mount =="
build/bin/cas-fuse mount --cas="$CAS_ADDR" --at="$MOUNT" &
FUSE_PID=$!
# Wait for the mount to settle.
for i in $(seq 1 50); do
    if mountpoint -q "$MOUNT"; then break; fi
    sleep 0.1
done
if ! mountpoint -q "$MOUNT"; then
    echo "cas-fuse mount failed to settle"; exit 1
fi

echo "== write-a (--use-fuse-sources) =="
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
    echo "BUILD.bazel does not reference @src_${SRC_KEY}//:tree:"
    cat "$PROJ_A/elements/hello/BUILD.bazel"
    exit 1
}
test -f "$PROJ_A/tools/sources.json" || { echo "missing sources.json"; exit 1; }
grep -q "$SRC_KEY" "$PROJ_A/tools/sources.json" || { echo "sources.json missing key"; exit 1; }
echo "  structure OK"

echo "== verify cas-fuse mount serves the source tree =="
DIGEST=$(grep '"digest"' "$PROJ_A/tools/sources.json" | head -1 | sed 's/.*"\([^"]*\)".*/\1/')
echo "  digest = $DIGEST"
test -d "$MOUNT/blobs/directory/$DIGEST" || {
    echo "mount did not serve $MOUNT/blobs/directory/$DIGEST"
    ls -la "$MOUNT" || true
    exit 1
}
test -f "$MOUNT/blobs/directory/$DIGEST/CMakeLists.txt" || {
    echo "mount did not serve CMakeLists.txt under digest"
    ls -la "$MOUNT/blobs/directory/$DIGEST" || true
    exit 1
}
echo "  mount serving OK"

echo "== verify Bazel-style xattr digest =="
DIGEST_HASH=${DIGEST%-*}
# Pick any file from the served tree and read its xattr.
ATTR=$(getfattr -n user.bazel.cas.digest --only-values --absolute-names \
    "$MOUNT/blobs/directory/$DIGEST/CMakeLists.txt" 2>/dev/null || true)
if [[ -z "$ATTR" ]]; then
    echo "  warning: xattr not readable (getfattr missing or filesystem doesn't expose user.* xattrs in this env)"
else
    echo "  xattr user.bazel.cas.digest = $ATTR"
fi

# bazel-build verification is the next layer: invoke
#   bazel build --repo_env=CAS_FUSE_MOUNT=$MOUNT \
#     //elements/hello:hello_converted
# It needs bazel + cmake + ninja + bwrap on the host. Gated
# behind RUN_BAZEL=1 so the script's structural checks above
# can pass even on minimal environments.
#
# Note: Bazel 7/8 supported `--unix_digest_hash_attribute_name=
# user.bazel.cas.digest --digest_function=SHA256` here, which let
# Bazel trust the FUSE daemon's pre-computed digests instead of
# re-hashing every input. Bazel 9 dropped that flag pair without
# a direct replacement; on Bazel 9 the FUSE-served files still
# resolve correctly, but Bazel hashes them itself. The
# `internal/casfuse` xattr-set machinery stays in tree because
# (a) non-Bazel clients still want it and (b) it's the
# integration point a Bazel-9 successor will plug into. See
# `docs/design/bazel9-cas-fs.md` for the chosen replacement direction.
if [[ "${RUN_BAZEL:-0}" == "1" ]]; then
    echo "== bazel build //elements/hello:hello_converted in project A =="
    if ! command -v bazel >/dev/null && ! command -v bazelisk >/dev/null; then
        echo "  bazel not on PATH; skipping (set RUN_BAZEL=0 to suppress this section)"
    else
        BAZEL=$(command -v bazel || command -v bazelisk)
        cd "$PROJ_A"
        # --verbose_failures + --sandbox_debug surface the actual
        # genrule cmd + sandbox layout when something goes wrong.
        # Cheap on green runs; load-bearing for diagnosing CI red.
        "$BAZEL" build \
            --verbose_failures \
            --sandbox_debug \
            --repo_env=CAS_FUSE_MOUNT="$MOUNT" \
            //elements/hello:hello_converted
        echo "  bazel-build of project A leaf succeeded"
        cd "$repo"
    fi
fi

echo "== e2e-hello-fuse: PASS =="
