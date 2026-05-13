package presets

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

func TestParse_BasicShape(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "debug", "cacheVariables": {"CMAKE_BUILD_TYPE": "Debug"}},
			{"name": "release", "cacheVariables": {"CMAKE_BUILD_TYPE": "Release"}}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []toolchain.Variant{
		{Name: "debug", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Debug"}},
		{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestParse_HiddenPresetsSkipped: hidden presets are abstract bases
// (typical pattern: "_base" with the binary dir pattern, then real
// presets inherit it). They must NOT appear in the Variant matrix
// because trying to configure with a hidden preset is meaningless.
func TestParse_HiddenPresetsSkipped(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "_base", "hidden": true, "cacheVariables": {"FOO": "bar"}},
			{"name": "real", "inherits": "_base"}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 visible variant; got %d: %+v", len(got), got)
	}
	if got[0].Name != "real" {
		t.Errorf("expected name=real; got %q", got[0].Name)
	}
	if got[0].CacheVars["FOO"] != "bar" {
		t.Errorf("inherited FOO not propagated; got %v", got[0].CacheVars)
	}
}

// TestParse_InheritsChainOverridesParent: child cacheVariables win
// over inherited ones. The merge order — parent first, child last —
// is the documented CMakePresets.json semantics.
func TestParse_InheritsChainOverridesParent(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "parent", "hidden": true, "cacheVariables": {"A": "from-parent", "B": "from-parent"}},
			{"name": "child", "inherits": "parent", "cacheVariables": {"A": "from-child", "C": "from-child"}}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1; got %d", len(got))
	}
	want := map[string]string{"A": "from-child", "B": "from-parent", "C": "from-child"}
	if !reflect.DeepEqual(got[0].CacheVars, want) {
		t.Errorf("got %+v, want %+v", got[0].CacheVars, want)
	}
}

// TestParse_InheritsArray: the `inherits` field can be a string OR
// an array of strings, and later parents override earlier ones on
// key collisions. CMakePresets defines this as the parent merge
// rule, and the JSON array order is the operator's declared
// intent — preserve it exactly. Two calls of `inherits: [p1, p2]`
// vs `inherits: [p2, p1]` SHOULD produce different effective
// values for keys both parents set.
func TestParse_InheritsArray(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "p1", "hidden": true, "cacheVariables": {"X": "p1"}},
			{"name": "p2", "hidden": true, "cacheVariables": {"X": "p2", "Y": "p2-only"}},
			{"name": "child_p1_first", "inherits": ["p1", "p2"]},
			{"name": "child_p2_first", "inherits": ["p2", "p1"]}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2; got %d", len(got))
	}
	gotByName := map[string]map[string]string{}
	for _, v := range got {
		gotByName[v.Name] = v.CacheVars
	}
	// p1 first, then p2 overrides X; Y from p2.
	wantP1First := map[string]string{"X": "p2", "Y": "p2-only"}
	if !reflect.DeepEqual(gotByName["child_p1_first"], wantP1First) {
		t.Errorf("child_p1_first: got %+v, want %+v", gotByName["child_p1_first"], wantP1First)
	}
	// p2 first, then p1 overrides X; Y still from p2 (p1 doesn't set it).
	wantP2First := map[string]string{"X": "p1", "Y": "p2-only"}
	if !reflect.DeepEqual(gotByName["child_p2_first"], wantP2First) {
		t.Errorf("child_p2_first: got %+v, want %+v", gotByName["child_p2_first"], wantP2First)
	}
}

// TestParse_InheritsRejectsMalformedShape: inherits whose value
// is neither a string nor a string array (e.g. a number, object,
// mixed array) used to silently parse as "no parents", producing
// a quietly wrong CacheVars merge. Now an explicit parse error.
func TestParse_InheritsRejectsMalformedShape(t *testing.T) {
	cases := map[string]string{
		"number":       `{"version":3,"configurePresets":[{"name":"p","inherits":42}]}`,
		"object":       `{"version":3,"configurePresets":[{"name":"p","inherits":{"foo":"bar"}}]}`,
		"mixed":        `{"version":3,"configurePresets":[{"name":"p","inherits":["a", 1]}]}`,
		"empty-string": `{"version":3,"configurePresets":[{"name":"p","inherits":""}]}`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Errorf("expected error for %s; got nil", label)
			}
		})
	}
}

func TestParse_InheritsCycleRejected(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "a", "inherits": "b"},
			{"name": "b", "inherits": "a"}
		]
	}`)
	_, err := Parse(body)
	if err == nil {
		t.Fatal("expected cycle error; got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParse_InheritsUnknownPresetRejected(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "a", "inherits": "missing"}
		]
	}`)
	_, err := Parse(body)
	if err == nil {
		t.Fatal("expected unknown-parent error; got nil")
	}
	if !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParse_CacheVariableObjectShape: cacheVariables values can be
// plain strings or {value, type} objects. Both paths must lift the
// value into Variant.CacheVars.
func TestParse_CacheVariableObjectShape(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "p", "cacheVariables": {
				"PLAIN": "string-value",
				"OBJECT": {"type": "STRING", "value": "object-value"},
				"BOOL_LIKE": "ON"
			}}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{
		"PLAIN":     "string-value",
		"OBJECT":    "object-value",
		"BOOL_LIKE": "ON",
	}
	if !reflect.DeepEqual(got[0].CacheVars, want) {
		t.Errorf("got %+v, want %+v", got[0].CacheVars, want)
	}
}

// TestParse_CacheVariableEmptyStringValue covers the
// `{"type": "STRING", "value": ""}` case: an intentionally-empty
// value must round-trip as the empty string, not fall through
// to the `%v` path and emit a map-like literal. The fix uses
// *string presence detection instead of nil-vs-empty heuristics.
func TestParse_CacheVariableEmptyStringValue(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "p", "cacheVariables": {
				"INTENTIONALLY_EMPTY": {"type": "STRING", "value": ""},
				"NON_EMPTY": {"type": "STRING", "value": "real"}
			}}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1; got %d", len(got))
	}
	if v, ok := got[0].CacheVars["INTENTIONALLY_EMPTY"]; !ok || v != "" {
		t.Errorf("INTENTIONALLY_EMPTY = %q (present=%v); want empty-string and present", v, ok)
	}
	if got[0].CacheVars["NON_EMPTY"] != "real" {
		t.Errorf("NON_EMPTY = %q; want real", got[0].CacheVars["NON_EMPTY"])
	}
}

// TestParse_CacheVariableNullMapsToEmpty: a JSON `null`
// cacheVariable used to round-trip as the literal string "<nil>"
// via fmt.Sprintf("%v", nil), which would emit -DKEY=<nil> to
// cmake. Map null to the empty string instead — matches
// kits.stringify and is the only intuitive interpretation.
func TestParse_CacheVariableNullMapsToEmpty(t *testing.T) {
	body := []byte(`{
		"version": 3,
		"configurePresets": [
			{"name": "p", "cacheVariables": {
				"NULL_VAR": null,
				"REAL_VAR": "real"
			}}
		]
	}`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got[0].CacheVars["NULL_VAR"] != "" {
		t.Errorf("NULL_VAR = %q; want empty", got[0].CacheVars["NULL_VAR"])
	}
	if got[0].CacheVars["REAL_VAR"] != "real" {
		t.Errorf("REAL_VAR = %q; want real", got[0].CacheVars["REAL_VAR"])
	}
}

// TestLoadFile_MissingReturnsNil keeps caller code clean — they can
// union LoadFile("CMakePresets.json") + LoadFile("CMakeUserPresets.json")
// without checking IsNotExist themselves. CMakeUserPresets.json is
// commonly absent (it's per-developer, .gitignore'd).
func TestLoadFile_MissingReturnsNil(t *testing.T) {
	got, err := LoadFile("/no/such/path/CMakePresets.json")
	if err != nil {
		t.Errorf("expected nil error for missing file; got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil variants; got %v", got)
	}
}

// TestLoadFile_ProbeProjectFixture is the canonical-catalog contract:
// converter/testdata/toolchain-probe/CMakePresets.json carries the
// project's variant matrix (build types + sanitizers + coverage + lto).
// This test loads it and asserts the catalog matches the Go-side
// FeatureVariants — the JSON is the single source of truth.
func TestLoadFile_ProbeProjectFixture(t *testing.T) {
	got, err := LoadFile("../../../testdata/toolchain-probe/CMakePresets.json")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("probe-project CMakePresets.json yielded no variants")
	}

	gotByName := map[string]toolchain.Variant{}
	for _, v := range got {
		gotByName[v.Name] = v
	}

	// Every SanitizerVariant must appear in the JSON catalog with
	// matching CacheVars; this is the cross-check the plan calls for.
	for _, want := range toolchain.FeatureVariants {
		got, ok := gotByName[want.Name]
		if !ok {
			t.Errorf("CMakePresets.json missing sanitizer preset %q", want.Name)
			continue
		}
		if !reflect.DeepEqual(got.CacheVars, want.CacheVars) {
			t.Errorf("preset %q CacheVars mismatch:\n got: %+v\nwant: %+v",
				want.Name, got.CacheVars, want.CacheVars)
		}
	}

	// Build-type presets are also expected to be present.
	for _, bt := range []string{"debug", "release", "relwithdebinfo", "minsizerel"} {
		if _, ok := gotByName[bt]; !ok {
			t.Errorf("CMakePresets.json missing build-type preset %q", bt)
		}
	}
}
