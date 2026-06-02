#!/bin/sh
# meta-cmake-stamp-volatile.sh — end-to-end gate for the VOLATILE_ stamp
# path (the date driver) and the stable+volatile status-file merge.
#
# Companion to meta-cmake-vcs-stamp.sh (which covers the STABLE_/VCS path).
# Drives convert-element-cmake against
# converter/testdata/sample-projects/stamp-volatile, whose CMakeLists
# captures BOTH a git revision (-> GIT_SHA, a STABLE_ stamp) AND a build
# date (`date +%Y-%m-%d` -> BUILD_DATE, a VOLATILE_ stamp) and feeds both
# into one configure_file (@GIT_SHA@ + @BUILD_DATE@ in version.h.in).
#
# Proves the driver-aware lift end to end:
#   1. convert exits 0 and the emitted BUILD carries BOTH
#      "GIT_SHA": "STABLE_GIT_SHA" and "BUILD_DATE": "VOLATILE_BUILD_DATE"
#      in stamp_values.
#   2. `bazel build //:version.h` WITH --stamp + a --workspace_status_command
#      that emits a STABLE_GIT_SHA line (-> stable-status.txt) AND a
#      VOLATILE_BUILD_DATE line (-> volatile-status.txt) re-reads BOTH live
#      values into the generated header. This is the load-bearing check for
#      PR 2: the cmake_configure_file rule passes ctx.version_file (volatile)
#      in addition to ctx.info_file (stable) because a VOLATILE_ key is
#      present, and the tool merges the two files' key namespaces.
#   3. The same build WITHOUT --stamp drops the live values: the git
#      revision falls back to the convert-time baked HEAD, and the volatile
#      date marker is absent (volatile-status carries no operator key), so
#      the build still succeeds on the baked fallbacks.
#
# Gating mirrors meta-cmake-vcs-stamp.sh: skips cleanly when cmake / ninja /
# git / go are absent, and self-skips the bazel half when neither bazel nor
# bazelisk is on PATH or the detected bazel is older than 9.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH (convert --source-root drives the Ninja generator)"; exit 0; }
command -v git >/dev/null 2>&1 || { echo "skip: git not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }

fixture_src="$repo_root/converter/testdata/sample-projects/stamp-volatile"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Stage the fixture as its own git repo so `git rev-parse HEAD` at
# cmake-configure time resolves to a known, isolated commit (the baked
# revision the no-stamp build falls back to).
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

# (1) Convert: the stamp lift records GIT_SHA -> STABLE_GIT_SHA (stable) and
# BUILD_DATE -> VOLATILE_BUILD_DATE (volatile); the configure_file lifts
# with both in stamp_values.
"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --lift-configure-file \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
  echo "FAIL: convert-element-cmake exited non-zero"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
}

for marker in 'stamp_values = {' '"GIT_SHA": "STABLE_GIT_SHA"' '"BUILD_DATE": "VOLATILE_BUILD_DATE"'; do
  if ! grep -qF -- "$marker" "$ws/BUILD.bazel"; then
    echo "FAIL: emitted BUILD missing stamp-lift marker: $marker"
    echo "   --- generated BUILD ---"
    sed 's/^/   /' "$ws/BUILD.bazel"
    exit 1
  fi
done
echo "ok  meta-cmake-stamp-volatile: convert lifts STABLE_GIT_SHA + VOLATILE_BUILD_DATE into stamp_values"

# --- bazel-build half (bazel >= 9) ---------------------------------------
if command -v bazel >/dev/null 2>&1; then
  BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
  BZL=bazelisk
else
  echo "ok  meta-cmake-stamp-volatile: bazel/bazelisk not on PATH, skipping build half"
  exit 0
fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in
  [0-9]*) ;;
  *) bzlmajor=0 ;;
esac
if [ "$bzlmajor" -lt 9 ]; then
  echo "ok  meta-cmake-stamp-volatile: bazel < 9, skipping build half"
  exit 0
fi

cat > "$ws/MODULE.bazel" <<EOF
module(name = "stampvolatile", version = "0.0.0")
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

# Distinctive live markers, distinct from the baked convert-time values, so a
# successful re-read is unambiguous. STABLE_GIT_SHA -> stable-status.txt;
# VOLATILE_BUILD_DATE (no STABLE_ prefix) -> volatile-status.txt.
live_sha="livestamprev0000000000000000000000111222"
live_date="2099-12-31"
status_cmd="$work_dir/status.sh"
printf '#!/bin/sh\necho "STABLE_GIT_SHA %s"\necho "VOLATILE_BUILD_DATE %s"\n' "$live_sha" "$live_date" > "$status_cmd"
chmod 0755 "$status_cmd"

bzlcache="$work_dir/.bazel"
rendered="$ws/bazel-bin/version.h"
build_version_h() {
  # shellcheck disable=SC2086  # intentional word-split of META_BAZEL_*_ARGS
  ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
    build ${META_BAZEL_BUILD_ARGS:-} "$@" //:version.h ) >"$work_dir/bazel.log" 2>&1
}

# (2) WITH --stamp: the rule reads ctx.info_file (STABLE_GIT_SHA) AND
# ctx.version_file (VOLATILE_BUILD_DATE); the tool merges both and overrides
# both template vars with the live values.
if ! build_version_h --stamp "--workspace_status_command=$status_cmd"; then
  echo "FAIL: bazel build //:version.h (--stamp) failed"
  sed 's/^/   /' "$work_dir/bazel.log"
  exit 1
fi
for want in "$live_sha" "$live_date"; do
  if ! grep -q "$want" "$rendered"; then
    echo "FAIL: --stamp build did not re-read a live value into version.h: $want"
    echo "   (proves the rule passes BOTH stable + volatile status and the tool merges them)"
    sed 's/^/   /' "$rendered"
    exit 1
  fi
done
echo "ok  meta-cmake-stamp-volatile: --stamp re-reads BOTH the live revision (stable) and date (volatile)"

# (3) WITHOUT --stamp: no operator status keys. Git falls back to the baked
# convert-time HEAD; the volatile date marker is absent (the build still
# succeeds on the baked fallbacks).
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
if grep -q "$live_date" "$rendered"; then
  echo "FAIL: no-stamp build leaked the live volatile date marker $live_date"
  echo "   (the volatile value must only appear under --stamp + workspace_status)"
  sed 's/^/   /' "$rendered"
  exit 1
fi
echo "ok  meta-cmake-stamp-volatile: no-stamp build falls back to the baked revision; no volatile leak"
