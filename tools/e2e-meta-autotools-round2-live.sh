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
#      resolves is actually mountable: bb_clientd serves it under
#      the canonical layout
#      <mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/.
#      This is the load-time resolution the _trace_repo Bazel rule
#      will perform when project A's converter genrule's srcs
#      reference @trace_<elem>//:trace.
#   4. (When bb_clientd + Bazel ≥ 9 are on PATH) Drive a real
#      bazel build of project A's per-element converter genrule.
#      The _trace_repo rule's load-time AC lookup hits the entry
#      we publish; the converter consumes the served trace dir
#      (resolved through the parameterised CAS_DIRECTORY_PREFIX);
#      the resulting BUILD.bazel.out emits cc rules instead of the
#      placeholder shape. End-to-end coverage of the round-2
#      _trace_repo path including --experimental_remote_output_service.
#
# Pipeline:
#
#   1. Stand up buildbarn (`make buildbarn-up`).
#   2. Build trace-publish + trace-lookup.
#   3. Stage a synthetic trace dir (trace.log + make-db.txt).
#   4. trace-publish against $CAS_ADDR; lookup; assert digests match.
#   5. (If bb_clientd available) bring it up, probe canonical layout,
#      assert trace.log + make-db.txt visible.
#   6. (If bb_clientd + Bazel ≥ 9) render projects A + B from
#      the autotools-greet fixture, re-publish under fixture's
#      srckey, bazel build //elements/greet:greet_build with the
#      parameterised env vars, assert BUILD.bazel.out is the fine
#      shape (cc_binary, not the placeholder).
#
# kind coverage: the publish/lookup wire contract this gate
# exercises (steps 1–3) is kind-agnostic — it tests
# SyntheticActionDigest(srckey) round-tripping through a real
# REAPI endpoint, which any kind that uses rules/traces.bzl's
# _trace_repo hits the same way. The set is whichever kinds
# write-a's traceDrivenSrckeyPatternsForKind returns non-nil
# for: kind:autotools (special-cased), any pipeline kind
# whose handler sets traceDrivenSrckeyPatterns (kind:make,
# kind:makemaker, kind:modulebuild, kind:manual, kind:script
# as of this writing — the list grows as new pipeline kinds
# opt in), and kind:cmake when --cmake-round2-fallback is
# enabled. Step 6's bazel-build half is autotools-specific
# (asserts the autotools converter emits fine cc rules from
# a published trace); the kind:cmake-round2-fallback
# equivalent is the render-half gate
# scripts/meta-cmake-round2-fallback.sh (which covers the
# shape) plus the kind-agnostic wire round-trip here.
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
PUB_LOG="$TMP/trace-publish.log"
if ! PUB_OUT=$("$repo/build/bin/trace-publish" \
        --cas="$CAS_ADDR" \
        --srckey="$SRCKEY" \
        --trace="$STAGE/trace.log" \
        --make-db="$STAGE/make-db.txt" 2>"$PUB_LOG"); then
    echo "trace-publish failed:"
    cat "$PUB_LOG"
    exit 1
fi
echo "  trace-publish printed: $PUB_OUT"
if [[ -z "$PUB_OUT" ]]; then
    echo "trace-publish printed nothing — expected '<hash>/<size>'"
    cat "$PUB_LOG"
    exit 1
fi

echo "== trace-lookup =="
LK_LOG="$TMP/trace-lookup.log"
if ! LK_OUT=$("$repo/build/bin/trace-lookup" \
        --cas="$CAS_ADDR" \
        --srckey="$SRCKEY" 2>"$LK_LOG"); then
    echo "trace-lookup failed:"
    cat "$LK_LOG"
    exit 1
fi
echo "  trace-lookup printed: $LK_OUT"
if [[ "$PUB_OUT" != "$LK_OUT" ]]; then
    echo "trace-lookup digest mismatch:"
    echo "  publish: $PUB_OUT"
    echo "  lookup:  $LK_OUT"
    cat "$LK_LOG"
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
# blob is mountable AND drive a real bazel build of project A's
# converter genrule end-to-end through the parameterised
# CAS_DIRECTORY_PREFIX path. This is the round-2 fine-path proof
# point — `_trace_repo`'s load-time AC lookup hits the entry
# we publish below, the converter consumes the served trace dir,
# and the resulting BUILD.bazel.out emits cc rules instead of
# the placeholder shape.
if command -v bb_clientd >/dev/null || [[ -n "${BB_CLIENTD_BIN:-}" ]]; then
    BB_CLIENTD_ROOT="${BB_CLIENTD_ROOT:-$TMP/bb_clientd}"
    export BB_CLIENTD_ROOT
    echo "== bb-clientd-up =="
    BB_CLIENTD_ROOT="$BB_CLIENTD_ROOT" BB_CLIENTD_CAS="$CAS_ADDR" make bb-clientd-up
    BB_CLIENTD_UP=1
    MOUNT="$BB_CLIENTD_ROOT/mount"
    GRPC_SOCK="$BB_CLIENTD_ROOT/grpc.sock"
    DIGEST_HASH=$(echo "$PUB_OUT" | cut -d/ -f1)
    echo "  digest hash = $DIGEST_HASH"
    # Canonical bb_clientd layout is
    #   <mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/
    # With this script's deploy (deploy/buildbarn/config/bb_clientd.jsonnet:
    # empty instance, sha256) the path collapses to
    # <mount>/cas//blobs/sha256/directory/<digest>/, which the OS
    # normalises to <mount>/cas/blobs/sha256/directory/<digest>/.
    # Probe that shape; tolerate two older flat layouts a previous
    # bb_clientd version may have served (kept for now so a daemon
    # version bump doesn't immediately break the gate). This probe
    # set does NOT cover non-default instance / digest_function
    # configs — when this script's deploy moves to a different
    # config, both this probe loop and the bazel-build half's
    # CAS_DIRECTORY_PREFIX value have to be updated together.
    SERVED=""
    for candidate in \
        "$MOUNT/cas/blobs/sha256/directory/$DIGEST_HASH" \
        "$MOUNT/cas/$DIGEST_HASH" \
        "$MOUNT/blobs/directory/$DIGEST_HASH"; do
        if test -d "$candidate" 2>/dev/null; then
            SERVED="$candidate"
            break
        fi
    done
    if [[ -z "$SERVED" ]]; then
        echo "bb_clientd mount does not serve published Directory under \$MOUNT=$MOUNT"
        echo "(verify deploy/buildbarn/config/bb_clientd.jsonnet matches the daemon version's schema;"
        echo " or — if you moved to a non-default instance / digest_function — update"
        echo " the probe loop above + this script's CAS_DIRECTORY_PREFIX value to match)"
        ls -la "$MOUNT" || true
        exit 1
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

    # bazel-build half: requires bazel >= 9 (RemoteOutputService).
    # Skips cleanly when bazel < 9 / not on PATH; the mount-half
    # above is still a meaningful gate without it.
    BAZEL=""
    if command -v bazel >/dev/null; then
        BAZEL=$(command -v bazel)
    elif command -v bazelisk >/dev/null; then
        BAZEL=$(command -v bazelisk)
    fi
    bazel_major=0
    if [[ -n "$BAZEL" ]]; then
        bazel_major=$("$BAZEL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
        case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
    fi
    if [[ -z "$BAZEL" ]] || [[ "$bazel_major" -lt 9 ]]; then
        echo "== bazel < 9 / not on PATH; skipping bazel-build half (mount-half PASS) =="
    else
        echo "== bazel-build half (round-2 fine path) =="
        # The round-2 fine path needs every binary the genrules
        # in projects A and B reference: write-a, the converter
        # binaries that get staged into //tools, and the trace
        # publisher/lookup we already built above.
        make converter >/dev/null
        CGO_ENABLED=0 go build -o "$repo/build/bin/write-a" ./cmd/write-a
        CGO_ENABLED=0 go build -o "$repo/build/bin/build-tracer" ./cmd/build-tracer
        CGO_ENABLED=0 go build -o "$repo/build/bin/convert-element-autotools" ./cmd/convert-element-autotools

        # Copy the fixture to a tmp dir and append a per-run nonce
        # to a file matching autotoolsSrckeyPatterns (configure is
        # content-included; see cmd/write-a/handler_autotools_native.go).
        # This makes write-a's computed srckey unique per run, so a
        # stale AC entry left over from a prior run can't satisfy
        # the lookup and produce a false positive when this run's
        # republish path is broken — the AC only hits when the
        # publish step BELOW lands an entry under THIS run's key.
        FIXTURE_SRC="$repo/testdata/meta-project/autotools-greet"
        FIXTURE="$TMP/fixture"
        mkdir -p "$FIXTURE"
        cp -r "$FIXTURE_SRC/." "$FIXTURE/"
        chmod -R u+w "$FIXTURE"
        NONCE="round2-live-$(date +%s)-$$-$RANDOM"
        echo "# round2-live nonce: $NONCE" >> "$FIXTURE/sources/configure"
        echo "  per-run nonce: $NONCE"

        PROJ_A="$TMP/projA"
        PROJ_B="$TMP/projB"
        rm -rf "$PROJ_A" "$PROJ_B"
        echo "  rendering projects A + B from $FIXTURE"
        "$repo/build/bin/write-a" \
            --bst "$FIXTURE/greet.bst" \
            --out "$PROJ_A" \
            --out-b "$PROJ_B" \
            --convert-element "$repo/build/bin/convert-element" \
            --convert-element-autotools "$repo/build/bin/convert-element-autotools" \
            --build-tracer-bin "$repo/build/bin/build-tracer" \
            --trace-publish-bin "$repo/build/bin/trace-publish" \
            --trace-lookup-bin "$repo/build/bin/trace-lookup"

        # The wire-roundtrip half above used a synthetic per-run
        # srckey. The bazel build below evaluates `_trace_repo`
        # against the per-run-mutated fixture's srckey, which is
        # unique per run thanks to the nonce above. Republish the
        # same synthetic trace under THAT srckey so the load-time
        # AC lookup hits.
        FIXTURE_SRCKEY=$(tr -d '[:space:]' < "$PROJ_A/elements/greet/srckey.txt")
        echo "  fixture srckey (nonce-mutated): $FIXTURE_SRCKEY"
        "$repo/build/bin/trace-publish" \
            --cas="$CAS_ADDR" \
            --srckey="$FIXTURE_SRCKEY" \
            --trace="$STAGE/trace.log" \
            --make-db="$STAGE/make-db.txt" >/dev/null
        echo "  republished trace under fixture srckey"

        echo "  bazel build //elements/greet:greet_build"
        # CAS_GRPC_ADDR / TRACE_LOOKUP_BIN — read by _trace_repo
        # at load time; it shells out to trace-lookup to query
        # the AC. CAS_FUSE_MOUNT / CAS_DIRECTORY_PREFIX — read
        # by _trace_repo to compute the symlink target into the
        # bb_clientd mount. --experimental_remote_output_service
        # — Bazel 9's xattr-fast-path replacement; bb_clientd
        # serves the input bytes through this socket so Bazel
        # trusts the daemon's reported digests.
        (cd "$PROJ_A" && "$BAZEL" build \
            --repo_env=CAS_GRPC_ADDR="$CAS_ADDR" \
            --repo_env=CAS_FUSE_MOUNT="$MOUNT" \
            --repo_env=CAS_DIRECTORY_PREFIX="cas//blobs/sha256" \
            --repo_env=TRACE_LOOKUP_BIN="$repo/build/bin/trace-lookup" \
            --experimental_remote_output_service="unix://$GRPC_SOCK" \
            //elements/greet:greet_build)

        BUILD_OUT="$PROJ_A/bazel-bin/elements/greet/BUILD.bazel.out"
        if [[ ! -f "$BUILD_OUT" ]]; then
            echo "round-2 bazel build did not produce BUILD.bazel.out at $BUILD_OUT"
            exit 1
        fi
        if grep -qF "Round-2 boot phase" "$BUILD_OUT"; then
            echo "round-2 BUILD.bazel.out is the placeholder — _trace_repo's AC lookup didn't hit"
            cat "$BUILD_OUT"
            exit 1
        fi
        # Synthetic trace was a single cc compile + link of greet.c
        # (see "stage synthetic trace dir" above). The converter
        # should recover a cc_binary(name="greet", srcs=["greet.c"]).
        for marker in 'cc_binary' 'name = "greet"' 'srcs = ["greet.c"]'; do
            if ! grep -qF -- "$marker" "$BUILD_OUT"; then
                echo "round-2 fine BUILD.bazel.out missing marker: $marker"
                cat "$BUILD_OUT"
                exit 1
            fi
        done
        echo "  round-2 fine BUILD.bazel.out OK — _trace_repo + parameterised CAS_DIRECTORY_PREFIX proven end-to-end"
    fi
else
    echo "== bb_clientd not on PATH; skipping mount-half + bazel-build half (publish/lookup wire half PASS) =="
fi

echo "== e2e-meta-autotools-round2-live: PASS =="
