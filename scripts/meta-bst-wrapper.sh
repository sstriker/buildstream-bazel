#!/bin/sh
# meta-bst-wrapper.sh — smoke test for tools/bst.
#
# tools/bst is the BuildStream-style CLI wrapper around write-a
# that lets `bst build <element.bst>` keep working against a
# converted project (and against `bst workspace open`-modified
# element source trees). Goal: BuildStream muscle memory survives
# the transition.
#
# This gate exercises the render half (no Bazel required) against
# three fixtures, plus the workspace open/close cycle on a copy of
# the cmake hello-world fixture. Bazel-build half is intentionally
# out of scope — the wrapper's bazel-build invocation is a thin
# shell-out to whatever Bazel is on PATH, and Bazel runs go through
# the existing per-kind render gates.
#
# Coverage:
#
#   1. kind:cmake single-element graph (hello-world.bst). Verifies
#      the cmake render path + target name (`hello-world`, no
#      _install suffix).
#   2. kind:autotools single-element graph (autotools-greet/greet.bst).
#      Verifies the autotools render path + the _install target
#      name suffix.
#   3. Multi-element graph (two-libs/runtime.bst → lib-a + lib-b).
#      Verifies the dep walker correctly follows `depends:` blocks
#      with bare element names (no .bst suffix), the flush-left
#      list style FDSDK uses, and emits per-element BUILD.bazel.
#   4. `bst workspace open` + `bst workspace close` round-trip on a
#      throwaway copy of the hello-world fixture.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

make -s all >/dev/null

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Per-run cache dirs keep state out of the operator's
# ~/.cache/buildstream-bazel; pinning lets the gate run from a
# clean slate every time.
export BST_CACHE_DIR="$work_dir/cache"

# 1) kind:cmake single-element.
out_1="$(./tools/bst build testdata/meta-project/hello-world.bst 2>&1)"
case "$out_1" in
    *"//elements/hello-world:hello-world"*) ;;
    *)
        echo "meta-bst-wrapper: hello-world target line missing or wrong" >&2
        echo "$out_1" >&2
        exit 1
        ;;
esac
hello_a="$(ls -d "$BST_CACHE_DIR"/*/A 2>/dev/null | head -1)"
hello_b="$(dirname "$hello_a")/B"
if [ ! -f "$hello_a/elements/hello-world/BUILD.bazel" ]; then
    echo "meta-bst-wrapper: hello-world project A BUILD missing" >&2
    exit 1
fi
if [ ! -f "$hello_b/elements/hello-world/BUILD.bazel" ]; then
    echo "meta-bst-wrapper: hello-world project B BUILD missing" >&2
    exit 1
fi

# 2) kind:autotools single-element. Should pick the _install target.
rm -rf "$BST_CACHE_DIR"
out_2="$(./tools/bst build testdata/meta-project/autotools-greet/greet.bst 2>&1)"
case "$out_2" in
    *"//elements/greet:greet_install"*) ;;
    *)
        echo "meta-bst-wrapper: autotools-greet target line missing or wrong" >&2
        echo "$out_2" >&2
        exit 1
        ;;
esac

# 3) Multi-element graph; flush-left bare-name deps. write-a's
# stderr reports the element count; assert it sees all three.
rm -rf "$BST_CACHE_DIR"
out_3="$(./tools/bst build testdata/meta-project/two-libs/runtime.bst 2>&1)"
case "$out_3" in
    *"3 elements"*) ;;
    *)
        echo "meta-bst-wrapper: two-libs dep walker dropped element(s); expected 3, got:" >&2
        echo "$out_3" >&2
        exit 1
        ;;
esac
two_libs_a="$(ls -d "$BST_CACHE_DIR"/*/A 2>/dev/null | head -1)"
for elem in runtime lib-a lib-b; do
    if [ ! -f "$two_libs_a/elements/$elem/BUILD.bazel" ]; then
        echo "meta-bst-wrapper: two-libs missing $elem/BUILD.bazel in A" >&2
        exit 1
    fi
done

# 4) workspace open/close round-trip on a throwaway fixture copy.
ws_fixture="$work_dir/wsfix"
mkdir -p "$ws_fixture/src"
echo 'int main(){return 0;}' > "$ws_fixture/src/main.c"
cat > "$ws_fixture/test.bst" <<'EOF'
kind: cmake

sources:
- kind: local
  path: src
EOF
orig_text="$(cat "$ws_fixture/test.bst")"

./tools/bst workspace open "$ws_fixture/test.bst" "$ws_fixture/ws" >/dev/null 2>&1
if [ ! -f "$ws_fixture/ws/main.c" ]; then
    echo "meta-bst-wrapper: workspace open didn't copy sources" >&2
    exit 1
fi
if ! grep -qF "path: $ws_fixture/ws" "$ws_fixture/test.bst"; then
    echo "meta-bst-wrapper: workspace open didn't rewrite path:" >&2
    cat "$ws_fixture/test.bst" >&2
    exit 1
fi

./tools/bst workspace close "$ws_fixture/test.bst" >/dev/null 2>&1
restored_text="$(cat "$ws_fixture/test.bst")"
if [ "$orig_text" != "$restored_text" ]; then
    echo "meta-bst-wrapper: workspace close didn't restore the .bst" >&2
    echo "--- expected ---" >&2
    echo "$orig_text" >&2
    echo "--- got ---" >&2
    echo "$restored_text" >&2
    exit 1
fi

echo "ok meta-bst-wrapper: render + workspace cycle clean"
