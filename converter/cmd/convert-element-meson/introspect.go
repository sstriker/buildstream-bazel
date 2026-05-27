// Meson introspection JSON parser. Reads <build>/meson-info/intro-*.json
// and exposes a minimal set of typed structs the converter consumes.
//
// The schema mirrors what `meson introspect --targets` (and siblings)
// produce — see docs/architecture.md and
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

// InstallPlanEntry is one row in intro-install_plan.json's
// `targets` / `headers` / `data` / ... sections. `Destination`
// embeds meson's directory placeholders ({prefix}, {bindir},
// {libdir_static}, {libdir_shared}, {includedir}, {datadir},
// {mandir}, ...) which the Phase B fallback resolves against
// the values from intro-buildoptions.json. `Tag` is meson's
// richer signal vs cmake's destination-path inference —
// canonical values include "runtime" (executables / shared
// libs), "devel" (static libs + headers), "man", "i18n",
// "doc". `Subproject` is non-nil + non-empty for entries
// living inside a meson subproject (already a Phase A
// refusal; the fallback inherits the same filter).
type InstallPlanEntry struct {
	Destination string  `json:"destination"`
	Tag         string  `json:"tag"`
	Subproject  *string `json:"subproject"`
}

// InstallPlan is the relevant subset of intro-install_plan.json.
// Keys in each section are absolute source-or-build paths;
// values describe where each artefact lands after
// `meson install --destdir=...`. The Phase B fallback consumes
// the `Targets` (compiled artefacts) and `Headers` (install_headers
// declarations) sections to seed per-target placeholder stubs.
// Other sections (`Data`, `InstallSubdirs`, `Man`, `Symlinks`,
// `Emptydir`) are decoded but not yet wired into the placeholder
// — they have no direct Bazel analog at the rule level.
type InstallPlan struct {
	Targets        map[string]InstallPlanEntry `json:"targets"`
	Headers        map[string]InstallPlanEntry `json:"headers"`
	Data           map[string]InstallPlanEntry `json:"data"`
	InstallSubdirs map[string]InstallPlanEntry `json:"install_subdirs"`
	Man            map[string]InstallPlanEntry `json:"man"`
	Symlinks       map[string]InstallPlanEntry `json:"symlinks"`
	Emptydir       map[string]InstallPlanEntry `json:"emptydir"`
}

// BuildOption is one entry in intro-buildoptions.json — the
// flat list of every configurable option meson exposes (core,
// directory, base, compiler, user). The Phase B fallback only
// consumes the `section: directory` rows (prefix / bindir /
// libdir / includedir / ...) to resolve install-plan placeholders.
type BuildOption struct {
	Name    string `json:"name"`
	Value   any    `json:"value"`
	Section string `json:"section"`
	Type    string `json:"type"`
}

// Introspect bundles every JSON document the converter consumes.
type Introspect struct {
	Targets      []Target
	ProjectInfo  ProjectInfo
	Dependencies []Dependency
	// InstallPlan + BuildOptions are populated by Load when the
	// matching intro-*.json files exist (always in live mode;
	// optionally in --info-dir mode so older recorded fixtures
	// keep loading). Phase A doesn't touch them; the Phase B
	// fallback path reads them via emitFallbackPlaceholder.
	InstallPlan  InstallPlan
	BuildOptions []BuildOption
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
	// intro-install_plan.json + intro-buildoptions.json — optional
	// for backward compat with offline --info-dir fixtures
	// recorded before Phase B landed. Missing files leave the
	// fields zero-valued; the fallback emitter treats an empty
	// InstallPlan as "nothing to place" and refuses cleanly.
	planPath := filepath.Join(metaInfoDir, "intro-install_plan.json")
	if _, err := os.Stat(planPath); err == nil {
		if err := readJSON(planPath, &out.InstallPlan); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", planPath, err)
	}
	optsPath := filepath.Join(metaInfoDir, "intro-buildoptions.json")
	if _, err := os.Stat(optsPath); err == nil {
		if err := readJSON(optsPath, &out.BuildOptions); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", optsPath, err)
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
