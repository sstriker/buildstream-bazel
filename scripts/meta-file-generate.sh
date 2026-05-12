#!/bin/sh
# meta-file-generate.sh — render gate for the file(GENERATE)
# lifter (Commit 3/4/5 of the lift-GENERATED PR).
#
# Runs convert-element against the captured file-generate
# fixture and asserts the rendered BUILD.bazel contains the
# expected genrule shapes for all three lift modes:
#
#   - INPUT form, genex-free → lifted with srcs + cmake-
#     configure-file invocation, cmake-codegen-lifted tag.
#   - CONTENT form, genex-free → lifted with --content-base64,
#     cmake-codegen-lifted tag.
#   - CONTENT form with $<...> → legacy bytes-embedded shape,
#     cmake-codegen-file-generate-genex audit tag, no
#     cmake-codegen-lifted tag.
#
# The Go test (TestEmit_FileGenerate_Golden) covers the same
# round-trip with full byte-stability; this gate exercises the
# binary's CLI surface so a regression in convert-element's
# flag plumbing or output writer also catches.
#
# Why a separate gate: parallel to meta-element-fold.sh and the
# autotools / make round-2 gates, each focused on one lift
# axis. CI runs gates individually; a single failing gate's
# diagnostic surface stays narrow.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/convert-element" ./converter/cmd/convert-element

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

src="$repo_root/converter/testdata/sample-projects/file-generate"
reply="$repo_root/converter/testdata/fileapi/file-generate"
out_build="$work_dir/BUILD.bazel"

"$bin_dir/convert-element" \
    --source-root "$src" \
    --reply-dir "$reply" \
    --lift-configure-file=true \
    --out-build "$out_build"

if [ ! -f "$out_build" ]; then
    echo "convert-element did not produce $out_build" >&2
    exit 1
fi

# Required content per lift mode.
required=$(cat <<'EOF'
name = "gen_version_h"
"src/version.h.in"
"//tools:cmake-configure-file"
"cmake-codegen-lifted"
name = "gen_banner_h"
--content-base64=
name = "gen_config_tag_h"
"cmake-codegen-file-generate-genex"
EOF
)

while IFS= read -r line; do
    if [ -z "$line" ]; then continue; fi
    if ! grep -qF -- "$line" "$out_build"; then
        echo "BUILD.bazel missing required substring: $line" >&2
        echo "--- BUILD.bazel ---" >&2
        cat "$out_build" >&2
        exit 1
    fi
done <<EOF
$required
EOF

# Negative: the genex-fallback genrule must NOT carry the
# lifted tag (otherwise the audit can't distinguish "lifted"
# from "fell back because of genex").
if awk '
    /name = "gen_config_tag_h"/ { in_blk = 1 }
    in_blk { print }
    in_blk && /^\)/ { exit }
' "$out_build" | grep -qF '"cmake-codegen-lifted"'; then
    echo "gen_config_tag_h must NOT carry cmake-codegen-lifted (it's the genex fallback)" >&2
    exit 1
fi

# Determinism: re-run, byte-diff outputs.
out_build_2="$work_dir/BUILD.bazel.2"
"$bin_dir/convert-element" \
    --source-root "$src" \
    --reply-dir "$reply" \
    --lift-configure-file=true \
    --out-build "$out_build_2"

if ! diff -q "$out_build" "$out_build_2" >/dev/null; then
    echo "convert-element output not deterministic across runs" >&2
    diff -u "$out_build" "$out_build_2" >&2
    exit 1
fi

echo "meta-file-generate: ok"
