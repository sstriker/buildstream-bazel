#!/bin/sh
# meta-cmake-vcs-stamp-function.sh — render gate for the VCS-stamp lift's
# helper-function (git_describe()) forwarded-stamp recovery.
#
# The fixture wraps the stamp in a helper: the execute_process OUTPUT_VARIABLE
# is a function-LOCAL the dump-vars top-level snapshot can't see, and the value
# is handed back via set(${_var} "${out}" PARENT_SCOPE) — the
# GetGitRevisionDescription.cmake / git_describe() shape SDL (and the hundreds
# of projects that copy that module) use. A clean (non-diagnostic) convert
# refuses the stamp (the local isn't captured) and ABORTS in pass 1. The
# converter recovers the set()-copy via the warm non-expanded-trace configure,
# and the forwarded-stamp rescue (OUTPUT_VARIABLE is the SrcVar of that copy)
# lifts the call so the convert COMPLETES instead of aborting.
#
# Contract proven here: a function-forwarded stamp no longer aborts the clean
# convert, and the recovery path actually ran. NOTE the value bakes into the
# configure_file output: the parent-scope target name (${_var}) isn't resolved
# to GIT_SHA, so GIT_SHA isn't marked a stamp var for the stamp_values lift.
# With round-2 (Phase B) unwired, baking lets the convert complete rather than
# dead-ending at the refusal; lifting a function-forwarded stamp to
# stamp_values (resolve the ${_var} return name) is a tracked follow-up.
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

fixture_src="$repo_root/converter/testdata/sample-projects/vcs-stamp-function"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Stage the fixture as its own git repo so the helper's `git rev-parse HEAD` at
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

# The convert must COMPLETE (was: aborts in pass 1 on the function-local
# stamp). --source-root must be absolute: the recovery filters the recovered
# set()-copies by source tree against the trace's absolute file paths.
"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --lift-configure-file \
  --two-pass-genex \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
  echo "FAIL: convert-element-cmake aborted — the forwarded stamp was not recovered"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
}

# The pass-1 stamp abort must actually have been recovered — otherwise a
# convert that happened to succeed would not be exercising the recovery path
# this gate exists to prove.
if ! grep -q 'recovered pass-1 stamp abort' "$work_dir/convert.stderr"; then
  echo "FAIL: the forwarded-stamp recovery did not run; the fixture isn't exercising the pass-1 abort path"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
fi

# The recovered revision reaches the lifted configure_file's substitution
# values (baked, per the NOTE above — a real 40-hex git sha there proves the
# forwarded value flowed through; the lift to stamp_values is the follow-up).
if ! grep -qE '"GIT_SHA": "[0-9a-f]{40}"' "$ws/BUILD.bazel"; then
  echo 'FAIL: expected the recovered git sha to reach the lifted configure_file values'
  grep -nE '"GIT_SHA"|stamp_values|cmake_configure_file' "$ws/BUILD.bazel" | sed 's/^/   /'
  exit 1
fi

echo 'ok  meta-cmake-vcs-stamp-function: git_describe() function-forwarded stamp (function-local OUTPUT_VARIABLE) is recovered so the clean convert completes instead of aborting (value bakes; stamp_values lift is a follow-up)'
