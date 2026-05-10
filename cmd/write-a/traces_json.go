package main

// tools/traces.json emission.
//
// Mirror of sources_json.go: one record per element whose kind
// participates in the trace-driven round-2 path, naming the
// element's content-narrowed srckey. The set is kind-agnostic —
// kind:autotools (special-cased), any pipeline kind whose
// handler sets traceDrivenSrckeyPatterns, and kind:cmake when
// --cmake-round2-fallback is enabled. The pipeline-kind opt-ins
// (as of this writing: kind:make / makemaker / modulebuild /
// manual / script) grow whenever a new handler sets the field;
// the source of truth lives on the per-kind handler rather than
// in this comment, so don't take the list as canonical. The
// traces module extension (rules/traces.bzl) reads this file
// at load time to declare one @trace_<key>//:trace repo per
// entry.
//
// The "key" field is the element name; Bazel's external-repo
// namespace requires a static identifier. The "srckey" field is
// the hex hash from srckey.txt that trace-lookup feeds into
// SyntheticActionDigest.

import (
	"encoding/json"
	"fmt"
	"sort"
)

type traceEntry struct {
	Key    string `json:"key"`
	Srckey string `json:"srckey"`
}

type tracesJSON struct {
	Traces []traceEntry `json:"traces"`
}

// collectTraces walks the graph, computes per-element srckeys
// for each element whose handler opts into the trace-driven
// round-2 path, and returns one entry per element sorted by key
// (deterministic rendering). Two opt-in shapes contribute:
//
//   - kind:autotools (handler_autotools_native.go, autotoolsHandler
//     wrapping pipelineHandler). Patterns: autotoolsSrckeyPatterns.
//   - Any pipelineHandler-shaped kind that sets
//     traceDrivenSrckeyPatterns on its registered handler
//     (handler_make.go's makeSrckeyPatterns, plus any future kind
//     that joins the trace-driven path the same way).
//
// Computed-on-demand rather than stored on element so the
// per-kind patterns stay scoped to the kind's handler.
func collectTraces(g *graph) (tracesJSON, error) {
	var entries []traceEntry
	for _, elem := range g.Elements {
		patterns := traceDrivenSrckeyPatternsForKind(elem.Bst.Kind)
		if patterns == nil {
			continue
		}
		hash, _, err := computeSrckey(elem, patterns)
		if err != nil {
			return tracesJSON{}, fmt.Errorf("element %q: compute srckey: %w", elem.Name, err)
		}
		entries = append(entries, traceEntry{Key: elem.Name, Srckey: hash})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return tracesJSON{Traces: entries}, nil
}

// traceDrivenSrckeyPatternsForKind returns the per-kind srckey
// pattern set when the kind is opted into trace-driven round-2,
// or nil otherwise. Source of truth for "is this kind in the
// trace-driven set" — used by collectTraces to populate
// tools/traces.json for project A's _trace_repo extension.
//
// kind:autotools is special-cased here because its dispatch lives
// in autotoolsHandler (handler_autotools_native.go) rather than
// going through pipelineHandler's traceDrivenSrckeyPatterns
// field. The autotools + pipeline arms only return non-nil when
// traceConfig.round2Enabled is set (with convertBin staged):
// without round-2 active, those kinds don't reference
// @trace_<elem>//:trace in their rendered BUILDs, so adding
// per-element trace_repo entries to tools/traces.json would
// instantiate Bazel repo rules at load time that nothing
// consumes. kind:cmake is special-cased too: it's not a pipeline
// handler, but joins the trace-driven path via the round-2
// fallback (Phase B) when cmakeConfig.round2FallbackEnabled is
// set, independent of autotools round-2.
func traceDrivenSrckeyPatternsForKind(kind string) *readPathsPatterns {
	autotoolsRound2 := traceConfig.convertBin != "" && traceConfig.round2Enabled
	if kind == "autotools" {
		if !autotoolsRound2 {
			return nil
		}
		return autotoolsSrckeyPatterns()
	}
	if kind == "cmake" && cmakeConfig.round2FallbackEnabled {
		return cmakeSrckeyPatterns()
	}
	h, ok := handlers[kind]
	if !ok {
		return nil
	}
	ph, ok := h.(pipelineHandler)
	if !ok {
		return nil
	}
	if !autotoolsRound2 {
		// Pipeline-kind dispatch through pipelineHandler.
		// shouldUseRound2() also requires this gate; without
		// it, traces.json populates entries for elements
		// whose rendered BUILD won't reference @trace_*.
		return nil
	}
	return ph.traceDrivenSrckeyPatterns
}

func marshalTracesJSON(s tracesJSON) ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// renderTracesUseExtension emits the use_extension + use_repo
// block for the traces module extension. Only emitted when the
// graph contains at least one element opted into the trace-
// driven path — that's any kind whose
// traceDrivenSrckeyPatternsForKind returns non-nil (kind:autotools
// when the trace-driven autotools converter is staged, pipeline
// kinds with traceDrivenSrckeyPatterns set on their handler, or
// kind:cmake when --cmake-round2-fallback is enabled). Otherwise
// project A / B's MODULE.bazel doesn't reference the extension
// at all.
func renderTracesUseExtension(t tracesJSON) string {
	if len(t.Traces) == 0 {
		return ""
	}
	var out string
	out += `
traces = use_extension("//rules:traces.bzl", "traces")
traces.from_json(path = "//tools:traces.json")
use_repo(
    traces,
`
	for _, e := range t.Traces {
		out += fmt.Sprintf("    %q,\n", "trace_"+e.Key)
	}
	out += ")\n"
	return out
}
