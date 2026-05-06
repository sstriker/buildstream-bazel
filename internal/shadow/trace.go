package shadow

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// TraceEvent is one record from `cmake --trace-expand --trace-format=json-v1`.
// We deliberately decode only the fields we read; cmake adds more.
type TraceEvent struct {
	File string   `json:"file"`
	Line int      `json:"line"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

// ParseTrace walks the cmake --trace-format=json-v1 stream once and
// returns every parseable event. Lines that aren't JSON-shaped or that
// fail to unmarshal are silently dropped (cmake prefixes its trace
// output with a banner line; that gets dropped here).
//
// Single-pass entry for callers that want to feed multiple extractors
// off one trace without paying the bytes.Split + json.Unmarshal cost
// per extractor.
func ParseTrace(traceRaw []byte) []TraceEvent {
	// Pre-size to roughly the line count to avoid repeated growth.
	lines := bytes.Count(traceRaw, []byte{'\n'}) + 1
	out := make([]TraceEvent, 0, lines)
	for _, line := range bytes.Split(traceRaw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev TraceEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// ExtractReadPaths returns every source-tree path that the trace shows being
// read (file content actually consumed, not merely referenced). Paths are
// returned package-relative (slash form) and deduplicated.
//
// Recognized read-causing commands:
//   - include(<file>)            : args[0]
//   - configure_file(<in> <out>) : args[0]
//   - file(READ|STRINGS|MD5|SHA*) : args[1]
//
// Anything outside sourceRoot, generated, or unresolvable is silently dropped
// (cmake's own bundled modules under /usr/share/cmake-* show up here and are
// not source-tree files).
func ExtractReadPaths(traceRaw []byte, sourceRoot string) []string {
	return extractReadPaths(ParseTrace(traceRaw), sourceRoot)
}

func extractReadPaths(events []TraceEvent, sourceRoot string) []string {
	seen := map[string]struct{}{}
	for _, ev := range events {
		collectReadPath(ev, sourceRoot, seen)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectReadPath classifies a single trace event for ExtractReadPaths
// and inserts the package-relative slash-style path into seen if it
// resolves inside sourceRoot. Shared between the legacy single-pass
// API and Decode's combined pass.
func collectReadPath(ev TraceEvent, sourceRoot string, seen map[string]struct{}) {
	path := readPathFor(ev)
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(ev.File), path)
	}
	rel, err := filepath.Rel(sourceRoot, path)
	if err != nil {
		return
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	seen[filepath.ToSlash(rel)] = struct{}{}
}

func readPathFor(ev TraceEvent) string {
	switch strings.ToLower(ev.Cmd) {
	case "include":
		if len(ev.Args) > 0 {
			return ev.Args[0]
		}
	case "configure_file":
		if len(ev.Args) > 0 {
			return ev.Args[0]
		}
	case "file":
		if len(ev.Args) >= 2 {
			switch strings.ToUpper(ev.Args[0]) {
			case "READ", "STRINGS", "MD5", "SHA1", "SHA224", "SHA256", "SHA384", "SHA512":
				return ev.Args[1]
			}
		}
	}
	return ""
}
