// Meson introspection JSON parser. Reads <build>/meson-info/intro-*.json
// and exposes a minimal set of typed structs the converter consumes.
//
// The schema mirrors what `meson introspect --targets` (and siblings)
// produce — see docs/design/meson-native-render.md and
// https://mesonbuild.com/IDE-integration.html. We intentionally model
// only the fields the converter cares about; unknown fields are
// silently ignored so meson schema additions don't break the parser.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Target is one entry in intro-targets.json.
//
// `target_sources` is heterogeneous in meson's JSON: cc-shaped entries
// carry `language`/`compiler`/`parameters`/`sources`; the linker entry
// carries `linker`/`parameters`. The parser keeps a single
// TargetSources slice and tags entries via the `language` and `linker`
// fields; consumers classify with TargetSource.IsCompile / .IsLinker.
type Target struct {
	Name            string         `json:"name"`
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	DefinedIn       string         `json:"defined_in"`
	Subproject      *string        `json:"subproject"` // pointer to distinguish null from ""
	Filename        []string       `json:"filename"`
	BuildByDefault  bool           `json:"build_by_default"`
	Installed       bool           `json:"installed"`
	InstallFilename []string       `json:"install_filename"`
	Dependencies    []string       `json:"dependencies"`
	Depends         []string       `json:"depends"`
	ExtraFiles      []string       `json:"extra_files"`
	TargetSources   []TargetSource `json:"target_sources"`
}

// TargetSource is one entry in target_sources. cc-shaped vs linker
// entries are distinguished at parse time: a non-empty Language means
// it's a compile group; otherwise the Linker field is populated.
type TargetSource struct {
	Language         string   `json:"language"`
	Machine          string   `json:"machine"`
	Compiler         []string `json:"compiler"`
	Parameters       []string `json:"parameters"`
	Sources          []string `json:"sources"`
	GeneratedSources []string `json:"generated_sources"`

	// Linker entries set this; compile entries leave it empty.
	Linker []string `json:"linker"`
}

// IsCompile returns true for cc-shaped entries (language non-empty).
func (s TargetSource) IsCompile() bool { return s.Language != "" }

// IsLinker returns true for linker-shaped entries.
func (s TargetSource) IsLinker() bool { return len(s.Linker) > 0 && s.Language == "" }

// ProjectInfo is the relevant subset of intro-projectinfo.json.
type ProjectInfo struct {
	Name    string `json:"descriptive_name"`
	Version string `json:"version"`
}

// Dependency is one entry in intro-dependencies.json.
type Dependency struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Version     string   `json:"version"`
	CompileArgs []string `json:"compile_args"`
	LinkArgs    []string `json:"link_args"`
}

// Introspect bundles every JSON document the converter consumes.
type Introspect struct {
	Targets      []Target
	ProjectInfo  ProjectInfo
	Dependencies []Dependency
}

// Load reads the meson-info directory and returns a parsed Introspect.
// Missing required files (intro-targets.json, intro-projectinfo.json)
// produce an error; missing optional files (intro-dependencies.json)
// leave their slice empty.
func Load(metaInfoDir string) (*Introspect, error) {
	out := &Introspect{}
	if err := readJSON(filepath.Join(metaInfoDir, "intro-targets.json"), &out.Targets); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(metaInfoDir, "intro-projectinfo.json"), &out.ProjectInfo); err != nil {
		return nil, err
	}
	// Dependencies file is optional. Newer meson always emits it; older
	// versions / projects with zero external deps may still write it as
	// "[]". Treat IsNotExist as empty; surface every other Stat error
	// (permissions, transient I/O) so the caller doesn't silently
	// proceed with an empty Dependencies slice when the file is
	// genuinely there but unreadable.
	depsPath := filepath.Join(metaInfoDir, "intro-dependencies.json")
	if _, err := os.Stat(depsPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", depsPath, err)
		}
	} else {
		if err := readJSON(depsPath, &out.Dependencies); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readJSON(path string, dst any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
