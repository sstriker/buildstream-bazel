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

	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain/presets"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain/projecta"
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
		variantsFrom    = fs.String("variants-from", "", "CMakePresets.json whose configurePresets become the variant axis")
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

	variants, err := presets.LoadFile(*variantsFrom)
	if err != nil {
		return fmt.Errorf("load presets: %w", err)
	}
	if len(variants) == 0 {
		return fmt.Errorf("no configurePresets in %s", *variantsFrom)
	}

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
