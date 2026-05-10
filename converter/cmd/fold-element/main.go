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
//	    --cell 'linux_x86_64|@platforms//os:linux,@platforms//cpu:x86_64|elements/libfoo/linux_x86_64/ir.json' \
//	    --cell 'darwin_arm64|@platforms//os:darwin,@platforms//cpu:arm64|elements/libfoo/darwin_arm64/ir.json'
//
// The --cell flag's value is <name>|<constraint1,constraint2,...>|<ir.json path>
// — three pipe-separated fields, where the constraints field
// is a comma-separated list of constraint_value labels. Pipe
// is the outer separator because Bazel constraint labels
// contain ":" (e.g. @platforms//os:linux), which would collide
// with a colon-separated layout. The SelectKey for each cell
// is auto-detected via elementfold.PickSelectKeys; multi-axis
// matrices that don't admit a single varying axis surface as
// an error the operator addresses (per the elementfold
// ROADMAP follow-up).
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
	fs.Var(&cells, "cell", "<name>|<constraint1,constraint2,...>|<ir.json path>; repeat for each platform (pipe is the outer separator because Bazel constraint labels embed colons)")
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
	seenNames := map[string]bool{}
	for _, raw := range cells {
		c, err := parseCell(raw)
		if err != nil {
			return fmt.Errorf("--cell %q: %w", raw, err)
		}
		if seenNames[c.name] {
			return fmt.Errorf("--cell %q: duplicate platform name %q (each --cell must have a unique name; the fold keys per-platform maps by it)", raw, c.name)
		}
		seenNames[c.name] = true
		parsed = append(parsed, c)
	}

	// SelectKey assignment via elementfold.PickSelectKeys: the
	// fold needs each cell tagged with a label that uniquely
	// identifies it within the matrix. Pre-set SelectKey values
	// (populated from a cell's optional 4th field, the
	// operator-declared config_setting label) are honoured
	// as-is; PickSelectKeys auto-detects only for platforms
	// without one. Auto-detection handles single-axis and
	// {linux, darwin}-style two-axis matrices; ambiguous ones
	// (e.g. {linux_x86_64, linux_aarch64, darwin_arm64}) error
	// out cleanly with a message naming the offending platform
	// unless the operator supplied a select_label for each.
	platforms := make([]elementfold.Platform, len(parsed))
	for i, c := range parsed {
		platforms[i] = elementfold.Platform{
			Name:        c.name,
			Constraints: c.constraints,
			SelectKey:   c.selectLabel,
		}
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
	selectLabel string // empty = let PickSelectKeys auto-detect
}

// parseCell decodes "<name>|<c1,c2,...>|<path>" or the
// 4-field "<name>|<c1,c2,...>|<path>|<select_label>" form.
// Requires exactly two or three pipes (3 or 4 fields): extra
// pipes are rejected rather than silently absorbed into the
// last field so accidental pipes in a path don't quietly
// mis-route. Commas split the constraints. Empty name, no
// constraints, and empty path are each rejected with a
// specific error. Pipe is the outer separator (rather than
// ":") because Bazel constraint labels embed colons
// (@platforms//os:linux); SplitN on ":" would shred them.
//
// The optional 4th field is the operator-declared
// config_setting label that overrides PickSelectKeys'
// auto-detection — the escalation path for matrices where no
// single constraint axis uniquely identifies each platform
// (e.g. {linux_x86_64, linux_aarch64, darwin_arm64}).
func parseCell(raw string) (parsedCell, error) {
	parts := strings.Split(raw, "|")
	if len(parts) < 3 || len(parts) > 4 {
		return parsedCell{}, fmt.Errorf("expected 3 or 4 %q-separated fields (<name>|<constraints>|<path>[|<select_label>]); got %d", "|", len(parts))
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return parsedCell{}, fmt.Errorf("empty name")
	}
	path := strings.TrimSpace(parts[2])
	if path == "" {
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
	var selectLabel string
	if len(parts) == 4 {
		selectLabel = strings.TrimSpace(parts[3])
		// Empty 4th field is treated as "absent" rather than
		// rejected so operators can keep a uniform 4-pipe form
		// across their cells and only fill in the override
		// where it's needed.
	}
	return parsedCell{
		name:        name,
		constraints: constraints,
		irJSONPath:  path,
		selectLabel: selectLabel,
	}, nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }
