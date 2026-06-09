#!/bin/sh
# Source-narrowing-compatibility lens (opt-in survey lens; SURVEY_NARROWING_COMPAT).
#
# The converter's translation is meant to be a pure function of the codemodel +
# trace + the build-system files (CMakeLists / *.cmake / configure_file *.in
# templates) — NOT of the .c / .cpp / .h SOURCE BYTES. No-source-read is the
# assumed RULE; the few legitimate exceptions (the fused-source textual-include
# scan: a .c that `#include`s another .c; and the generated-source-root-include
# rewrite) are PUBLISHED by the converter via --out-source-reads. This lens is
# the empirical proof for the survey, AND the measure of the exception:
#
#   1. Convert the REAL source tree, capturing (a) the trace's configure-read
#      source-tree paths (--out-read-paths) and (b) the converter's DECLARED
#      source-byte reads (--out-source-reads).
#   2. Make a copy with every .c/.cpp/.h truncated to 0 bytes EXCEPT the read-set,
#      the declared source-byte reads, and all build-system files (CMakeLists /
#      *.cmake / *.in — always kept real so cmake still configures).
#   3. Re-run the SAME convert against the narrowed copy.
#   4. Assert the emitted BUILD is byte-identical (modulo the source-root prefix +
#      ephemeral build dir, both normalized).
#
# A diff means the converter read a zeroed file's bytes WITHOUT declaring it — an
# UNDECLARED source-byte read (a narrowing-soundness bug); the diff names the
# affected srcs/hdrs. A member with declared reads is NOT a failure — it's the
# measured exception, surfaced as `source-reads: N` in the result so the
# "exception, not the rule" assumption is auditable per member. Report-only;
# writes <out>/narrowing-compat.json.
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
sreads="$out/narrow-source-reads.json"

# 1. Real convert + read-set capture (configure reads + declared source-byte reads).
# shellcheck disable=SC2086
if ! $cec --source-root "$src" $splitflag $pkgflag --emit-source-comments \
        --out-build "$real_out" --out-read-paths "$reads" --out-source-reads "$sreads" $extra_args \
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
# keep-set = configure reads ∪ DECLARED source-byte reads (--out-source-reads).
# The declared source reads are the converter's published exceptions (fused-
# source includers etc.); keeping them real means a member that legitimately
# reads them does NOT spuriously diff — only an UNDECLARED read (a zeroed file
# that wasn't published) can still change the BUILD and FAIL. Strip JSON
# quoting/punctuation to bare relative paths.
sed -n 's/^[[:space:]]*"\(.*\)"[,]\{0,1\}[[:space:]]*$/\1/p' "$reads" | sort -u > "$keep"
src_reads=""
if [ -f "$sreads" ]; then
    src_reads="$(sed -n 's/^[[:space:]]*"\(.*\)"[,]\{0,1\}[[:space:]]*$/\1/p' "$sreads" | sort -u)"
    printf '%s\n' "$src_reads" | sed '/^$/d' >> "$keep"
    sort -u "$keep" -o "$keep"
fi
src_reads_n="$(printf '%s' "$src_reads" | sed '/^$/d' | grep -c . || true)"
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

# source_reads: the declared exception set, emitted as a JSON array so the
# result records exactly which source files the converter's translation
# depended on (empty confirms the no-source-read assumption for this member).
src_reads_json="[]"
if [ "$src_reads_n" -gt 0 ]; then
    src_reads_json="$(printf '%s\n' "$src_reads" | sed '/^$/d' | sed 's/"/\\"/g;s/.*/"&"/' | paste -sd, -)"
    src_reads_json="[$src_reads_json]"
fi

if diff -q "$norm_real" "$norm_narrow" >/dev/null 2>&1; then
    printf '{"member":"%s","ok":true,"source_reads":%s,"diffs":[]}\n' "$name" "$src_reads_json" > "$out/narrowing-compat.json"
    echo "ok (source-reads: $src_reads_n)"
else
    # A diff after keeping the declared reads real = an UNDECLARED source-byte
    # read. Report the diverging lines — they name the srcs/hdrs the converter
    # read from a zeroed-and-undeclared file.
    d="$(diff "$norm_real" "$norm_narrow" | sed 's/"/\\"/g' | head -40 | tr '\n' '~')"
    printf '{"member":"%s","ok":false,"source_reads":%s,"diff":"%s"}\n' "$name" "$src_reads_json" "$d" > "$out/narrowing-compat.json"
    echo "FAIL (undeclared source-byte read; declared source-reads: $src_reads_n)"
fi
