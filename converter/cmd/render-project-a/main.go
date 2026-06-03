// render-project-a renders a BUILD.bazel that drives the toolchain
// probe matrix as Bazel actions: one genrule per (variant, platform)
// cell, each invoking probe-cell with the variant's cache vars and
// exec_compatible_with set to the platform's constraints.
//
// Inputs:
//   - --variants-from CMakePresets.json (path to CMakePresets.json
//     whose configurePresets become the variant axis)
//   - --platforms-json (JSON file describing the target platforms;
//     [{"name": "...", "constraints": ["...", ...]}, ...])
//   - --out (output directory; the rendered BUILD.bazel lands here)
//   - --cmake-source-label / --cmake-lists-label / --probe-cell-label
//     (Bazel labels the genrules reference)
//
// Output: <out>/BUILD.bazel
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain/kits"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain/presets"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain/projecta"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "render-project-a:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("render-project-a", flag.ContinueOnError)
	var (
		out             = fs.String("out", "", "output directory; BUILD.bazel lands here. Created if absent.")
		variantsFrom    = fs.String("variants-from", "", "CMakePresets.json whose configurePresets become the build (variant) axis")
		kitsFrom        = fs.String("kits-from", "", "optional cmake-kits.json whose kits become the compiler axis; cross-producted with the variant axis")
		platformsJSON   = fs.String("platforms-json", "", "JSON file with target platforms: [{name,constraints[]}, ...]")
		cmakeSrcLabel   = fs.String("cmake-source-label", "//probe:source", "Bazel label of the cmake source filegroup")
		cmakeListsLabel = fs.String("cmake-lists-label", "//probe:CMakeLists.txt", "Bazel label of the CMakeLists.txt file")
		probeCellLabel  = fs.String("probe-cell-label", "//tools:probe-cell", "Bazel label of the probe-cell binary")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *out == "" {
		return fmt.Errorf("--out is required")
	}
	if *variantsFrom == "" {
		return fmt.Errorf("--variants-from is required")
	}
	if *platformsJSON == "" {
		return fmt.Errorf("--platforms-json is required")
	}

	// presets.LoadFile returns (nil, nil) when the file is missing,
	// which is the right shape for callers unioning Presets +
	// UserPresets. Here --variants-from is required, so a missing
	// or unreadable file is an operator error: surface it
	// distinctly from "file present but empty configurePresets".
	if _, err := os.Stat(*variantsFrom); err != nil {
		return fmt.Errorf("--variants-from %s: %w", *variantsFrom, err)
	}
	variants, err := presets.LoadFile(*variantsFrom)
	if err != nil {
		return fmt.Errorf("load presets: %w", err)
	}
	if len(variants) == 0 {
		return fmt.Errorf("no configurePresets in %s", *variantsFrom)
	}

	// Optional compiler axis: cmake-kits.json. When provided, the probe
	// matrix becomes the cross-product (kit × variant), and each cell
	// records its kit so unify-toolchains emits one toolchain per
	// (platform, kit). When absent, VariantMatrix returns the variants
	// unchanged — the single-toolchain-per-platform path.
	var kitVariants []toolchain.Variant
	if *kitsFrom != "" {
		if _, err := os.Stat(*kitsFrom); err != nil {
			return fmt.Errorf("--kits-from %s: %w", *kitsFrom, err)
		}
		kitVariants, err = kits.LoadFile(*kitsFrom)
		if err != nil {
			return fmt.Errorf("load kits: %w", err)
		}
		if len(kitVariants) == 0 {
			return fmt.Errorf("no kits in %s", *kitsFrom)
		}
	}
	variants = toolchain.VariantMatrix(kitVariants, variants)

	platforms, err := loadPlatforms(*platformsJSON)
	if err != nil {
		return fmt.Errorf("load platforms: %w", err)
	}

	body, err := projecta.RenderToolchainProbe(projecta.ToolchainProbeArgs{
		CmakeSourceLabel: *cmakeSrcLabel,
		CmakeListsLabel:  *cmakeListsLabel,
		ProbeCellLabel:   *probeCellLabel,
		Variants:         variants,
		Platforms:        platforms,
	})
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fmt.Errorf("mkdir out: %w", err)
	}
	target := filepath.Join(*out, "BUILD.bazel")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	fmt.Fprintf(os.Stderr, "render-project-a: wrote %s (%d cells)\n", target, len(variants)*len(platforms))
	return nil
}

func loadPlatforms(path string) ([]projecta.Platform, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name        string   `json:"name"`
		Constraints []string `json:"constraints"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make([]projecta.Platform, 0, len(raw))
	for _, r := range raw {
		out = append(out, projecta.Platform{
			Name:        r.Name,
			Constraints: r.Constraints,
		})
	}
	return out, nil
}
