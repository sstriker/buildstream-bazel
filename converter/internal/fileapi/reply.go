// Package fileapi parses CMake's File API v1 reply directory.
//
// The reply directory is produced by cmake when run with files staged under
// <build>/.cmake/api/v1/query/. We consume five object kinds:
//
//   - codemodel-v2: project/target structure, sources, compile/link flags.
//   - toolchains-v1: per-language compiler identification and implicit dirs.
//   - cmakeFiles-v1: every CMakeLists / .cmake file consumed at configure.
//   - cache-v2: post-configure cache entries.
//   - configureLog-v1: pointer to CMakeConfigureLog.yaml (cmake 3.26+).
//
// All parsing is pure; no I/O outside the supplied directory — the
// configureLog object decodes only its sidecar JSON, which records the
// absolute path to the YAML log. Callers that want the log events read
// the YAML separately via LoadConfigureLogYAML so this package keeps
// its single-directory I/O scope.
package fileapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Reply is a parsed reply directory rooted at Path.
type Reply struct {
	Path       string
	Index      Index
	Codemodel  Codemodel
	Toolchains Toolchains
	CMakeFiles CMakeFiles
	Cache      Cache
	// Targets maps target id to its parsed details for the primary
	// configuration (Codemodel.Configurations[0].Name — the first
	// declared config, typically "Release"). Single-config callers
	// (BuildType-driven) get the only config's data here; multi-config
	// callers (BuildTypes-driven) get the primary config and consult
	// TargetsByConfig for the rest. Populated by Load.
	Targets map[string]Target
	// TargetsByConfig maps target id → config name → parsed details.
	// Carries one entry per (target id, configuration) pair declared
	// in Codemodel.Configurations. Populated by Load whenever the
	// reply has more than one Configuration entry (multi-config
	// generators emit one per CMAKE_CONFIGURATION_TYPES entry); nil
	// for single-config replies where Targets already carries
	// everything. Phase 5 of the generator-parity uplift (ROADMAP.md)
	// consumes this for the per-config compile/link fragment fold.
	TargetsByConfig map[string]map[string]Target
	// Directories carries the parsed directory-*.json content for every
	// ConfigDirectory.JSONFile referenced from Codemodel. Indexed by
	// JSONFile basename (the codemodel's per-config Directories[]
	// entries reference them this way). Empty when no directories carry
	// install rules.
	Directories map[string]Directory
	// ConfigureLog is the parsed configureLog-v1 sidecar (cmake 3.26+).
	// Nil when cmake < 3.26 or the project didn't fire any
	// configureLog-aware events during configure. Carries the path
	// to CMakeConfigureLog.yaml; callers load the YAML separately
	// via LoadConfigureLogYAML when they need event data.
	ConfigureLog *ConfigureLog
}

// Load reads every consumed object from a reply directory. Returns an error if
// the index is missing, malformed, or references a missing object file.
func Load(replyDir string) (*Reply, error) {
	idx, err := loadIndex(replyDir)
	if err != nil {
		return nil, fmt.Errorf("fileapi: load index: %w", err)
	}
	r := &Reply{
		Path:        replyDir,
		Index:       idx,
		Targets:     map[string]Target{},
		Directories: map[string]Directory{},
	}

	if err := loadReplyObjects(r, replyDir); err != nil {
		return nil, err
	}
	if err := loadReplyConfigurations(r, replyDir); err != nil {
		return nil, err
	}
	return r, nil
}

// loadReplyObjects reads each supported top-level reply object (codemodel,
// toolchains, cmakeFiles, cache, configureLog) into r. Unknown object kinds are
// skipped (forward-compat with newer cmakes); a supported kind at an
// unsupported schema major is a hard error.
func loadReplyObjects(r *Reply, replyDir string) error {
	for _, obj := range r.Index.Objects {
		path := filepath.Join(replyDir, obj.JSONFile)
		want, known := SupportedObjectMajors[obj.Kind]
		if !known {
			// Unknown kind — cmake may add new object kinds later;
			// skipping silently lets us coexist with future cmakes
			// rather than failing on every reply that ships one.
			continue
		}
		if obj.Version.Major != want {
			return fmt.Errorf("fileapi: %s schema major %d.%d not supported (this loader handles major %d); upgrade convert-element-cmake or pin cmake to a compatible version",
				obj.Kind, obj.Version.Major, obj.Version.Minor, want)
		}
		switch obj.Kind {
		case "codemodel":
			if err := readJSON(path, &r.Codemodel); err != nil {
				return fmt.Errorf("fileapi: codemodel: %w", err)
			}
		case "toolchains":
			if err := readJSON(path, &r.Toolchains); err != nil {
				return fmt.Errorf("fileapi: toolchains: %w", err)
			}
		case "cmakeFiles":
			if err := readJSON(path, &r.CMakeFiles); err != nil {
				return fmt.Errorf("fileapi: cmakeFiles: %w", err)
			}
		case "cache":
			if err := readJSON(path, &r.Cache); err != nil {
				return fmt.Errorf("fileapi: cache: %w", err)
			}
		case "configureLog":
			var cl ConfigureLog
			if err := readJSON(path, &cl); err != nil {
				return fmt.Errorf("fileapi: configureLog: %w", err)
			}
			r.ConfigureLog = &cl
		}
	}
	return nil
}

// loadReplyConfigurations reads the per-configuration target + directory
// objects into r. Targets[] mirrors the primary (first-declared) configuration;
// for multi-config replies TargetsByConfig carries each config's targets too.
func loadReplyConfigurations(r *Reply, replyDir string) error {
	// Determine the primary configuration name (first declared in
	// Codemodel.Configurations). For single-config replies there's
	// exactly one. Empty when the codemodel carries no configurations
	// (degenerate but theoretically possible — leave Targets empty).
	primaryConfig := ""
	if len(r.Codemodel.Configurations) > 0 {
		primaryConfig = r.Codemodel.Configurations[0].Name
	}
	multiConfig := len(r.Codemodel.Configurations) > 1
	if multiConfig {
		r.TargetsByConfig = map[string]map[string]Target{}
	}
	for _, cfg := range r.Codemodel.Configurations {
		for _, tref := range cfg.Targets {
			path := filepath.Join(replyDir, tref.JSONFile)
			var t Target
			if err := readJSON(path, &t); err != nil {
				return fmt.Errorf("fileapi: target %s: %w", tref.Name, err)
			}
			// Targets[] mirrors the primary configuration. Without
			// this gate, multi-config replies would overwrite the
			// map with whichever config iterated last — non-
			// deterministic from a caller's perspective.
			if cfg.Name == primaryConfig {
				r.Targets[tref.Id] = t
			}
			if multiConfig {
				if _, ok := r.TargetsByConfig[tref.Id]; !ok {
					r.TargetsByConfig[tref.Id] = map[string]Target{}
				}
				r.TargetsByConfig[tref.Id][cfg.Name] = t
			}
		}
		for _, d := range cfg.Directories {
			if d.JSONFile == "" {
				continue
			}
			if _, seen := r.Directories[d.JSONFile]; seen {
				continue
			}
			path := filepath.Join(replyDir, d.JSONFile)
			var dir Directory
			if err := readJSON(path, &dir); err != nil {
				return fmt.Errorf("fileapi: directory %s: %w", d.JSONFile, err)
			}
			r.Directories[d.JSONFile] = dir
		}
	}
	return nil
}

// loadIndex finds the lexicographically-greatest index-*.json (per File API
// docs: "the most recent one") and parses it.
func loadIndex(replyDir string) (Index, error) {
	matches, err := filepath.Glob(filepath.Join(replyDir, "index-*.json"))
	if err != nil {
		return Index{}, err
	}
	if len(matches) == 0 {
		return Index{}, fmt.Errorf("no index-*.json under %s", replyDir)
	}
	sort.Strings(matches)
	var idx Index
	if err := readJSON(matches[len(matches)-1], &idx); err != nil {
		return Index{}, err
	}
	return idx, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
