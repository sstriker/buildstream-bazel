#!/usr/bin/env bash
# e2e-meta-autotools-round2-live: live-AC round-trip gate for the
# round-2 trace rendezvous, exercised against real buildbarn (+
# optionally bb_clientd) rather than the in-process LocalStore the
# unit tests use.
#
# What this gate proves:
#
#   1. trace-publish writes an AC entry against a real REAPI gRPC
#      endpoint. It packs a synthetic trace dir as a CAS Directory,
#      uploads the blobs, and calls UpdateActionResult under
#      SyntheticActionDigest(srckey).
#   2. trace-lookup re-derives the synthetic key from the same
#      srckey, GetActionResult comes back with a populated AR, and
#      the printed root Directory digest matches what trace-publish
#      reported.
#   3. (When bb_clientd is on PATH) The Directory blob trace-lookup
#      resolves is actually mountable: bb_clientd serves it at
#      <mount>/cas/<digest>/ with the two staged files visible.
#      This is the load-time resolution the _trace_repo Bazel rule
#      will perform when project A's converter genrule's srcs
#      reference @trace_<elem>//:trace.
#
# The bazel-side end-to-end (write-a → bazel build B → publish →
# write-a re-render → bazel build A → AC-hit → fine cc rules)
# requires a real autotools fixture with the full tool chain in
# the genrule sandbox; that's a separate larger gate. This gate
# locks in the wire-level publish/lookup contract that the larger
# gate depends on.
#
# Pipeline:
#
#   1. Stand up buildbarn (`make buildbarn-up`).
#   2. Build trace-publish + trace-lookup.
#   3. Stage a synthetic trace dir (trace.log + make-db.txt).
#   4. trace-publish against $CAS_ADDR.
#   5. trace-lookup against $CAS_ADDR; assert digest matches.
#   6. (If bb_clientd available) bring it up, list
#      <mount>/cas/<digest>/, assert trace.log + make-db.txt visible.
#
# Skips cleanly when docker compose / buildbarn aren't available.
# Tears down everything via trap.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"

TMP="$(mktemp -d -t e2e-round2-live.XXXXXX)"
BUILDBARN_COMPOSE="${BUILDBARN_COMPOSE:-deploy/buildbarn/docker-compose.yml}"
CAS_ADDR="${CAS_ADDR:-127.0.0.1:8980}"

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

skip_reason() {
    echo "== e2e-meta-autotools-round2-live: $1, skipping =="
    exit 0
}

if ! command -v docker >/dev/null; then
    skip_reason "docker not on PATH (buildbarn stack runs via docker compose)"
fi
if ! docker compose version >/dev/null 2>&1; then
    skip_reason "docker compose plugin missing"
fi
if ! docker info >/dev/null 2>&1; then
    skip_reason "docker daemon not reachable"
fi

echo "== build binaries =="
CGO_ENABLED=0 go build -o "$repo/build/bin/trace-publish" ./cmd/trace-publish
CGO_ENABLED=0 go build -o "$repo/build/bin/trace-lookup" ./cmd/trace-lookup

echo "== buildbarn-up =="
make buildbarn-up >/dev/null
BUILDBARN_UP=1

echo "== stage synthetic trace dir =="
STAGE="$TMP/stage"
mkdir -p "$STAGE"
# Realistic-ish trace + make-db payloads; the wire contract
# doesn't care about content, but using non-trivial bytes guards
# against zero-length-file edge cases in the upload path.
cat > "$STAGE/trace.log" <<'EOF'
execve("/usr/bin/cc", ["cc", "-c", "-o", "greet.o", "greet.c"], 0x0) = 0
execve("/usr/bin/cc", ["cc", "-o", "greet", "greet.o"], 0x0) = 0
EOF
cat > "$STAGE/make-db.txt" <<'EOF'
# (round-2 live-AC gate synthetic make-db)
greet.o: greet.c
	cc -c -o greet.o greet.c
greet: greet.o
	cc -o greet greet.o
EOF

# Use a per-run srckey so this gate doesn't collide with prior
# runs' AC entries (buildbarn's AC isn't volume-cleaned between
# runs of this gate; the synthetic key namespaces by srckey hex).
SRCKEY="round2-live-$(date +%s)-$$"
echo "  srckey = $SRCKEY"

echo "== trace-publish =="
PUB_OUT=$("$repo/build/bin/trace-publish" \
    --cas="$CAS_ADDR" \
    --srckey="$SRCKEY" \
    --trace="$STAGE/trace.log" \
    --make-db="$STAGE/make-db.txt")
echo "  trace-publish printed: $PUB_OUT"
if [[ -z "$PUB_OUT" ]]; then
    echo "trace-publish printed nothing — expected '<hash>/<size>'"
    exit 1
fi

echo "== trace-lookup =="
LK_OUT=$("$repo/build/bin/trace-lookup" \
    --cas="$CAS_ADDR" \
    --srckey="$SRCKEY")
echo "  trace-lookup printed: $LK_OUT"
if [[ "$PUB_OUT" != "$LK_OUT" ]]; then
    echo "trace-lookup digest mismatch:"
    echo "  publish: $PUB_OUT"
    echo "  lookup:  $LK_OUT"
    exit 1
fi
echo "  publish/lookup digests match — wire roundtrip OK"

echo "== trace-lookup miss for an unrelated srckey =="
MISS=$("$repo/build/bin/trace-lookup" \
    --cas="$CAS_ADDR" \
    --srckey="round2-live-MISS-$(date +%s)-$$" || true)
if [[ -n "$MISS" ]]; then
    echo "trace-lookup unexpectedly returned a digest for an unpublished srckey: $MISS"
    exit 1
fi
echo "  unrelated srckey ⇒ empty stdout (clean miss) OK"

# bb_clientd-half: if available, prove the published Directory
# blob is mountable. This is the load-time resolution the
# _trace_repo Bazel rule will perform.
if command -v bb_clientd >/dev/null || [[ -n "${BB_CLIENTD_BIN:-}" ]]; then
    BB_CLIENTD_ROOT="${BB_CLIENTD_ROOT:-$TMP/bb_clientd}"
    export BB_CLIENTD_ROOT
    echo "== bb-clientd-up =="
    BB_CLIENTD_ROOT="$BB_CLIENTD_ROOT" BB_CLIENTD_CAS="$CAS_ADDR" make bb-clientd-up
    BB_CLIENTD_UP=1
    MOUNT="$BB_CLIENTD_ROOT/mount"
    DIGEST_HASH=$(echo "$PUB_OUT" | cut -d/ -f1)
    echo "  digest hash = $DIGEST_HASH"
    if ! test -d "$MOUNT/cas/$DIGEST_HASH" 2>/dev/null \
         && ! test -d "$MOUNT/blobs/directory/$DIGEST_HASH" 2>/dev/null; then
        echo "bb_clientd mount does not serve published Directory at"
        echo "  $MOUNT/cas/$DIGEST_HASH"
        echo "or"
        echo "  $MOUNT/blobs/directory/$DIGEST_HASH"
        echo "(this is what _trace_repo expects to symlink at load time)"
        ls -la "$MOUNT" || true
        exit 1
    fi
    SERVED=""
    if test -d "$MOUNT/cas/$DIGEST_HASH"; then
        SERVED="$MOUNT/cas/$DIGEST_HASH"
    else
        SERVED="$MOUNT/blobs/directory/$DIGEST_HASH"
    fi
    echo "  bb_clientd serves trace dir at: $SERVED"
    for f in trace.log make-db.txt; do
        if [[ ! -f "$SERVED/$f" ]]; then
            echo "  expected file $f missing under $SERVED"
            ls -la "$SERVED" || true
            exit 1
        fi
    done
    echo "  trace.log + make-db.txt both visible — _trace_repo load-time path proven"
else
    echo "== bb_clientd not on PATH; skipping mount-half (publish/lookup wire half PASS) =="
fi

echo "== e2e-meta-autotools-round2-live: PASS =="
