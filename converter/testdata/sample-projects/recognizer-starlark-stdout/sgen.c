/* Bazel-time twin of the convert-time `sgen` shell script: read the spec's
 * integer and emit the generated header to stdout. */
#include <stdio.h>
int main(int argc, char **argv) {
    if (argc < 2) return 1;
    FILE *f = fopen(argv[1], "r");
    if (!f) return 1;
    int v = 0;
    if (fscanf(f, "%d", &v) != 1) { fclose(f); return 1; }
    fclose(f);
    printf("#define GEN_VALUE %d\n", v);
    return 0;
}
