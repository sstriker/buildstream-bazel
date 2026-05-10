#!/bin/sh
# spike-autotools-trace.sh — converter-pipeline validation gate.
#
# Drives convert-element-trace end-to-end against two
# fixtures (autotools-greet, autotools-libapp): stages the
# source tree, runs ./configure + make under build-tracer,
# feeds the resulting trace into convert-element-trace,
# asserts the rendered BUILD.bazel.out matches expectations.
#
# This validates the converter pipeline in isolation. The
# write-a integration (kind:autotools handler wrapping the
# install genrule in build-tracer + convert-element-trace)
# is exercised end-to-end via scripts/meta-autotools-native.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

bin="$work_dir/convert-element-trace"
tracer="$work_dir/build-tracer"
CGO_ENABLED=0 go build -o "$bin" ./cmd/convert-element-trace
CGO_ENABLED=0 go build -o "$tracer" ./cmd/build-tracer

# run_fixture stages, traces, converts, asserts.
#
#   $1 — fixture name (testdata/meta-project/<name>/sources/)
#   $2 — assertion grep file (lines = required substrings)
#   $3 — optional imports.json (relative to repo root)
run_fixture() {
    name="$1"
    asserts_file="$2"
    imports="${3:-}"

    fixture_root="testdata/meta-project/$name/sources"
    if [ ! -d "$fixture_root" ]; then
        echo "spike-autotools-trace: missing fixture $fixture_root" >&2
        exit 1
    fi

    src="$work_dir/$name-src"
    cp -r "$fixture_root" "$src"
    chmod -R u+w "$src"

    trace="$work_dir/$name-trace.log"
    build_out="$work_dir/$name-BUILD.bazel.out"

    (cd "$src" && "$tracer" --out="$trace" -- \
        sh -c './configure --prefix=/usr >/dev/null 2>&1 && make >/dev/null 2>&1') \
        || {
        echo "spike-autotools-trace[$name]: build failed under tracer" >&2
        head -100 "$trace" >&2
        exit 1
    }

    if [ -n "$imports" ]; then
        "$bin" --trace "$trace" --out-build "$build_out" --imports-manifest "$imports"
    else
        "$bin" --trace "$trace" --out-build "$build_out"
    fi

    while IFS= read -r marker; do
        [ -z "$marker" ] && continue
        if ! grep -qF "$marker" "$build_out"; then
            echo "spike-autotools-trace[$name]: rendered BUILD.bazel.out missing marker: $marker" >&2
            cat "$build_out" >&2
            exit 1
        fi
    done < "$asserts_file"

    echo "spike-autotools-trace[$name]: ok"
    echo "--- $name BUILD.bazel.out ---"
    cat "$build_out"
}

# autotools-greet: single cc_binary.
greet_asserts="$work_dir/greet-asserts.txt"
cat > "$greet_asserts" <<'EOF'
cc_binary(
name = "greet"
srcs = ["greet.c"]
EOF
run_fixture autotools-greet "$greet_asserts"

# autotools-libapp: cc_library + cc_binary, with both in-trace
# (`-lfoo`) and imports-manifest (`-lz`) dep edges.
libapp_asserts="$work_dir/libapp-asserts.txt"
cat > "$libapp_asserts" <<'EOF'
load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")
cc_library(
name = "foo"
srcs = ["bar.c", "foo.c"]
linkstatic = True
cc_binary(
name = "myapp"
srcs = ["myapp.c"]
deps = ["//elements/zlib:zlib", ":foo"]
EOF
run_fixture autotools-libapp "$libapp_asserts" \
    "testdata/meta-project/autotools-libapp/imports.json"
