#!/bin/sh
# survey-gazelle-roundtrip.sh — run the gazelle_cc round-trip on WILD
# corpus projects, as the strongest lens-2 (structural idiom) check.
#
# Lens 2 ("are we emitting non-idiomatic Bazel?") has three oracles
# (see docs/survey-corpus.md): bazelidiom.Audit (semantic, per-survey),
# buildifier -mode=diff (syntactic, no-op by construction), and the
# gazelle round-trip (structural — "is the output already what
# gazelle_cc would generate?"). The first two run in the normal survey;
# this script adds the third for arbitrary corpus members rather than
# only the project-B fixture (scripts/meta-cmake-split-gazelle.sh).
#
# The idea: convert a real CMake project with --split-packages, overlay
# the emitted BUILDs onto a copy of its sources in a scratch Bazel
# module wired with gazelle_cc, then `bazel run //:gazelle`. If the pass
# is NOT a no-op, gazelle_cc would relocate/rewrite our cc_* rules —
# i.e. the split output isn't structurally idiomatic. A clean run + a
# second-pass fixpoint is the healthy state.
#
# Usage:
#   scripts/survey-gazelle-roundtrip.sh <name>=<src-dir> [<name>=<src-dir> ...]
#
# Env:
#   META_GAZELLE_USE_HOST_GO=1   use host Go toolchain instead of
#       gazelle_cc's transitive go_sdk.download (sandbox: go.dev egress
#       blocked). Mirrors scripts/meta-cmake-split-gazelle.sh.
#   GAZELLE_DIRS="d1 d2"         survey only these source subdirs of a
#       project (e.g. "build/cmake" for zstd). Applied to every <name>.
#   META_BAZEL_STARTUP_ARGS / META_BAZEL_BUILD_ARGS  passthrough.
#
# Skips cleanly (exit 0, "render OK" style) when bazel>=9 / cmake / ninja
# aren't present — same contract as the meta-* gates.

set -eu

if [ "$#" -lt 1 ]; then
    echo "usage: $0 <name>=<src-dir> [<name>=<src-dir> ...]" >&2
    exit 2
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir=$(mktemp -d)
trap 'chmod -R u+w "$work_dir" 2>/dev/null || true; rm -rf "$work_dir"' EXIT

bin_dir="$work_dir/bin"
mkdir -p "$bin_dir"
CGO_ENABLED=0 go build -o "$bin_dir/convert-element-cmake" ./converter/cmd/convert-element-cmake

# === Tool gating (mirrors meta-cmake-split-gazelle.sh). ===
have_tools=1
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "survey-gazelle-roundtrip: bazel not on PATH; nothing to run" >&2
    have_tools=0
fi
if [ "$have_tools" = "1" ]; then
    major=$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')
    if [ -z "$major" ] || [ "$major" -lt 9 ]; then
        echo "survey-gazelle-roundtrip: bazel < 9 (bzlmod + load() floor); skipping" >&2
        have_tools=0
    fi
fi
for tool in cmake ninja; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "survey-gazelle-roundtrip: $tool not on PATH; skipping" >&2
        have_tools=0
    fi
done
if [ "$have_tools" = "0" ]; then
    echo "survey-gazelle-roundtrip: prerequisites missing; skipped (not a failure)"
    exit 0
fi

bzl_cache="${SURVEY_GAZELLE_BZL_CACHE:-$HOME/.cache/survey-gazelle-bazel}"
mkdir -p "$bzl_cache"
META_BAZEL_STARTUP_ARGS=${META_BAZEL_STARTUP_ARGS:-}
META_BAZEL_BUILD_ARGS=${META_BAZEL_BUILD_ARGS:-}

# module_scaffold writes MODULE.bazel + APPENDS the gazelle_cc wiring to
# the converter-written root BUILD.bazel (must merge — the converter owns
# the root BUILD's package() + cc rules). Versions match write-a's
# --gazelle-cc emission (cmd/write-a/main.go) so the harness exercises
# the same gazelle_cc the production flow wires.
module_scaffold() {
    m="$1"
    cat >"$m/MODULE.bazel" <<'EOF'
module(name = "survey_gazelle_roundtrip", version = "0.0.0")

# Dep set must cover every repo the converter can emit load() lines
# against: rules_cc (cc_library/cc_binary) + rules_pkg (install(FILES)/
# install(DIRECTORY) -> pkg_files). Versions match write-a's project-B
# MODULE.bazel emission (cmd/write-a/main.go) so the harness exercises
# the same dependency universe the production flow wires.
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "rules_pkg", version = "1.0.1")
bazel_dep(name = "rules_go", version = "0.59.0")
bazel_dep(name = "gazelle", version = "0.46.0")
bazel_dep(name = "gazelle_cc", version = "0.5.0")
EOF
    if [ "${META_GAZELLE_USE_HOST_GO:-}" = "1" ]; then
        cat >>"$m/MODULE.bazel" <<'EOF'

# Sandbox: gazelle_cc's transitive go_sdk.download hits go.dev (blocked).
# Use the host Go toolchain instead.
go_sdk = use_extension("@rules_go//go:extensions.bzl", "go_sdk")
go_sdk.host()
EOF
    fi
    # Append the gazelle_binary + gazelle pair to the root BUILD (the
    # converter wrote it with package()+cc rules; we add the driver).
    cat >>"$m/BUILD.bazel" <<'EOF'

load("@gazelle//:def.bzl", "gazelle", "gazelle_binary")

gazelle_binary(
    name = "gazelle_cc_bin",
    languages = ["@gazelle_cc//language/cc"],
)

gazelle(
    name = "gazelle",
    gazelle = ":gazelle_cc_bin",
)
EOF
}

# run_gazelle runs `bazel run //:gazelle` in module dir $1, propagating
# the real exit status (no masking `| tail`).
run_gazelle() {
    m="$1"
    rc=0
    log="$work_dir/gazelle.log"
    # shellcheck disable=SC2086
    (cd "$m" && "$BZL" --output_user_root="$bzl_cache" --noworkspace_rc \
        $META_BAZEL_STARTUP_ARGS \
        run //:gazelle --lockfile_mode=off $META_BAZEL_BUILD_ARGS) >"$log" 2>&1 || rc=$?
    tail -15 "$log"
    return $rc
}

overall=0

for spec in "$@"; do
    name="${spec%%=*}"
    src="${spec#*=}"
    if [ "$name" = "$spec" ] || [ -z "$src" ]; then
        echo "survey-gazelle-roundtrip: bad spec '$spec' (want <name>=<src-dir>)" >&2
        overall=1
        continue
    fi
    if [ ! -d "$src" ]; then
        echo "survey-gazelle-roundtrip: $name: source dir '$src' not found (fetch it first)" >&2
        overall=1
        continue
    fi

    # Optionally retarget to a source subdir (zstd's build/cmake, etc.).
    cm_root="$src"
    if [ -n "${GAZELLE_DIRS:-}" ]; then
        for sub in $GAZELLE_DIRS; do
            if [ -d "$src/$sub" ] && [ -f "$src/$sub/CMakeLists.txt" ]; then
                cm_root="$src/$sub"
                break
            fi
        done
    fi

    echo "=== $name ($cm_root) ==="
    m="$work_dir/m-$name"
    mkdir -p "$m"
    # Copy the source tree so the converted per-package BUILDs sit beside
    # the sources they reference (split output is co-located by design).
    cp -a "$cm_root/." "$m/"
    # Strip the project's OWN Bazel setup so gazelle/bazel see only our
    # converted tree + the gazelle_cc scaffold — not the project's
    # bundled module/registry/lockfile/rc (different dependency universe)
    # nor its hand-written BUILD files (which target a different layout
    # and would race our split output). gazelle then starts from our
    # converted BUILDs alone.
    find "$m" \( -name MODULE.bazel -o -name MODULE.bazel.lock \
        -o -name WORKSPACE -o -name WORKSPACE.bazel -o -name WORKSPACE.bzlmod \
        -o -name .bazelrc -o -name .bazelversion \
        -o -name BUILD -o -name BUILD.bazel \) -type f -delete 2>/dev/null || true

    # Convert with --split-packages, overlaying BUILDs at module root.
    if ! "$bin_dir/convert-element-cmake" \
        --source-root "$cm_root" \
        --split-packages \
        --bazel-package-path "" \
        --diagnostics \
        --out-build "$m/BUILD.bazel" \
        >"$work_dir/convert-$name.log" 2>&1
    then
        echo "FAIL $name: convert failed (cmake configure or convert) — see log"
        tail -8 "$work_dir/convert-$name.log" | sed 's/^/   /'
        overall=1
        continue
    fi

    module_scaffold "$m"

    # snapshot_builds <destdir>: copy every BUILD.bazel under $m into
    # destdir at its module-relative path (re-find each time so gazelle-
    # created BUILDs are captured too).
    snapshot_builds() {
        dest="$1"
        rm -rf "$dest"; mkdir -p "$dest"
        find "$m" -name BUILD.bazel | while IFS= read -r f; do
            rel="${f#"$m"/}"
            mkdir -p "$dest/$(dirname "$rel")"
            cp "$f" "$dest/$rel"
        done
    }

    before="$work_dir/before-$name"
    mid="$work_dir/mid-$name"
    after="$work_dir/after-$name"

    snapshot_builds "$before"
    if ! run_gazelle "$m"; then
        # A gazelle/bazel ERROR means the converted BUILDs don't even
        # LOAD — a non-idiomatic emission the converter produced (e.g.
        # the split glob-labelize bug: `glob(["//pkg:dir/**"])`). That's
        # a real lens-2 defect: HARD FAIL regardless of strict mode.
        echo "FAIL $name: \`bazel run //:gazelle\` errored — converted BUILDs don't load (non-idiomatic emission; see log above)"
        overall=1
        continue
    fi
    snapshot_builds "$mid"

    # Second pass: does gazelle_cc CONVERGE to a fixpoint? A second-pass
    # diff means oscillating/unstable output. On a WILD standalone
    # project this often reflects gazelle_cc's own resolver limits (e.g.
    # unmapped internal-header includes it can't attribute to a library
    # without the project's real Bazel config) as much as our output, so
    # by default it's a REPORTED datapoint, not a red gate — keeping the
    # instrument pointable at arbitrary corpus members. SURVEY_GAZELLE_STRICT=1
    # escalates non-convergence to a hard failure for CI gating.
    if ! run_gazelle "$m"; then
        echo "FAIL $name: second \`bazel run //:gazelle\` errored (see log above)"
        overall=1
        continue
    fi
    snapshot_builds "$after"
    if ! diff -ru "$mid" "$after" >"$work_dir/fixpoint-$name.txt" 2>&1; then
        nfp_changed=$(grep -c '^diff ' "$work_dir/fixpoint-$name.txt" 2>/dev/null || echo "?")
        if [ "${SURVEY_GAZELLE_STRICT:-}" = "1" ]; then
            echo "FAIL $name: gazelle_cc is NOT a fixpoint — second pass changed $nfp_changed BUILD file(s) (strict mode):"
            sed 's/^/   /' "$work_dir/fixpoint-$name.txt" | head -40
            overall=1
        else
            echo "nonfixpoint $name: gazelle_cc did not converge — second pass changed $nfp_changed BUILD file(s) (often gazelle_cc's standalone resolver limits, e.g. internal-header attribution; datapoint, not a hard failure — set SURVEY_GAZELLE_STRICT=1 to gate). First 30 lines:"
            sed 's/^/   /' "$work_dir/fixpoint-$name.txt" | head -30
        fi
        continue
    fi

    # Idiom signal (reported, not failed): how much the FIRST pass
    # changed. gazelle_cc relocating/rewriting our cc_* rules means the
    # split output isn't yet in gazelle's canonical layout. A no-op is
    # ideal; drift is a quality datapoint, not a hard failure (the
    # converter+gazelle_cc design intends gazelle to own the layout
    # going forward — see scripts/meta-cmake-split-gazelle.sh).
    if diff -ru "$before" "$mid" >"$work_dir/drift-$name.txt" 2>&1; then
        echo "ok  $name: gazelle_cc round-trip is a no-op AND a fixpoint (structurally idiomatic)"
    else
        changed=$(grep -c '^diff ' "$work_dir/drift-$name.txt" 2>/dev/null || echo "?")
        echo "drift $name: gazelle_cc rewrote $changed BUILD file(s) on the first pass (converges to a fixpoint; see diff). Idiom datapoint, not a hard failure:"
        sed 's/^/   /' "$work_dir/drift-$name.txt" | head -50
    fi
done

echo ""
# Verdicts: ok (no-op + fixpoint) / drift (rewrites, converges) /
# nonfixpoint (doesn't converge — datapoint unless strict) are all
# success exits; only a load/parse error (or strict-mode non-fixpoint)
# sets overall=1, because that's the converter emitting Bazel that
# doesn't even load.
if [ "$overall" = "0" ]; then
    echo "ok survey-gazelle-roundtrip: every project's converted BUILDs load + gazelle_cc maintains them (see per-project verdicts above)"
else
    echo "survey-gazelle-roundtrip: at least one project's converted BUILDs failed to load under gazelle_cc (non-idiomatic emission); see FAIL lines above"
fi
exit "$overall"
