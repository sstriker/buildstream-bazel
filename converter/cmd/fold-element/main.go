// fold-element composes N per-platform ir.Package JSONs into
// one unified BUILD.bazel via elementfold + bazel.Emit. The
// orchestrator's per-element multi-platform fan-out spawns
// this binary as a post-process step after the per-platform
// convert-element Actions complete.
//
// Usage:
//
//	fold-element \
//	    --out-build elements/libfoo/BUILD.bazel \
//	    --cell linux_x86_64:@platforms//os:linux,@platforms//cpu:x86_64:elements/libfoo/linux_x86_64/ir.json \
//	    --cell darwin_arm64:@platforms//os:darwin,@platforms//cpu:arm64:elements/libfoo/darwin_arm64/ir.json
//
// The --cell flag's value is <name>:<constraint1,constraint2,...>:<ir.json path>
// — three colon-separated fields, where the constraints field
// is a comma-separated list of constraint_value labels. The
// SelectKey for each cell is auto-detected via
// elementfold.PickSelectKeys; multi-axis matrices that don't
// admit a single varying axis surface as an error the operator
// addresses (per the elementfold ROADMAP follow-up).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/elementfold"
	"github.com/sstriker/cmake-to-bazel/converter/internal/emit/bazel"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fold-element:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("fold-element", flag.ContinueOnError)
	outBuild := fs.String("out-build", "", "destination path for the unified BUILD.bazel")
	var cells stringSliceFlag
	fs.Var(&cells, "cell", "<name>:<constraint1,constraint2,...>:<ir.json path>; repeat for each platform")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *outBuild == "" {
		return fmt.Errorf("--out-build is required")
	}
	if len(cells) == 0 {
		return fmt.Errorf("at least one --cell is required")
	}

	parsed := make([]parsedCell, 0, len(cells))
	for _, raw := range cells {
		c, err := parseCell(raw)
		if err != nil {
			return fmt.Errorf("--cell %q: %w", raw, err)
		}
		parsed = append(parsed, c)
	}

	// SelectKey assignment via elementfold.PickSelectKeys: the
	// fold needs each cell tagged with a constraint label that
	// uniquely identifies it within the matrix. Auto-detection
	// handles single-axis and {linux, darwin}-style two-axis
	// matrices; ambiguous ones (e.g. {linux_x86_64,
	// linux_aarch64, darwin_arm64}) error out cleanly with a
	// message naming the offending platform.
	platforms := make([]elementfold.Platform, len(parsed))
	for i, c := range parsed {
		platforms[i] = elementfold.Platform{Name: c.name, Constraints: c.constraints}
	}
	keys, err := elementfold.PickSelectKeys(platforms)
	if err != nil {
		return err
	}

	foldCells := make([]elementfold.Cell, len(parsed))
	for i, c := range parsed {
		body, err := os.ReadFile(c.irJSONPath)
		if err != nil {
			return fmt.Errorf("read cell %q ir.json %q: %w", c.name, c.irJSONPath, err)
		}
		var pkg ir.Package
		if err := json.Unmarshal(body, &pkg); err != nil {
			return fmt.Errorf("parse cell %q ir.json %q: %w", c.name, c.irJSONPath, err)
		}
		foldCells[i] = elementfold.Cell{
			Platform: elementfold.Platform{
				Name:        c.name,
				Constraints: c.constraints,
				SelectKey:   keys[c.name],
			},
			Pkg: &pkg,
		}
	}

	merged, err := elementfold.Fold(foldCells)
	if err != nil {
		return err
	}
	out, err := bazel.Emit(merged)
	if err != nil {
		return fmt.Errorf("emit unified BUILD.bazel: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*outBuild), 0o755); err != nil {
		return fmt.Errorf("mkdir out: %w", err)
	}
	if err := os.WriteFile(*outBuild, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *outBuild, err)
	}
	return nil
}

type parsedCell struct {
	name        string
	constraints []string
	irJSONPath  string
}

// parseCell decodes "<name>:<c1,c2,...>:<path>". Two colons
// split the three fields; commas split the constraints. Empty
// constraints ("") and empty paths are rejected.
func parseCell(raw string) (parsedCell, error) {
	// SplitN with 3 keeps any colons in the path field intact
	// (e.g. on Windows that's not a concern for this binary's
	// actual use, but keeps the parser robust).
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		return parsedCell{}, fmt.Errorf("expected <name>:<constraints>:<path>")
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return parsedCell{}, fmt.Errorf("empty name")
	}
	if parts[2] == "" {
		return parsedCell{}, fmt.Errorf("empty path")
	}
	constraintsRaw := strings.Split(parts[1], ",")
	constraints := make([]string, 0, len(constraintsRaw))
	for _, c := range constraintsRaw {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		constraints = append(constraints, c)
	}
	if len(constraints) == 0 {
		return parsedCell{}, fmt.Errorf("no constraints")
	}
	return parsedCell{
		name:        name,
		constraints: constraints,
		irJSONPath:  parts[2],
	}, nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }
