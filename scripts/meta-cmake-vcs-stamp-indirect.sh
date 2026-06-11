#!/bin/sh
# meta-cmake-vcs-stamp-indirect.sh — render gate for the VCS-stamp lift's
# set()-indirection (the warm second --trace configure pass).
#
# The fixture captures git rev-parse into GIT_SHA, copies it verbatim
# `set(VERSION ${GIT_SHA})`, and feeds the COPY into a configure_file
# (@VERSION@) — the Google-Benchmark shape. A single --trace-expand pass
# can't see the copy (expansion erases ${GIT_SHA}); the converter's warm
# second --trace configure recovers `set(VERSION ${GIT_SHA})`, so VERSION
# inherits GIT_SHA's workspace-status key and the configure_file lifts to
# stamp_values = {"VERSION": "STABLE_GIT_SHA"}.
#
# This is the convert-half contract — that the COPIED variable lifts, and
# that the warm second pass actually ran. The Bazel `--stamp` re-render of
# a stamp_values entry (the rule's ctx.info_file wiring) is gated by
# meta-cmake-vcs-stamp.sh's direct case; the rule treats a direct and an
# indirected entry identically, so it isn't re-proven here.
#
# Gating: skips cleanly when cmake / ninja / git / go / make are absent.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH (convert --source-root drives the Ninja generator)"; exit 0; }
command -v git >/dev/null 2>&1 || { echo "skip: git not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }
command -v make >/dev/null 2>&1 || { echo "skip: make not on PATH (needed to build the converter)"; exit 0; }

fixture_src="$repo_root/converter/testdata/sample-projects/vcs-stamp-indirect"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Stage the fixture as its own git repo so `git rev-parse HEAD` at
# cmake-configure time resolves to an isolated commit. --no-gpg-sign so the
# gate doesn't depend on the operator's commit-signing setup.
ws="$work_dir/ws"
mkdir -p "$ws"
cp -R "$fixture_src/." "$ws/"
(
  cd "$ws" && git init -q && git add -A &&
    git -c commit.gpgsign=false -c user.email=ci@example.com -c user.name=ci \
      commit --no-gpg-sign -qm init
)

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --lift-configure-file \
  --two-pass-genex \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
  echo "FAIL: convert-element-cmake exited non-zero"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
}

# The warm second --trace pass must actually have run (else the lift would
# be vacuous — pass 1 alone can't see the set() copy).
if ! grep -q 'warm second configure for:.*VCS-stamp' "$work_dir/convert.stderr"; then
  echo "FAIL: the warm second --trace pass did not run; the indirection lift would be vacuous"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
fi

# The COPIED variable (VERSION), not the direct stamp var (GIT_SHA), must
# carry the stamp_values entry — that is the indirection the second pass
# recovers via set(VERSION ${GIT_SHA}).
if ! grep -qF -- '"VERSION": "STABLE_GIT_SHA"' "$ws/BUILD.bazel"; then
  echo 'FAIL: set()-indirection not lifted — expected stamp_values {"VERSION": "STABLE_GIT_SHA"}'
  echo "   --- generated BUILD (stamp lines) ---"
  grep -nE 'stamp_values|cmake_configure_file' "$ws/BUILD.bazel" | sed 's/^/   /'
  exit 1
fi

echo 'ok  meta-cmake-vcs-stamp-indirect: set(VERSION ${GIT_SHA}) indirection lifts to stamp_values {VERSION: STABLE_GIT_SHA} via the warm second --trace pass'
