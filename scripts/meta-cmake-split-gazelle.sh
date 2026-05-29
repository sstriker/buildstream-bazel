#!/bin/sh
# meta-cmake-split-gazelle.sh — end-to-end gate for the
# "gazelle_cc owns the layout" flow (Phase-8b continuous conversion).
#
# The converter bootstraps the per-directory split BUILDs (the
# --split-packages output, see scripts/meta-cmake-split-build.sh); this
# gate proves that gazelle_cc can then MAINTAIN that output without
# destroying it:
#
#   - write-a --split-packages --gazelle-cc renders project B with the
#     gazelle_cc wiring (bazel_dep gazelle/gazelle_cc/rules_go in
#     MODULE.bazel + a gazelle_binary/gazelle pair in the root BUILD).
#   - project A bazel-builds the cmake_split_convert TreeArtifact;
#     stage-b merges it into project B.
#   - `bazel run //:gazelle` (top-level — project B is one build tree)
#     canonicalizes the layout (gazelle_cc relocates cc_library targets
#     to their source dirs and prefers implementation_deps — both
#     accepted). gazelle_cc only manages cc_* rules, so the top-level
#     pass leaves the operator scaffolding (root gazelle rules, tools/)
#     untouched; no per-element scoping needed.
#
# Asserts after the gazelle pass:
#   (a) the build still works — `bazel build //elements/subdir-library/...`.
#   (b) the install-export cc_imports SURVIVED — toplib_import /
#       util_import still exist in the post-gazelle BUILD tree (proves
#       the whole-rule # keep fix; gazelle_cc can't regenerate them).
#   (c) FIXPOINT — a second gazelle pass produces NO diff.
#
# Bazel-availability + cmake/ninja skip guards and META_BAZEL_*_ARGS
# overrides mirror scripts/meta-cmake-split-build.sh. The sandbox's
# blocked go.dev SDK download is handled by overlaying go_sdk.host()
# into project B's overlay.MODULE.bazel when META_GAZELLE_USE_HOST_GO=1
# (CI leaves it unset to use gazelle_cc's transitive go_sdk.download).

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir=$(mktemp -d)
# Bazel materializes declare_directory (TreeArtifact) outputs read-only —
# restore write perms before removing or `rm -rf` fails on the packages/
# dirs.
trap 'chmod -R u+w "$work_dir" 2>/dev/null || true; rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake
CGO_ENABLED=0 go build -o "$bin_dir/stage-b" ./cmd/stage-b

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst testdata/meta-project/split-cmake/subdir-library.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" \
    --split-packages \
    --gazelle-cc

# === Render-phase checks: gazelle_cc is wired into project B. ===
module_b="$B/MODULE.bazel"
root_build_b="$B/BUILD.bazel"
if ! grep -q 'bazel_dep(name = "gazelle_cc"' "$module_b"; then
    echo "meta-cmake-split-gazelle: project B MODULE.bazel missing gazelle_cc bazel_dep" >&2
    cat "$module_b" >&2
    exit 1
fi
for want in 'bazel_dep(name = "gazelle"' 'bazel_dep(name = "rules_go"'; do
    if ! grep -q "$want" "$module_b"; then
        echo "meta-cmake-split-gazelle: project B MODULE.bazel missing $want" >&2
        exit 1
    fi
done
if ! grep -q 'gazelle_binary(' "$root_build_b"; then
    echo "meta-cmake-split-gazelle: project B root BUILD missing gazelle_binary rule" >&2
    cat "$root_build_b" >&2
    exit 1
fi
if ! grep -q '@gazelle_cc//language/cc' "$root_build_b"; then
    echo "meta-cmake-split-gazelle: project B root BUILD missing @gazelle_cc//language/cc" >&2
    exit 1
fi
if ! grep -q 'name = "gazelle"' "$root_build_b"; then
    echo "meta-cmake-split-gazelle: project B root BUILD missing gazelle target" >&2
    exit 1
fi
echo "meta-cmake-split-gazelle: render OK"

# === Bazel-availability gating (mirrors meta-cmake-split-build.sh). ===
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-cmake-split-gazelle: render OK; bazel not on PATH, skipping build + gazelle phase"
    exit 0
fi
major=$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')
if [ -z "$major" ] || [ "$major" -lt 9 ]; then
    echo "meta-cmake-split-gazelle: render OK; bazel < 9 (the bzlmod + load() floor), skipping build + gazelle phase"
    exit 0
fi
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null; then
        echo "meta-cmake-split-gazelle: render OK; $tool not on PATH, skipping build + gazelle phase"
        exit 0
    fi
done

# Sandbox / local: gazelle_cc's transitive go_sdk.download(1.24.0) hits
# go.dev, which the sandbox blocks. Overlay go_sdk.host() so the
# operator's host Go toolchain is used instead. CI leaves
# META_GAZELLE_USE_HOST_GO unset and uses the normal download path.
# rules_go is a visible bazel_dep (write-a emitted it with --gazelle-cc),
# so @rules_go resolves for the use_extension.
if [ "${META_GAZELLE_USE_HOST_GO:-}" = "1" ]; then
    cat >>"$B/overlay.MODULE.bazel" <<'EOF'

# meta-cmake-split-gazelle.sh (META_GAZELLE_USE_HOST_GO=1): use the host
# Go toolchain instead of gazelle_cc's transitive go_sdk.download, which
# the sandbox can't reach (go.dev egress blocked).
go_sdk = use_extension("@rules_go//go:extensions.bzl", "go_sdk")
go_sdk.host()
EOF
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"

# run_bazel runs a bazel command and PROPAGATES its exit status (under
# `set -e` a failing build aborts the gate). Output is captured to a log
# and the tail echoed for diagnostics — deliberately NOT a `| tail`
# pipeline, which under POSIX sh would mask bazel's exit code behind
# tail's success and silently turn a failed build into a green gate.
bzl_log="$work_dir/bazel.log"
run_bazel() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    rc=0
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS) >"$bzl_log" 2>&1 || rc=$?
    tail -20 "$bzl_log"
    return $rc
}

# run_gazelle runs `bazel run //:gazelle` TOP-LEVEL over the whole project
# B tree (no per-element `-- <pkg>` scoping). Project B is one build tree,
# so gazelle_cc is a workspace tool: a top-level pass gives a global
# fixpoint and consistent cross-element resolution (a per-element run on a
# producer wouldn't propagate gazelle_cc's cross-package relocations/renames
# to consumers elsewhere). It's clean — gazelle_cc only manages cc_* rules,
# so it leaves the operator scaffolding (the root gazelle_binary/gazelle
# rules, tools/) untouched. The per-element `-- $changed` form is an
# incremental-driver optimization (see scripts/meta-gazelle-roundtrip.sh),
# not needed here. META_BAZEL_BUILD_ARGS (e.g. --registry) are bazel build
# flags and stay before any `--`. Captures output + propagates bazel's real
# exit status (no masking `| tail` at the call site).
run_gazelle() {
    rc=0
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$B" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        run //:gazelle $META_BAZEL_BUILD_ARGS) >"$bzl_log" 2>&1 || rc=$?
    tail -20 "$bzl_log"
    return $rc
}

# === Build project A's cmake_split_convert TreeArtifact + stage into B. ===
run_bazel "$A" build //elements/subdir-library:subdir-library_converted
"$bin_dir/stage-b" --project-a "$A" --project-b "$B" >/dev/null
for want in \
    "elements/subdir-library/BUILD.bazel" \
    "elements/subdir-library/src/util/BUILD.bazel" \
    "elements/subdir-library/include/BUILD.bazel"; do
    if [ ! -f "$B/$want" ]; then
        echo "meta-cmake-split-gazelle: stage-b did not stage $want into project B" >&2
        exit 1
    fi
done
# stage-b stages the TreeArtifact read-only; gazelle needs to rewrite the
# BUILDs, so restore write perms on the staged element tree.
chmod -R u+w "$B/elements/subdir-library"
echo "meta-cmake-split-gazelle: project A built + staged the split tree into B"

# === First gazelle pass: gazelle_cc canonicalizes / owns the layout. ===
run_gazelle
echo "meta-cmake-split-gazelle: gazelle_cc first pass done"

# (a) The build still works after gazelle's canonicalization.
#     We can't `build //elements/subdir-library/...` wholesale: the
#     install-export targets (the cmake_config_bundle filegroup +
#     toplib_import / util_import cc_imports) reference placeholder files
#     (lib/cmake/.../*.cmake, lib/lib*.a) that the round-1 split shape
#     never materializes — they're cross-element export stubs, not
#     buildable here. Instead, build every cc_library gazelle_cc left in
#     the tree (it relocates toplib's cc_library into src/ and may rename
#     it after its directory, so the target set is gazelle-determined —
#     a query is more robust than a hard-coded label list). The real
#     compile + cross-package include wiring is what must survive.
# shellcheck disable=SC2086 # META_BAZEL_STARTUP_ARGS is intentionally word-split.
cc_targets=$(cd "$B" && "$BZL" --output_user_root="$bzl_cache" $META_BAZEL_STARTUP_ARGS query 'kind(cc_library, //elements/subdir-library/...)' 2>/dev/null)
if [ -z "$cc_targets" ]; then
    echo "meta-cmake-split-gazelle: no cc_library targets found post-gazelle (canonicalization lost them?)" >&2
    find "$B/elements/subdir-library" -name 'BUILD*' -exec sh -c 'echo "=== $1 ==="; cat "$1"' _ {} \; >&2
    exit 1
fi
# shellcheck disable=SC2086 # cc_targets is an intentional label list.
run_bazel "$B" build $cc_targets
echo "meta-cmake-split-gazelle: build still works post-gazelle (cc_library targets: $(echo "$cc_targets" | tr '\n' ' '))"

# (b) The install-export cc_imports SURVIVED the gazelle pass — proves
#     the whole-rule # keep fix (gazelle_cc can't regenerate them, so
#     without rule-level keep it would delete them).
for want in toplib_import util_import; do
    if ! grep -rq "$want" "$B/elements/subdir-library"; then
        echo "meta-cmake-split-gazelle: install-export cc_import '$want' was DELETED by gazelle (keep-fix regression)" >&2
        find "$B/elements/subdir-library" -name 'BUILD*' -exec sh -c 'echo "=== $1 ==="; cat "$1"' _ {} \; >&2
        exit 1
    fi
done
echo "meta-cmake-split-gazelle: install-export cc_imports (toplib_import, util_import) survived"

# (c) FIXPOINT — a second top-level gazelle pass must produce no diff
#     anywhere in the converted tree (snapshot all of elements/, not just
#     one element, since the pass is workspace-wide).
before="$work_dir/before-fixpoint"
after="$work_dir/after-fixpoint"
rm -rf "$before" "$after"
cp -r "$B/elements" "$before"
run_gazelle
cp -r "$B/elements" "$after"
if ! diff -ru "$before" "$after"; then
    echo "meta-cmake-split-gazelle: gazelle_cc is NOT a fixpoint — second pass changed the BUILD tree" >&2
    exit 1
fi
echo "meta-cmake-split-gazelle: gazelle_cc is a fixpoint (second pass is a no-op)"

echo "ok meta-cmake-split-gazelle: render + build A + stage-b + gazelle_cc canonicalize, cc_import install-exports survive, second pass is a fixpoint"
