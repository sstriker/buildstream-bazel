// imports-wrapper-gen synthesizes the prebuilt-wrapper package for a
// hand-written imports manifest: per export, a cc_import over the
// prefix-staged archive plus a cc_library wrapper carrying the
// export's declared closure (Export.Deps) as REAL Bazel deps — and
// rewrites the manifest to point at the wrappers with Deps CLEARED,
// preserving the schema invariant (Deps non-empty ⇔ the label does
// not model its own deps; see internal/manifest.Export.Deps).
// Executable exports (kind="executable" — protoc-shaped installed
// programs) become filegroups over their bin paths instead, the
// file-shaped labels genrule tool lifts consume. Manifests whose Deps
// close a cycle among the exports are refused up front (Bazel rejects
// cyclic deps at load time).
//
// This is the "complete the loop" tool for host-install prefixes: the
// hand-written manifest stays the single source of truth, and the
// wrapper BUILD — Bazel-native transitivity, visible to every
// consumer and to bazel query — falls out mechanically instead of
// being hand-maintained. After generation, consumers converted
// against the OUTPUT manifest wire exactly one direct edge per
// imported target (the wrapper), with the closure riding Bazel
// transitivity instead of the consumer-side Deps wiring.
//
// Usage:
//
//	imports-wrapper-gen \
//	  --manifest exports.json \
//	  --package prebuilts/greet \
//	  --out-build <prefix>/BUILD.bazel \
//	  --out-manifest exports.wrapped.json
//
// The BUILD is written for the package that contains the prefix tree
// (--package names its workspace-relative path): cc_import
// static_library attributes reference the archives by their
// prefix-relative paths (link_paths with the /opt/prefix/ anchor
// stripped), so dropping the BUILD at the staged prefix root makes
// every reference resolve in place.
//
// Exit codes: 0 success, 1 I/O or manifest errors, 64 usage.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
	wrappergen "github.com/sstriker/buildstream-bazel/internal/wrappergen"
)

func main() {
	fs := flag.NewFlagSet("imports-wrapper-gen", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "input imports manifest (exports.json shape)")
	pkgPath := fs.String("package", "", "workspace-relative Bazel package the wrapper BUILD lands in (the staged prefix root)")
	outBuild := fs.String("out-build", "", "output BUILD.bazel path")
	outManifest := fs.String("out-manifest", "", "output manifest path (labels repointed at the wrappers, deps cleared)")
	element := fs.String("element", "", "generate for this element only (default: every element with importable exports)")
	_ = fs.Parse(os.Args[1:])
	if *manifestPath == "" || *pkgPath == "" || *outBuild == "" || *outManifest == "" {
		fmt.Fprintln(os.Stderr, "imports-wrapper-gen: --manifest, --package, --out-build and --out-manifest are required")
		os.Exit(64)
	}

	im, err := manifest.LoadDoc(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "imports-wrapper-gen: %v\n", err)
		os.Exit(1)
	}
	build, rewritten, err := wrappergen.Generate(im, *pkgPath, *element)
	if err != nil {
		fmt.Fprintf(os.Stderr, "imports-wrapper-gen: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outBuild, build, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "imports-wrapper-gen: %v\n", err)
		os.Exit(1)
	}
	if err := wrappergen.WriteManifest(*outManifest, rewritten); err != nil {
		fmt.Fprintf(os.Stderr, "imports-wrapper-gen: %v\n", err)
		os.Exit(1)
	}
}
