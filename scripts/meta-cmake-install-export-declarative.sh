#!/bin/sh
# meta-cmake-install-export-declarative.sh — render gate for the
# Phase 6 declarative install(EXPORT) IR projection (EmitInputs from
# codemodel-only sources).
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/install-export-declarative, an
# install(TARGETS ... EXPORT) + install(EXPORT) fixture matching the
# CMakePackageConfigHelpers canonical shape. The classifier must
# verdict the bundle as declarative; the EmitInputs walk must
# synthesize artifact paths + cmake_config_bundle from codemodel
# Target.NameOnDisk + Target.Install.Destinations + the installer's
# Destination — no convert-time cmake --install runs.
#
# Asserts (in the rendered BUILD.bazel.out):
#   1. convert-element-cmake exits 0.
#   2. `cc_import` rule per exported library (foo_import,
#      bar_import) with the right static_library / shared_library
#      attr pointing at lib/libfoo.a / lib/libbar.so.<soversion>.
#   3. `filegroup(name = "cmake_config_bundle"` carrying the
#      synthesized lib/cmake/foopkg/foopkgTargets.cmake file.
#   4. NO `_install_tree_extract` genrule (the round-2 fallback
#      must not fire for declarative bundles).
#   5. The `cmake-codegen-install-export-import` tag is on the
#      cc_import — surfaces the Phase 6 export-derived facade so
#      cmakecfg's bundle synthesizer can de-duplicate it.
#
# Hard architectural constraint exercised: convert is metadata-only.
# This gate runs without bazel; the cmake-availability gate at the
# top is the only prerequisite. There is no `cmake --build` /
# `cmake --install` invocation anywhere in the gate's setup — the
# artifact paths come from the codemodel.

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

fixture="$repo_root/converter/testdata/sample-projects/install-export-declarative"
work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

out_build="$work_dir/BUILD.bazel.out"
out_exports="$work_dir/exports.json"

# --bazel-package-path frames the synthesized cmake_config_bundle /
# *_import labels absolutely so the resolved-lift manifest-synth (M3)
# assertions below see cross-element-resolvable labels.
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --out-build "$out_build" \
    --out-exports "$out_exports" \
    --bazel-package-path "elements/foopkg" \
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

# Assert 2a: foo cc_import with static archive
if ! grep -q 'name = "foo_import"' "$out_build"; then
    fail "foo_import cc_import missing"
fi
if ! grep -q 'static_library = "lib/libfoo.a"' "$out_build"; then
    fail "foo_import static_library attr missing or wrong path"
fi

# Assert 2b: bar cc_import with shared library. Match the
# SOVERSION-suffixed shape cmake records as Target.NameOnDisk
# (libbar.so.1 on this fixture's SOVERSION=1).
if ! grep -q 'name = "bar_import"' "$out_build"; then
    fail "bar_import cc_import missing"
fi
if ! grep -q 'shared_library = "lib/libbar.so' "$out_build"; then
    fail "bar_import shared_library attr missing or wrong path"
fi

# Assert 3: cmake_config_bundle filegroup with the synthesized
# foopkgTargets.cmake reference.
if ! grep -q 'name = "cmake_config_bundle"' "$out_build"; then
    fail "cmake_config_bundle filegroup missing"
fi
if ! grep -q 'lib/cmake/foopkg/foopkgTargets.cmake' "$out_build"; then
    fail "cmake_config_bundle srcs missing foopkgTargets.cmake"
fi

# Assert 4: no install_tree_extract fallback genrule.
if grep -q '_install_tree_extract' "$out_build"; then
    fail "round-2 _install_tree_extract fired; declarative path should bypass it"
fi

# Assert 5: Phase 6 tag present on the cc_import facade.
if ! grep -q 'cmake-codegen-install-export-import' "$out_build"; then
    fail "Phase 6 install-export-import tag missing on cc_import"
fi

exports_fail() {
    echo "FAIL: $1"
    echo "   --- generated exports.json ---"
    sed 's/^/   /' "$out_exports"
    exit 1
}

# Assert 6 (resolved-lift manifest-synth, M3): the exports.json carries
# the absolute cmake_config_bundle filegroup label so a cross-element
# find_package(<Pkg> CONFIG) consumer can resolve straight to the
# producer's synthesized bundle.
if ! grep -q '"cmake_config_bundle_label": "//elements/foopkg:cmake_config_bundle"' "$out_exports"; then
    exports_fail "exports.json missing cmake_config_bundle_label = //elements/foopkg:cmake_config_bundle"
fi

# Assert 7: the exports.json carries the per-artifact <lib>_import
# cc_import facade labels, mirroring the BUILD-shape cc_import targets
# asserted above (foo_import, bar_import), absolutely framed.
if ! grep -q '"cmake_import_labels"' "$out_exports"; then
    exports_fail "exports.json missing cmake_import_labels list"
fi
if ! grep -q '"//elements/foopkg:foo_import"' "$out_exports"; then
    exports_fail "exports.json cmake_import_labels missing //elements/foopkg:foo_import"
fi
if ! grep -q '"//elements/foopkg:bar_import"' "$out_exports"; then
    exports_fail "exports.json cmake_import_labels missing //elements/foopkg:bar_import"
fi

echo "ok  meta-cmake-install-export-declarative: cc_import + cmake_config_bundle emitted from codemodel without install-tree fallback; exports.json carries resolved bundle + import labels"
