#!/bin/sh
# meta-cmake-proto-cross-package.sh — render+build gate for cross-package proto
# recognition: a multi-package project where pkg/b/b.proto imports
# pkg/a/a.proto, each protoc'd in its own add_subdirectory.
#
# Exercises two things the single-package fixtures don't:
#   1. native-rule SUB-PACKAGE placement — a_proto/a_cc_proto land in //pkg/a
#      (not flattened to root), so the basename srcs=["a.proto"] resolve;
#   2. cross-package import deps — b_proto gets deps=["//pkg/a:a_proto"] from
#      b.proto's `import "pkg/a/a.proto"`, so the import resolves under Bazel.
#
# Then bazel-builds //pkg/b:b_cc_proto (proves the cross-package edge).
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/proto-cross-package"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
fail() { echo "FAIL: $1"; shift; for f in "$@"; do sed 's/^/   /' "$f" 2>/dev/null || true; done; exit 1; }

out="$work_dir/out"; mkdir -p "$out"
"$bin_dir/convert-element-cmake" --source-root "$fixture" \
    --out-build "$out/BUILD.bazel" --recognize-codegen --split-packages \
    >"$work_dir/convert.out" 2>"$work_dir/convert.err" || fail "convert failed" "$work_dir/convert.err"

[ -f "$out/pkg/a/BUILD.bazel" ] || fail "a_proto not placed in its own //pkg/a package" "$work_dir/convert.err"
[ -f "$out/pkg/b/BUILD.bazel" ] || fail "b_proto not placed in its own //pkg/b package" "$work_dir/convert.err"
grep -qE '^proto_library\(' "$out/pkg/a/BUILD.bazel" || fail "pkg/a missing proto_library" "$out/pkg/a/BUILD.bazel"
grep -qF 'name = "a_proto"' "$out/pkg/a/BUILD.bazel" || fail "pkg/a a_proto name wrong" "$out/pkg/a/BUILD.bazel"
grep -qF 'srcs = ["a.proto"]' "$out/pkg/a/BUILD.bazel" || fail "a_proto srcs should be the package-local basename" "$out/pkg/a/BUILD.bazel"
grep -qF '"//pkg/a:a_proto"' "$out/pkg/b/BUILD.bazel" || fail "b_proto missing cross-package import dep //pkg/a:a_proto" "$out/pkg/b/BUILD.bazel"
echo "ok  meta-cmake-proto-cross-package: native rules placed per-package; b_proto deps //pkg/a:a_proto from the .proto import"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-proto-cross-package: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-proto-cross-package: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"; mkdir -p "$ws/pkg/a" "$ws/pkg/b"
cp "$fixture/pkg/a/a.proto" "$ws/pkg/a/"
cp "$fixture/pkg/b/b.proto" "$ws/pkg/b/"
cp "$out/pkg/a/BUILD.bazel" "$ws/pkg/a/BUILD.bazel"
cp "$out/pkg/b/BUILD.bazel" "$ws/pkg/b/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "crosspkg", version = "0.0.0")
bazel_dep(name = "protobuf", version = "29.3")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //pkg/b:b_cc_proto ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building //pkg/b:b_cc_proto failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-proto-cross-package: //pkg/b:b_cc_proto builds (cross-package import resolves via the deps edge)"
