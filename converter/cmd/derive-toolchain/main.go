// derive-toolchain probes cmake's view of the host toolchain and
// emits a Bazel cc_toolchain bundle (BUILD.bazel +
// cc_toolchain_config.bzl) plus a cmake-side toolchain file
// (toolchain.cmake) the orchestrator passes per-conversion to skip
// cmake's compiler-detection probe.
//
// Two operating modes:
//
//	derive-toolchain --reply-dir <dir> --out <dir>
//	    Reads a pre-recorded cmake File API reply (any cmake-
//	    configured project will do) and emits the bundle. Useful
//	    when the operator drives cmake themselves or when running
//	    against a fixture.
//
//	derive-toolchain --probe <cmake-source-root> --build-root <dir> --out <dir>
//	    Drives the variant matrix end-to-end: runs cmake against
//	    the probe project once per Variant in toolchain.DefaultVariants
//	    (baseline + Debug/Release/RelWithDebInfo/MinSizeRel),
//	    folds the results via toolchain.Observe, and emits the
//	    bundle off the resolved per-mode flag sets. This is the
//	    common path — the operator points at
//	    converter/testdata/toolchain-probe/ and lets us drive
//	    cmake.
//
// See docs/toolchain-derivation-plan.md for the full design.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/bazeltoolchain"
	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmaketoolchain"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

func main() {
	fs := flag.NewFlagSet("derive-toolchain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	replyDir := fs.String("reply-dir", "", "cmake File API reply directory (e.g. <build>/.cmake/api/v1/reply). Mutually exclusive with --probe.")
	probeSrc := fs.String("probe", "", "cmake source root to probe; runs cmake configure across the variant matrix and folds via toolchain.Observe. Mutually exclusive with --reply-dir.")
	buildRoot := fs.String("build-root", "", "scratch dir for per-variant cmake build dirs (--probe mode only). Created if absent.")
	outDir := fs.String("out", "", "output directory; created if absent. BUILD.bazel + cc_toolchain_config.bzl + toolchain.cmake land here.")
	pkgName := fs.String("package-name", "toolchain", "Bazel package name for the emitted rules (purely cosmetic; affects the toolchain identifier suffix).")
	targetLibc := fs.String("target-libc", "", "target libc identifier (glibc, musl, macosx, ...). Auto-derived from CMAKE_SYSTEM_NAME when empty.")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(64)
	}
	if (*replyDir == "") == (*probeSrc == "") {
		fmt.Fprintln(os.Stderr, "derive-toolchain: pass exactly one of --reply-dir or --probe")
		fs.Usage()
		os.Exit(64)
	}
	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "derive-toolchain: --out is required")
		fs.Usage()
		os.Exit(64)
	}
	if *probeSrc != "" && *buildRoot == "" {
		fmt.Fprintln(os.Stderr, "derive-toolchain: --build-root is required with --probe")
		os.Exit(64)
	}

	// Two execution paths share the back half (emit + write):
	//   replyDir -> Model              -> bazeltoolchain.Emit
	//   probeSrc -> []ProbeResult      -> Observe -> bazeltoolchain.EmitResolved
	// The cmake-toolchain file (toolchain.cmake) only needs Model;
	// in --probe mode we use the baseline variant's Model since
	// that's what represents the always-on layer.
	var (
		bundle    *bazeltoolchain.Bundle
		baseModel *toolchain.Model
		err       error
	)
	if *replyDir != "" {
		baseModel, bundle, err = fromReplyDir(*replyDir, *pkgName, *targetLibc)
	} else {
		baseModel, bundle, err = fromProbe(*probeSrc, *buildRoot, *pkgName, *targetLibc)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive-toolchain: %v\n", err)
		os.Exit(1)
	}

	cmakeBody, err := cmaketoolchain.Emit(baseModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive-toolchain: emit cmake toolchain: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "derive-toolchain: mkdir %s: %v\n", *outDir, err)
		os.Exit(1)
	}
	for name, body := range bundle.Files {
		dst := filepath.Join(*outDir, name)
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "derive-toolchain: write %s: %v\n", dst, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "derive-toolchain: wrote %s (%d bytes)\n", dst, len(body))
	}
	cmakeDst := filepath.Join(*outDir, "toolchain.cmake")
	if err := os.WriteFile(cmakeDst, cmakeBody, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "derive-toolchain: write %s: %v\n", cmakeDst, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "derive-toolchain: wrote %s (%d bytes)\n", cmakeDst, len(cmakeBody))
}

// fromReplyDir is the legacy single-reply path: load one File API
// reply, emit a single-variant Bundle. compile_flags pick up
// CMAKE_<LANG>_FLAGS from the reply; per-mode slots stay empty.
// Useful for fixture-driven smoke tests and operators who already
// have a reply on disk.
func fromReplyDir(replyDir, pkgName, libc string) (*toolchain.Model, *bazeltoolchain.Bundle, error) {
	r, err := fileapi.Load(replyDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load reply: %w", err)
	}
	m, err := toolchain.FromReply(r)
	if err != nil {
		return nil, nil, fmt.Errorf("extract: %w", err)
	}
	b, err := bazeltoolchain.Emit(m, bazeltoolchain.Config{
		PackageName: pkgName,
		TargetLibc:  libc,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("emit: %w", err)
	}
	return m, b, nil
}

// fromProbe is the production path: run cmake against the probe
// project once per Variant in DefaultVariants, fold via Observe,
// emit a multi-variant Bundle whose cc_toolchain_config carries
// per-mode (dbg / opt) flag sets distinct from the always-on
// baseline.
func fromProbe(srcRoot, buildRoot, pkgName, libc string) (*toolchain.Model, *bazeltoolchain.Bundle, error) {
	abs := func(p string) string {
		out, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return out
	}
	srcRoot = abs(srcRoot)
	buildRoot = abs(buildRoot)

	results, err := toolchain.Probe(context.Background(), toolchain.ProbeOptions{
		SourceRoot: srcRoot,
		BuildRoot:  buildRoot,
		Stdout:     os.Stderr,
		Stderr:     os.Stderr,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("probe: %w", err)
	}
	rt := toolchain.Observe(results)
	if rt == nil {
		return nil, nil, fmt.Errorf("probe: no results to observe")
	}
	b, err := bazeltoolchain.EmitResolved(rt, bazeltoolchain.Config{
		PackageName: pkgName,
		TargetLibc:  libc,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("emit: %w", err)
	}
	return rt.Base, b, nil
}
