#!/bin/sh
# meta-cmake-vcs-stamp.sh — render gate for the VCS-stamp lift.
#
# Drives convert-element-cmake against
# converter/testdata/sample-projects/vcs-stamp, whose CMakeLists captures a
# git revision via `execute_process(git rev-parse OUTPUT_VARIABLE GIT_SHA)`
# and feeds it into a `configure_file` (@GIT_SHA@ in version.h.in). The
# converter classifies the execute_process as a stamp (BucketStamp) and
# lifts the configure_file to a cmake_configure_file rule carrying
# `stamp_values = {GIT_SHA: STABLE_GIT_SHA}`.
#
# Then it proves the lift is load-bearing at Bazel build time:
#   1. convert exits 0 and the emitted BUILD carries
#      stamp_values = {"GIT_SHA": "STABLE_GIT_SHA"}.
#   2. `bazel build //:version.h` WITH --stamp + a --workspace_status_command
#      that emits STABLE_GIT_SHA re-reads the LIVE revision (not the
#      convert-time one) into the generated header.
#   3. The same build WITHOUT --stamp falls back to the convert-time baked
#      revision (the cmake-configured git HEAD), so the header still builds.
#
# Gating: skips cleanly when cmake / git / go are absent, and self-skips the
# bazel half when neither bazel nor bazelisk is on PATH, or the detected
# bazel is older than 9 — the convert + lift assertion is the always-on
# contract.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v git >/dev/null 2>&1 || { echo "skip: git not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }

fixture_src="$repo_root/converter/testdata/sample-projects/vcs-stamp"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Stage the fixture as its OWN git repo in a temp tree so `git rev-parse
# HEAD` at cmake-configure time resolves to a known, isolated commit (not
# this repo's HEAD). The recorded sha is the convert-time "baked" value the
# no-stamp build must fall back to.
ws="$work_dir/ws"
mkdir -p "$ws"
cp -R "$fixture_src/." "$ws/"
(
  cd "$ws" && git init -q && git add -A &&
    git -c commit.gpgsign=false -c user.email=ci@example.com -c user.name=ci \
      commit --no-gpg-sign -qm init
)
baked_sha="$(cd "$ws" && git rev-parse HEAD)"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null
CGO_ENABLED=0 go build -o "$bin_dir/cmake-configure-file" ./cmd/cmake-configure-file

# (1) Convert: cmake runs git rev-parse against the staged repo; dump-vars
# captures GIT_SHA; the stamp lift records GIT_SHA -> STABLE_GIT_SHA and the
# configure_file lifts with stamp_values.
"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --lift-configure-file \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
  echo "FAIL: convert-element-cmake exited non-zero"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
}

for marker in 'stamp_values = {' '"GIT_SHA": "STABLE_GIT_SHA"'; do
  if ! grep -qF -- "$marker" "$ws/BUILD.bazel"; then
    echo "FAIL: emitted BUILD missing stamp-lift marker: $marker"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$ws/BUILD.bazel"
    exit 1
  fi
done
echo "ok  meta-cmake-vcs-stamp: convert lifts the git stamp to stamp_values = {GIT_SHA: STABLE_GIT_SHA}"

# --- bazel-build half (bazel >= 9) ---------------------------------------
# Prefer bazel, fall back to bazelisk (the launcher the repo's gates expect),
# mirroring scripts/meta-cmake-split-build.sh / meta-cmake-genex-probe.sh.
if command -v bazel >/dev/null 2>&1; then
  BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
  BZL=bazelisk
else
  echo "ok  meta-cmake-vcs-stamp: bazel/bazelisk not on PATH, skipping build half"
  exit 0
fi
bzlmajor="$("$BZL" --version 2>/dev/null | sed -n 's/^bazel \([0-9]*\).*/\1/p')"
{ [ -n "$bzlmajor" ] && [ "$bzlmajor" -ge 9 ] 2>/dev/null; } || { echo "ok  meta-cmake-vcs-stamp: bazel < 9, skipping build half"; exit 0; }

# Stage the bazel workspace: the converted BUILD + sources are already in
# $ws. Add the bzlmod MODULE (rules_buildstream_bazel via local_path_override
# for the cmake_configure_file rule, plus rules_cc / bazel_skylib the emit
# loads) and the //tools:cmake-configure-file tool (mirrors run-fidelity).
cat > "$ws/MODULE.bazel" <<EOF
module(name = "vcsstamp", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
bazel_dep(name = "rules_buildstream_bazel", version = "0.0.0")
local_path_override(module_name = "rules_buildstream_bazel", path = "$repo_root/rules_buildstream_bazel")
EOF
mkdir -p "$ws/tools"
cp "$bin_dir/cmake-configure-file" "$ws/tools/cmake-configure-file.bin"
chmod 0755 "$ws/tools/cmake-configure-file.bin"
cat > "$ws/tools/BUILD.bazel" <<'EOF'
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

# Wrap the prebuilt Go tool as an executable target: the cmake_configure_file
# rule's `tool` attr is executable (cfg=exec), so a bare exports_files source
# file is rejected ("misplaced here").
native_binary(
    name = "cmake-configure-file",
    src = "cmake-configure-file.bin",
    out = "cmake-configure-file",
    visibility = ["//visibility:public"],
)
EOF

# A workspace-status script emitting a LIVE revision distinct from the baked
# one, so a successful re-read is unambiguous.
live_sha="livestamprev0000000000000000000000111222"
status_cmd="$work_dir/status.sh"
printf '#!/bin/sh\necho "STABLE_GIT_SHA %s"\n' "$live_sha" > "$status_cmd"
chmod 0755 "$status_cmd"

bzlcache="$work_dir/.bazel"
rendered="$ws/bazel-bin/version.h"
build_version_h() { # args: extra build flags
  # shellcheck disable=SC2086  # intentional word-split of META_BAZEL_*_ARGS
  ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
    build ${META_BAZEL_BUILD_ARGS:-} "$@" //:version.h ) >"$work_dir/bazel.log" 2>&1
}

# (2) WITH --stamp: the rule reads ctx.info_file (populated by the status
# command) and the tool overrides GIT_SHA with the live value.
if ! build_version_h --stamp "--workspace_status_command=$status_cmd"; then
  echo "FAIL: bazel build //:version.h (--stamp) failed"
  sed 's/^/   /' "$work_dir/bazel.log"
  exit 1
fi
if ! grep -q "$live_sha" "$rendered"; then
  echo "FAIL: --stamp build did not re-read the live revision into version.h"
  echo "   wanted $live_sha; got:"
  sed 's/^/   /' "$rendered"
  exit 1
fi
echo "ok  meta-cmake-vcs-stamp: --stamp re-reads the live revision into version.h"

# (3) WITHOUT --stamp: STABLE_GIT_SHA isn't in the status file, so the tool
# keeps the convert-time baked value — the header still builds.
if ! build_version_h; then
  echo "FAIL: bazel build //:version.h (no --stamp) failed"
  sed 's/^/   /' "$work_dir/bazel.log"
  exit 1
fi
if ! grep -q "$baked_sha" "$rendered"; then
  echo "FAIL: no-stamp build lost the baked fallback revision"
  echo "   wanted $baked_sha; got:"
  sed 's/^/   /' "$rendered"
  exit 1
fi
echo "ok  meta-cmake-vcs-stamp: no-stamp build falls back to the baked revision"
