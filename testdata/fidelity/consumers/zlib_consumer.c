// Consumer-fidelity test for zlib.
//
// Calls a representative subset of zlib's exported API; the resulting
// object file's symbol set (defined + undefined) reflects what
// downstream code links against. Compiled twice (once with cmake's
// installed headers, once via Bazel against the converted target);
// fidelity-compare diffs the two .o's, classifying inlining /
// hardening differences as benign.
#include <zlib.h>
#include <stddef.h>
#include <string.h>

int run_zlib_smoke(const char *src, unsigned long src_len, unsigned char *dst, unsigned long *dst_len) {
    int rc = compress(dst, dst_len, (const Bytef *)src, src_len);
    if (rc != Z_OK) return rc;
    const char *v = zlibVersion();
    return (v == NULL || v[0] == '\0') ? -1 : 0;
}
