package shadow

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// TraceEvent is one record from `cmake --trace-expand --trace-format=json-v1`.
// We deliberately decode only the fields we read; cmake adds more.
//
// IMMUTABLE: ParseTrace memoizes its result and hands the SAME []TraceEvent
// (and thus the same per-event Args slices) to every caller, so a TraceEvent
// must be treated as read-only. Never assign through a field of a parsed event
// (`ev.Args[i] = …`) or sort/append-in-place a field slice — a mutation
// through the shared slice silently corrupts every later consumer (e.g. a
// warm-pass re-lower reading the cached events after pass 1). Derive new
// values instead.
type TraceEvent struct {
	File string   `json:"file"`
	Line int      `json:"line"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
	// Frame is cmake's call-stack depth for this event (1 = a command
	// written at a CMakeLists.txt's top level; deeper inside function/macro
	// bodies). Used to recover the declaring directory scope of a command
	// that physically executes in an include()d/function-wrapper module
	// (e.g. abseil's add_library inside the absl_cc_library function).
	Frame int `json:"frame"`
	// Defer is cmake's deferred-call id ("__0", "__1", …) when this event
	// is the EXECUTION of a call scheduled via cmake_language(DEFER CALL …);
	// empty for ordinary events (cmake emits `"defer": null`). A deferred
	// event keeps the REGISTRATION site's file/line, so commands that
	// resolve relative paths against the executing directory scope (a
	// relative configure_file output) need the registration's DEFER
	// DIRECTORY to anchor correctly — see deferDirectoryIndex.
	Defer string `json:"defer"`
}

// ParseTrace walks the cmake --trace-format=json-v1 stream once and
// returns every parseable event. Lines that aren't JSON-shaped or that
// fail to unmarshal are silently dropped (cmake prefixes its trace
// output with a banner line; that gets dropped here).
//
// Single-pass entry for callers that want to feed multiple extractors
// off one trace without paying the bytes.Split + json.Unmarshal cost
// per extractor.
//
// Memoized: one convert parses the SAME trace blob many times — Decode and
// the platform-conditional Tier-2 extractor both run inside parseTraceFacts,
// the driver Decodes it again for a couple of single fields, and every warm
// second-pass re-lower repeats all of that. Profiling a mid/large convert
// (abseil) put ~42% of the Go translation time in encoding/json re-parsing
// the same bytes. A tiny fingerprint-keyed cache turns the repeats into map
// hits. The returned slice is SHARED across callers — they all treat it
// read-only (verified: every extractor only indexes/compares event fields),
// so no defensive copy is made.
func ParseTrace(traceRaw []byte) []TraceEvent {
	if len(traceRaw) == 0 {
		return nil
	}
	key := traceFingerprint(traceRaw)
	if ev, ok := traceMemoGet(key); ok {
		return ev
	}
	events := parseTraceUncached(traceRaw)
	traceMemoPut(key, events)
	return events
}

func parseTraceUncached(traceRaw []byte) []TraceEvent {
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

// traceKey fingerprints a trace blob for the memo. The length plus a 64-bit
// FNV-1a over a bounded sample (the whole blob when small, else head+middle+
// tail windows) is O(1) per call yet collision-safe in practice for the
// handful of distinct traces one convert sees — two DIFFERENT traces would
// need an identical length AND identical sampled bytes.
type traceKey struct {
	n int
	h uint64
}

func traceFingerprint(b []byte) traceKey {
	h := fnv.New64a()
	const win = 4096
	if len(b) <= 3*win {
		h.Write(b)
	} else {
		h.Write(b[:win])
		mid := len(b) / 2
		h.Write(b[mid : mid+win])
		h.Write(b[len(b)-win:])
	}
	return traceKey{n: len(b), h: h.Sum64()}
}

// Bounded memo (most-recent-last LRU). Capacity 2 covers the within-convert
// trace set — the primary expanded trace (parsed repeatedly in one pass-1
// burst) coexisting with one warm-pass / nested trace — while keeping at most
// two parsed-event slices alive (each ~the trace's size), so the cache never
// balloons memory on a huge-trace project (LLVM/VTK).
const traceMemoCap = 2

type traceMemoEntry struct {
	key    traceKey
	events []TraceEvent
}

var (
	traceMemoMu sync.Mutex
	traceMemo   []traceMemoEntry
)

func traceMemoGet(key traceKey) ([]TraceEvent, bool) {
	traceMemoMu.Lock()
	defer traceMemoMu.Unlock()
	for i := range traceMemo {
		if traceMemo[i].key == key {
			e := traceMemo[i]
			// Move-to-end (most-recently-used).
			traceMemo = append(traceMemo[:i], traceMemo[i+1:]...)
			traceMemo = append(traceMemo, e)
			return e.events, true
		}
	}
	return nil, false
}

func traceMemoPut(key traceKey, events []TraceEvent) {
	traceMemoMu.Lock()
	defer traceMemoMu.Unlock()
	for i := range traceMemo {
		if traceMemo[i].key == key {
			return // already cached (lost a race; harmless)
		}
	}
	traceMemo = append(traceMemo, traceMemoEntry{key: key, events: events})
	if len(traceMemo) > traceMemoCap {
		traceMemo = traceMemo[len(traceMemo)-traceMemoCap:]
	}
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
	out := sliceutil.SortedKeys(seen)
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

// ReadsBuildType reports whether the project's OWN configure logic consults
// CMAKE_BUILD_TYPE — an event whose file sits inside sourceRoot carrying the
// variable name in its args (`if(CMAKE_BUILD_TYPE STREQUAL "Debug")`,
// `set(CMAKE_BUILD_TYPE ...)`; if()/STREQUAL keep bare variable NAMES even
// under --trace-expand). Gates the per-config configure_file bake passes:
// a project that never reads CMAKE_BUILD_TYPE can't derive configure_file
// content from it, so the extra single-config reconfigures are skipped.
// cmake's own modules read the variable constantly — the in-source-tree
// filter keeps them from triggering the passes on every project.
func ReadsBuildType(traceRaw []byte, sourceRoot string) bool {
	for _, ev := range ParseTrace(traceRaw) {
		if !inSourceTree(ev.File, sourceRoot) {
			continue
		}
		for _, a := range ev.Args {
			if strings.Contains(a, "CMAKE_BUILD_TYPE") {
				return true
			}
		}
	}
	return false
}
