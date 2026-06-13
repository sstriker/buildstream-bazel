#!/bin/sh
# meta-cmake-file-download-http.sh — render+build gate for the
# file(DOWNLOAD) http_file hand-off (ROADMAP campaign item 3).
#
# file(DOWNLOAD) bakes the fetched bytes HERMETICALLY by default (no
# network at build time — the downstream-envelope contract). On top of
# that, the converter surfaces a structured `download` todo carrying the
# ready-to-paste http_file MODULE stanza (urls + sha256 translated from
# the traced EXPECTED_HASH + the @repo//file label) for an operator who
# wants the repo-rule form.
#
# Asserts: the hermetic bake renders with the download facet; the
# download todo carries the http_file stanza with the SAME sha256 cmake
# verified; then bazel-builds the consumer against the baked header.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

fixture="$repo_root/converter/testdata/sample-projects/file-download-http"
expected_sha="$(sha256sum "$fixture/payload.h.in" | awk '{print $1}')"
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

grep -qF 'cmake-codegen-download-bake' "$build" || fail "file(DOWNLOAD) did not bake with the download facet"
python3 - "$work_dir/todos.json" "$expected_sha" <<'PY'
import json, sys
todos = json.load(open(sys.argv[1]))["todos"]
sha = sys.argv[2]
dl = [t for t in todos if t["kind"] == "download"]
assert len(dl) == 1, f"expected 1 download todo, got {len(dl)}"
st = dl[0]["suggested_shape"]
for want in ("http_file(", f'sha256 = "{sha}"', "@dl_dl_config_h//file"):
    assert want in st, f"download stanza missing {want!r}:\n{st}"
assert dl[0]["disposition"] == "improvement", dl[0]["disposition"]
print("download todo + http_file stanza OK")
PY
echo "ok  meta-cmake-file-download-http: file(DOWNLOAD) bakes hermetically + emits the http_file stanza (sha256 from EXPECTED_HASH)"

# --- Bazel-build half: the consumer compiles against the baked header ---
if command -v bazel >/dev/null 2>&1; then BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then BZL=bazelisk
else echo "ok  meta-cmake-file-download-http: bazel not on PATH, skipping build half"; exit 0; fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 7 ]; then echo "ok  meta-cmake-file-download-http: bazel < 7, skipping build half"; exit 0; fi

ws="$work_dir/ws"
mkdir -p "$ws/src"
cp "$fixture"/src/lib.c "$ws/src/"
cp "$build" "$ws/BUILD.bazel"
cat > "$ws/MODULE.bazel" <<EOF
module(name = "dlhttp", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF
bzlcache="$work_dir/.bzcache"
# shellcheck disable=SC2086
if ! ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
        build ${META_BAZEL_BUILD_ARGS:-} //:dlhttp ) >"$work_dir/bazel.log" 2>&1; then
    echo "FAIL: consumer failed to build against the baked downloaded header"
    sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-file-download-http: consumer compiles against the baked downloaded header"
