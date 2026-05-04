#!/bin/sh
# meta-autotools-determinism.sh — verifies build-tracer's
# canonical trace + filtered make-db.txt are byte-stable
# across two clean runs of the same autotools build.
#
# Why: the trace + make-db are foundation inputs to the
# 2-phase srckey design. If their bytes drift across
# runs of an otherwise-identical build, a registered
# trace (round-1) can't be reused by a downstream lookup
# (round-2) — every action would cache-miss and the
# narrow-cache value evaporates. This gate locks in the
# determinism property at the trace-emission layer.
#
# Driven outside Bazel: emulates what the install genrule
# does (build-tracer wraps configure/make/make-install
# with --normalize-prefix flags pointing at INSTALL_ROOT;
# `make -np` post-processed through the same sed filter
# the genrule uses). Each run uses a fresh INSTALL_ROOT
# (different mktemp absolute path) — exactly the form
# of drift the canonical-trace work neutralizes.
#
# This is a stronger property than "the gate runs in
# Bazel and the action cache reuses outputs" — the action
# cache reusing is true by tautology. We're checking that
# IF Bazel were to re-execute the action (clean cache,
# force re-run), the bytes would match.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/build-tracer" ./cmd/build-tracer

fixture="$repo_root/testdata/meta-project/autotools-greet/sources"

# Filter idiomatic to the autotools install genrule's
# `make -np` post-processing — drop file mtimes + the
# file-count summary lines that vary across runs.
filter_make_db() {
    sed -E '
        /^#[[:space:]]+Last modified /d
        /\(device [0-9]+, inode [0-9]+\): [0-9]+ files,/d
        /^# [0-9]+ files,.*impossibilities in /d
        /^# Make data base, printed on /d
        /^# Finished Make data base on /d
    '
}

run_pipeline() {
    work="$1"
    out_trace="$2"
    out_db="$3"
    mkdir -p "$work"
    cp -r "$fixture/." "$work/"
    chmod -R u+w "$work"
    install_root="$(mktemp -d)"
    build_root="$work"
    (
        cd "$work"
        export NOCONFIGURE=1
        "$bin_dir/build-tracer" \
            --normalize-prefix="$install_root=/INSTALL_ROOT" \
            --normalize-prefix="$build_root=/BUILD_ROOT" \
            --out="$out_trace" \
            -- sh -c "
                ./configure --prefix=/usr >/dev/null
                make >/dev/null
                make install DESTDIR=\"$install_root\" >/dev/null
            "
        # make-db filter mirrors the autotools install genrule's
        # post-processing (handler_autotools_native.go's
        # autotoolsConverterStep): drop variable diagnostic
        # lines, then substitute action-time mktemp paths with
        # the same stable placeholders the trace uses.
        ( make -np 2>/dev/null || true ) \
            | filter_make_db \
            | sed -e "s|$install_root|/INSTALL_ROOT|g" \
                  -e "s|$build_root|/BUILD_ROOT|g" \
            > "$out_db"
    )
    rm -rf "$install_root"
}

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

run_pipeline "$work_dir/run1" "$work_dir/trace1.log" "$work_dir/db1.txt"
run_pipeline "$work_dir/run2" "$work_dir/trace2.log" "$work_dir/db2.txt"

sha1_trace=$(sha256sum "$work_dir/trace1.log" | cut -d' ' -f1)
sha2_trace=$(sha256sum "$work_dir/trace2.log" | cut -d' ' -f1)
sha1_db=$(sha256sum "$work_dir/db1.txt" | cut -d' ' -f1)
sha2_db=$(sha256sum "$work_dir/db2.txt" | cut -d' ' -f1)

echo "meta-autotools-determinism: trace.log run1=$sha1_trace run2=$sha2_trace"
echo "meta-autotools-determinism: make-db.txt run1=$sha1_db run2=$sha2_db"

ok=1
if [ "$sha1_trace" != "$sha2_trace" ]; then
    echo "meta-autotools-determinism: trace.log differs across runs" >&2
    diff "$work_dir/trace1.log" "$work_dir/trace2.log" | head -40 >&2
    ok=0
fi
if [ "$sha1_db" != "$sha2_db" ]; then
    echo "meta-autotools-determinism: make-db.txt differs across runs" >&2
    diff "$work_dir/db1.txt" "$work_dir/db2.txt" | head -40 >&2
    ok=0
fi

if [ "$ok" -ne 1 ]; then
    exit 1
fi

echo "meta-autotools-determinism: ok (trace.log + make-db.txt byte-stable across two clean runs)"
