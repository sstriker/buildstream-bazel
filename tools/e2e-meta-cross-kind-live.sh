#!/usr/bin/env bash
# e2e-meta-cross-kind-live: end-to-end live-AC gate for the
# cross-element configure-step bootstrap (PR-4). Two-element
# fixture (testdata/meta-project/cross-kind/):
#
#   - auto-prod (kind:autotools): the bundle producer.
#   - cons (kind:cmake): the bundle consumer; does
#     find_package(autoprod CONFIG REQUIRED).
#
# Pre-PR-4 the cons converter genrule's cmake configure would
# fail this find_package — cmakeDepBundleLabels filtered to
# kind=cmake deps, silently skipping auto-prod, leaving $PREFIX
# empty for the autoprod package.
#
# Post-PR-4 the flow is:
#
#   1. (Pre-stage, simulating a previous round-2 trace_build run
#      that already converged auto-prod.) Stage a synthetic
#      cmake-config bundle that includes lib/cmake/autoprod/
#      autoprodConfig.cmake. Publish it to the AC under
#      SyntheticConfigDigest(auto-prod's srckey).
#   2. bazel build //elements/cons:cons_converted in project A.
#      The cons element's converter genrule consumes
#      :auto-prod_trace_load via srcs. trace_load's action shells
#      to trace-lookup, materializes the published bundle into
#      bazel-bin/elements/auto-prod/auto-prod_trace_load/
#      cmake-config-bundle.tar. The converter's dep-extract
#      shell loop untars it into $PREFIX/lib/cmake/autoprod/.
#      cmake configure resolves find_package(autoprod CONFIG).
#      convert-element-cmake emits a fine cc_library for cons.
#   3. Assert the cons BUILD.bazel.out has cc rules — proving
#      find_package succeeded, proving the bundle bytes flowed
#      correctly through the AC, through trace_load's materialize,
#      through the converter's dep-extract, into cmake configure.
#
# Skips cleanly when docker compose / buildbarn / bb_clientd /
# bazel >= 9 / cmake / ninja aren't available.

set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"

TMP="$(mktemp -d -t e2e-cross-kind-live.XXXXXX)"
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
    msg="$1"
    # BST_RE_GATE_REQUIRE flips skips into hard failures. CI sets
    # it; local dev workstations leave it unset. Without this
    # guard a green CI job can't be distinguished from a quiet
    # opt-out (every prereq check below is a skip path).
    if [ -n "${BST_RE_GATE_REQUIRE:-}" ]; then
        echo "== e2e-meta-cross-kind-live: $msg — BST_RE_GATE_REQUIRE is set, so this is a hard failure ==" >&2
        exit 1
    fi
    echo "== e2e-meta-cross-kind-live: $msg, skipping =="
    exit 0
}

if ! command -v docker >/dev/null; then
    skip_reason "docker not on PATH"
fi
if ! docker compose version >/dev/null 2>&1; then
    skip_reason "docker compose plugin missing"
fi
if ! docker info >/dev/null 2>&1; then
    skip_reason "docker daemon not reachable"
fi
if ! command -v bb_clientd >/dev/null && [[ -z "${BB_CLIENTD_BIN:-}" ]]; then
    skip_reason "bb_clientd not on PATH (this gate needs the output-side mount)"
fi
BAZEL=""
if command -v bazel >/dev/null; then
    BAZEL=$(command -v bazel)
elif command -v bazelisk >/dev/null; then
    BAZEL=$(command -v bazelisk)
fi
if [[ -z "$BAZEL" ]]; then
    skip_reason "bazel/bazelisk not on PATH"
fi
bazel_major=$("$BAZEL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in [0-9]*) ;; *) bazel_major=0 ;; esac
if [[ "$bazel_major" -lt 9 ]]; then
    skip_reason "bazel < 9 (needs --experimental_remote_output_service)"
fi
if ! command -v cmake >/dev/null; then
    skip_reason "cmake not on PATH (the cons converter runs cmake configure)"
fi

echo "== build binaries =="
make converter >/dev/null
CGO_ENABLED=0 go build -o "$repo/build/bin/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$repo/build/bin/build-tracer" ./cmd/build-tracer
CGO_ENABLED=0 go build -o "$repo/build/bin/convert-element-trace" ./cmd/convert-element-trace
CGO_ENABLED=0 go build -o "$repo/build/bin/trace-publish" ./cmd/trace-publish
CGO_ENABLED=0 go build -o "$repo/build/bin/trace-lookup" ./cmd/trace-lookup

echo "== buildbarn-up =="
make buildbarn-up >/dev/null
BUILDBARN_UP=1

# Copy the fixture and append a nonce to auto-prod's configure
# so write-a's computed srckey is unique per run (avoids stale-
# AC false positives like the autotools-round2-live gate does).
FIXTURE_SRC="$repo/testdata/meta-project/cross-kind"
FIXTURE="$TMP/fixture"
mkdir -p "$FIXTURE/sources"
cp -r "$FIXTURE_SRC/cons.bst" "$FIXTURE/"
cp -r "$FIXTURE_SRC/auto-prod.bst" "$FIXTURE/"
cp -r "$FIXTURE_SRC/sources/." "$FIXTURE/sources/"
chmod -R u+w "$FIXTURE"
NONCE="cross-kind-live-$(date +%s)-$$-$RANDOM"
echo "# cross-kind-live nonce: $NONCE" >> "$FIXTURE/sources/auto-prod/configure"
echo "  per-run nonce: $NONCE"

echo "== write-a render =="
PROJ_A="$TMP/projA"
PROJ_B="$TMP/projB"
"$repo/build/bin/write-a" \
    --rules-package-path "$repo/rules_buildstream_bazel" \
    --bst "$FIXTURE/cons.bst" \
    --bst "$FIXTURE/auto-prod.bst" \
    --out "$PROJ_A" \
    --out-b "$PROJ_B" \
    --convert-element-cmake "$repo/build/bin/convert-element-cmake" \
    --convert-element-trace "$repo/build/bin/convert-element-trace" \
    --build-tracer-bin "$repo/build/bin/build-tracer" \
    --trace-publish-bin "$repo/build/bin/trace-publish" \
    --trace-lookup-bin "$repo/build/bin/trace-lookup"

echo "== pre-stage auto-prod's config bundle =="
# Read auto-prod's srckey (write-a computed it from the nonce-
# mutated configure). Synthesize a bundle that contains the
# autoprodConfig.cmake the cons element's find_package needs,
# then publish it to the AC under SyntheticConfigDigest.
AUTOPROD_SRCKEY=$(tr -d '[:space:]' < "$PROJ_A/elements/auto-prod/srckey.txt")
echo "  auto-prod srckey = $AUTOPROD_SRCKEY"

BUNDLE_STAGE="$TMP/bundle-stage"
mkdir -p "$BUNDLE_STAGE/lib/cmake/autoprod" "$BUNDLE_STAGE/lib/pkgconfig"
cat > "$BUNDLE_STAGE/lib/cmake/autoprod/autoprodConfig.cmake" <<'EOF'
# Synthesized by e2e-meta-cross-kind-live. cmake configure for
# the cons element resolves this on find_package(autoprod CONFIG).
if(NOT TARGET autoprod::autoprod)
    add_library(autoprod::autoprod INTERFACE IMPORTED)
endif()
EOF
cat > "$BUNDLE_STAGE/lib/pkgconfig/autoprod.pc" <<'EOF'
prefix=/
libdir=/lib
includedir=/include

Name: autoprod
Version: 0.1.0
Description: Cross-kind live-AC gate synthetic pc file.
Libs: -L${libdir} -lautoprod
Cflags: -I${includedir}
EOF
BUNDLE_TAR="$TMP/cmake-config-bundle.tar"
( cd "$BUNDLE_STAGE" && tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
    -cf "$BUNDLE_TAR" . )

# Stage a minimal trace so the trace half also publishes (the
# trace_load action queries both; bundle hit + trace miss would
# be unusual but the action handles it). The cons converter
# doesn't read the trace bytes here — it's pass-2 cmake
# configure, not the trace-driven converter — but trace_load
# materializes trace.log regardless, and the publish needs a
# trace path to satisfy --trace.
TRACE_STAGE="$TMP/trace.log"
printf 'execve("/bin/true", ["true"], 0x0) = 0\n' > "$TRACE_STAGE"
"$repo/build/bin/trace-publish" \
    --cas="$CAS_ADDR" \
    --srckey="$AUTOPROD_SRCKEY" \
    --trace="$TRACE_STAGE" \
    --config-bundle="$BUNDLE_TAR" >/dev/null
echo "  published trace + bundle for auto-prod's srckey"

echo "== bb-clientd-up =="
BB_CLIENTD_ROOT="${BB_CLIENTD_ROOT:-$TMP/bb_clientd}"
export BB_CLIENTD_ROOT
BB_CLIENTD_ROOT="$BB_CLIENTD_ROOT" BB_CLIENTD_CAS="$CAS_ADDR" make bb-clientd-up
BB_CLIENTD_UP=1
GRPC_SOCK="$BB_CLIENTD_ROOT/grpc.sock"

echo "== bazel build //elements/cons:cons_converted in project A =="
# cons's converter genrule:
#   1. Pulls auto-prod's trace_load output (action-time AC
#      lookup) — this fetches the published config bundle.
#   2. Extracts the bundle into $PREFIX/lib/cmake/autoprod/.
#   3. Runs cmake configure under $PREFIX. find_package(autoprod
#      CONFIG REQUIRED) resolves against the bundle's
#      autoprodConfig.cmake.
#   4. convert-element-cmake reads the codemodel + trace and
#      emits a fine cc_library for cons (no Tier-1 refusal).
(cd "$PROJ_A" && "$BAZEL" build \
    --action_env=CAS_GRPC_ADDR="$CAS_ADDR" \
    --experimental_remote_output_service="unix://$GRPC_SOCK" \
    //elements/cons:cons_converted)

# Assertions on the consumer's BUILD.bazel.out.
CONS_OUT="$PROJ_A/bazel-bin/elements/cons/BUILD.bazel.out"
if [[ ! -f "$CONS_OUT" ]]; then
    echo "consumer BUILD.bazel.out not produced at $CONS_OUT"
    exit 1
fi
# Fine cc_library means find_package resolved successfully —
# the bundle bytes made it from the AC into the cmake configure
# step. If the bundle didn't reach configure, cmake would have
# failed with "Could not find a package configuration file
# provided by autoprod" and convert-element-cmake would Tier-1
# with cmake-failed.
if ! grep -qE '^cc_library' "$CONS_OUT"; then
    echo "consumer BUILD.bazel.out missing cc_library — find_package(autoprod) likely didn't resolve"
    cat "$CONS_OUT"
    exit 1
fi
if ! grep -qF 'name = "cons"' "$CONS_OUT"; then
    echo "consumer BUILD.bazel.out missing the cons target"
    cat "$CONS_OUT"
    exit 1
fi
echo "  consumer BUILD.bazel.out has cc_library — find_package(autoprod) resolved through the AC"

# trace_load action materialized the bundle as a declared output
# (PR-4: expect_config_bundle = True). Verify the file landed.
BUNDLE_OUT="$PROJ_A/bazel-bin/elements/auto-prod/auto-prod_trace_load/cmake-config-bundle.tar"
if [[ ! -f "$BUNDLE_OUT" ]]; then
    echo "trace_load did not declare cmake-config-bundle.tar at $BUNDLE_OUT"
    exit 1
fi
echo "  trace_load materialized auto-prod's cmake-config-bundle.tar"

# Byte equivalence: the bundle bazel materialized should equal
# the one we published (round-trip through buildbarn's CAS).
if ! cmp -s "$BUNDLE_TAR" "$BUNDLE_OUT"; then
    echo "trace_load's materialized bundle bytes diverge from the published bytes"
    exit 1
fi
echo "  bundle bytes equal published bytes — full round-trip OK"

echo "== e2e-meta-cross-kind-live: PASS =="
