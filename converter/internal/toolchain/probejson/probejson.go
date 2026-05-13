// Package probejson defines the on-disk format for one probe cell.
//
// A probe cell is one (variant, platform) pair: cmake configure runs
// against the probe project for that variant under that platform's
// exec environment, producing a fileapi.Reply. probejson serializes
// that pair into a JSON document the cross-host unifier (Stage 5)
// reads back to fold into a ResolvedToolchain.
//
// The format is symmetric: Marshal(variant, reply) writes a
// ProbeJSON; Unmarshal reads one back. Path inside fileapi.Reply is
// dropped on serialization — it points at the cmake build dir on
// the cell's executor, which has no meaning to the unifier.
//
// SchemaVersion lets us evolve the format. Loaders reject documents
// whose version they don't recognize so a stale unifier doesn't
// silently misread a newer probe-cell's output.
package probejson

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

// SchemaVersion is the on-disk format version. Bump when the
// embedded fileapi.Reply / Variant shape changes incompatibly.
const SchemaVersion = 1

// ProbeJSON is the top-level document. SchemaVersion validation is
// done explicitly by Unmarshal so callers don't need to inspect it.
type ProbeJSON struct {
	SchemaVersion int               `json:"schemaVersion"`
	Variant       toolchain.Variant `json:"variant"`
	Reply         ReplyJSON         `json:"reply"`
}

// ReplyJSON mirrors fileapi.Reply minus Path (which is meaningless
// across hosts). Each field is the same JSON-tagged struct fileapi
// already exposes, so Marshal/Unmarshal round-trips through
// encoding/json without custom logic.
type ReplyJSON struct {
	Index       fileapi.Index                `json:"index"`
	Codemodel   fileapi.Codemodel            `json:"codemodel"`
	Toolchains  fileapi.Toolchains           `json:"toolchains"`
	CMakeFiles  fileapi.CMakeFiles           `json:"cmakeFiles"`
	Cache       fileapi.Cache                `json:"cache"`
	Targets     map[string]fileapi.Target    `json:"targets,omitempty"`
	Directories map[string]fileapi.Directory `json:"directories,omitempty"`
}

// Marshal serializes one (variant, reply) pair to a pretty-printed
// JSON document. Pretty printing is chosen for human-readable
// per-cell artifacts during debugging — the unifier doesn't care
// about the formatting; reviewers do.
//
// Volatile cache entries are scrubbed before serialization: cmake's
// File API surfaces the absolute build-dir path in
// CMAKE_BINARY_DIR / *_BINARY_DIR / CMAKE_FIND_PACKAGE_REDIRECTS_DIR
// / etc., and that path varies across runs even for byte-identical
// inputs (Bazel sandbox roots, tmp dir suffixes). Letting them
// reach probe.json would make per-cell artifacts churn across
// runs, breaking remote-cache reuse and adding noise to the
// unifier's Observe partition. The filter mirrors cmakerun's
// volatile-vars list applied to the dump-vars feature; see
// cmakerun.filterVolatilePaths for the rationale.
func Marshal(variant toolchain.Variant, reply *fileapi.Reply) ([]byte, error) {
	if reply == nil {
		return nil, fmt.Errorf("probejson: nil reply")
	}
	cache := reply.Cache
	cache.Entries = filterVolatileCacheEntries(cache.Entries)
	p := ProbeJSON{
		SchemaVersion: SchemaVersion,
		Variant:       variant,
		Reply: ReplyJSON{
			Index:       reply.Index,
			Codemodel:   scrubCodemodelPaths(reply.Codemodel),
			Toolchains:  reply.Toolchains,
			CMakeFiles:  scrubCMakeFilesPaths(reply.CMakeFiles),
			Cache:       cache,
			Targets:     scrubTargetPaths(reply.Targets),
			Directories: scrubDirectoryPaths(reply.Directories),
		},
	}
	return json.MarshalIndent(p, "", "  ")
}

// scrubCodemodelPaths returns a value-copy of c with every
// absolute build/source path cleared. cmake's File API records
// these as the recording machine's paths (e.g.
// /tmp/sandbox-abc/source); leaving them in place makes
// probe.json host-specific even after volatile cache entries
// are filtered, which defeats the cross-host fold the unifier
// performs. Configurations[].Directories[] are deep-copied so
// the caller's Reply isn't mutated.
func scrubCodemodelPaths(c fileapi.Codemodel) fileapi.Codemodel {
	c.Paths.Source = ""
	c.Paths.Build = ""
	if len(c.Configurations) > 0 {
		cfgs := make([]fileapi.Configuration, len(c.Configurations))
		for i, cfg := range c.Configurations {
			if len(cfg.Directories) > 0 {
				dirs := make([]fileapi.ConfigDirectory, len(cfg.Directories))
				for j, d := range cfg.Directories {
					d.Source = ""
					d.Build = ""
					dirs[j] = d
				}
				cfg.Directories = dirs
			}
			cfgs[i] = cfg
		}
		c.Configurations = cfgs
	}
	return c
}

// scrubCMakeFilesPaths clears the absolute source/build root
// fields from a CMakeFiles object. Returns a value-copy so the
// caller's Reply isn't mutated.
func scrubCMakeFilesPaths(f fileapi.CMakeFiles) fileapi.CMakeFiles {
	f.Paths.Source = ""
	f.Paths.Build = ""
	return f
}

// scrubTargetPaths clears Target.Paths in every entry of the map.
// Returns a fresh map so the caller's Targets isn't mutated.
func scrubTargetPaths(in map[string]fileapi.Target) map[string]fileapi.Target {
	if in == nil {
		return nil
	}
	out := make(map[string]fileapi.Target, len(in))
	for k, t := range in {
		t.Paths.Source = ""
		t.Paths.Build = ""
		out[k] = t
	}
	return out
}

// scrubDirectoryPaths clears Directory.Paths in every entry of
// the map. Returns a fresh map so the caller's Directories isn't
// mutated.
func scrubDirectoryPaths(in map[string]fileapi.Directory) map[string]fileapi.Directory {
	if in == nil {
		return nil
	}
	out := make(map[string]fileapi.Directory, len(in))
	for k, d := range in {
		d.Paths.Source = ""
		d.Paths.Build = ""
		out[k] = d
	}
	return out
}

// filterVolatileCacheEntries drops fileapi cache entries whose
// values would naturally vary per run. Two filters compose:
//
//  1. Name-based: anything ending in `_BINARY_DIR` / `_SOURCE_DIR`
//     plus a small list of cmake-internal path-bearing names
//     (CMAKE_HOME_DIRECTORY, CMAKE_FIND_PACKAGE_REDIRECTS_DIR, the
//     host-cmake-binary pointers, etc.).
//
//  2. Value-based: any entry whose value contains a build-dir path
//     as substring. The build-dir prefixes are extracted from
//     CMAKE_BINARY_DIR + every *_BINARY_DIR entry, so derived path
//     vars (e.g. project paths inside the build tree, log file
//     paths) drop too without a hand-maintained allowlist.
//
// Both filters mirror cmakerun.filterVolatilePaths intent. We don't
// share code because this operates on []fileapi.CacheEntry while
// cmakerun's helper operates on map[string]string; the duplication
// is small enough to be cheaper than an extracted helper package.
func filterVolatileCacheEntries(in []fileapi.CacheEntry) []fileapi.CacheEntry {
	if len(in) == 0 {
		return in
	}
	var prefixes []string
	seen := map[string]bool{}
	for _, e := range in {
		if e.Value == "" {
			continue
		}
		if e.Name == "CMAKE_BINARY_DIR" || strings.HasSuffix(e.Name, "_BINARY_DIR") {
			if !seen[e.Value] {
				prefixes = append(prefixes, e.Value)
				seen[e.Value] = true
			}
		}
	}
	out := make([]fileapi.CacheEntry, 0, len(in))
	for _, e := range in {
		if isVolatileCacheVarName(e.Name) {
			continue
		}
		if containsAnyPrefix(e.Value, prefixes) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func isVolatileCacheVarName(name string) bool {
	if strings.HasSuffix(name, "_BINARY_DIR") || strings.HasSuffix(name, "_SOURCE_DIR") {
		return true
	}
	switch name {
	case "CMAKE_HOME_DIRECTORY",
		"CMAKE_CACHEFILE_DIR",
		"CMAKE_FILES_DIRECTORY",
		"CMAKE_FIND_PACKAGE_REDIRECTS_DIR",
		"CMAKE_CURRENT_FUNCTION_LIST_DIR",
		"CMAKE_CURRENT_FUNCTION_LIST_FILE",
		"CMAKE_CURRENT_LIST_DIR",
		"CMAKE_CURRENT_LIST_FILE",
		"CMAKE_BUILD_TOOL",
		"CMAKE_COMMAND",
		"CMAKE_CTEST_COMMAND",
		"CMAKE_CPACK_COMMAND",
		"CMAKE_ROOT":
		return true
	}
	return false
}

func containsAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// Unmarshal parses a ProbeJSON document. Returns the Variant + a
// *fileapi.Reply with Path unset. SchemaVersion mismatches are a
// hard error so a stale tool can't silently misread a newer probe
// artifact.
func Unmarshal(data []byte) (toolchain.Variant, *fileapi.Reply, error) {
	var p ProbeJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return toolchain.Variant{}, nil, fmt.Errorf("probejson: parse: %w", err)
	}
	if p.SchemaVersion != SchemaVersion {
		return toolchain.Variant{}, nil, fmt.Errorf("probejson: schemaVersion %d not supported (this build expects %d)", p.SchemaVersion, SchemaVersion)
	}
	return p.Variant, &fileapi.Reply{
		Index:       p.Reply.Index,
		Codemodel:   p.Reply.Codemodel,
		Toolchains:  p.Reply.Toolchains,
		CMakeFiles:  p.Reply.CMakeFiles,
		Cache:       p.Reply.Cache,
		Targets:     p.Reply.Targets,
		Directories: p.Reply.Directories,
	}, nil
}
