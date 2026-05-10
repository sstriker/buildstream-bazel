// pyproject.toml parser.
//
// Models the subset of PEP 621 + per-backend config that
// convert-element-pyproject's lowering pass actually consumes.
// Unknown fields are ignored at parse time so future PEP
// additions don't break us — schema validation lives in
// `lower.go` against the parsed struct.
package main

import (
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// Pyproject is the parsed pyproject.toml.
type Pyproject struct {
	BuildSystem BuildSystem `toml:"build-system"`
	Project     Project     `toml:"project"`
	Tool        Tool        `toml:"tool"`
}

// BuildSystem is the [build-system] table (PEP 518).
// `Backend` (mapped from `build-backend`) is the dotted path
// to the build backend's API module: "flit_core.buildapi",
// "hatchling.build", "setuptools.build_meta",
// "poetry.core.masonry.api", etc. v1 dispatches on this.
type BuildSystem struct {
	Backend  string   `toml:"build-backend"`
	Requires []string `toml:"requires"`
}

// Project is the [project] table (PEP 621). Models the
// fields the lowering pass cares about; `Other` is intentionally
// absent — unknown keys flow through go-toml's default ignore.
type Project struct {
	Name         string              `toml:"name"`
	Version      string              `toml:"version"`
	Dynamic      []string            `toml:"dynamic"`
	Dependencies []string            `toml:"dependencies"`
	Scripts      map[string]string   `toml:"scripts"`
	GUIScripts   map[string]string   `toml:"gui-scripts"`
	OptionalDeps map[string][]string `toml:"optional-dependencies"`
}

// Tool is the [tool] table; per-backend configuration lives
// here. Each backend's relevant subset is parsed conditionally
// in backends.go.
type Tool struct {
	Flit       *Flit       `toml:"flit"`
	Hatch      *Hatch      `toml:"hatch"`
	Setuptools *Setuptools `toml:"setuptools"`
	Poetry     *Poetry     `toml:"poetry"`
}

// Flit is [tool.flit].
type Flit struct {
	Module *FlitModule `toml:"module"`
}

// FlitModule is [tool.flit.module].
type FlitModule struct {
	Name string `toml:"name"`
}

// Hatch is [tool.hatch].
type Hatch struct {
	Build *HatchBuild `toml:"build"`
}

// HatchBuild is [tool.hatch.build].
type HatchBuild struct {
	Targets *HatchTargets `toml:"targets"`
}

// HatchTargets is [tool.hatch.build.targets]. Only the
// `wheel.packages` slot is consumed by v1; sdist / app /
// per-target overrides are out of scope.
type HatchTargets struct {
	Wheel *HatchWheel `toml:"wheel"`
}

// HatchWheel is [tool.hatch.build.targets.wheel].
type HatchWheel struct {
	Packages []string `toml:"packages"`
}

// Setuptools is [tool.setuptools]. setuptools is the most
// permissive backend — `Packages` can be a literal list OR
// a `find` directive. We capture the raw TOML for `Packages`
// (interface{}) and discriminate in backends.go.
type Setuptools struct {
	// Packages can be:
	//   - a list of strings: ["foo", "foo.bar"]
	//   - a {find = {...}} table.
	// go-toml represents either as map[string]any / []any.
	Packages    any                 `toml:"packages"`
	PackageDir  map[string]string   `toml:"package-dir"`
	PackageData map[string][]string `toml:"package-data"`
	Dynamic     map[string]any      `toml:"dynamic"`
}

// Poetry is [tool.poetry]. Only `Packages` is consumed.
type Poetry struct {
	Packages []PoetryPackage `toml:"packages"`
}

// PoetryPackage is one entry in [tool.poetry].packages.
type PoetryPackage struct {
	Include string `toml:"include"`
	From    string `toml:"from"`
}

// Load reads pyproject.toml from disk and parses it. Unknown
// fields are silently ignored (go-toml's default); structural
// errors (malformed TOML, type mismatches on known fields)
// surface as a typed `pyproject-parse-failed` Tier-1 failure.
func Load(path string) (*Pyproject, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, newFailure(pyprojectParseFailed, "read %s: %v", path, err)
	}
	var p Pyproject
	if err := toml.Unmarshal(body, &p); err != nil {
		return nil, newFailure(pyprojectParseFailed, "parse %s: %v", path, err)
	}
	return &p, nil
}

// SetuptoolsFindDirective decodes setuptools' `[tool.setuptools.packages.find]`
// shape from the raw `Packages` TOML value. Returns ok=false
// when `Packages` is a list (operator gave an explicit package
// list) or absent.
type SetuptoolsFindDirective struct {
	Where      []string `toml:"where"`
	Include    []string `toml:"include"`
	Exclude    []string `toml:"exclude"`
	Namespaces *bool    `toml:"namespaces"`
}

// SetuptoolsExplicitPackages decodes the literal `Packages` list
// when setuptools was configured with a static list (not a
// find directive). Returns nil when `Packages` is absent or
// is a find-directive map.
func (s *Setuptools) ExplicitPackages() []string {
	if s == nil || s.Packages == nil {
		return nil
	}
	raw, ok := s.Packages.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if str, ok := e.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// FindDirective decodes setuptools' packages.find{} shape.
// Returns nil when Packages is absent or is an explicit list.
func (s *Setuptools) FindDirective() (*SetuptoolsFindDirective, error) {
	if s == nil || s.Packages == nil {
		return nil, nil
	}
	asMap, ok := s.Packages.(map[string]any)
	if !ok {
		return nil, nil
	}
	findRaw, ok := asMap["find"]
	if !ok {
		return nil, nil
	}
	// Round-trip through TOML so go-toml's nested-table decoding
	// kicks in — easier than walking the map[string]any by hand.
	body, err := toml.Marshal(findRaw)
	if err != nil {
		return nil, fmt.Errorf("re-marshal find directive: %w", err)
	}
	var fd SetuptoolsFindDirective
	if err := toml.Unmarshal(body, &fd); err != nil {
		return nil, fmt.Errorf("decode find directive: %w", err)
	}
	if len(fd.Where) == 0 {
		fd.Where = []string{"."}
	}
	return &fd, nil
}
