#include <stdio.h>
int main(int argc, char **argv) {
    /* trivial echo CLI so the roundtrip test has a built artifact */
    if (argc > 1) printf("%s\n", argv[1]);
    return 0;
}
