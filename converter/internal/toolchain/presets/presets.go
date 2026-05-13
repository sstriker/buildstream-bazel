// Package presets parses CMakePresets.json (and CMakeUserPresets.json)
// and lifts each `configurePresets` entry into a toolchain.Variant.
//
// Why presets are a Variant source: real-world cmake projects already
// declare their build matrix here — Debug/Release/RelWithDebInfo
// presets, sanitizer presets, alt-compiler presets. Reading them
// directly means operators don't have to re-declare the matrix in
// our discovery layer; the project's own CMakePresets.json is the
// source of truth.
//
// Scope: only `configurePresets` is consumed (the variant-defining
// presets). buildPresets / testPresets / packagePresets are
// orthogonal to the configure-time probe matrix and ignored.
//
// Inherit chains are resolved: child presets layer their
// cacheVariables on top of the parent's (and grandparent's, and so
// on); on key collisions, the child wins. Cycle detection raises a
// hard error rather than recursing forever.
package presets

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

// LoadFile reads a CMakePresets.json (or CMakeUserPresets.json) and
// returns the configurePresets as []toolchain.Variant. Missing file
// → (nil, nil) so callers can union LoadFile("CMakePresets.json")
// + LoadFile("CMakeUserPresets.json") without special-casing
// absence.
//
// Order: configurePresets are returned in the order they appear in
// the JSON. Hidden presets (`"hidden": true`) are skipped — by
// convention hidden presets are abstract bases for inherits chains
// and aren't meant to be configured directly.
func LoadFile(path string) ([]toolchain.Variant, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("presets: read %s: %w", path, err)
	}
	return Parse(body)
}

// Parse decodes raw JSON into []Variant. Exposed so callers with
// in-memory presets (test fixtures, derived data) can skip the
// filesystem.
func Parse(body []byte) ([]toolchain.Variant, error) {
	var doc presetsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("presets: parse: %w", err)
	}
	return materialize(doc.ConfigurePresets)
}

// materialize resolves inherits chains and turns each (non-hidden)
// preset into a Variant. Each preset's effective cacheVariables map
// is the union of every ancestor's cacheVariables, child winning
// on key collisions.
func materialize(presets []configurePreset) ([]toolchain.Variant, error) {
	byName := make(map[string]*configurePreset, len(presets))
	for i := range presets {
		p := &presets[i]
		if p.Name == "" {
			return nil, fmt.Errorf("presets: configurePreset at index %d has empty name", i)
		}
		if _, dup := byName[p.Name]; dup {
			return nil, fmt.Errorf("presets: duplicate configurePreset name %q", p.Name)
		}
		byName[p.Name] = p
	}

	var out []toolchain.Variant
	for _, p := range presets {
		if p.Hidden {
			continue
		}
		merged, err := resolveCacheVars(&p, byName, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, toolchain.Variant{
			Name:      p.Name,
			CacheVars: merged,
		})
	}
	return out, nil
}

// resolveCacheVars walks the inherits chain depth-first. ancestors
// is the path of preset names already in resolution; revisiting
// any of them is a cycle.
func resolveCacheVars(p *configurePreset, byName map[string]*configurePreset, ancestors []string) (map[string]string, error) {
	for _, a := range ancestors {
		if a == p.Name {
			return nil, fmt.Errorf("presets: inherits cycle through %q", p.Name)
		}
	}
	merged := map[string]string{}

	// Inherits: declared order — CMakePresets says later parents
	// override earlier ones on key collision, so we walk parents
	// in the order the JSON listed them.
	parentNames, err := p.parents()
	if err != nil {
		return nil, fmt.Errorf("presets: %q: %w", p.Name, err)
	}
	for _, parentName := range parentNames {
		parent, ok := byName[parentName]
		if !ok {
			return nil, fmt.Errorf("presets: %q inherits unknown preset %q", p.Name, parentName)
		}
		parentVars, err := resolveCacheVars(parent, byName, append(ancestors, p.Name))
		if err != nil {
			return nil, err
		}
		for k, v := range parentVars {
			merged[k] = v
		}
	}

	// Apply this preset's cacheVariables — child wins on collisions.
	for k, raw := range p.CacheVariables {
		val, err := raw.stringValue()
		if err != nil {
			return nil, fmt.Errorf("presets: %q: %s: %w", p.Name, k, err)
		}
		merged[k] = val
	}

	if len(merged) == 0 {
		return nil, nil
	}
	return merged, nil
}

// presetsDoc mirrors the top-level CMakePresets.json shape. We only
// need version + configurePresets; the rest (buildPresets,
// testPresets, ...) are ignored.
type presetsDoc struct {
	Version          int               `json:"version"`
	ConfigurePresets []configurePreset `json:"configurePresets"`
}

// configurePreset is one entry under `configurePresets`. Only the
// fields relevant to the variant matrix are decoded; binaryDir,
// generator, etc. are untouched.
type configurePreset struct {
	Name           string                 `json:"name"`
	Hidden         bool                   `json:"hidden"`
	Inherits       json.RawMessage        `json:"inherits"`
	CacheVariables map[string]cacheVarRaw `json:"cacheVariables"`
}

// parents returns the parent preset names from `inherits` in
// declared order. The JSON field can be a single string or an
// array of strings; both shapes are normalized to a slice. Order
// is preserved exactly because CMakePresets semantics says
// "later inherited presets override earlier ones on key
// collisions" — sorting alphabetically here would silently swap
// which parent's value wins on a duplicate key.
//
// Any other shape (number, object, mixed array, etc.) is
// reported as an error rather than silently treated as
// "no parents" — that flavour of mis-parse used to produce a
// quietly wrong CacheVars merge.
func (p *configurePreset) parents() ([]string, error) {
	if len(p.Inherits) == 0 {
		return nil, nil
	}
	// Try array first.
	var arr []string
	if err := json.Unmarshal(p.Inherits, &arr); err == nil {
		return arr, nil
	}
	// Fall back to single string.
	var s string
	if err := json.Unmarshal(p.Inherits, &s); err == nil {
		if s == "" {
			return nil, fmt.Errorf("inherits is an empty string; expected a preset name or an array of names")
		}
		return []string{s}, nil
	}
	return nil, fmt.Errorf("inherits has unsupported shape: %s", string(p.Inherits))
}

// cacheVarRaw handles the two shapes a cacheVariables value can take:
// a plain string, or an object { "type": "...", "value": "..." }.
type cacheVarRaw struct {
	raw json.RawMessage
}

func (c *cacheVarRaw) UnmarshalJSON(data []byte) error {
	c.raw = append(c.raw[:0], data...)
	return nil
}

func (c cacheVarRaw) stringValue() (string, error) {
	if len(c.raw) == 0 {
		return "", nil
	}
	// Plain string first — the most common shape ("FOO": "bar").
	var s string
	if err := json.Unmarshal(c.raw, &s); err == nil {
		return s, nil
	}
	// Object shape second: { "type": "STRING", "value": "..." }.
	// Decode `value` as *string so an intentionally-empty
	// `"value": ""` is distinguishable from "no value field" —
	// without the pointer the empty string would be ambiguous
	// with the zero value of a string-typed field on a non-object
	// shape, and we'd fall through to the %v path.
	var obj struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(c.raw, &obj); err == nil && obj.Value != nil {
		return *obj.Value, nil
	}
	// Bool / number — coerce via fmt. JSON null maps to the empty
	// string (matches kits.stringify; the alternative
	// fmt.Sprintf("%v", nil) emits the literal "<nil>" which would
	// surface to cmake as -DKEY=<nil>, almost certainly unintended).
	// Reject anything else (an object without a value field, an
	// array, etc.) so callers see a clear error rather than a
	// garbled coercion.
	var v any
	if err := json.Unmarshal(c.raw, &v); err == nil {
		switch v.(type) {
		case nil:
			return "", nil
		case bool, float64:
			return fmt.Sprintf("%v", v), nil
		}
	}
	return "", fmt.Errorf("unrecognized cacheVariable shape: %s", string(c.raw))
}
