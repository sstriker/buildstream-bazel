#!/bin/sh
# meta-cmake-split-packages.sh — render gate for --split-packages.
#
# Drives convert-element-cmake against the committed subdir-library
# sample project (top-level CMakeLists + add_subdirectory(src/util),
# both including the project-root include/ header dir) in BOTH modes:
#
#   ON  (--split-packages): one BUILD.bazel per directory ("gazelle
#       model"). Asserts the per-dir BUILD tree exists, that the
#       cross-package dep labels are present, that exports.json carries
#       the sub-package label for util, and that buildifier -mode=diff
#       is a no-op on every emitted BUILD.
#   OFF (control): a single monolithic BUILD.bazel, no subdir BUILDs.
#
# Runs cmake live so recorded paths match the test-host paths; skips
# cleanly when cmake isn't on PATH (the converter needs it to configure).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! command -v cmake >/dev/null 2>&1; then
    echo "skip: cmake not on PATH"
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

src_root="$repo_root/converter/testdata/sample-projects/subdir-library"
pkg_path="elements/subdir-library"

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

# --- ON: split-packages ---------------------------------------------
on_dir="$work_dir/on"
mkdir -p "$on_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$src_root" \
    --split-packages \
    --bazel-package-path "$pkg_path" \
    --out-build "$on_dir/BUILD.bazel" \
    --out-exports "$on_dir/exports.json" \
    >"$on_dir/convert.stdout" 2>"$on_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake --split-packages exited non-zero"
    sed 's/^/   stderr: /' "$on_dir/convert.stderr"
    exit 1
}

for f in "BUILD.bazel" "src/util/BUILD.bazel" "include/BUILD.bazel"; do
    if [ ! -f "$on_dir/$f" ]; then
        echo "FAIL: split mode did not emit $f"
        find "$on_dir" -name BUILD.bazel | sed 's/^/   got: /'
        exit 1
    fi
done

# Root toplib depends on the sub-package util label + the synthesized
# include header lib.
if ! grep -q "//$pkg_path/src/util" "$on_dir/BUILD.bazel"; then
    echo "FAIL: root BUILD missing cross-package util dep label"
    sed 's/^/   /' "$on_dir/BUILD.bazel"
    exit 1
fi
if ! grep -q "//$pkg_path/include:include_headers" "$on_dir/BUILD.bazel"; then
    echo "FAIL: root BUILD missing synthesized include header-lib dep label"
    sed 's/^/   /' "$on_dir/BUILD.bazel"
    exit 1
fi

# src/util's util.c is re-relativized to the sub-package.
if ! grep -q '"util.c"' "$on_dir/src/util/BUILD.bazel"; then
    echo "FAIL: src/util BUILD did not re-relativize util.c"
    sed 's/^/   /' "$on_dir/src/util/BUILD.bazel"
    exit 1
fi

# include package carries the synthesized header cc_library.
if ! grep -q 'name = "include_headers"' "$on_dir/include/BUILD.bazel"; then
    echo "FAIL: include BUILD missing synthesized header cc_library"
    sed 's/^/   /' "$on_dir/include/BUILD.bazel"
    exit 1
fi

# exports.json carries the sub-package label for util.
if ! grep -q "//$pkg_path/src/util:util" "$on_dir/exports.json"; then
    echo "FAIL: exports.json missing sub-package label for util"
    sed 's/^/   /' "$on_dir/exports.json"
    exit 1
fi

# --- OFF: control single-BUILD --------------------------------------
off_dir="$work_dir/off"
mkdir -p "$off_dir"
"$bin_dir/convert-element-cmake" \
    --source-root "$src_root" \
    --bazel-package-path "$pkg_path" \
    --out-build "$off_dir/BUILD.bazel" \
    >"$off_dir/convert.stdout" 2>"$off_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake (OFF control) exited non-zero"
    sed 's/^/   stderr: /' "$off_dir/convert.stderr"
    exit 1
}
if [ -f "$off_dir/src/util/BUILD.bazel" ]; then
    echo "FAIL: OFF control should not emit per-directory BUILDs"
    exit 1
fi
if ! grep -q 'name = "util"' "$off_dir/BUILD.bazel"; then
    echo "FAIL: OFF control single BUILD missing util target"
    exit 1
fi

# --- buildifier no-op gate (best effort install) --------------------
buildifier_bin=""
if command -v buildifier >/dev/null 2>&1; then
    buildifier_bin="$(command -v buildifier)"
elif command -v go >/dev/null 2>&1; then
    GOBIN="$work_dir/gobin" go install github.com/bazelbuild/buildtools/buildifier@latest \
        >"$work_dir/buildifier-install.log" 2>&1 || true
    if [ -x "$work_dir/gobin/buildifier" ]; then
        buildifier_bin="$work_dir/gobin/buildifier"
    fi
fi
if [ -n "$buildifier_bin" ]; then
    if ! "$buildifier_bin" -mode=diff -r "$on_dir" >"$work_dir/buildifier.diff" 2>&1; then
        echo "FAIL: buildifier -mode=diff found non-canonical BUILD output"
        sed 's/^/   /' "$work_dir/buildifier.diff"
        exit 1
    fi
    echo "ok  meta-cmake-split-packages: buildifier -mode=diff clean on every emitted BUILD"
else
    echo "note: buildifier unavailable; skipped the -mode=diff no-op gate"
fi

echo "ok  meta-cmake-split-packages: per-dir BUILD tree, cross-package labels, exports.json sub-package label, OFF control single BUILD"
