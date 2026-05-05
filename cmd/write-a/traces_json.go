package main

// tools/traces.json emission.
//
// Mirror of sources_json.go: one record per kind:autotools element
// in the graph, naming the element's content-narrowed srckey. The
// traces module extension (rules/traces.bzl) reads this file at
// load time to declare one @trace_<key>//:trace repo per entry.
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

// collectTraces walks the graph, computes per-element srckeys for
// each kind:autotools element, and returns one entry per element
// sorted by key (deterministic rendering). Computed-on-demand
// rather than stored on element so the autotools-specific patterns
// stay scoped to the autotools handler.
func collectTraces(g *graph) (tracesJSON, error) {
	patterns := autotoolsSrckeyPatterns()
	var entries []traceEntry
	for _, elem := range g.Elements {
		if elem.Bst.Kind != "autotools" {
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

func marshalTracesJSON(s tracesJSON) ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// renderTracesUseExtension emits the use_extension + use_repo
// block for the traces module extension. Only emitted when the
// graph contains at least one kind:autotools element AND the
// trace-driven path is enabled (autotoolsConfig.convertBin set);
// otherwise project A / B's MODULE.bazel doesn't reference the
// extension at all.
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
