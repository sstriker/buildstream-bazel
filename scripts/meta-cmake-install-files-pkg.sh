#!/bin/sh
# meta-cmake-install-files-pkg.sh — render gate for the Phase 1
# slice 1b install(FILES)/install(DIRECTORY) → pkg_files lowering.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/install-files-pkg, a fixture with
# install(FILES ... DESTINATION ...) + install(DIRECTORY ...
# DESTINATION ...) calls. The lowering pass must route the "file" /
# "directory" Directory.Installers entries to rules_pkg pkg_files
# targets, carrying the install DESTINATION as the pkg_files `prefix`
# attribute (instead of the old opaque filegroup that dropped the
# destination).
#
# Asserts (in the rendered BUILD.bazel.out):
#   1. convert-element-cmake exits 0.
#   2. The @rules_pkg//pkg:mappings.bzl load is emitted (so the
#      consuming project resolves pkg_files).
#   3. A pkg_files rule for each install destination with the right
#      prefix attribute:
#        - install(FILES ... DESTINATION share/installfilespkg)
#        - install(FILES include/greeter.h DESTINATION include)
#        - install(DIRECTORY docs/ DESTINATION share/doc)
#   4. NO bare `filegroup(name = "install_files__..."` /
#      `install_directory__...` shape remains (the old lowering).
#   5. NO `_install_tree_extract` round-2 fallback genrule fires
#      (declarative pkg_files is the convert-time answer).
#
# Hard architectural constraint exercised: convert is metadata-only.
# This gate runs without bazel; the cmake-availability gate at the top
# is the only prerequisite. There is no `cmake --build` /
# `cmake --install` invocation — the installer entries come from the
# File API codemodel.

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

fixture="$repo_root/converter/testdata/sample-projects/install-files-pkg"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"

"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
    echo "FAIL: convert-element-cmake exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.stderr"
    exit 1
}

fail() {
    echo "FAIL: $1"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$out_build"
    exit 1
}

# Assert 2: rules_pkg load present. The fixture has an
# install(DIRECTORY) so the load also pulls "strip_prefix" (the
# directory-glob shape renders strip_prefix.from_pkg(...)); match the
# pkg_files symbol without pinning the rest of the load line.
if ! grep -q 'load("@rules_pkg//pkg:mappings.bzl", "pkg_files"' "$out_build"; then
    fail "rules_pkg pkg_files load missing"
fi

# Assert 3a: install(FILES ... DESTINATION share/installfilespkg).
if ! grep -q 'name = "install_files__share_installfilespkg"' "$out_build"; then
    fail "pkg_files for share/installfilespkg destination missing"
fi
if ! grep -q 'prefix = "share/installfilespkg"' "$out_build"; then
    fail "pkg_files prefix for share/installfilespkg missing or wrong"
fi

# Assert 3b: install(FILES include/greeter.h DESTINATION include).
if ! grep -q 'name = "install_files__include"' "$out_build"; then
    fail "pkg_files for include destination missing"
fi
if ! grep -q 'prefix = "include"' "$out_build"; then
    fail "pkg_files prefix for include missing or wrong"
fi

# Assert 3c: install(DIRECTORY docs/ DESTINATION share/doc). The
# directory case must GLOB the source dir's contents and strip the
# source dir (slice 1b fix): a bare directory in pkg_files srcs does
# not package the dir's files — a consuming pkg_tar fails with
# IsADirectoryError. So assert the glob + strip_prefix shape, not the
# old bare `srcs = ["docs"]`.
if ! grep -q 'name = "install_directory__share_doc"' "$out_build"; then
    fail "pkg_files for share/doc directory destination missing"
fi
if ! grep -q 'prefix = "share/doc"' "$out_build"; then
    fail "pkg_files prefix for share/doc missing or wrong"
fi
if ! grep -q 'srcs = glob(\["docs/\*\*"\])' "$out_build"; then
    fail "install(DIRECTORY) docs/ should glob the dir contents (srcs = glob([\"docs/**\"]))"
fi
if ! grep -q 'strip_prefix = strip_prefix.from_pkg("docs")' "$out_build"; then
    fail "install(DIRECTORY) docs/ should strip the source dir (strip_prefix.from_pkg(\"docs\"))"
fi

# Assert 3d: install(FILES data/greeting.txt RENAME GREETING DESTINATION
# share/renamed). The File API records this as a {"from","to"} object on
# a Type=="file" installer; the lowering lifts "to" onto the pkg_files
# `renames` map. Without it the renamed file was dropped entirely.
if ! grep -q 'name = "install_files__share_renamed"' "$out_build"; then
    fail "pkg_files for share/renamed (RENAME) destination missing — renamed FILES installer dropped?"
fi
if ! grep -q '"data/greeting.txt": "GREETING"' "$out_build"; then
    fail "install(FILES ... RENAME GREETING) should emit renames = {\"data/greeting.txt\": \"GREETING\"}"
fi

# Assert 3e: install(DIRECTORY include DESTINATION include-tree) — the
# NO-trailing-slash form (dir itself into dest). Globs the dir but
# preserves the dir name: strips only the parent (here "include" sits at
# the package root, so NO strip_prefix at all), landing files at
# include-tree/include/<rel> rather than include-tree/<rel>.
if ! grep -q 'name = "install_directory__include_tree"' "$out_build"; then
    fail "pkg_files for include-tree (no-trailing-slash DIRECTORY) missing"
fi
# The include-tree target must glob include/** and carry NO strip_prefix
# (the dir name is preserved under the prefix). Check the rule block.
include_tree_block="$(awk '/name = "install_directory__include_tree"/{f=1} f{print} f&&/^\)/{exit}' "$out_build")"
if ! printf '%s\n' "$include_tree_block" | grep -q 'srcs = glob(\["include/\*\*"\])'; then
    fail "install(DIRECTORY include) should glob include/** (got: $include_tree_block)"
fi
if printf '%s\n' "$include_tree_block" | grep -q 'strip_prefix'; then
    fail "install(DIRECTORY include) (no trailing slash) must NOT strip the dir — name is preserved (got: $include_tree_block)"
fi

# Assert 4: no bare install filegroup shape (the old lowering).
if grep -E 'filegroup\(\s*$' "$out_build" | grep -q . && \
   grep -B1 'name = "install_files__' "$out_build" | grep -q 'filegroup('; then
    fail "old filegroup install shape still present; install(FILES) should lower to pkg_files"
fi

# Assert 5: no round-2 install-tree fallback.
if grep -q '_install_tree_extract' "$out_build"; then
    fail "round-2 _install_tree_extract fired; declarative pkg_files should bypass it"
fi

echo "ok  meta-cmake-install-files-pkg: install(FILES)/install(DIRECTORY) lowered to pkg_files with prefix = <dest>"
