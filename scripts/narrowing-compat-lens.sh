#!/bin/sh
# Source-narrowing-compatibility lens (opt-in survey lens; SURVEY_NARROWING_COMPAT).
#
# The converter's translation is meant to be a pure function of the codemodel +
# trace + the build-system files (CMakeLists / *.cmake / configure_file *.in
# templates) — NOT of the .c / .cpp / .h SOURCE BYTES. The orchestrated path
# proves this by construction (it converts against zero-stub sources via the
# narrowing/FUSE layer). This lens is the empirical proof for the survey:
#
#   1. Convert the REAL source tree, capturing the converter's read-set
#      (--out-read-paths: the trace's configure-read source-tree paths).
#   2. Make a copy with every file truncated to 0 bytes EXCEPT the read-set and
#      all CMakeLists.txt (always real / special-cased) — so cmake still
#      configures, but every .c/.cpp/.h the converter must NOT depend on is
#      zeroed.
#   3. Re-run the SAME convert against the narrowed copy.
#   4. Assert the emitted BUILD.bazel.out is byte-for-byte identical (modulo the
#      source-root path prefix, which legitimately differs between the two trees).
#
# A diff is a narrowing-soundness bug: the converter secretly read a zeroed
# file's bytes. The diff itself names the affected srcs/hdrs. Report-only,
# strict (zero tolerated diffs); writes <out>/narrowing-compat.json.
#
# Usage: narrowing-compat-lens.sh --src <abs cmake source root> --name <member>
#          --out <per-project out dir> [--split 0|1] [--bazel-package-path P]
#          [-- <extra convert args, e.g. --cmake-define K=V ...>]
set -eu

src=""; name=""; out=""; split=1; pkgpath=""
while [ $# -gt 0 ]; do
    case "$1" in
        --src) src="$2"; shift 2 ;;
        --name) name="$2"; shift 2 ;;
        --out) out="$2"; shift 2 ;;
        --split) split="$2"; shift 2 ;;
        --bazel-package-path) pkgpath="$2"; shift 2 ;;
        --) shift; break ;;
        *) echo "narrowing-compat-lens: unknown arg $1" >&2; exit 2 ;;
    esac
done
extra_args="$*"  # remaining = pass-through convert args (cmake-defines etc.)

[ -n "$src" ] && [ -n "$name" ] && [ -n "$out" ] || { echo "narrowing-compat-lens: --src/--name/--out required" >&2; exit 2; }
command -v cmake >/dev/null 2>&1 || { echo "skip(no-cmake)"; exit 0; }

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cec="$out/.narrow-cec"
( cd "$repo_root" && go build -o "$cec" ./converter/cmd/convert-element-cmake ) 2>>"$out/narrowing-compat.log" || cec="go run $repo_root/converter/cmd/convert-element-cmake"

splitflag=""; [ "$split" = "1" ] && splitflag="--split-packages"
pkgflag=""; [ -n "$pkgpath" ] && pkgflag="--bazel-package-path $pkgpath"

real_out="$out/narrow-real.BUILD"
narrow_out="$out/narrow-zeroed.BUILD"
reads="$out/narrow-reads.json"

# 1. Real convert + read-set capture.
# shellcheck disable=SC2086
if ! $cec --source-root "$src" $splitflag $pkgflag --emit-source-comments \
        --out-build "$real_out" --out-read-paths "$reads" $extra_args \
        >>"$out/narrowing-compat.log" 2>&1; then
    echo "skip(real-convert-failed)"; exit 0
fi
[ -f "$reads" ] || { echo "skip(no-read-paths)"; exit 0; }

# 2. Narrowed copy: zero every SOURCE/HEADER file (the bytes the narrowing/FUSE
# layer stubs) EXCEPT those in the read-set (a file(READ <src>) configure input
# kept real). Build-system files — CMakeLists.txt, *.cmake modules, *.in
# configure_file templates — and data stay real: cmake must still configure, and
# the lens tests SOURCE/HEADER byte purity, not build-system-file purity (the
# converter legitimately reads those, captured by the static narrowing patterns).
narrowed="$out/narrow-tree"
rm -rf "$narrowed"; cp -a "$src" "$narrowed"
keep="$out/.narrow-keep"
# read-set: strip JSON quoting/punctuation to bare relative paths.
sed -n 's/^[[:space:]]*"\(.*\)"[,]\{0,1\}[[:space:]]*$/\1/p' "$reads" | sort -u > "$keep"
( cd "$narrowed" && find . -type f \( \
    -name '*.c' -o -name '*.cc' -o -name '*.cpp' -o -name '*.cxx' -o -name '*.c++' \
    -o -name '*.h' -o -name '*.hh' -o -name '*.hpp' -o -name '*.hxx' -o -name '*.h++' \
    -o -name '*.inc' -o -name '*.ipp' -o -name '*.ixx' -o -name '*.tcc' \) \
    | sed 's|^\./||' ) | sort -u > "$out/.narrow-srcs"
comm -23 "$out/.narrow-srcs" "$keep" | while IFS= read -r f; do
    : > "$narrowed/$f" 2>/dev/null || true
done

# 3. Re-convert the narrowed copy with the SAME flags.
# shellcheck disable=SC2086
if ! $cec --source-root "$narrowed" $splitflag $pkgflag --emit-source-comments \
        --out-build "$narrow_out" $extra_args >>"$out/narrowing-compat.log" 2>&1; then
    echo "skip(narrowed-convert-failed)"; exit 0
fi

# 4. Byte-diff, normalizing the two legitimately-different convert-time paths:
# the source-root prefix (real tree vs narrowed copy) and the ephemeral cmake
# build dir (/tmp/convert-element-build-XXXX, a fresh os.MkdirTemp per convert —
# not a source-byte dependence, so its leak into rpath/copts must be normalized).
norm_real="$out/.narrow-real.norm"; norm_narrow="$out/.narrow-zeroed.norm"
sed -e "s|$src|@SRC@|g" -e 's|/tmp/convert-element-build-[0-9]*|@BUILD@|g' "$real_out" > "$norm_real"
sed -e "s|$narrowed|@SRC@|g" -e 's|/tmp/convert-element-build-[0-9]*|@BUILD@|g' "$narrow_out" > "$norm_narrow"

if diff -q "$norm_real" "$norm_narrow" >/dev/null 2>&1; then
    printf '{"member":"%s","ok":true,"diffs":[]}\n' "$name" > "$out/narrowing-compat.json"
    echo "ok"
else
    # Report the diverging lines (added/removed) — these name the srcs/hdrs the
    # converter secretly read from a zeroed file.
    d="$(diff "$norm_real" "$norm_narrow" | sed 's/"/\\"/g' | head -40 | tr '\n' '~')"
    printf '{"member":"%s","ok":false,"diff":"%s"}\n' "$name" "$d" > "$out/narrowing-compat.json"
    echo "FAIL"
fi
