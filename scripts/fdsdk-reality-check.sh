#!/bin/sh
# fdsdk-reality-check.sh — probes write-a against real freedesktop-sdk
# content and reports which gap each curated element hits first.
#
# This is research / triage, not an acceptance gate: success criteria
# is "exits cleanly with a per-element status report"; a regression in
# any individual element doesn't fail the script. As features land in
# write-a, prior failures should disappear from the report.
#
# Usage:
#   FDSDK_DIR=/path/to/fdsdk-clone scripts/fdsdk-reality-check.sh
#
# If FDSDK_DIR is unset, the script tries /tmp/fdsdk; if that doesn't
# exist either it prints the clone command and exits 0 (the gap survey
# is documented in docs/fdsdk-reality-check.md, so a fresh checkout
# isn't required to read the findings).
#
# See docs/fdsdk-reality-check.md for the corresponding gap analysis.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

FDSDK_DIR="${FDSDK_DIR:-/tmp/fdsdk}"
if [ ! -d "$FDSDK_DIR/elements" ]; then
    cat <<EOF
fdsdk-reality-check: FDSDK clone not found at $FDSDK_DIR.

To run the probe:
    git clone --depth=1 https://gitlab.com/freedesktop-sdk/freedesktop-sdk \
        /tmp/fdsdk
    FDSDK_DIR=/tmp/fdsdk scripts/fdsdk-reality-check.sh

The findings from the most recent survey (without re-running) live
in docs/fdsdk-reality-check.md.
EOF
    exit 0
fi

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null 2>&1
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

# Curated probe set: each entry exercises one or more bullet points
# from docs/fdsdk-reality-check.md. The probe runs against the .bst in
# isolation (no FDSDK project.conf), so the first failure surfaces
# whichever loader / handler gap matches.
probes="
elements/components/bzip2.bst         | kind:stack — path-qualified deps + project.conf includes
elements/components/expat.bst         | kind:cmake — public: block + kind:git_repo source
elements/components/aom.bst           | kind:cmake — (?) arch conditional
elements/bootstrap/bzip2.bst          | kind:manual — element-level (@) include + build-depends + junction-targeted dep
elements/components/tar.bst           | kind:autotools — (@) include + build-depends + path-qualified deps
elements/public-stacks/runtime-minimal.bst | kind:stack — top-level runtime target; deepest stack + filter resolution
elements/components/glib.bst          | kind:meson — meson with extensive deps + (?) branches
elements/flatpak-images/vainfo.bst    | kind:flatpak_image — image-building (currently unsupported handler)
"

ok=0
fail=0
report=""

# Each probe runs against an isolated copy of the .bst (no FDSDK
# project.conf alongside) so the first failure surfaces the per-
# element gap rather than always tripping on project.conf's (@):
# composition directive (which is its own punch-list item, surveyed
# separately below). kind:local sources resolve relative to the
# .bst's directory, so a copy-in-isolation run can't fetch real
# source paths — but every probe trips on parsing or graph
# resolution before it would consult sources.
while IFS='|' read -r path desc; do
    path=$(echo "$path" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    desc=$(echo "$desc" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    [ -z "$path" ] && continue
    src="$FDSDK_DIR/$path"
    if [ ! -f "$src" ]; then
        report="$report\n  SKIP    $path  (not found in this FDSDK checkout)"
        continue
    fi
    elem_dir="$work_dir/probe-$(basename "$path" .bst)"
    mkdir -p "$elem_dir"
    cp "$src" "$elem_dir/$(basename "$path")"
    out_a="$elem_dir/A"
    out_b="$elem_dir/B"
    err=$("$bin_dir/write-a" \
        --rules-package-path "$repo_root/rules_buildstream_bazel" \
        --bst "$elem_dir/$(basename "$path")" \
        --out "$out_a" \
        --out-b "$out_b" \
        --convert-element-cmake "$bin_dir/convert-element-cmake" 2>&1 >/dev/null) || true
    if [ -z "$err" ]; then
        ok=$((ok+1))
        report="$report\n  OK      $path"
        report="$report\n          $desc"
    else
        fail=$((fail+1))
        first_err=$(echo "$err" | head -1)
        report="$report\n  FAIL    $path"
        report="$report\n          $desc"
        report="$report\n          → $first_err"
    fi
done <<EOF
$probes
EOF

# In-place probes — same curated set as above, but run against the
# real FDSDK tree (no isolation), so write-a's project.conf parser,
# (@): composer, and path-qualified element resolver all engage
# against actual content. The first failure each one hits is the
# deepest gap real-FDSDK hits today.
in_place_report=""
in_place_ok=0
in_place_fail=0
while IFS='|' read -r path desc; do
    path=$(echo "$path" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    desc=$(echo "$desc" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    [ -z "$path" ] && continue
    src="$FDSDK_DIR/$path"
    if [ ! -f "$src" ]; then
        in_place_report="$in_place_report\n  SKIP    $path"
        continue
    fi
    out_a="$work_dir/inplace-A-$(basename "$path" .bst)"
    out_b="$work_dir/inplace-B-$(basename "$path" .bst)"
    err=$("$bin_dir/write-a" \
        --rules-package-path "$repo_root/rules_buildstream_bazel" \
        --bst "$src" \
        --out "$out_a" \
        --out-b "$out_b" \
        --convert-element-cmake "$bin_dir/convert-element-cmake" 2>&1 >/dev/null) || true
    if [ -z "$err" ]; then
        in_place_ok=$((in_place_ok+1))
        in_place_report="$in_place_report\n  OK      $path"
    else
        in_place_fail=$((in_place_fail+1))
        first_err=$(echo "$err" | head -1)
        in_place_report="$in_place_report\n  FAIL    $path"
        in_place_report="$in_place_report\n          → $first_err"
    fi
done <<EOF
$probes
EOF

# Synthesized multi-element FDSDK-shape probe. Renders a tiny
# project (synthetic project.conf + element-path: elements +
# multiple .bst files exercising path-qualified deps, build-depends,
# multi-source elements, source `directory:` flag, and public:
# block tolerance). Demonstrates write-a's multi-element resolution
# pipeline end-to-end without depending on real FDSDK content —
# useful for proving forward progress on the loader / resolver
# punch-list items even when the curated isolated probes (which
# are diagnostic for first-failure) all trip on earlier gaps.
synth_dir="$work_dir/synth"
mkdir -p "$synth_dir/elements/components" "$synth_dir/elements/bootstrap" \
         "$synth_dir/src-bar" "$synth_dir/src-extra" "$synth_dir/src-imp"
cat > "$synth_dir/project.conf" <<'CONF_EOF'
variables:
  prefix: /usr
element-path: elements
CONF_EOF
cat > "$synth_dir/src-bar/CMakeLists.txt" <<'CMAKE_EOF'
cmake_minimum_required(VERSION 3.20)
project(bar)
CMAKE_EOF
cat > "$synth_dir/src-extra/extra.txt" <<'EOF'
extra payload
EOF
cat > "$synth_dir/src-imp/data.txt" <<'EOF'
imp data
EOF
cat > "$synth_dir/elements/bootstrap/bar.bst" <<EOF
kind: cmake
sources:
- kind: local
  path: $synth_dir/src-bar
EOF
# Multi-source kind:import exercising the source `directory:` flag,
# kind:git_repo source metadata (recorded but skipped at staging),
# plus a public: block (real FDSDK declares all three heavily).
cat > "$synth_dir/elements/components/data.bst" <<EOF
kind: import
sources:
- kind: local
  path: $synth_dir/src-imp
- kind: local
  path: $synth_dir/src-extra
  directory: extras
- kind: git_repo
  url: alias:repo.git
  ref: deadbeef
  track: master
public:
  bst:
    split-rules:
      runtime:
        - "/data/**"
EOF
cat > "$synth_dir/elements/components/foo.bst" <<'BST_EOF'
kind: stack
build-depends:
- bootstrap/bar.bst
runtime-depends:
- components/data.bst
BST_EOF
synth_err=$("$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$synth_dir/elements/components/foo.bst" \
    --bst "$synth_dir/elements/bootstrap/bar.bst" \
    --bst "$synth_dir/elements/components/data.bst" \
    --out "$synth_dir/A" \
    --out-b "$synth_dir/B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake" 2>&1 >/dev/null) || true
# Confirm the multi-source / directory: source actually staged
# correctly (parse + dep resolution is necessary but not sufficient;
# the BUILD render must land the files at the expected paths).
synth_extras_check=""
if [ -z "$synth_err" ]; then
    if [ ! -f "$synth_dir/B/elements/components/data/extras/extra.txt" ]; then
        synth_extras_check="multi-source directory:extras failed to stage extra.txt under elements/components/data/extras/"
    fi
fi

# Multi-element subgraph probes — for each curated element, walk
# its transitive dep tree iteratively (load, see "dep X not in
# graph" error, add X's .bst, retry) until either the load
# succeeds or write-a reports a non-dep-resolution failure.
# Surfaces what's actually next once single-element-load isn't the
# bottleneck.
#
# Capped at MAX_ITER iterations per element so a runaway dep
# walker doesn't hang the probe. Real FDSDK elements have ~10-100
# transitive deps; the cap is intentionally generous.
MAX_ITER=300
subgraph_report=""
subgraph_ok=0
subgraph_fail=0
while IFS='|' read -r path desc; do
    path=$(echo "$path" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    desc=$(echo "$desc" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    [ -z "$path" ] && continue
    src="$FDSDK_DIR/$path"
    if [ ! -f "$src" ]; then
        subgraph_report="$subgraph_report\n  SKIP    $path"
        continue
    fi
    set -- --bst "$src"
    iter=0
    final_err=""
    while [ "$iter" -lt "$MAX_ITER" ]; do
        iter=$((iter+1))
        out_a="$work_dir/sub-A-$(basename "$path" .bst)"
        out_b="$work_dir/sub-B-$(basename "$path" .bst)"
        rm -rf "$out_a" "$out_b"
        err=$("$bin_dir/write-a" "$@" \
            --rules-package-path "$repo_root/rules_buildstream_bazel" \
            --out "$out_a" \
            --out-b "$out_b" \
            --convert-element-cmake "$bin_dir/convert-element-cmake" 2>&1 >/dev/null) || true
        if [ -z "$err" ]; then
            final_err=""
            break
        fi
        first=$(echo "$err" | head -1)
        # Extract the missing dep name from
        #   element "X" depends on "Y" which is not in the graph
        missing=$(echo "$first" | sed -n 's/.*depends on "\([^"]*\)" which is not in the graph.*/\1/p')
        if [ -z "$missing" ]; then
            final_err="$first"
            break
        fi
        candidate="$FDSDK_DIR/elements/$missing.bst"
        if [ ! -f "$candidate" ]; then
            final_err="missing dep \"$missing\" not found at $candidate"
            break
        fi
        # Skip if already added (loop guard).
        already=0
        for added in "$@"; do
            if [ "$added" = "$candidate" ]; then
                already=1
                break
            fi
        done
        if [ "$already" = "1" ]; then
            final_err="dep walker looped on $missing"
            break
        fi
        set -- "$@" --bst "$candidate"
    done
    if [ -z "$final_err" ]; then
        subgraph_ok=$((subgraph_ok+1))
        # Count the additional .bsts that got walked in.
        deps_loaded=$(( ($# - 2) / 2 ))
        subgraph_report="$subgraph_report\n  OK      $path  ($deps_loaded transitive deps)"
    else
        subgraph_fail=$((subgraph_fail+1))
        subgraph_report="$subgraph_report\n  FAIL    $path"
        subgraph_report="$subgraph_report\n          → $final_err"
    fi
done <<EOF
$probes
EOF

total=$((ok+fail))
printf "fdsdk-reality-check: %d/%d isolated-element probes succeeded\n\n" "$ok" "$total"
printf "%b\n" "$report"
echo
in_place_total=$((in_place_ok+in_place_fail))
printf "In-place probes (real FDSDK tree, real project.conf): %d/%d succeeded\n" "$in_place_ok" "$in_place_total"
printf "%b\n" "$in_place_report"
echo
sub_total=$((subgraph_ok+subgraph_fail))
printf "Subgraph probes (curated element + transitive deps walked iteratively): %d/%d succeeded\n" "$subgraph_ok" "$sub_total"
printf "%b\n" "$subgraph_report"
echo
echo "Synthetic multi-element probe (FDSDK-shape: kind:cmake + kind:import"
echo "+ kind:stack with path-qualified deps, build-depends + runtime-depends,"
echo "multi-source element with directory: flag, public: block):"
if [ -n "$synth_err" ]; then
    first_err=$(echo "$synth_err" | head -1)
    echo "  FAIL    synthetic"
    echo "          → $first_err"
elif [ -n "$synth_extras_check" ]; then
    echo "  FAIL    synthetic"
    echo "          → $synth_extras_check"
else
    echo "  OK      synthetic"
fi
echo
echo "See docs/fdsdk-reality-check.md for the prioritized punch list."
