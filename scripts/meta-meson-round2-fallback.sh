#!/bin/sh
# meta-meson-round2-fallback.sh — render-half acceptance gate for
# kind:meson's Phase B fallback shape. Sister of
# scripts/meta-cmake-round2-fallback.sh; see
# docs/design/meson-round2-fallback.md for the architecture.
#
# When the operator passes --meson-round2-fallback +
# --convert-element-meson + --build-tracer-bin + --trace-publish-bin
# + --trace-lookup-bin to write-a, every kind:meson element renders
# with:
#
#   1. Project A's per-element converter genrule threads
#      --unsupported-target-fallback=true into convert-element-meson
#      so native-lowering refusals (subproject / custom_target /
#      generated_sources / cross-compile / unresolved-dependency /
#      unknown target type) produce the install-plan-driven
#      placeholder shape (per-target cc_import / sh_binary stubs +
#      extract genrule pointing at install_tree.tar) instead of
#      Tier-1 exit.
#   2. Project B's per-element BUILD emits a real install genrule
#      wrapping `meson setup --prefix=/ --libdir=lib + ninja + meson
#      install --destdir + tar` under build-tracer, plus inline
#      trace-publish (when CAS_GRPC_ADDR is set in the action env).
#   3. build-tracer + trace-publish + trace-lookup stage into both
#      projects' tools/ so the //tools:X labels resolve from either
#      side.
#
# The Bazel-build half is intentionally out of scope here (the wire-
# level publish/lookup contract is unit-tested via cas.LocalStore in
# cmd/trace-{publish,lookup}/main_test.go; the bazel-side end-to-end
# is queued behind the trace-driven convergence research follow-on,
# since v1 doesn't yet wire A's load-time @trace_<elem>//:trace
# lookup into a refusal-refinement loop).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-meson" ./converter/cmd/convert-element-meson
CGO_ENABLED=0 go build -o "$bin_dir/build-tracer" ./cmd/build-tracer
CGO_ENABLED=0 go build -o "$bin_dir/trace-publish" ./cmd/trace-publish
CGO_ENABLED=0 go build -o "$bin_dir/trace-lookup" ./cmd/trace-lookup

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

A="$work_dir/A"
B="$work_dir/B"

# Reuse the meson-greet fixture — kind:meson element whose round-2-
# fallback rendered shape we want to assert.
fixture="testdata/meta-project"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/meson-greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --convert-element-meson "$bin_dir/convert-element-meson" \
    --build-tracer-bin "$bin_dir/build-tracer" \
    --trace-publish-bin "$bin_dir/trace-publish" \
    --trace-lookup-bin "$bin_dir/trace-lookup" \
    --meson-round2-fallback

# A-side: converter genrule threads the fallback flag AND pulls
# :meson-greet_trace_load into srcs (the action-time AC lookup;
# trace-driven convergence research follow-on teaches
# convert-element-meson to consume the trace bytes).
a_build="$A/elements/meson-greet/BUILD.bazel"
for marker in \
    '--unsupported-target-fallback=true' \
    '"//tools:convert-element-meson"' \
    'load("@rules_buildstream_bazel//rules:traces.bzl", "trace_load")' \
    'name = "meson-greet_trace_load"' \
    'expect_make_db = False' \
    '":meson-greet_trace_load"'; do
    if ! grep -qF -- "$marker" "$a_build"; then
        echo "meta-meson-round2-fallback: A-side BUILD missing marker: $marker" >&2
        cat "$a_build" >&2
        exit 1
    fi
done
if grep -qF -- '"@trace_meson-greet//:trace"' "$a_build"; then
    echo "meta-meson-round2-fallback: A-side BUILD unexpectedly contains legacy @trace_*//:trace label" >&2
    cat "$a_build" >&2
    exit 1
fi

# rules/traces.bzl renders in both projects. tools/traces.json is
# no longer emitted.
for path in \
    "$A/rules/traces.bzl" \
    "$B/rules/traces.bzl"; do
    if [ -f "$path" ]; then
        echo "meta-meson-round2-fallback: $path unexpectedly rendered (rules now load from @rules_buildstream_bazel//rules)" >&2
        exit 1
    fi
done
for path in \
    "$A/tools/traces.json" \
    "$B/tools/traces.json"; do
    if [ -f "$path" ]; then
        echo "meta-meson-round2-fallback: $path unexpectedly emitted (legacy load-time wiring)" >&2
        exit 1
    fi
done

# A's MODULE.bazel must NOT declare the legacy `traces` extension.
mod_a="$A/MODULE.bazel"
for unwanted in \
    'use_extension("//rules:traces.bzl", "traces")' \
    '"trace_meson-greet"'; do
    if grep -qF -- "$unwanted" "$mod_a"; then
        echo "meta-meson-round2-fallback: A MODULE.bazel unexpectedly contains legacy traces extension wiring: $unwanted" >&2
        cat "$mod_a" >&2
        exit 1
    fi
done

# B-side: real install genrule replaces the placeholder.
b_build="$B/elements/meson-greet/BUILD.bazel"
for marker in \
    'name = "meson-greet_trace_build"' \
    'tags = ["trace_build"]' \
    '"install_tree.tar"' \
    '"trace.log"' \
    '"//tools:build-tracer"' \
    '"//tools:trace-publish"' \
    'meson setup' \
    '--prefix=/' \
    '--libdir=lib' \
    'ninja -C' \
    'meson install' \
    'CAS_GRPC_ADDR' \
    '--srckey=' \
    '--config-bundle=' \
    'CONFIG_BUNDLE_DIR'; do
    if ! grep -qF -- "$marker" "$b_build"; then
        echo "meta-meson-round2-fallback: B-side BUILD missing marker: $marker" >&2
        cat "$b_build" >&2
        exit 1
    fi
done
if grep -qF 'BUILD_NOT_YET_STAGED' "$b_build"; then
    echo "meta-meson-round2-fallback: B-side still has the placeholder; should have the install genrule" >&2
    cat "$b_build" >&2
    exit 1
fi

# srckey.txt is staged in B (trace-publish reads it).
if [ ! -f "$B/elements/meson-greet/srckey.txt" ]; then
    echo "meta-meson-round2-fallback: missing $B/elements/meson-greet/srckey.txt" >&2
    exit 1
fi

# build-tracer + trace-publish + trace-lookup stage into both
# projects' tools/. Wiring all three at once means the trace-driven
# convergence research follow-on (teaching convert-element-meson to
# consume @trace_<elem>//:trace to refine refusals) is purely a
# converter-side change — no further write-a / staging work.
for path in \
    "$A/tools/convert-element-meson" \
    "$A/tools/build-tracer" \
    "$A/tools/trace-publish" \
    "$A/tools/trace-lookup" \
    "$B/tools/build-tracer" \
    "$B/tools/trace-publish" \
    "$B/tools/trace-lookup"; do
    if [ ! -x "$path" ]; then
        echo "meta-meson-round2-fallback: missing executable $path" >&2
        exit 1
    fi
done

# Standalone-converter assertion: when meson is on PATH, run
# convert-element-meson directly against a fixture with a refusal
# (custom_target with @CURRENT_SOURCE_DIR@ — v1 refuses) and verify
# the fallback emits the install-plan-driven placeholder shape:
# cc_import for the static lib, sh_binary for the executable, extract
# genrule untarring install_tree.tar.
if command -v meson >/dev/null; then
    refusal_fixture="$work_dir/refusal-fixture"
    mkdir -p "$refusal_fixture/include" "$refusal_fixture/src"
    cat > "$refusal_fixture/meson.build" <<'EOF'
project('refuser', 'c')
inc = include_directories('include')
libfoo = static_library('foo', 'src/foo.c', include_directories: inc, install: true)
# @CURRENT_SOURCE_DIR@ in custom_target argv triggers
# unsupported-meson-custom-target in the v1 lift.
ct = custom_target('gen', output: 'gen.h',
  command: ['cp', '@CURRENT_SOURCE_DIR@/include/foo.h', '@OUTPUT@'])
executable('foo-bin', 'src/main.c', link_with: libfoo,
  include_directories: inc, install: true)
install_headers('include/foo.h')
EOF
    cat > "$refusal_fixture/include/foo.h" <<'EOF'
const char *foo_msg(void);
EOF
    cat > "$refusal_fixture/src/foo.c" <<'EOF'
#include "foo.h"
const char *foo_msg(void) { return "foo"; }
EOF
    cat > "$refusal_fixture/src/main.c" <<'EOF'
#include "foo.h"
int main(void) { return foo_msg() == 0; }
EOF
    out_build="$work_dir/standalone-BUILD.out"

    # First: without --unsupported-target-fallback, expect Tier-1.
    if "$bin_dir/convert-element-meson" \
        --source-root="$refusal_fixture" \
        --out-build="$out_build" >"$work_dir/standalone-strict.log" 2>&1; then
        echo "meta-meson-round2-fallback: standalone converter unexpectedly SUCCEEDED in strict mode (should have refused custom_target)" >&2
        cat "$work_dir/standalone-strict.log" >&2
        exit 1
    fi
    if ! grep -qF 'unsupported-meson-custom-target' "$work_dir/standalone-strict.log"; then
        echo "meta-meson-round2-fallback: strict-mode refusal missing typed code 'unsupported-meson-custom-target':" >&2
        cat "$work_dir/standalone-strict.log" >&2
        exit 1
    fi

    # Then: with the fallback flag, expect success + placeholder shape.
    "$bin_dir/convert-element-meson" \
        --source-root="$refusal_fixture" \
        --out-build="$out_build" \
        --unsupported-target-fallback=true >"$work_dir/standalone-fallback.log" 2>&1
    for marker in \
        '_install_tree_extract' \
        '"install_tree.tar"' \
        '"install_tree/lib/libfoo.a"' \
        '"install_tree/bin/foo-bin"' \
        '"install_tree/include/foo.h"' \
        'cc_import' \
        'sh_binary' \
        'static_library = "install_tree/lib/libfoo.a"' \
        'srcs = ["install_tree/bin/foo-bin"]' \
        'meson-codegen-target-fallback'; do
        if ! grep -qF -- "$marker" "$out_build"; then
            echo "meta-meson-round2-fallback: standalone fallback BUILD missing marker: $marker" >&2
            cat "$out_build" >&2
            exit 1
        fi
    done
    echo "meta-meson-round2-fallback: standalone converter ok (strict refuses; fallback emits placeholder)"
else
    echo "meta-meson-round2-fallback: meson not on PATH, skipping standalone converter check"
fi

echo "meta-meson-round2-fallback: render OK"
echo "meta-meson-round2-fallback: ok"
