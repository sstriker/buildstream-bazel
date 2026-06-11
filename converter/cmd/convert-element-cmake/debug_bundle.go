package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
)

// streamCopyFile copies src to dst (created with perm) via io.Copy, so a
// large captured input (a VTK-scale trace.jsonl) isn't read fully into
// memory. The destination's parent must already exist.
func streamCopyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// debugBundleInput reports whether a build-dir-relative path names one of
// the converter's PRIMARY INPUT artifacts worth capturing in the
// --out-debug-bundle dir: the File API query+reply tree, the
// --trace-expand log, the ninja graph, the compile database, the variable
// dump, the cache, and the configure log. The capture walk is recursive
// over the cmake build dir, so nested/recursive configure dirs (which live
// as subdirs, each with its own .cmake/api/v1/reply + trace.jsonl +
// build.ninja) are matched by this same predicate — no special-casing.
func debugBundleInput(rel string) bool {
	slash := filepath.ToSlash(rel)
	// File API query + reply objects (codemodel / cache / cmakeFiles /
	// toolchains / configureLog sidecar) for the outer and every nested
	// configure.
	if strings.Contains(slash, ".cmake/api/v1/") {
		return true
	}
	switch filepath.Base(slash) {
	case "trace.jsonl", "trace-plain.jsonl",
		"compile_commands.json",
		cmakerun.VarsDumpFilename, // cmake-to-bazel.vars.dump
		"CMakeCache.txt",
		"CMakeConfigureLog.yaml":
		return true
	}
	// build.ninja + its CMakeFiles/*.ninja includes (rules.ninja, …) so the
	// ninja graph re-parses offline.
	return strings.HasSuffix(slash, ".ninja")
}

// captureDebugBundle copies the primary input artifacts (debugBundleInput)
// from the cmake build dir into bundleDir for offline debugging/replay.
// Soft by contract: a capture failure WARNS but never fails the run — the
// bundle is a diagnostic aid, not a conversion output. In --reply-dir-only
// replay (no live build dir) it derives the build dir from the reply dir's
// grandparent, and skips with a note when none can be determined.
func captureDebugBundle(bundleDir, hostBuildDir, replyDir string) {
	src := hostBuildDir
	if src == "" && replyDir != "" {
		// build/.cmake/api/v1/reply → three parents up is the build dir.
		src = filepath.Clean(filepath.Join(replyDir, "..", "..", ".."))
	}
	if src == "" {
		fmt.Fprintln(os.Stderr, "convert-element-cmake: --out-debug-bundle: no cmake build dir available to capture (pure --reply-dir replay with no derivable build dir); skipping")
		return
	}
	if err := saveDebugBundle(src, bundleDir); err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: --out-debug-bundle: capture failed: %v\n", err)
	}
}

// saveDebugBundle does the copy: walk buildDir, copy every debugBundleInput
// file into bundleDir preserving the build-dir-relative layout, then write
// a short README. Non-regular entries (symlinks, ExternalProject stamps, …)
// are skipped rather than failing — a build dir is broader than the
// toolchain-signal reply dir copyDirContents guards, so a stray symlink
// isn't an error here. guardDstDir + clearDirContents reuse the same
// dangerous-path / stale-content discipline as the toolchain-signal copy.
func saveDebugBundle(buildDir, bundleDir string) error {
	if err := guardDstDir(bundleDir); err != nil {
		return err
	}
	if err := clearDirContents(bundleDir); err != nil {
		return err
	}
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}
	copied := 0
	var byteCount int64
	if err := filepath.Walk(buildDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(buildDir, p)
		if rerr != nil {
			return rerr
		}
		if !debugBundleInput(rel) {
			return nil
		}
		dst := filepath.Join(bundleDir, rel)
		if rerr := os.MkdirAll(filepath.Dir(dst), 0o755); rerr != nil {
			return rerr
		}
		// Stream the copy rather than os.ReadFile-into-memory: a
		// VTK-scale trace.jsonl runs to hundreds of MB, and this flag's
		// whole point is debugging large/gnarly projects, so don't load
		// the largest captured input fully into memory.
		if rerr := streamCopyFile(p, dst, info.Mode().Perm()); rerr != nil {
			return rerr
		}
		copied++
		byteCount += info.Size()
		return nil
	}); err != nil {
		return err
	}
	readme := filepath.Join(bundleDir, "BUNDLE-README.txt")
	if err := os.WriteFile(readme, []byte(debugBundleReadme(buildDir, bundleDir, copied)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"convert-element-cmake: --out-debug-bundle: captured %d primary input file(s) (%d bytes) from %s into %s; replay with --cmake-build-dir %s\n",
		copied, byteCount, buildDir, bundleDir, bundleDir)
	return nil
}

// debugBundleReadme is the human-facing manifest written into the bundle so
// a later debugger knows what it's looking at and how to replay it.
func debugBundleReadme(buildDir, bundleDir string, n int) string {
	return fmt.Sprintf(`convert-element-cmake debug bundle
==================================

Captured %d primary INPUT artifact(s) of a converter run, from the cmake
build dir:
  %s

Contents (build-dir-relative layout preserved, including every nested /
recursive configure as a subdir):
  .cmake/api/v1/{query,reply}/        cmake File API objects (codemodel, cache,
                                      cmakeFiles, toolchains, configureLog sidecar)
  trace.jsonl                         the cmake --trace-expand log the lower reads
  build.ninja + **/*.ninja            the ninja graph (build.ninja + its includes)
  compile_commands.json               the compile database
  cmake-to-bazel.vars.dump            the captured cmake variable namespace
  CMakeCache.txt                      the configure cache
  CMakeFiles/CMakeConfigureLog.yaml   the configure log (cmake 3.26+)

Replay the converter offline against this capture:
  convert-element-cmake --cmake-build-dir %s [other flags...]

Debug aid only — the project source tree is NOT included.
`, n, buildDir, bundleDir)
}
