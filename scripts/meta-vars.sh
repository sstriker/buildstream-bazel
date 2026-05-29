#!/bin/sh
# meta-vars.sh — acceptance gate for the BuildStream variable resolver.
#
# The vars-greet fixture is a kind:manual element whose .bst overrides
# %{prefix} (project default /usr → /opt/freedesktop-sdk) and defines
# a fresh per-element variable %{greeting-dir} that composes onto a
# derived default (%{datadir} = %{prefix}/share). A single install
# command references both, so the resolver has to:
#
#   - look up %{greeting-dir} in element variables,
#   - recursively expand its RHS through %{datadir} → %{prefix}/share,
#   - apply the element-level prefix override on the way down, and
#   - leave %{install-root} as a runtime sentinel that the genrule
#     cmd's $$INSTALL_ROOT swap resolves at action time.
#
# Pipeline:
#   1. cmd/write-a parses greet.bst, renders project A (kind:manual
#      genrule with the install command variable-expanded). Render-
#      phase grep checks: the rendered cmd contains the resolved
#      path /opt/freedesktop-sdk/share/greetings/hello.txt with
#      %{install-root} mapped to $$INSTALL_ROOT, and no literal
#      %{prefix} / %{greeting-dir} / %{datadir} leak through.
#   2. bazel build //elements/greet:greet_install in project A.
#   3. The driver reads bazel-bin/elements/greet/greet_install/install/
#      and asserts opt/freedesktop-sdk/share/greetings/hello.txt
#      exists with the fixture's expected content.
#
# Bazel-availability gating + META_BAZEL_*_ARGS overrides mirror
# scripts/meta-manual.sh.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/write-a" ./cmd/write-a

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT

A="$work_dir/A"
B="$work_dir/B"

fixture="testdata/meta-project/vars-greet"

"$bin_dir/write-a" \
    --rules-package-path "$repo_root/rules_buildstream_bazel" \
    --bst "$fixture/greet.bst" \
    --out "$A" \
    --out-b "$B" \
    --convert-element-cmake "$bin_dir/convert-element-cmake"

# Render-phase checks.
for f in MODULE.bazel BUILD.bazel \
        elements/greet/BUILD.bazel \
        elements/greet/sources/greeting.txt; do
    if [ ! -f "$A/$f" ]; then
        echo "meta-vars: missing rendered project A file $f" >&2
        exit 1
    fi
done
# The resolved install command, with %{prefix} → /opt/freedesktop-sdk
# and %{greeting-dir} → %{datadir}/greetings → /opt/.../share/greetings.
# %{install-root} is the runtime sentinel that becomes $$INSTALL_ROOT.
for marker in 'name = "greet_install"' \
              '# === install ===' \
              '$$INSTALL_ROOT/opt/freedesktop-sdk/share/greetings/hello.txt' \
              'pipeline_install('; do
    if ! grep -qF "$marker" "$A/elements/greet/BUILD.bazel"; then
        echo "meta-vars: project A greet BUILD missing marker: $marker" >&2
        cat "$A/elements/greet/BUILD.bazel" >&2
        exit 1
    fi
done
# Negative checks: nothing %{...} should leak through unsubstituted
# (besides the runtime sentinel %{install-root} which the cmd
# rendering replaces with $$INSTALL_ROOT, so it must not appear
# either in the BUILD).
for leak in '%{prefix}' '%{datadir}' '%{greeting-dir}' '%{install-root}'; do
    if grep -qF "$leak" "$A/elements/greet/BUILD.bazel"; then
        echo "meta-vars: unsubstituted reference $leak leaked into rendered BUILD:" >&2
        grep -nF "$leak" "$A/elements/greet/BUILD.bazel" >&2
        exit 1
    fi
done
echo "meta-vars: render OK"

# Bazel phase. Same gating as the other meta-* drivers.
if command -v bazel >/dev/null; then
    BZL=bazel
elif command -v bazelisk >/dev/null; then
    BZL=bazelisk
else
    echo "meta-vars: render OK; bazel not on PATH, skipping build phase"
    exit 0
fi
bazel_major=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bazel_major" in
    [0-9]*) ;;
    *) bazel_major=0 ;;
esac
if [ "$bazel_major" -lt 9 ]; then
    echo "meta-vars: render OK; bazel $($BZL --version | head -1) is < 9 (Bazel 9 is the floor: bzlmod + load() requirement for cc_*), skipping build phase"
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

# === bazel build project A ===
run_bazel "$A" build //elements/greet:greet_install 2>&1 | tail -10
# The install root is a TreeArtifact directory (declare_directory),
# read in place (no untar).
extract_dir="$A/bazel-bin/elements/greet/greet_install/install"
if [ ! -d "$extract_dir" ]; then
    echo "meta-vars: install-root TreeArtifact not produced at $extract_dir" >&2
    exit 1
fi

# === Verify in place ===
greeting="$extract_dir/opt/freedesktop-sdk/share/greetings/hello.txt"
if [ ! -f "$greeting" ]; then
    echo "meta-vars: install root missing opt/freedesktop-sdk/share/greetings/hello.txt" >&2
    echo "  install root contents:" >&2
    find "$extract_dir" | sed 's/^/    /' >&2
    exit 1
fi
content=$(cat "$greeting")
expected="Hello from a custom prefix!"
if [ "$content" != "$expected" ]; then
    echo "meta-vars: hello.txt content mismatch" >&2
    echo "  want: $expected" >&2
    echo "  got:  $content" >&2
    exit 1
fi
echo "meta-vars: install-root TreeArtifact contains opt/freedesktop-sdk/share/greetings/hello.txt with expected content"

echo "meta-vars: ok (element variable overrides resolved through derived defaults; install tarball lands at the overridden path)"
