#!/usr/bin/env bash
# spike-libffi: drive a real GNU-style autotools project
# through the converter end-to-end, surface what breaks.
#
# libffi is the chosen target because it stresses the four
# things our existing autotools fixtures don't exercise:
#
#   1. Real autoconf — configure.ac uses AC_PREREQ([2.71]),
#      AC_CONFIG_HEADERS([fficonfig.h]) for HAVE_* macros,
#      AC_CANONICAL_TARGET for host detection, AC_CHECK_TOOL,
#      AC_CHECK_SIZEOF, AX_COMPILER_VENDOR (autoconf-archive),
#      and many more probes that actually run cc on the host
#      at configure time.
#   2. Recursive automake — top-level Makefile.am has
#      SUBDIRS = include testsuite man (+ conditional doc),
#      so the trace contains nested make invocations whose
#      .o paths are relative to the subdir, not the toplevel.
#   3. Real libtool — LT_INIT brings in the actual libtool
#      script wrapper. compile / link goes through
#      `libtool --mode=compile cc ...` rather than direct cc
#      / ar exec. The trace will see libtool spawning cc and
#      ar as children, plus libtool's own .lo / .la
#      bookkeeping artifacts.
#   4. Generated headers — configure generates fficonfig.h
#      from fficonfig.h.in; compile commands reference it
#      through -I/build/dir paths that don't exist in the
#      source tree.
#
# This script downloads libffi source on demand into
# build/spike-libffi/ (gitignored — no source code in the
# repo, licenses stay separate). It then writes a .bst
# pointing at the extracted tree and invokes write-a + bazel
# build.
#
# Goal: NOT to produce a green build. Goal is to surface the
# inventory of failure modes a real autotools project hits,
# so we know what to prioritize for kind:autotools depth vs
# the current fixture corpus's breadth-of-shape coverage.
#
# Output: stdout narrates each step; on failure, the script
# exits 0 and writes a SUMMARY block at the end with the
# observed failure mode. Exit code is for tooling sanity, not
# spike pass/fail.

set -euo pipefail

VERSION="${LIBFFI_VERSION:-3.4.6}"
URL="${LIBFFI_URL:-https://github.com/libffi/libffi/releases/download/v${VERSION}/libffi-${VERSION}.tar.gz}"

repo="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo"

work="$repo/build/spike-libffi"
mkdir -p "$work"

step() { printf '\n=== %s ===\n' "$*"; }

step "Download libffi-${VERSION} (or use cache)"
tarball="$work/libffi-${VERSION}.tar.gz"
if [ ! -s "$tarball" ]; then
    if command -v curl >/dev/null; then
        curl -L --fail -o "$tarball" "$URL"
    elif command -v wget >/dev/null; then
        wget -O "$tarball" "$URL"
    else
        echo "spike-libffi: neither curl nor wget on PATH" >&2
        exit 1
    fi
fi
ls -lh "$tarball"

step "Extract"
src="$work/libffi-${VERSION}"
if [ ! -d "$src" ]; then
    tar -xf "$tarball" -C "$work"
fi
echo "extracted to: $src"
ls "$src" | head -20

step "Inspect autotools surface (static analysis)"
echo "configure.ac highlights:"
awk '/^AC_(INIT|PREREQ|CANONICAL|CONFIG_HEADERS|CHECK_HEADERS|CHECK_FUNCS|CHECK_LIB|CHECK_SIZEOF|CHECK_TOOL)|^LT_INIT|^AM_INIT_AUTOMAKE/' "$src/configure.ac" | head -10
echo
echo "top Makefile.am SUBDIRS:"
awk '/^SUBDIRS/' "$src/Makefile.am" | head -3
echo
echo "config.h.in templates:"
( cd "$src" && ls *.h.in 2>/dev/null ) || echo "  (none)"
echo
echo "libtool m4 macros (LT_INIT brings these in):"
ls "$src/m4" 2>/dev/null | grep -E '^(libtool|lt)' | head -5 || echo "  no m4/lt*"

step "Generate libffi.bst"
bst="$work/libffi.bst"
cat > "$bst" <<EOF
kind: autotools

sources:
- kind: local
  path: libffi-${VERSION}
EOF
cat "$bst"

step "Build the converter binaries"
make all >/dev/null
echo "binaries staged in build/bin/"

step "Render project A + B via write-a"
out_a="$work/A"
out_b="$work/B"
rm -rf "$out_a" "$out_b"
render_rc=0
"$repo/build/bin/write-a" \
    --bst "$bst" \
    --out "$out_a" \
    --out-b "$out_b" \
    --convert-element-cmake "$repo/build/bin/convert-element-cmake" \
    --convert-element-trace "$repo/build/bin/convert-element-trace" \
    --build-tracer-bin "$repo/build/bin/build-tracer" \
    --trace-round1 \
    >"$work/write-a.log" 2>&1 || render_rc=$?
if [ "$render_rc" -ne 0 ]; then
    echo "write-a failed (exit $render_rc); see $work/write-a.log:"
    tail -30 "$work/write-a.log"
    cat <<EOF

=== SUMMARY: write-a render failed ===

Failure point: cmd/write-a couldn't render projects A + B
from the libffi .bst. This is unexpected for kind:autotools —
write-a should accept any .bst pointing at any source tree.
Fault is in the loader / staging layer, not the
autotools-specific path.

EOF
    exit 0
fi
echo "render OK"

step "Inspect the rendered B-side BUILD"
build="$out_b/elements/libffi/BUILD.bazel"
if [ -f "$build" ]; then
    wc -l "$build"
    echo "first 20 lines:"
    head -20 "$build"
else
    echo "no B-side BUILD.bazel; layout:"
    find "$out_b" -name 'BUILD.bazel' | head -10
fi

step "Run bazel build (where the actual surprises will surface)"
if ! command -v bazel >/dev/null && ! command -v bazelisk >/dev/null; then
    cat <<EOF

=== SUMMARY: render OK, bazel not on PATH ===

The spike rendered both projects successfully. To exercise
the actual build, install bazelisk (or bazel >= 7) and:

  cd $out_b
  bazel build //elements/libffi:libffi_install

The B-side BUILD that would run:
  $build

The genrule's command runs configure + make + make install
under build-tracer, with PATH=/usr/local/bin:/usr/bin:/bin.
That PATH must include autoconf, automake, libtool, m4, and
perl for libffi's configure to even start.

EOF
    exit 0
fi

BAZEL=$(command -v bazel || command -v bazelisk)
build_rc=0
( cd "$out_b" && "$BAZEL" build //elements/libffi:libffi_install ) >"$work/bazel-build.log" 2>&1 || build_rc=$?
if [ "$build_rc" -eq 0 ]; then
    cat <<EOF

=== SUMMARY: build succeeded (genuinely surprising) ===

If this prints, libffi converted end-to-end. Inspect:
  $out_b/bazel-bin/elements/libffi/BUILD.bazel.out

to verify the cc rules look sensible (libffi has libffi
proper + arch-specific .S sources; the converter should
recover both).

EOF
    exit 0
fi

last="$(tail -60 "$work/bazel-build.log" | tr -d '\r')"
cat <<EOF

=== SUMMARY: bazel build failed (the expected outcome) ===

Failure point: bazel build inside the project-B genrule for
libffi. See $work/bazel-build.log for the full log; last 60
lines below. Likely root causes (each surfaces a distinct
gap in our autotools coverage):

  - **autotools host chain not on PATH** — libffi's
    configure needs autoconf >= 2.71 + automake + libtool +
    m4 + perl. Our genrule PATH is only
    /usr/local/bin:/usr/bin:/bin; if those aren't installed
    under /usr*, configure won't bootstrap.
  - **Recursive automake / SUBDIRS** — top-level make
    recurses into include/, testsuite/, man/ via
    \`\$(MAKE) -C subdir\`. The trace records compile events
    with the cwd of the inner make; the converter's
    objToCompile map keys by ev.Out path. Sub-relative paths
    from different SUBDIRS can collide or land in the wrong
    archive.
  - **libtool wrapper** — LT_INIT brings in the libtool
    script. compile / link goes through
    \`libtool --mode={compile,link} cc/ld ...\` rather than
    direct cc / ar / ld. The classifier sees the cc / ar
    children spawned by libtool, so EVENT detection works,
    but .lo / .la artifacts and the libtool dance itself
    aren't represented in BUILD.bazel.out.
  - **fficonfig.h** — configure generates fficonfig.h from
    fficonfig.h.in in the build dir. compile commands
    reference it via -I\$builddir. Our converter's
    source-rel mapping doesn't account for build-dir-only
    headers; lookups will miss.
  - **arch-specific .S sources** — libffi's src/<arch>/
    selects assembly files at configure time based on
    \$host. Our fixtures don't exercise .S; the converter
    needs to thread these through cc_library srcs alongside
    .c.

$last

EOF
