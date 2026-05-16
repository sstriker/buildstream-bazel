//go:build e2e

// dropin_e2e validates the synthesized cmake-config bundle by staging it
// alongside built artifacts in a fake install prefix, then running a real
// `find_package(hello CONFIG REQUIRED)` against it from an unrelated downstream
// consumer. If a real cmake can't tell the difference between our synthesized
// bundle and what install(EXPORT) would have produced, neither can a Bazel
// build sourcing it.
//
// We exercise system cmake/ninja directly.
package cmakecfg_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

func TestE2E_HelloWorld_BundleDropIn(t *testing.T) {
	ctx := t.Context()
	helloSrc, err := filepath.Abs("../../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "prefix")
	helloBuild := filepath.Join(tmp, "build-hello")
	consumerSrc := filepath.Join(tmp, "consumer-src")
	consumerBuild := filepath.Join(tmp, "consumer-build")

	// 1. Build hello-world the normal way (no shadow); this gives
	//    us a real libhello.a to drop into the synthesized prefix.
	if err := os.MkdirAll(helloBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(ctx, t, "cmake", "-S", helloSrc, "-B", helloBuild, "-G", "Ninja", "-DCMAKE_BUILD_TYPE=Release")
	mustRun(ctx, t, "cmake", "--build", helloBuild)

	// 2. Stage prefix: include/, lib/, lib/cmake/hello/.
	mustMkdirAll(t, filepath.Join(prefix, "include"))
	mustMkdirAll(t, filepath.Join(prefix, "lib"))
	bundleDir := filepath.Join(prefix, "lib", "cmake", "hello")
	mustMkdirAll(t, bundleDir)
	mustCopy(t, filepath.Join(helloSrc, "include", "hello.h"), filepath.Join(prefix, "include", "hello.h"))
	mustCopy(t, filepath.Join(helloBuild, "libhello.a"), filepath.Join(prefix, "lib", "libhello.a"))

	// 3. Synthesize the bundle from the recorded fixture (path independent of
	//    the build above; this exercises the real production path).
	r, err := fileapi.Load("../../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: helloSrc})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for name, body := range bundle.Files {
		if err := os.WriteFile(filepath.Join(bundleDir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// 4. Author a downstream consumer and configure+build it against the
	//    staged prefix.
	mustMkdirAll(t, consumerSrc)
	mustWriteFile(t, filepath.Join(consumerSrc, "CMakeLists.txt"), []byte(`cmake_minimum_required(VERSION 3.20)
project(consumer LANGUAGES C)
find_package(hello CONFIG REQUIRED)
add_executable(consumer main.c)
target_link_libraries(consumer PRIVATE hello::hello)
`))
	mustWriteFile(t, filepath.Join(consumerSrc, "main.c"), []byte(`#include "hello.h"
#include <string.h>
int main(void) {
    return strcmp(hello_message(), "Hello, World!") == 0 ? 0 : 1;
}
`))

	mustRun(ctx, t, "cmake",
		"-S", consumerSrc, "-B", consumerBuild, "-G", "Ninja",
		"-DCMAKE_BUILD_TYPE=Release",
		"-DCMAKE_PREFIX_PATH="+prefix,
	)
	mustRun(ctx, t, "cmake", "--build", consumerBuild)

	// 5. Run the binary; exit code 0 means hello::hello resolved, linked, and
	//    returned the expected string.
	mustRun(ctx, t, filepath.Join(consumerBuild, "consumer"))
}

func mustRun(ctx context.Context, t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = testWriter2{t}
	cmd.Stderr = testWriter2{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, p string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustCopy(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

type testWriter2 struct{ t *testing.T }

func (w testWriter2) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
