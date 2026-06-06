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
# the forwarded-stamp rescue (OUTPUT_VARIABLE is the SrcVar of that copy) lifts
# the call so the convert COMPLETES, AND the parent-scope return name (${_var})
# is resolved to the caller argument (GIT_SHA) so the value LIFTS to
# stamp_values instead of baking.
#
# Then it proves the lift is load-bearing at Bazel build time, exactly like
# meta-cmake-vcs-stamp.sh's direct case:
#   1. convert exits 0, the recovery path ran, and the emitted BUILD carries
#      stamp_values = {"GIT_SHA": "STABLE_GIT_SHA"} (re-keyed to the caller
#      argument, not the meaningless function-local `out`).
#   2. `bazel build //:version.h` WITH --stamp + a --workspace_status_command
#      that emits STABLE_GIT_SHA re-reads the LIVE revision into the header.
#   3. The same build WITHOUT --stamp falls back to the convert-time baked
#      revision, so the header still builds.
#
# Gating: skips cleanly when cmake / ninja / git / go / make are absent, and
# self-skips the bazel half when neither bazel nor bazelisk is on PATH, or the
# detected bazel is older than 9 — the convert + lift assertion is always-on.
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
# gate doesn't depend on the operator's commit-signing setup. The recorded sha
# is the convert-time "baked" fallback the no-stamp build must fall back to.
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

# (1) The convert must COMPLETE (was: aborts in pass 1 on the function-local
# stamp). --source-root must be absolute: the recovery filters the recovered
# set()-copies + function-forwards by source tree against the trace's absolute
# file paths.
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

# The forwarded stamp must LIFT to stamp_values keyed on the caller argument
# (GIT_SHA → STABLE_GIT_SHA), re-derived from the call-site name rather than
# the function-local `out` (which the operator's --workspace_status_command
# would never name).
for marker in 'stamp_values = {' '"GIT_SHA": "STABLE_GIT_SHA"'; do
  if ! grep -qF -- "$marker" "$ws/BUILD.bazel"; then
    echo "FAIL: emitted BUILD missing function-forwarded stamp-lift marker: $marker"
    grep -nE '"GIT_SHA"|stamp_values|cmake_configure_file' "$ws/BUILD.bazel" | sed 's/^/   /'
    exit 1
  fi
done
echo 'ok  meta-cmake-vcs-stamp-function: git_describe() function-forwarded stamp lifts to stamp_values = {GIT_SHA: STABLE_GIT_SHA} (parent-scope return name resolved to the caller arg)'

# --- bazel-build half (bazel >= 9) ---------------------------------------
if command -v bazel >/dev/null 2>&1; then
  BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
  BZL=bazelisk
else
  echo "ok  meta-cmake-vcs-stamp-function: bazel/bazelisk not on PATH, skipping build half"
  exit 0
fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in
  [0-9]*) ;;
  *) bzlmajor=0 ;;
esac
if [ "$bzlmajor" -lt 9 ]; then
  echo "ok  meta-cmake-vcs-stamp-function: bazel < 9, skipping build half"
  exit 0
fi

# Stage the bzlmod workspace (mirrors meta-cmake-vcs-stamp.sh): the converted
# BUILD + sources are already in $ws; add the MODULE + the
# //tools:cmake-configure-file tool the lifted rule references.
cat > "$ws/MODULE.bazel" <<EOF
module(name = "vcsstampfunction", version = "0.0.0")
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

native_binary(
    name = "cmake-configure-file",
    src = "cmake-configure-file.bin",
    out = "cmake-configure-file",
    visibility = ["//visibility:public"],
)
EOF

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

# (2) WITH --stamp: the rule reads the live STABLE_GIT_SHA from the status file.
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
echo "ok  meta-cmake-vcs-stamp-function: --stamp re-reads the live revision into version.h"

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
echo "ok  meta-cmake-vcs-stamp-function: no-stamp build falls back to the baked revision"
