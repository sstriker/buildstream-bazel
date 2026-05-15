#!/bin/sh
# meta-gazelle-roundtrip.sh — Phase 7c/7d/8b acceptance gate for the
# gazelle-roundtrip story.
#
# Drives the full pipeline against the hello-world fixture, then
# exercises the post-conversion gazelle-prep contract:
#
#   1. write-a + bazel-build pass (same as scripts/meta-hello.sh)
#      produces project A's per-element BUILD.bazel.out.
#   2. stage-b copies the converted outputs into project B and
#      reports the `elements/<name>` packages whose content changed
#      ($changed) — the Phase 8b "what just re-converted" signal.
#      An idempotent re-stage must report nothing changed.
#   3. Phase 7d: the staged BUILD.bazel carries the
#      `# gazelle:cc_search` file-head directive mirroring the
#      emitted cc_library's `includes`.
#   4. build-cc-index walks project B's elements/ to populate
#      tools/cc_index.json + tools/python_modules.json from the
#      emitted cc_library hdrs (plus the .h-in-srcs cheap
#      mitigation) and any py_library / py_binary names.
#   5. Assertions on the populated indexes — the known hello-
#      world headers must resolve to the expected labels.
#   6. Phase 8b tail: relax-keeps, then `bazel run //:gazelle`,
#      both targeted at $changed (not the whole workspace). Both
#      steps skip outright on an empty changed set — an unguarded
#      relax-keeps / `gazelle --` with no args degrades to a full
#      project-B walk. The gazelle invocation is further guarded
#      on the //:gazelle target existing — present → run; absent
#      (gazelle_cc not yet a bazel_dep) → skip with a message.
#      Both the populated and empty-set tail paths are exercised.
#   7. (Optional) Run buildifier --mode=diff against project B and
#      assert no diff. Skipped if buildifier isn't on PATH.
#
# Run from repo root: scripts/meta-gazelle-roundtrip.sh

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a
CGO_ENABLED=0 go build -o "$bin_dir/stage-b" ./cmd/stage-b
CGO_ENABLED=0 go build -o "$bin_dir/build-cc-index" ./cmd/build-cc-index
CGO_ENABLED=0 go build -o "$bin_dir/relax-keeps" ./cmd/relax-keeps

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

"$bin_dir/write-a" \
    --bst testdata/meta-project/hello-world.bst \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Validate the Phase 7b/8 stub files write-a emitted exist.
# - cc_index.json + python_modules.json start as `{}` (Phase 7c
#   rewrites them).
# - gazelle-rewritable.json starts as a comment + empty patterns
#   list (Phase 8 — operator-edited to declare rewritable
#   genrule patterns).
for f in tools/cc_index.json tools/python_modules.json tools/gazelle-rewritable.json; do
    if [ ! -f "$B/$f" ]; then
        echo "meta-gazelle-roundtrip: Phase 7b/8 stub $f missing in project B" >&2
        exit 1
    fi
done
# overlay.MODULE.bazel at project B root (Phase 8 operator-owned
# overlay loaded by MODULE.bazel's include()).
if [ ! -f "$B/overlay.MODULE.bazel" ]; then
    echo "meta-gazelle-roundtrip: Phase 8 overlay.MODULE.bazel missing in project B" >&2
    exit 1
fi

# Bazel phase: same gating as meta-hello.sh.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-gazelle-roundtrip: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
# Strict-numeric check: bazel --version sometimes prefixes
# its output with timestamps or wrapper banners on CI runners
# (bazelisk under `set -x` shows lines like `[08:30:42]`); the
# `awk $2` extraction can pick those up, and a `cut -d.` of a
# non-version string yields garbage. Require bazel_major to be
# all digits before doing arithmetic — otherwise treat as
# "version unknown, skip build phase" rather than letting the
# subsequent [ ... -lt 9 ] error out with "Illegal number".
case "$bazel_major" in
    ''|*[!0-9]*) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-gazelle-roundtrip: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
    exit 0
fi

META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

bzl_cache="$work_dir/.bazel"

run_bazel() {
    workspace="$1"
    shift
    cmd="$1"
    shift
    # shellcheck disable=SC2086 # META_BAZEL_*_ARGS is intentionally word-split.
    (cd "$workspace" && "$BZL" --output_user_root="$bzl_cache" \
        $META_BAZEL_STARTUP_ARGS \
        "$cmd" "$@" $META_BAZEL_BUILD_ARGS)
}

# phase8b_relax <changed-list>
# relax-keeps over just the changed elements, guarded on a
# non-empty set. An unguarded `relax-keeps` with no package args
# falls back to a full project-B walk (see cmd/relax-keeps's own
# docs) — so with nothing re-converted the guard skips it outright
# rather than silently widening the tail to O(workspace).
phase8b_relax() {
    if [ -z "$1" ]; then
        echo "meta-gazelle-roundtrip: no elements changed; skipping relax-keeps"
        return 0
    fi
    # shellcheck disable=SC2086 # $1 is a newline list fed as args.
    "$bin_dir/relax-keeps" --root "$B" $1
}

# phase8b_gazelle <changed-list>
# `bazel run //:gazelle` over just the changed elements, guarded
# on a non-empty set (a bare `gazelle --` with nothing after the
# `--` rewrites the whole workspace) and on the //:gazelle target
# existing (gazelle_cc isn't a bazel_dep yet — pending a bcr
# release; absent → skip with a message).
phase8b_gazelle() {
    if [ -z "$1" ]; then
        echo "meta-gazelle-roundtrip: no elements changed; skipping gazelle invocation"
        return 0
    fi
    if (cd "$B" && "$BZL" --output_user_root="$bzl_cache" query //:gazelle) >/dev/null 2>&1; then
        # shellcheck disable=SC2086 # $1 is a newline list fed as args.
        run_bazel "$B" run //:gazelle -- $1 2>&1 | tail -5
        echo "meta-gazelle-roundtrip: gazelle ran, targeted at changed elements: [$1]"
    else
        echo "meta-gazelle-roundtrip: //:gazelle not defined (gazelle_cc not yet a bazel_dep); skipping gazelle invocation"
    fi
}

# === Project A build (produces BUILD.bazel.out) ===
run_bazel "$A" build //elements/hello-world:hello-world_converted 2>&1 | tail -5
build_out_a="$A/bazel-bin/elements/hello-world/BUILD.bazel.out"
if [ ! -f "$build_out_a" ]; then
    echo "meta-gazelle-roundtrip: BUILD.bazel.out not produced" >&2
    exit 1
fi

# === Stage A's output into B (Phase 8b: changed-element signal) ===
# stage-b copies every element's converted BUILD.bazel.out into
# project B and prints the `elements/<name>` packages whose content
# actually changed. That changed set is what the targeted gazelle
# step below consumes — the write-a + Bazel path's replacement for
# the orchestrator's res.Converted.
changed=$("$bin_dir/stage-b" --project-a "$A" --project-b "$B")
if ! printf '%s\n' "$changed" | grep -qx "elements/hello-world"; then
    echo "meta-gazelle-roundtrip: stage-b did not report elements/hello-world as changed on first stage" >&2
    echo "  changed set was: [$changed]" >&2
    exit 1
fi
echo "meta-gazelle-roundtrip: stage-b staged + reported changed: [$changed]"

# Re-stage with no re-conversion in between: stage-b must report an
# empty changed set. This proves the signal is a content diff, not a
# blind copy — a driver re-running the pipeline without source edits
# would skip the gazelle step entirely.
restage=$("$bin_dir/stage-b" --project-a "$A" --project-b "$B")
if [ -n "$restage" ]; then
    echo "meta-gazelle-roundtrip: stage-b reported changes on an idempotent re-stage: [$restage]" >&2
    exit 1
fi
echo "meta-gazelle-roundtrip: stage-b re-stage is a no-op (content-diff signal works)"

# === Phase 7d: # gazelle:cc_search directive present ===
# The hello-world fixture's CMakeLists sets
# target_include_directories(hello ... include), so the emitted
# cc_library carries includes = ["include"] and the cc emitter
# must mirror it as a file-head `# gazelle:cc_search include`
# directive (deduped + sorted across the package's targets).
staged_build="$B/elements/hello-world/BUILD.bazel"
if ! grep -qx "# gazelle:cc_search include" "$staged_build"; then
    echo "meta-gazelle-roundtrip: staged BUILD.bazel missing '# gazelle:cc_search include' directive (Phase 7d)" >&2
    cat "$staged_build" >&2
    exit 1
fi
echo "meta-gazelle-roundtrip: # gazelle:cc_search directive present (Phase 7d)"

# === Populate cc_index.json + python_modules.json ===
"$bin_dir/build-cc-index" \
    --root "$B" \
    --out-cc-index tools/cc_index.json \
    --out-python-modules tools/python_modules.json

# Assertions: hello-world's exported header must map to the
# canonical label. Phase 3's label shortening (//pkg:pkg → //pkg)
# applies only when the cc_library's target name equals the
# package basename. The hello-world fixture's CMakeLists defines
# `add_library(hello ...)` — target "hello" in package
# "elements/hello-world" — so basename ("hello-world") differs
# from target name ("hello") and the canonical form keeps the
# `:hello` tail.
expected_header="elements/hello-world/include/hello.h"
expected_label="//elements/hello-world:hello"
if ! grep -qE "\"$expected_header\":[[:space:]]*\"$expected_label\"" "$B/tools/cc_index.json"; then
    echo "meta-gazelle-roundtrip: cc_index.json missing $expected_header → $expected_label mapping" >&2
    cat "$B/tools/cc_index.json" >&2
    exit 1
fi
echo "meta-gazelle-roundtrip: cc_index.json populated; hello.h → $expected_label"

# === build-cc-index --imports-manifest: external-repo edge ===
# A genuinely-external dep's header / module universe lives outside
# project B; only the imports manifest knows the resolving label.
# Re-run build-cc-index with a hand-written manifest and assert the
# external header / module fold in while the in-project hello.h
# claim survives (the manifest only gap-fills).
cat > "$work_dir/external-imports.json" <<'EOF'
{
  "version": 1,
  "elements": [
    {
      "name": "openssl",
      "exports": [
        {
          "cmake_target": "OpenSSL::SSL",
          "bazel_label": "@openssl//:ssl",
          "exported_headers": ["openssl/ssl.h"]
        }
      ]
    },
    {
      "name": "requests",
      "exports": [
        {
          "cmake_target": "Requests::Requests",
          "bazel_label": "@pip//requests",
          "import_modules": ["requests"]
        }
      ]
    }
  ]
}
EOF
"$bin_dir/build-cc-index" \
    --root "$B" \
    --out-cc-index tools/cc_index.json \
    --out-python-modules tools/python_modules.json \
    --imports-manifest "$work_dir/external-imports.json"
if ! grep -qE '"openssl/ssl\.h":[[:space:]]*"@openssl//:ssl"' "$B/tools/cc_index.json"; then
    echo "meta-gazelle-roundtrip: cc_index.json missing folded external header openssl/ssl.h" >&2
    cat "$B/tools/cc_index.json" >&2
    exit 1
fi
if ! grep -qE '"requests":[[:space:]]*"@pip//requests"' "$B/tools/python_modules.json"; then
    echo "meta-gazelle-roundtrip: python_modules.json missing folded external module requests" >&2
    cat "$B/tools/python_modules.json" >&2
    exit 1
fi
# In-project header must still resolve to its own label — the
# manifest fold is additive, not a replacement.
if ! grep -qE "\"$expected_header\":[[:space:]]*\"$expected_label\"" "$B/tools/cc_index.json"; then
    echo "meta-gazelle-roundtrip: imports-manifest fold clobbered the in-project header mapping" >&2
    cat "$B/tools/cc_index.json" >&2
    exit 1
fi
echo "meta-gazelle-roundtrip: --imports-manifest folded external edge (openssl/ssl.h, requests)"

# === Phase 8b: targeted relax-keeps over the changed elements ===
# The Phase 8b gazelle tail runs relax-keeps + gazelle over only the
# elements stage-b reported as changed (`$changed`), not the whole
# workspace. Here that's `elements/hello-world`. With the default
# empty patterns list in tools/gazelle-rewritable.json, relax-keeps
# must produce no BUILD changes — the operator hasn't opted any
# genrule pattern into rewriting yet.
build_before=$(cat "$B/elements/hello-world/BUILD.bazel")
phase8b_relax "$changed"
build_after=$(cat "$B/elements/hello-world/BUILD.bazel")
if [ "$build_before" != "$build_after" ]; then
    echo "meta-gazelle-roundtrip: relax-keeps modified BUILD with default empty patterns:" >&2
    diff <(echo "$build_before") <(echo "$build_after") >&2 || true
    exit 1
fi
echo "meta-gazelle-roundtrip: relax-keeps (empty patterns, targeted at \$changed) is a no-op as expected"

# === Optional: buildifier no-op contract ===
# Phase 3 promised buildifier --mode=fix is a no-op against our
# emit. Verify that's still true after Phase 7a's # keep markers
# + Phase 7b's directives + Phase 7c's populated indexes.
if command -v buildifier >/dev/null; then
    # Run against the per-element + tools BUILD.bazel files.
    # Capture diffs (--mode=diff is non-destructive; exit 4 = diffs
    # found, exit 0 = clean, other = bug).
    bf_out=$(buildifier --mode=diff -r "$B" 2>&1) || bf_rc=$?
    bf_rc=${bf_rc:-0}
    if [ "$bf_rc" = 4 ]; then
        echo "meta-gazelle-roundtrip: buildifier --mode=diff reports changes (Phase 3 no-op contract violated):" >&2
        echo "$bf_out" >&2
        exit 1
    fi
    if [ "$bf_rc" != 0 ]; then
        echo "meta-gazelle-roundtrip: buildifier failed with exit $bf_rc:" >&2
        echo "$bf_out" >&2
        exit 1
    fi
    echo "meta-gazelle-roundtrip: buildifier --mode=diff clean (Phase 3 contract preserved)"
else
    echo "meta-gazelle-roundtrip: buildifier not on PATH; skipping no-op assertion"
fi

# === Phase 8b: targeted gazelle over the changed elements ===
# The post-conversion gazelle step is an opt-in tail on the driver:
# once the operator declares gazelle / gazelle_cc in
# overlay.MODULE.bazel, the driver runs
# `bazel run //:gazelle -- <changed elements>` so only the packages
# that re-converted on this run get rewritten. This gate IS such a
# driver — it always attempts the tail. phase8b_gazelle self-guards
# on the //:gazelle target existing (gazelle_cc isn't a bazel_dep
# yet — pending a bcr release; absent → skip with a message).
phase8b_gazelle "$changed"

# Exercise the empty-changed-set guard. An idempotent re-run
# produces an empty changed set (proven above: $restage is empty),
# and both tail steps must then skip outright. Without the guard,
# `relax-keeps` with no args falls back to a full project-B walk
# and `bazel run //:gazelle --` with nothing after the `--`
# rewrites the whole workspace — both silently turning the
# "targeted, O(changed)" tail into O(workspace), the exact thing
# Phase 8b exists to avoid. The main tail above always runs with
# the non-empty first $changed, so the no-op path is covered here.
echo "meta-gazelle-roundtrip: exercising the empty-changed-set tail guard"
phase8b_relax "$restage"
phase8b_gazelle "$restage"

echo "meta-gazelle-roundtrip: ok (write-a render + bazel-build + stage-b + build-cc-index + relax-keeps + buildifier no-op + Phase 8b gazelle tail)"
