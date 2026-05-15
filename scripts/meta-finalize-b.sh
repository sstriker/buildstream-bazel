#!/bin/sh
# meta-finalize-b.sh — render-half acceptance gate for
# cmd/finalize-b, the deliverable handover step in the cross-
# element configure-step bootstrap stack. See
# docs/design/finalize-b.md for the architectural shape.
#
# Builds finalize-b, stages a synthetic "converged project B"
# tree with a mix of converged-fine elements (have cc rules) and
# unconverged elements (only trace_build), invokes finalize-b
# with --in / --out, and asserts:
#
#   - The converged element's BUILD has cc rules preserved + the
#     trace_load + trace_build + intermediate filegroups +
#     load() statements pruned.
#   - The unconverged element's BUILD is preserved verbatim.
#   - The MODULE.bazel's bazel_dep on rules_buildstream_bazel is
#     pruned IFF no surviving BUILD references it (i.e. in the
#     "every element converged" subcase).
#   - Re-running finalize-b on its own output is byte-stable
#     (idempotence).
#   - The tool refuses to write to a non-empty --out.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

CGO_ENABLED=0 go build -o "$bin_dir/finalize-b" ./cmd/finalize-b

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Subcase 1: every element is converged → MODULE.bazel pruned.
B="$work_dir/B-converged"
mkdir -p "$B/elements/demo"
cat > "$B/MODULE.bazel" <<'EOF'
module(name = "p", version = "0.0.0")

bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(
    module_name = "rules_buildstream_bazel",
    path = "/abs",
)

bazel_dep(name = "rules_cc", version = "0.0.17")
EOF
cat > "$B/BUILD.bazel" <<'EOF'
# root
EOF
cat > "$B/elements/demo/BUILD.bazel" <<'EOF'
load("@rules_buildstream_bazel//rules:traces.bzl", "trace_load")
load("@rules_cc//cc:defs.bzl", "cc_library")

trace_load(
    name = "demo_trace_load",
    srckey = "abc",
    trace_lookup = "//tools:trace-lookup",
)

genrule(
    name = "demo_trace_build",
    srcs = ["s"],
    outs = ["install_tree.tar"],
    cmd = "echo",
    tags = ["trace_build"],
)

filegroup(
    name = "install_tree.tar",
    srcs = ["install_tree.tar"],
)

cc_library(
    name = "libdemo",
    srcs = ["demo.c"],
)
EOF

OUT="$work_dir/out-converged"
"$bin_dir/finalize-b" --in "$B" --out "$OUT"

# Per-element BUILD: cc_library preserved, scaffolding removed.
build_out="$OUT/elements/demo/BUILD.bazel"
for marker in \
    'cc_library' \
    'name = "libdemo"' \
    'load("@rules_cc//cc:defs.bzl"'; do
    if ! grep -qF -- "$marker" "$build_out"; then
        echo "meta-finalize-b: converged BUILD missing marker $marker" >&2
        cat "$build_out" >&2
        exit 1
    fi
done
for banned in \
    'trace_load(' \
    '"demo_trace_build"' \
    '"trace_build"' \
    '"install_tree.tar"' \
    '@rules_buildstream_bazel//rules:traces.bzl'; do
    if grep -qF -- "$banned" "$build_out"; then
        echo "meta-finalize-b: converged BUILD unexpectedly contains $banned" >&2
        cat "$build_out" >&2
        exit 1
    fi
done

# MODULE.bazel: rules_buildstream_bazel pruned, rules_cc stays.
mod_out="$OUT/MODULE.bazel"
if grep -qF 'rules_buildstream_bazel' "$mod_out"; then
    echo "meta-finalize-b: converged MODULE.bazel still contains rules_buildstream_bazel" >&2
    cat "$mod_out" >&2
    exit 1
fi
if ! grep -qF 'rules_cc' "$mod_out"; then
    echo "meta-finalize-b: converged MODULE.bazel missing rules_cc" >&2
    cat "$mod_out" >&2
    exit 1
fi

# Idempotence: re-finalizing should produce byte-identical output.
OUT2="$work_dir/out-converged-2"
"$bin_dir/finalize-b" --in "$OUT" --out "$OUT2"
if ! diff -ru "$OUT" "$OUT2" > "$work_dir/diff.log"; then
    echo "meta-finalize-b: idempotence broken; finalize-b(finalize-b(x)) != finalize-b(x)" >&2
    cat "$work_dir/diff.log" >&2
    exit 1
fi

# Subcase 2: at least one element unconverged → rules_buildstream_bazel
# stays.
BMIX="$work_dir/B-mixed"
mkdir -p "$BMIX/elements/conv" "$BMIX/elements/unconv"
cp "$B/MODULE.bazel" "$BMIX/MODULE.bazel"
cp "$B/BUILD.bazel" "$BMIX/BUILD.bazel"
cp "$B/elements/demo/BUILD.bazel" "$BMIX/elements/conv/BUILD.bazel"
sed -i 's/demo_trace_load/unconv_trace_load/g; s/demo_trace_build/unconv_trace_build/g' "$BMIX/elements/conv/BUILD.bazel"
# Unconverged: no cc rules.
cat > "$BMIX/elements/unconv/BUILD.bazel" <<'EOF'
load("@rules_buildstream_bazel//rules:traces.bzl", "trace_load")

trace_load(
    name = "unconv_trace_load",
    srckey = "xyz",
    trace_lookup = "//tools:trace-lookup",
)

genrule(
    name = "unconv_trace_build",
    srcs = ["s"],
    outs = ["install_tree.tar"],
    cmd = "echo",
    tags = ["trace_build"],
)
EOF

OUTMIX="$work_dir/out-mixed"
"$bin_dir/finalize-b" --in "$BMIX" --out "$OUTMIX"

# Unconverged element: preserved verbatim.
unconv_out="$OUTMIX/elements/unconv/BUILD.bazel"
for marker in \
    'trace_load(' \
    'unconv_trace_build' \
    '@rules_buildstream_bazel'; do
    if ! grep -qF -- "$marker" "$unconv_out"; then
        echo "meta-finalize-b: unconverged element should be preserved; missing $marker" >&2
        cat "$unconv_out" >&2
        exit 1
    fi
done
# MODULE.bazel keeps rules_buildstream_bazel.
if ! grep -qF 'rules_buildstream_bazel' "$OUTMIX/MODULE.bazel"; then
    echo "meta-finalize-b: mixed-converged MODULE.bazel should still contain rules_buildstream_bazel" >&2
    cat "$OUTMIX/MODULE.bazel" >&2
    exit 1
fi

# Refusing to overwrite --out.
set +e
"$bin_dir/finalize-b" --in "$B" --out "$OUT" 2>"$work_dir/overwrite-err.log"
overwrite_rc=$?
set -e
if [ "$overwrite_rc" -eq 0 ]; then
    echo "meta-finalize-b: --out already-exists case unexpectedly succeeded" >&2
    exit 1
fi
if ! grep -qF 'already exists' "$work_dir/overwrite-err.log"; then
    echo "meta-finalize-b: --out already-exists error missing the expected diagnostic" >&2
    cat "$work_dir/overwrite-err.log" >&2
    exit 1
fi

echo "meta-finalize-b: ok"
