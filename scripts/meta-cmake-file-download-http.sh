#!/bin/sh
# meta-cmake-file-download-http.sh — render+build gate for the
# file(DOWNLOAD) http_file hand-off (ROADMAP campaign item 3).
#
# file(DOWNLOAD) bakes the fetched bytes HERMETICALLY by default (no
# network at build time — the downstream-envelope contract). On top of
# that, the converter surfaces a structured `download` todo carrying the
# ready-to-paste http_file MODULE stanza (urls + SRI integrity translated
# from the traced EXPECTED_HASH + the @repo//file label) for an operator
# who wants the repo-rule form.
#
# --lift-download (opt-in) goes further: the producer becomes a genrule
# copying @<repo>//file from an http_file repo, and --out-download-repos
# writes the download-repos.json lockfile the staged module extension
# reads (the two-phase lockfile flow).
#
# Asserts: the hermetic bake renders with the download facet; the download
# todo carries the http_file stanza with the SRI integrity derived from
# the sha256 cmake verified; --lift-download renders the fetch genrule +
# the lockfile; then bazel-builds the consumer against the baked header.
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
import base64, binascii, json, sys
todos = json.load(open(sys.argv[1]))["todos"]
sri = "sha256-" + base64.b64encode(binascii.unhexlify(sys.argv[2])).decode()
dl = [t for t in todos if t["kind"] == "download"]
assert len(dl) == 1, f"expected 1 download todo, got {len(dl)}"
st = dl[0]["suggested_shape"]
for want in ("http_file(", f'integrity = "{sri}"', "@dl_dl_config_h//file"):
    assert want in st, f"download stanza missing {want!r}:\n{st}"
assert dl[0]["disposition"] == "improvement", dl[0]["disposition"]
print("download todo + http_file stanza OK (SRI integrity from EXPECTED_HASH)")
PY
echo "ok  meta-cmake-file-download-http: file(DOWNLOAD) bakes hermetically + emits the http_file stanza (SRI integrity from EXPECTED_HASH)"

# --- Lift half: --lift-download rewires the producer to a fetch genrule
# sourcing @<repo>//file, and --out-download-repos writes the lockfile. ---
build_lift="$work_dir/BUILD.lift.bazel"
lock="$work_dir/download-repos.json"
"$bin_dir/convert-element-cmake" \
    --source-root "$fixture" \
    --lift-download \
    --out-build "$build_lift" \
    --out-download-repos "$lock" \
    >"$work_dir/convert.lift.stdout" 2>"$work_dir/convert.lift.stderr" || {
    echo "FAIL: convert-element-cmake --lift-download exited non-zero"
    sed 's/^/   stderr: /' "$work_dir/convert.lift.stderr"
    exit 1
}
grep -qF 'cmake-codegen-download-fetch' "$build_lift" || { echo "FAIL: --lift-download did not emit the fetch facet"; sed 's/^/   /' "$build_lift"; exit 1; }
grep -qF '@dl_dl_config_h//file' "$build_lift" || { echo "FAIL: fetch genrule does not source the http_file repo"; sed 's/^/   /' "$build_lift"; exit 1; }
grep -qF 'cmake-codegen-download-bake' "$build_lift" && { echo "FAIL: --lift-download still byte-baked the download"; sed 's/^/   /' "$build_lift"; exit 1; }
python3 - "$lock" "$expected_sha" <<'PY'
import base64, binascii, json, sys
lock = json.load(open(sys.argv[1]))
sri = "sha256-" + base64.b64encode(binascii.unhexlify(sys.argv[2])).decode()
assert lock["schema_version"] == 1, lock
repos = lock["repos"]
assert len(repos) == 1, f"expected 1 repo, got {len(repos)}: {repos}"
r = repos[0]
assert r["repo"] == "dl_dl_config_h", r
assert r["url"].startswith("file://") and r["url"].endswith("/payload.h.in"), r
assert r["integrity"] == sri, (r["integrity"], sri)
assert r["downloaded_file_path"] == "dl_config.h", r
print("download-repos.json lockfile OK (repo + url + SRI integrity + downloaded_file_path)")
PY
echo "ok  meta-cmake-file-download-http: --lift-download renders the fetch genrule + the download-repos.json lockfile"

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
