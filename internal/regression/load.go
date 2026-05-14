// Package regression reads the orchestrator's per-run output directory
// and computes typed diffs across two runs.
//
// Inputs are the JSON files M3a produces under <out>/manifest/:
//
//	converted.json    — the list of successfully-converted elements
//	failures.json     — Tier-1 entries per failed element
//	determinism.json  — path -> sha256 over <out>/elements/
//
// LoadRun reads all three (tolerating a missing failures.json or
// determinism.json — first-run shape), classifies elements as
// converted-with-fingerprint or failed-with-code, and exposes a flat
// `Outcome` per element that the diff layer compares.
package regression

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Run is one orchestrator output directory's regression-relevant state.
type Run struct {
	Path     string             // path to <out>/
	Outcomes map[string]Outcome // element name -> outcome
}

// Outcome captures everything the diff layer needs to know about one
// element from one run. Exactly one of Converted or Failure is populated.
type Outcome struct {
	Element string `json:"element"`

	// Converted is non-nil for successfully-converted elements.
	Converted *ConvertedOutcome `json:"converted,omitempty"`

	// Failure is non-nil for elements that exited Tier-1.
	Failure *FailureOutcome `json:"failure,omitempty"`
}

// ConvertedOutcome carries the per-element fingerprint computed from
// determinism.json: a sha256 of the per-element file hashes (sorted).
// Two runs with byte-identical outputs produce the same fingerprint.
type ConvertedOutcome struct {
	// Fingerprint is a hex sha256 covering every output file under
	// <out>/elements/<elem-name>/, derived from determinism.json. Used
	// by the diff layer to detect drift even when both runs marked the
	// element converted.
	Fingerprint string `json:"fingerprint"`

	// Files maps relative-to-elements path -> sha256 hex (the verbatim
	// determinism.json subset for this element). Useful for deeper
	// diffs ("which file under the element actually moved?").
	Files map[string]string `json:"files,omitempty"`
}

// FailureOutcome mirrors converter Tier-1 failure.json shape with the
// element name stamped in.
type FailureOutcome struct {
	Tier    int    `json:"tier"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// LoadRun reads the manifest files under <path>/manifest/ and merges
// them into a Run. Missing optional files (failures.json,
// determinism.json) are tolerated — they appear as empty lists rather
// than errors.
//
// converted.json is required; without it we don't know which elements
// the orchestrator processed.
func LoadRun(path string) (*Run, error) {
	r := &Run{
		Path:     path,
		Outcomes: map[string]Outcome{},
	}

	conv, err := loadConverted(filepath.Join(path, "manifest", "converted.json"))
	if err != nil {
		return nil, err
	}

	fails, err := loadFailures(filepath.Join(path, "manifest", "failures.json"))
	if err != nil {
		return nil, err
	}

	det, err := loadDeterminism(filepath.Join(path, "manifest", "determinism.json"))
	if err != nil {
		return nil, err
	}

	for _, name := range conv {
		fp, files := elementFingerprint(name, det)
		r.Outcomes[name] = Outcome{
			Element: name,
			Converted: &ConvertedOutcome{
				Fingerprint: fp,
				Files:       files,
			},
		}
	}
	for _, f := range fails {
		// A failure entry takes precedence over a converted entry; in
		// practice both shouldn't appear (orchestrator either lands the
		// element in Converted or Failed) but be defensive.
		r.Outcomes[f.Element] = Outcome{
			Element: f.Element,
			Failure: &FailureOutcome{
				Tier:    f.Tier,
				Code:    f.Code,
				Message: f.Message,
			},
		}
	}
	return r, nil
}

// Names returns element names from r.Outcomes in sorted order. Useful
// for stable iteration in tests and reports.
func (r *Run) Names() []string {
	out := make([]string, 0, len(r.Outcomes))
	for n := range r.Outcomes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ----- on-disk schemas (mirror what orchestrator writes) ---------------

type convertedDoc struct {
	Version  int `json:"version"`
	Elements []struct {
		Name string `json:"name"`
	} `json:"elements"`
}

type failuresDoc struct {
	Version  int `json:"version"`
	Elements []struct {
		Element string `json:"element"`
		Tier    int    `json:"tier"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"elements"`
}

type determinismDoc struct {
	Version int `json:"version"`
	Files   []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func loadConverted(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("regression: load converted.json: %w", err)
	}
	var doc convertedDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("regression: parse converted.json: %w", err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("regression: %s: unsupported version %d", path, doc.Version)
	}
	out := make([]string, 0, len(doc.Elements))
	for _, e := range doc.Elements {
		out = append(out, e.Name)
	}
	return out, nil
}

func loadFailures(path string) ([]struct {
	Element string
	Tier    int
	Code    string
	Message string
}, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("regression: load failures.json: %w", err)
	}
	var doc failuresDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("regression: parse failures.json: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return nil, fmt.Errorf("regression: %s: unsupported version %d", path, doc.Version)
	}
	out := make([]struct {
		Element string
		Tier    int
		Code    string
		Message string
	}, 0, len(doc.Elements))
	for _, e := range doc.Elements {
		out = append(out, struct {
			Element string
			Tier    int
			Code    string
			Message string
		}{e.Element, e.Tier, e.Code, e.Message})
	}
	return out, nil
}

func loadDeterminism(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("regression: load determinism.json: %w", err)
	}
	var doc determinismDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("regression: parse determinism.json: %w", err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("regression: %s: unsupported version %d", path, doc.Version)
	}
	out := make(map[string]string, len(doc.Files))
	for _, f := range doc.Files {
		out[f.Path] = f.SHA256
	}
	return out, nil
}

// elementFingerprint extracts the per-element subset of determinism.json
// (every file under <element-name>/) and computes a sha256 over the
// sorted "<rel>\t<filehash>\n" lines. Returns ("", nil) if the element
// has no recorded files (e.g. determinism.json wasn't written).
func elementFingerprint(name string, det map[string]string) (string, map[string]string) {
	if len(det) == 0 {
		return "", nil
	}
	prefix := name + "/"
	files := map[string]string{}
	for p, h := range det {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		files[strings.TrimPrefix(p, prefix)] = h
	}
	if len(files) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := newSHA256()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\t%s\n", k, files[k])
	}
	return h.HexSum(), files
}
