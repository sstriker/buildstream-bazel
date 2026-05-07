//go:build !(linux && amd64)

package main

// nativeBackendAvailable reports whether the build target
// supports the native ptrace backend. The amd64+linux variant
// returns true; every other GOOS/GOARCH builds this stub which
// disables the native path so build-tracer always falls back
// to the strace shim.
func nativeBackendAvailable() bool { return false }

// runNative is the no-op stub on non-supported targets. The
// caller checks nativeBackendAvailable first; this exists only
// so the call site compiles on every platform.
func runNative(out string, args []string, captureReads bool) int {
	_ = out
	_ = args
	_ = captureReads
	return 1
}
