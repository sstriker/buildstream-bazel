#!/bin/sh
# meta-cmake-e-tar-create.sh — render+build gate for the cmake -E tar
# CREATE lift to pkg_tar (rules_pkg), via the codegen-recognizer
# registry's generic native-rule substrate.
#
# The fixture packages two committed source files into a gzip tarball at
# configure time:
#   execute_process(COMMAND cmake -E tar czf <build>/bundle.tar.gz a.txt b.txt)
# Historically refused ("cmake -E tar not in the v1 supported-op set").
# The lift emits a pkg_tar carrying srcs (source labels), out, and
# extension=tar.gz — the idiomatic Bazel packaging rule — with the
# @rules_pkg//pkg:tar.bzl load auto-emitted by the native-rule machinery.
#
# Asserts the pkg_tar renders, then bazel-builds it and verifies the
# archive is a valid gzip tar containing both files.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/cmake-e-tar-create"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
build="$work_dir/BUILD.bazel"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$build" \
    --conversion-todos-report "$work_dir/todos.json" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() { echo "FAIL: $1"; sed 's/^/   /' "$build" 2>/dev/null || true; exit 1; }

grep -qF 'load("@rules_pkg//pkg:tar.bzl", "pkg_tar")' "$build" || fail "rules_pkg tar load not emitted"
grep -qF 'pkg_tar(' "$build" || fail "cmake -E tar create not lifted to pkg_tar"
grep -qF 'out = "bundle.tar.gz"' "$build" || fail "archive out not set"
grep -qF 'extension = "tar.gz"' "$build" || fail "gzip compression not mapped to extension"
grep -q '"unsupported-execute-process"' "$work_dir/todos.json" && fail "cmake -E tar create still refuses"

echo "ok  meta-cmake-e-tar-create: cmake -E tar create lifted to pkg_tar (rules_pkg)"

# --- Bazel-build half ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-e-tar-create: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-e-tar-create: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"
mkdir -p "$ws"
cp "$fixture"/a.txt "$fixture"/b.txt "$ws/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "tarcreate", version = "0.0.0")
bazel_dep(name = "rules_pkg", version = "1.0.1")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:tar_bundle_tar_gz ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: building the pkg_tar archive failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
arch="$ws/bazel-bin/bundle.tar.gz"
[ -f "$arch" ] || fail "archive not produced at bazel-bin/bundle.tar.gz"
gzip -t "$arch" 2>/dev/null || { echo "FAIL: archive is not valid gzip"; exit 1; }
contents="$(tar tzf "$arch" 2>/dev/null | sort | tr '\n' ' ')"
case "$contents" in
  *a.txt*b.txt*) ;;
  *) echo "FAIL: archive missing members; got: $contents"; exit 1 ;;
esac
echo "ok  meta-cmake-e-tar-create: pkg_tar builds a valid gzip archive containing both files"
