package genexeval

import (
	"errors"
	"strings"
	"testing"
)

func evalString(t *testing.T, template string, ctx Context) (string, error) {
	t.Helper()
	nodes, err := Parse([]byte(template))
	if err != nil {
		t.Fatalf("parse %q: %v", template, err)
	}
	b, err := Eval(nodes, ctx)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func TestEval_PlainText(t *testing.T) {
	got, err := evalString(t, "no genex here", Context{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "no genex here" {
		t.Errorf("got %q", got)
	}
}

func TestEval_Config(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ctx  Context
		want string
	}{
		{"plain", "$<CONFIG>", Context{Config: "Release"}, "Release"},
		{"match", "$<CONFIG:Release>", Context{Config: "Release"}, "1"},
		{"mismatch", "$<CONFIG:Debug>", Context{Config: "Release"}, "0"},
		{"case-insensitive", "$<CONFIG:RELEASE>", Context{Config: "Release"}, "1"},
		{"multi-arg first matches", "$<CONFIG:Debug,Release>", Context{Config: "Release"}, "1"},
		{"multi-arg none matches", "$<CONFIG:Debug,RelWithDebInfo>", Context{Config: "Release"}, "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalString(t, c.in, c.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_Config_UnsetIsUnsupported(t *testing.T) {
	_, err := evalString(t, "$<CONFIG>", Context{})
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnsupportedError, got %v", err)
	}
	if ue.Op != "CONFIG" {
		t.Errorf("UnsupportedError.Op = %q", ue.Op)
	}
}

func TestEval_CompilerID(t *testing.T) {
	ctx := Context{CompilerID: map[string]string{"C": "GNU", "CXX": "GNU"}}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "$<COMPILER_ID>", "GNU"},
		{"match", "$<COMPILER_ID:GNU>", "1"},
		{"mismatch", "$<COMPILER_ID:MSVC>", "0"},
		{"multi-arg", "$<COMPILER_ID:Clang,GNU>", "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_CompilerID_LanguageHint(t *testing.T) {
	// When CompilerLanguage is set, the evaluator picks that
	// language's id specifically rather than the first-seen.
	ctx := Context{
		CompilerID:       map[string]string{"C": "Clang", "CXX": "GNU"},
		CompilerLanguage: "CXX",
	}
	got, err := evalString(t, "$<COMPILER_ID:GNU>", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1" {
		t.Errorf("CXX-keyed lookup should pick GNU; got %q", got)
	}
}

// TestEval_CompilerID_DeterministicFallback asserts that when
// the Context has no language hint AND none of the documented
// preferred-languages (C / CXX / OBJC / OBJCXX / Fortran) are
// present, the "last resort: any entry" fallback picks the
// lexicographically-first language deterministically rather
// than relying on map iteration order. Repeated lookups
// against the same Context must return the same id.
func TestEval_CompilerID_DeterministicFallback(t *testing.T) {
	// Only an exotic language is set — none of the preferred
	// list match. Two different ids so the "any entry" fallback
	// has a real choice to make.
	ctx := Context{CompilerID: map[string]string{
		"Swift":  "AppleClang",
		"CSharp": "MSBuild",
		"Pascal": "FPC",
	}}
	const iterations = 50
	first, err := evalString(t, "$<COMPILER_ID>", ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted order: CSharp < Pascal < Swift → MSBuild wins.
	if first != "MSBuild" {
		t.Errorf("expected lexicographically-first CSharp's MSBuild as fallback; got %q", first)
	}
	for i := 0; i < iterations; i++ {
		got, err := evalString(t, "$<COMPILER_ID>", ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("iteration %d: got %q, expected %q (non-deterministic fallback)", i, got, first)
		}
	}
}

func TestEval_PlatformID(t *testing.T) {
	ctx := Context{PlatformID: "Linux"}
	cases := []struct{ in, want string }{
		{"$<PLATFORM_ID>", "Linux"},
		{"$<PLATFORM_ID:Linux>", "1"},
		{"$<PLATFORM_ID:Darwin>", "0"},
		{"$<PLATFORM_ID:Darwin,Linux>", "1"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_Booleans(t *testing.T) {
	ctx := Context{Config: "Release"}
	cases := []struct{ in, want string }{
		{"$<AND:1,1>", "1"},
		{"$<AND:1,0>", "0"},
		{"$<OR:0,1>", "1"},
		{"$<OR:0,0>", "0"},
		{"$<NOT:0>", "1"},
		{"$<NOT:1>", "0"},
		{"$<IF:1,yes,no>", "yes"},
		{"$<IF:0,yes,no>", "no"},
		{"$<IF:$<CONFIG:Release>,prod,dev>", "prod"},
		{"$<IF:$<CONFIG:Debug>,prod,dev>", "dev"},
		// nested boolean
		{"$<AND:$<CONFIG:Release>,1>", "1"},
		{"$<OR:$<CONFIG:Debug>,$<CONFIG:Release>>", "1"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_Booleans_NonCanonicalRefused(t *testing.T) {
	// cmake accepts "TRUE" / "YES" / "ON" / etc. as truthy,
	// but our v1 evaluator only models "0" / "1" to avoid
	// silently diverging. Other forms surface as
	// UnsupportedError → lifter falls back.
	_, err := evalString(t, "$<AND:TRUE,1>", Context{})
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnsupportedError, got %v", err)
	}
}

func TestEval_BoolOp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$<BOOL:>", "0"},
		{"$<BOOL:0>", "0"},
		{"$<BOOL:1>", "1"},
		{"$<BOOL:hello>", "1"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, Context{})
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_BoolOp_NonCanonicalRefused(t *testing.T) {
	_, err := evalString(t, "$<BOOL:FALSE>", Context{})
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnsupportedError, got %v", err)
	}
}

func TestEval_StringOps(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$<UPPER_CASE:hello>", "HELLO"},
		{"$<LOWER_CASE:WORLD>", "world"},
		{"$<STREQUAL:a,a>", "1"},
		{"$<STREQUAL:a,b>", "0"},
		{"$<UPPER_CASE:$<CONFIG>>", "RELEASE"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, Context{Config: "Release"})
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_LiteralOneAndZero(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$<1:emit>", "emit"},
		{"$<0:skip>", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, Context{})
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestEval_UnsupportedTargetGenex(t *testing.T) {
	// Target-evaluator-dependent ops surface as
	// UnsupportedError — the lifter routes to (b) / legacy.
	cases := []string{
		"$<TARGET_FILE:foo>",
		"$<TARGET_OBJECTS:foo>",
		"$<TARGET_PROPERTY:foo,prop>",
		"$<INSTALL_INTERFACE:foo>",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := evalString(t, in, Context{})
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError, got %v", err)
			}
		})
	}
}

// TestEval_TargetProperty_SupportedSubset covers the v1
// evaluator's TARGET_PROPERTY support: NAME / TYPE / SOURCES /
// IMPORTED resolve from Context.Targets when the target is
// captured, anything else surfaces as UnsupportedError so the
// lifter falls through to (b) / legacy.
func TestEval_TargetProperty_SupportedSubset(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {
			Type:     "STATIC_LIBRARY",
			Sources:  "src/a.c;src/b.c",
			Imported: false,
		},
		"bar": {
			Type:     "INTERFACE_LIBRARY",
			Imported: true,
		},
	}}
	cases := []struct{ in, want string }{
		{"$<TARGET_PROPERTY:foo,NAME>", "foo"},
		{"$<TARGET_PROPERTY:foo,TYPE>", "STATIC_LIBRARY"},
		{"$<TARGET_PROPERTY:foo,SOURCES>", "src/a.c;src/b.c"},
		{"$<TARGET_PROPERTY:foo,IMPORTED>", "FALSE"},
		{"$<TARGET_PROPERTY:bar,IMPORTED>", "TRUE"},
		{"$<TARGET_PROPERTY:bar,TYPE>", "INTERFACE_LIBRARY"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestEval_TargetProperty_Refusals asserts every refusal mode
// surfaces as UnsupportedError with a clear Reason: missing
// target, unsupported property, wrong-arity call, missing
// Context field.
func TestEval_TargetProperty_Refusals(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {Type: "STATIC_LIBRARY"},
	}}
	cases := []struct{ name, in string }{
		{"missing target", "$<TARGET_PROPERTY:nonexistent,NAME>"},
		// LINK_DIRECTORIES isn't in the supported set — Phase 3
		// expanded coverage to the INTERFACE_* aggregates the
		// probe-genex hook captures, but per-target build-only
		// properties (LINK_DIRECTORIES, COMPILE_PDB_NAME,
		// POSITION_INDEPENDENT_CODE, ...) still need either a
		// codemodel-side projection or a probe extension.
		{"unsupported property", "$<TARGET_PROPERTY:foo,LINK_DIRECTORIES>"},
		// Single-arg form has no convert-time meaning for file(GENERATE).
		{"one-arg form", "$<TARGET_PROPERTY:NAME>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalString(t, c.in, ctx)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError, got %v", err)
			}
			if ue.Reason == "" {
				t.Errorf("UnsupportedError.Reason should not be empty")
			}
		})
	}
}

// TestEval_TargetFile_Resolves asserts the single-arg
// `$<TARGET_FILE:t>` path: when Context.Targets[t].FileLocation
// is set, the eval returns those bytes verbatim. The lifter
// populates this with cmake's recorded artifact path at
// convert time (for the byte-equal check) and the
// cmake-configure-file tool overrides it at Bazel time via
// --target-file=<name>=<path>.
func TestEval_TargetFile_Resolves(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {FileLocation: "/build/libfoo.a"},
		"bar": {FileLocation: "bazel-bin/pkg/libbar.so"},
	}}
	cases := []struct{ in, want string }{
		{"$<TARGET_FILE:foo>", "/build/libfoo.a"},
		{"$<TARGET_FILE:bar>", "bazel-bin/pkg/libbar.so"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestEval_TargetFile_Refusals covers the typed-refusal paths:
// missing target, empty FileLocation, wrong arity. All surface
// as UnsupportedError so the lifter falls back cleanly.
func TestEval_TargetFile_Refusals(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ctx  Context
	}{
		{
			"missing target",
			"$<TARGET_FILE:nonexistent>",
			Context{Targets: map[string]TargetInfo{"foo": {FileLocation: "/p"}}},
		},
		{
			"empty FileLocation",
			"$<TARGET_FILE:foo>",
			Context{Targets: map[string]TargetInfo{"foo": {Type: "STATIC_LIBRARY"}}},
		},
		{
			"empty Context.Targets",
			"$<TARGET_FILE:foo>",
			Context{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalString(t, c.in, c.ctx)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError, got %v", err)
			}
			if ue.Op != "TARGET_FILE" {
				t.Errorf("Op = %q want TARGET_FILE", ue.Op)
			}
		})
	}
}

// TestEval_TargetFile_Variants exercises the six on-disk-path
// variants that derive from FileLocation:
//
//   - TARGET_FILE_DIR / TARGET_LINKER_FILE_DIR → filepath.Dir
//   - TARGET_FILE_NAME / TARGET_LINKER_FILE_NAME → filepath.Base
//   - TARGET_LINKER_FILE / TARGET_SONAME_FILE → Linux v1 alias
//     to TARGET_FILE (identity)
//
// Each shares the lifter's existing --target-file=<name>=<path>
// wire; the evaluator does the derivation at Bazel time.
func TestEval_TargetFile_Variants(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {FileLocation: "/build/lib/libfoo.a"},
	}}
	cases := []struct{ in, want string }{
		{"$<TARGET_FILE:foo>", "/build/lib/libfoo.a"},
		{"$<TARGET_FILE_DIR:foo>", "/build/lib"},
		{"$<TARGET_FILE_NAME:foo>", "libfoo.a"},
		{"$<TARGET_LINKER_FILE:foo>", "/build/lib/libfoo.a"},
		{"$<TARGET_LINKER_FILE_DIR:foo>", "/build/lib"},
		{"$<TARGET_LINKER_FILE_NAME:foo>", "libfoo.a"},
		{"$<TARGET_SONAME_FILE:foo>", "/build/lib/libfoo.a"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestEval_TargetFile_Variants_Refusals confirms the variants
// share the same refusal modes as TARGET_FILE — missing target
// and empty FileLocation both surface UnsupportedError with the
// op label set to the originating op (TARGET_FILE_DIR etc.)
// rather than the underlying TARGET_FILE — so the lifter's
// fallback diagnostics name the template's actual op.
func TestEval_TargetFile_Variants_Refusals(t *testing.T) {
	cases := []struct {
		name, in, wantOp string
		ctx              Context
	}{
		{
			"missing target / TARGET_FILE_DIR",
			"$<TARGET_FILE_DIR:nope>",
			"TARGET_FILE_DIR",
			Context{},
		},
		{
			"empty FileLocation / TARGET_LINKER_FILE_NAME",
			"$<TARGET_LINKER_FILE_NAME:foo>",
			"TARGET_LINKER_FILE_NAME",
			Context{Targets: map[string]TargetInfo{"foo": {Type: "STATIC_LIBRARY"}}},
		},
		{
			"missing target / TARGET_SONAME_FILE",
			"$<TARGET_SONAME_FILE:nope>",
			"TARGET_SONAME_FILE",
			Context{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalString(t, c.in, c.ctx)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError, got %v", err)
			}
			if ue.Op != c.wantOp {
				t.Errorf("Op = %q want %q", ue.Op, c.wantOp)
			}
		})
	}
}

func TestEval_UnknownGenex(t *testing.T) {
	_, err := evalString(t, "$<TOTALLY_MADE_UP:foo>", Context{})
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnsupportedError, got %v", err)
	}
	if !strings.Contains(ue.Error(), "unknown") {
		t.Errorf("error %q should mention unknown", ue.Error())
	}
}

func TestEval_MixedTemplate(t *testing.T) {
	// Real-world shape: a header with config + platform genexes
	// interspersed with normal text.
	template := `// build: $<CONFIG>
#define BUILD_CONFIG_RELEASE $<CONFIG:Release>
#define IS_LINUX $<PLATFORM_ID:Linux>
#define COMPILER "$<COMPILER_ID>"
$<IF:$<CONFIG:Debug>,#define DEBUG_ENABLED,#define NDEBUG>
`
	want := `// build: Release
#define BUILD_CONFIG_RELEASE 1
#define IS_LINUX 1
#define COMPILER "GNU"
#define NDEBUG
`
	ctx := Context{
		Config:     "Release",
		PlatformID: "Linux",
		CompilerID: map[string]string{"C": "GNU", "CXX": "GNU"},
	}
	got, err := evalString(t, template, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestEval_PartialEvaluationPropagatesUnsupported(t *testing.T) {
	// A template mixing supported + unsupported genexes: the
	// whole evaluation must error, so the lifter falls back
	// rather than emitting wrong bytes.
	_, err := evalString(t, "// build: $<CONFIG>\n#define TARGET $<TARGET_FILE:foo>\n", Context{Config: "Release"})
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnsupportedError, got %v", err)
	}
	if ue.Op != "TARGET_FILE" {
		t.Errorf("UnsupportedError.Op = %q", ue.Op)
	}
}

// TestEval_TargetProperty_InterfaceAggregates covers the
// post-Phase-3 path: probe-genex provides cmake's resolved
// INTERFACE_* aggregate, the lifter loads it into TargetInfo,
// the evaluator returns the value verbatim.
func TestEval_TargetProperty_InterfaceAggregates(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {
			Type:                        "STATIC_LIBRARY",
			InterfaceIncludeDirectories: "/src/include;/src/extra",
			InterfaceCompileDefinitions: "FOO=1;BAR=2",
			InterfaceLinkLibraries:      "bar;baz",
			InterfaceCompileOptions:     "-Wall;-Werror",
			InterfaceLinkOptions:        "-Wl,--as-needed",
		},
	}}
	cases := []struct{ in, want string }{
		{"$<TARGET_PROPERTY:foo,INTERFACE_INCLUDE_DIRECTORIES>", "/src/include;/src/extra"},
		{"$<TARGET_PROPERTY:foo,INTERFACE_COMPILE_DEFINITIONS>", "FOO=1;BAR=2"},
		{"$<TARGET_PROPERTY:foo,INTERFACE_LINK_LIBRARIES>", "bar;baz"},
		{"$<TARGET_PROPERTY:foo,INTERFACE_COMPILE_OPTIONS>", "-Wall;-Werror"},
		{"$<TARGET_PROPERTY:foo,INTERFACE_LINK_OPTIONS>", "-Wl,--as-needed"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := evalString(t, c.in, ctx)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestEval_TargetProperty_InterfaceEmpty: when the probe captured
// an empty INTERFACE_* (target has no value set), the evaluator
// resolves to the empty string — distinguishing "field unset" from
// "field aggregates to empty" isn't possible from struct state
// alone, and cmake itself emits the empty string for unset
// INTERFACE_* anyway.
func TestEval_TargetProperty_InterfaceEmpty(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {Type: "STATIC_LIBRARY"},
	}}
	got, err := evalString(t, "$<TARGET_PROPERTY:foo,INTERFACE_LINK_LIBRARIES>", ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestEval_TargetObjects covers the post-Phase-3 path: probe-genex
// captured an OBJECT_LIBRARY's resolved object list, the evaluator
// returns it verbatim.
func TestEval_TargetObjects(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"objlib": {
			Type:    "OBJECT_LIBRARY",
			Objects: "/build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o",
		},
	}}
	got, err := evalString(t, "$<TARGET_OBJECTS:objlib>", ctx)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	want := "/build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEval_TargetObjects_Refusals covers the three rejection
// paths: missing target, missing Objects field, wrong arity.
func TestEval_TargetObjects_Refusals(t *testing.T) {
	ctx := Context{Targets: map[string]TargetInfo{
		"foo": {Type: "STATIC_LIBRARY"}, // not OBJECT_LIBRARY; Objects unset
	}}
	cases := []struct{ name, in string }{
		{"missing target", "$<TARGET_OBJECTS:ghost>"},
		{"empty objects", "$<TARGET_OBJECTS:foo>"},
		{"wrong arity", "$<TARGET_OBJECTS:foo,bar>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := evalString(t, c.in, ctx)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError, got %v", err)
			}
			if ue.Op != "TARGET_OBJECTS" {
				t.Errorf("UnsupportedError.Op = %q", ue.Op)
			}
		})
	}
}
