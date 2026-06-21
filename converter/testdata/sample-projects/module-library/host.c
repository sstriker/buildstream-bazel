/* Loads the MODULE by its EXACT cmake filename and calls into it. Exits 0
 * only when dlopen("libplugin.so") resolves and plugin_entry() returns 7 —
 * i.e. only when the converted cc_shared_library kept cmake's exact name. */
#include <dlfcn.h>
#include <stdio.h>

int main(void) {
    void *h = dlopen("libplugin.so", RTLD_NOW);
    if (!h) {
        printf("dlopen failed: %s\n", dlerror());
        return 1;
    }
    int (*fn)(void) = (int (*)(void))dlsym(h, "plugin_entry");
    if (!fn) {
        printf("dlsym failed: %s\n", dlerror());
        return 2;
    }
    return fn() == 7 ? 0 : 3;
}
