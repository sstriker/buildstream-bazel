#!/bin/sh
# meta-cmake-file-write-stamp.sh — render+build gate for the file(WRITE)
# VCS-stamp wiring (ROADMAP follow-on to the file() writer index).
#
# The fixture captures a git revision via
# `execute_process(git rev-parse OUTPUT_VARIABLE GIT_SHA)` and writes it
# into a build-dir header DIRECTLY with file(WRITE) (no .h.in template):
#   file(WRITE ${CMAKE_CURRENT_BINARY_DIR}/version.h
#        "#define GIT_SHA \"${GIT_SHA}\"\n")
# A naive recovery bakes the frozen convert-time revision. The wiring
# recognizes the file(WRITE) as a configure_file in disguise — the
# NON-EXPANDED trace keeps the `${GIT_SHA}` marker, the expanded trace
# the rendered bytes — and routes it through the configure_file
# stamp_values machinery so @GIT_SHA@ re-reads the LIVE revision from
# the Bazel workspace status at build time.
#
# Asserts: (1) convert --lift-configure-file emits a cmake_configure_file
# carrying stamp_values = {GIT_SHA: STABLE_GIT_SHA} + the
# file-writer-stamp facet; (2) `bazel build //:version.h` WITH --stamp
# re-reads the live revision; (3) WITHOUT --stamp falls back to the
# baked revision; (4) the consumer library compiles against the header.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v git >/dev/null 2>&1 || { echo "skip: git not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }

fixture_src="$repo_root/converter/testdata/sample-projects/file-write-stamp"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

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

"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --lift-configure-file \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.stdout" 2>"$work_dir/convert.stderr" || {
  echo "FAIL: convert-element-cmake exited non-zero"
  sed 's/^/   stderr: /' "$work_dir/convert.stderr"
  exit 1
}

for marker in 'stamp_values = {' '"GIT_SHA": "STABLE_GIT_SHA"' 'cmake-codegen-file-writer-stamp'; do
  if ! grep -qF -- "$marker" "$ws/BUILD.bazel"; then
    echo "FAIL: emitted BUILD missing file(WRITE) stamp-lift marker: $marker"
    sed 's/^/   /' "$ws/BUILD.bazel"
    exit 1
  fi
done
# The frozen write_file bake must NOT be how version.h is produced.
if grep -qF 'baked_version_h' "$ws/BUILD.bazel"; then
  echo "FAIL: version.h still baked frozen instead of stamp-wired"
  sed 's/^/   /' "$ws/BUILD.bazel"
  exit 1
fi
echo "ok  meta-cmake-file-write-stamp: file(WRITE) git stamp lifted to stamp_values = {GIT_SHA: STABLE_GIT_SHA}"

# A second convert WITHOUT --lift-configure-file must fall back to the
# frozen write_file bake (the tool isn't staged in that envelope).
"$bin_dir/convert-element-cmake" --source-root "$ws" --out-build "$work_dir/nolift.BUILD" \
  >/dev/null 2>&1 || { echo "FAIL: no-lift convert exited non-zero"; exit 1; }
grep -qF 'baked_version_h' "$work_dir/nolift.BUILD" \
  || { echo "FAIL: without --lift-configure-file the file(WRITE) must bake (frozen) — tool not staged"; exit 1; }
echo "ok  meta-cmake-file-write-stamp: without the lift tier the file(WRITE) bakes frozen (graceful fallback)"

# --- bazel-build half (bazel >= 9) ---
if command -v bazel >/dev/null 2>&1; then
  BZL=bazel
elif command -v bazelisk >/dev/null 2>&1; then
  BZL=bazelisk
else
  echo "ok  meta-cmake-file-write-stamp: bazel/bazelisk not on PATH, skipping build half"
  exit 0
fi
bzlmajor=$("$BZL" --version 2>&1 | head -1 | awk '{print $2}' | cut -d. -f1)
case "$bzlmajor" in [0-9]*) ;; *) bzlmajor=0 ;; esac
if [ "$bzlmajor" -lt 9 ]; then
  echo "ok  meta-cmake-file-write-stamp: bazel < 9, skipping build half"
  exit 0
fi

cat > "$ws/MODULE.bazel" <<EOF
module(name = "filewritestamp", version = "0.0.0")
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
build_target() { # args: target + extra flags
  # shellcheck disable=SC2086
  ( cd "$ws" && "$BZL" --output_user_root="$bzlcache" ${META_BAZEL_STARTUP_ARGS:-} \
    build ${META_BAZEL_BUILD_ARGS:-} "$@" ) >"$work_dir/bazel.log" 2>&1
}

if ! build_target --stamp "--workspace_status_command=$status_cmd" //:version.h; then
  echo "FAIL: bazel build //:version.h (--stamp) failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
if ! grep -q "$live_sha" "$rendered"; then
  echo "FAIL: --stamp build did not re-read the live revision into version.h"
  echo "   wanted $live_sha; got:"; sed 's/^/   /' "$rendered"; exit 1
fi
echo "ok  meta-cmake-file-write-stamp: --stamp re-reads the live revision into the file(WRITE) header"

if ! build_target //:version.h; then
  echo "FAIL: bazel build //:version.h (no --stamp) failed"; sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
if ! grep -q "$baked_sha" "$rendered"; then
  echo "FAIL: no-stamp build lost the baked fallback revision"
  echo "   wanted $baked_sha; got:"; sed 's/^/   /' "$rendered"; exit 1
fi
echo "ok  meta-cmake-file-write-stamp: no-stamp build falls back to the baked revision"

# The consumer library compiles against the stamped header.
if ! build_target --stamp "--workspace_status_command=$status_cmd" //:filewritestamp; then
  echo "FAIL: consumer library //:filewritestamp failed to build against the stamped header"
  sed 's/^/   /' "$work_dir/bazel.log"; exit 1
fi
echo "ok  meta-cmake-file-write-stamp: the consumer library compiles against the stamp-wired header"
