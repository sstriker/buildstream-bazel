// Package fileapi parses CMake's File API v1 reply directory.
//
// The reply directory is produced by cmake when run with files staged under
// <build>/.cmake/api/v1/query/. We consume four object kinds:
//
//   - codemodel-v2: project/target structure, sources, compile/link flags.
//   - toolchains-v1: per-language compiler identification and implicit dirs.
//   - cmakeFiles-v1: every CMakeLists / .cmake file consumed at configure.
//   - cache-v2: post-configure cache entries.
//
// All parsing is pure; no I/O outside the supplied directory.
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
	// Targets maps target id to its parsed details. Populated lazily by
	// Load, keyed by the id from Codemodel.Configurations[].Targets[].Id.
	Targets map[string]Target
	// Directories carries the parsed directory-*.json content for every
	// ConfigDirectory.JSONFile referenced from Codemodel. Indexed by
	// JSONFile basename (the codemodel's per-config Directories[]
	// entries reference them this way). Empty when no directories carry
	// install rules.
	Directories map[string]Directory
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

	for _, obj := range idx.Objects {
		path := filepath.Join(replyDir, obj.JSONFile)
		switch obj.Kind {
		case "codemodel":
			if err := readJSON(path, &r.Codemodel); err != nil {
				return nil, fmt.Errorf("fileapi: codemodel: %w", err)
			}
		case "toolchains":
			if err := readJSON(path, &r.Toolchains); err != nil {
				return nil, fmt.Errorf("fileapi: toolchains: %w", err)
			}
		case "cmakeFiles":
			if err := readJSON(path, &r.CMakeFiles); err != nil {
				return nil, fmt.Errorf("fileapi: cmakeFiles: %w", err)
			}
		case "cache":
			if err := readJSON(path, &r.Cache); err != nil {
				return nil, fmt.Errorf("fileapi: cache: %w", err)
			}
			r.Cache.buildIndex()
		}
	}

	for _, cfg := range r.Codemodel.Configurations {
		for _, tref := range cfg.Targets {
			path := filepath.Join(replyDir, tref.JSONFile)
			var t Target
			if err := readJSON(path, &t); err != nil {
				return nil, fmt.Errorf("fileapi: target %s: %w", tref.Name, err)
			}
			r.Targets[tref.Id] = t
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
				return nil, fmt.Errorf("fileapi: directory %s: %w", d.JSONFile, err)
			}
			r.Directories[d.JSONFile] = dir
		}
	}

	return r, nil
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
