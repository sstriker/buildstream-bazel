// probe-cell is the worker binary each project-A genrule invokes
// to produce one (variant, platform) probe artifact. It reads
// flags describing the cmake source root and the variant's cache
// vars, runs cmakerun.Configure for that one cell, loads the
// fileapi reply, and emits a probejson document at --out.
//
// One process = one cell. Bazel handles the matrix: project A's
// BUILD.bazel declares one genrule per (variant, platform), each
// invoking probe-cell with that cell's flags and exec_compatible_with
// set to the platform's constraints.
//
// The binary is intentionally minimal — the variant flag plumbing
// uses cmakerun.Options.ExtraCacheVars (Stage 1) directly, with
// CMAKE_BUILD_TYPE lifted to the dedicated slot to match the same
// last-wins discipline toolchain.Probe applies.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/cmakerun"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain/probejson"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "probe-cell:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cmakeSource = flag.String("cmake-source", "", "cmake source root")
		variantName = flag.String("variant", "", "variant name (recorded in the output's Variant.Name)")
		outPath     = flag.String("out", "", "output JSON path")
		buildDirArg = flag.String("build-dir", "", "build dir to use; created if absent. Empty → tmp dir cleaned up after.")
	)
	var cacheVars stringSliceFlag
	flag.Var(&cacheVars, "cache-var", "cmake -D KEY=VALUE pair (repeatable)")
	flag.Parse()

	if *cmakeSource == "" {
		return fmt.Errorf("--cmake-source is required")
	}
	if *outPath == "" {
		return fmt.Errorf("--out is required")
	}
	if *variantName == "" {
		// Allowing this would emit probe.json with an empty
		// Variant.Name, which breaks unify-toolchains' grouping
		// (the platform/variant filename split assumes both
		// halves are non-empty) and makes artifacts ambiguous.
		// Callers wanting "no overrides" should pass
		// --variant=baseline explicitly.
		return fmt.Errorf("--variant is required")
	}

	cv := map[string]string{}
	for _, kv := range cacheVars {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("--cache-var %q: expected KEY=VALUE", kv)
		}
		cv[k] = v
	}
	variant := toolchain.Variant{Name: *variantName, CacheVars: cv}

	buildDir := *buildDirArg
	if buildDir == "" {
		tmp, err := os.MkdirTemp("", "probe-cell-build-*")
		if err != nil {
			return fmt.Errorf("mkdir tmp build dir: %w", err)
		}
		defer os.RemoveAll(tmp)
		buildDir = tmp
	} else if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return fmt.Errorf("mkdir build dir: %w", err)
	}

	// Lift CMAKE_BUILD_TYPE into the dedicated slot; everything
	// else flows into ExtraCacheVars (the Stage 1 -D pass-through).
	// Mirrors toolchain.cmakeOptionsFor without exporting it.
	opts := cmakerun.Options{
		SourceRoot: *cmakeSource,
		BuildDir:   buildDir,
		BuildType:  variant.CacheVars["CMAKE_BUILD_TYPE"],
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}
	for k, v := range variant.CacheVars {
		if k == "CMAKE_BUILD_TYPE" {
			continue
		}
		if opts.ExtraCacheVars == nil {
			opts.ExtraCacheVars = make(map[string]string, len(variant.CacheVars))
		}
		opts.ExtraCacheVars[k] = v
	}

	reply, err := cmakerun.Configure(context.Background(), opts)
	if err != nil {
		return fmt.Errorf("cmake configure: %w", err)
	}
	r, err := fileapi.Load(reply.Path)
	if err != nil {
		return fmt.Errorf("fileapi load: %w", err)
	}

	body, err := probejson.Marshal(variant, r)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(*outPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *outPath, err)
	}
	return nil
}

// stringSliceFlag is a flag.Value that accumulates repeats. Used
// for --cache-var which appears multiple times on a typical cmd
// line (one per cmake -D pair).
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }
