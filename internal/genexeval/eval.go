package genexeval

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Context carries the configure-time facts the evaluator
// consults. All fields are optional — an evaluator without the
// requested field surfaces a typed UnsupportedError, which the
// lifter routes to the next fallback shape ((b) capture, then
// legacy).
//
// Fields mirror cmake's per-build context that genexes read:
//
//   - Config: the build type cmake configured for (`Release`,
//     `Debug`, ...). Drives `$<CONFIG>` and `$<CONFIG:...>`.
//   - CompilerID: the per-language compiler id cmake detected
//     (`GNU`, `Clang`, `MSVC`, ...). Drives `$<COMPILER_ID>`
//     and `$<COMPILER_ID:...>`. Keyed by language ("C", "CXX",
//     ...); the evaluator picks the language out of
//     CompilerLanguage when it's set, otherwise falls back to
//     the first entry.
//   - PlatformID: cmake's CMAKE_SYSTEM_NAME (`Linux`, `Darwin`,
//     `Windows`, ...). Drives `$<PLATFORM_ID>` and
//     `$<PLATFORM_ID:...>`.
//   - CompilerLanguage: the per-source language being
//     translated (`C`, `CXX`). file(GENERATE) templates may
//     reference this when used by add_custom_command on a
//     language-specific output; for the v1 lifter it's usually
//     unset (file(GENERATE) is language-agnostic).
//
// Future additions can extend Context without changing the
// evaluator's signature (a typed-refusal genex stays typed-
// refused until both Context and Eval gain the matching
// field).
type Context struct {
	Config           string            `json:"config,omitempty"`
	CompilerID       map[string]string `json:"compiler_id,omitempty"`
	PlatformID       string            `json:"platform_id,omitempty"`
	CompilerLanguage string            `json:"compiler_language,omitempty"`

	// Targets is the per-target context the evaluator consults
	// for `$<TARGET_PROPERTY:t,p>` and (future) related per-
	// target ops. Keyed by cmake target name (the unqualified
	// name, e.g. "foo", not the Bazel label). The lifter
	// populates this from the fileapi codemodel at convert time
	// — the evaluator supports the cmake-direct properties
	// (NAME, TYPE, SOURCES, IMPORTED) plus the four INTERFACE_*
	// aggregates (INCLUDE_DIRECTORIES, COMPILE_DEFINITIONS,
	// COMPILE_OPTIONS, LINK_LIBRARIES) whose convert-time
	// values the lifter assembles by walking the codemodel +
	// trace dep chain. INTERFACE_LINK_OPTIONS isn't in the
	// convert-time set (cmake doesn't surface it on the trace
	// side in a shape we can dedup against the link line) but
	// surfaces via the probe-genex hook when present.
	Targets map[string]TargetInfo `json:"targets,omitempty"`
}

// TargetInfo carries the per-target facts the evaluator
// consults. Captures the cmake-direct properties (TYPE /
// SOURCES / IMPORTED) plus the four INTERFACE_* aggregates
// (INCLUDE_DIRECTORIES, COMPILE_DEFINITIONS, COMPILE_OPTIONS,
// LINK_LIBRARIES) the lifter assembles at convert time by
// walking the codemodel + trace dep chain. Probe-genex hook
// values (cmake's own evaluator output) override the
// convert-time aggregate when both are present.
type TargetInfo struct {
	// Type is cmake's target type (`EXECUTABLE`,
	// `STATIC_LIBRARY`, `SHARED_LIBRARY`, `INTERFACE_LIBRARY`,
	// ...). Maps to `$<TARGET_PROPERTY:t,TYPE>`.
	Type string `json:"type,omitempty"`

	// Sources is the target's source file list,
	// semicolon-joined to match cmake's list serialization
	// when stored as a property string. Maps to
	// `$<TARGET_PROPERTY:t,SOURCES>`.
	Sources string `json:"sources,omitempty"`

	// Imported is true for targets imported via
	// `add_library(... IMPORTED)`. Maps to
	// `$<TARGET_PROPERTY:t,IMPORTED>` returning `"TRUE"` /
	// `"FALSE"` per cmake's documented serialization for the
	// boolean property.
	Imported bool `json:"imported,omitempty"`

	// FileLocation is the on-disk path of the target's primary
	// build artifact. Maps to `$<TARGET_FILE:t>`. At convert
	// time the lifter populates this with cmake's recorded
	// build-dir-relative artifact path (for the byte-equal
	// check against cmake's rendered output to pass). At Bazel
	// time the cmake-configure-file tool overrides it via the
	// repeatable `--target-file=<name>=<path>` flag, where the
	// path is a `$(location //pkg:t)` Bazel substituted at
	// action time. The marshaled Context payload OMITS
	// FileLocation (json:"-") so the lifted cmd stays byte-
	// stable across recording machines — only the
	// `--target-file` flag values shape the Bazel-time bytes.
	FileLocation string `json:"-"`

	// Objects is the semicolon-separated list of object-file
	// paths a `$<TARGET_OBJECTS:t>` genex resolves to. Populated
	// by lifters that have cmake's resolved object list — Phase
	// 3 of the generator-parity uplift reads this from the
	// probe-genex hook's per-target objects.txt file (only
	// emitted for OBJECT_LIBRARY targets). Empty for non-
	// OBJECT_LIBRARY targets; TARGET_OBJECTS resolution surfaces
	// an UnsupportedError when the field is empty so the lifter
	// can fall back to legacy bytes.
	Objects string `json:"objects,omitempty"`

	// InterfaceIncludeDirectories / InterfaceCompileDefinitions /
	// InterfaceCompileOptions / InterfaceLinkLibraries /
	// InterfaceLinkOptions carry the post-walk values of cmake's
	// INTERFACE_* properties — the aggregates cmake assembles by
	// walking the dependency graph.
	//
	// Two population paths, layered:
	//
	//   1. Convert-time aggregation (lower/buildGenexTargets):
	//      walks the codemodel Dependencies[] graph + the
	//      trace's target_link_libraries arms transitively,
	//      accumulating PUBLIC/INTERFACE contributions from
	//      target_include_directories /
	//      target_compile_definitions /
	//      target_compile_options /
	//      target_link_libraries calls. Fills
	//      InterfaceIncludeDirectories /
	//      InterfaceCompileDefinitions /
	//      InterfaceCompileOptions / InterfaceLinkLibraries.
	//      Best-effort: depends on the trace being available,
	//      which is the usual case for live cmake runs.
	//
	//   2. Probe-genex hook (cmake's own generator-phase
	//      evaluator output): when the hook ran at configure
	//      time, the resulting per-target interface_<P>.txt
	//      file overrides the convert-time aggregate. Cmake
	//      itself is the source of truth when its evaluator's
	//      output is available; this is also the only path
	//      that populates InterfaceLinkOptions.
	//
	// Each value is the semicolon-separated list cmake produces;
	// consumers split on `;` to recover the cmake list.
	InterfaceIncludeDirectories string `json:"interface_include_directories,omitempty"`
	InterfaceCompileDefinitions string `json:"interface_compile_definitions,omitempty"`
	InterfaceCompileOptions     string `json:"interface_compile_options,omitempty"`
	InterfaceLinkLibraries      string `json:"interface_link_libraries,omitempty"`
	InterfaceLinkOptions        string `json:"interface_link_options,omitempty"`
}

// UnsupportedError signals a genex shape the evaluator
// recognizes by op name but deliberately refuses — for cases
// the (a) shape can't safely evaluate at convert-element-cmake
// time. The lifter pattern-matches on this error to decide
// whether to fall back to (b) / legacy (UnsupportedError → try
// next shape) vs surface a real bug (any other error → bail).
type UnsupportedError struct {
	Op     string
	Reason string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("genex `$<%s>`: %s", e.Op, e.Reason)
}

// Eval evaluates parsed nodes against ctx and returns the
// concatenated bytes. Returns an UnsupportedError for genexes
// the v1 evaluator deliberately doesn't model (target-
// evaluator-dependent ops like `$<TARGET_FILE:...>`); any
// other error is a genuine evaluation failure (e.g., missing
// Context field for an op the evaluator does support).
func Eval(nodes []Node, ctx Context) ([]byte, error) {
	var buf bytes.Buffer
	for _, n := range nodes {
		switch v := n.(type) {
		case chunkNode:
			buf.Write(v.Bytes)
		case genexNode:
			b, err := evalGenex(v, ctx)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		default:
			return nil, fmt.Errorf("internal: unknown node type %T", n)
		}
	}
	return buf.Bytes(), nil
}

// evalGenex dispatches a single genex by op name.
func evalGenex(g genexNode, ctx Context) ([]byte, error) {
	switch g.Op {
	// Parameterless or single-arg config / compiler / platform
	// queries.
	case "CONFIG":
		return evalConfig(g, ctx)
	case "COMPILER_ID":
		return evalCompilerID(g, ctx)
	case "PLATFORM_ID":
		return evalPlatformID(g, ctx)
	case "COMPILER_LANGUAGE":
		return evalCompilerLanguage(g, ctx)

	// Boolean combinators.
	case "AND":
		return evalAnd(g, ctx)
	case "OR":
		return evalOr(g, ctx)
	case "NOT":
		return evalNot(g, ctx)
	case "IF":
		return evalIf(g, ctx)
	case "BOOL":
		return evalBool(g, ctx)

	// String operations.
	case "UPPER_CASE":
		return evalUpperCase(g, ctx)
	case "LOWER_CASE":
		return evalLowerCase(g, ctx)
	case "STREQUAL":
		return evalStreq(g, ctx)

	// The literal-boolean / digit-expression form: `$<0:str>`,
	// `$<1:str>`. cmake treats these as conditional emit.
	case "0":
		return nil, nil // `$<0:str>` → empty
	case "1":
		return evalLiteralOne(g, ctx)

	// Per-target ops the evaluator models. TARGET_PROPERTY
	// resolves both the cmake-direct properties (NAME / TYPE /
	// SOURCES / IMPORTED) and the four INTERFACE_* aggregates
	// (INCLUDE_DIRECTORIES, COMPILE_DEFINITIONS, COMPILE_OPTIONS,
	// LINK_LIBRARIES); INTERFACE_LINK_OPTIONS rides via the
	// probe-genex hook path only.
	case "TARGET_PROPERTY":
		return evalTargetProperty(g, ctx)

	// $<TARGET_FILE:t> — resolves via Context.Targets[t].FileLocation.
	// The lifter populates FileLocation at convert time with
	// cmake's recorded artifact path (for the byte-equal check)
	// and at Bazel time the cmake-configure-file tool overrides
	// it via --target-file=<name>=$(location //pkg:t) so Bazel
	// substitutes the action-time path.
	case "TARGET_FILE":
		return evalTargetFile(g, ctx)

	// On-disk-path variants that derive from FileLocation.
	// Unix v1: LINKER_FILE / SONAME_FILE are the same artifact
	// as TARGET_FILE (no Windows import-library distinction,
	// no Mach-O SONAME distinction). FILE_DIR / FILE_NAME are
	// filepath.Dir / filepath.Base of FileLocation. All six
	// reuse the lifter's existing --target-file flag wire —
	// the evaluator computes the derivation at Bazel time
	// against the same FileLocation override the
	// cmake-configure-file tool plumbs in. The byte-equal
	// check at convert time catches any cross-platform
	// disagreement (e.g. cmake on Windows would render a
	// `.lib` for LINKER_FILE; our Linux-alias would render
	// the `.dll` and the verify-pass would fail, falling
	// back to (b)/legacy).
	case "TARGET_FILE_DIR":
		return evalTargetFileDerived(g, ctx, "TARGET_FILE_DIR", filepathDir)
	case "TARGET_FILE_NAME":
		return evalTargetFileDerived(g, ctx, "TARGET_FILE_NAME", filepathBase)
	case "TARGET_LINKER_FILE", "TARGET_SONAME_FILE":
		return evalTargetFileDerived(g, ctx, g.Op, identityPath)
	case "TARGET_LINKER_FILE_DIR":
		return evalTargetFileDerived(g, ctx, g.Op, filepathDir)
	case "TARGET_LINKER_FILE_NAME":
		return evalTargetFileDerived(g, ctx, g.Op, filepathBase)

	// Target-evaluator-dependent forms. Typed refusal so the
	// lifter knows to fall back rather than treat as a bug.
	//
	// Note: COMPILE_LANGUAGE here is cmake's per-source dispatch
	// genex (e.g. `$<COMPILE_LANGUAGE:CXX>` evaluates to 1/0
	// based on the language being compiled when this property
	// is read by cmake's target-evaluator) — distinct from the
	// configure-time-evaluator's Context.CompilerLanguage field
	// (which the supported `COMPILER_LANGUAGE` op above
	// consults). Easy to confuse from the names; the rule of
	// thumb is "COMPILE_*" is cmake's per-source-file dispatch
	// (target-evaluator-time) and "COMPILER_*" is the
	// compiler-identity context the configure-time evaluator
	// already has from CMAKE_<LANG>_COMPILER_ID.
	// $<TARGET_OBJECTS:t> — resolves via Context.Targets[t].Objects.
	// Populated by lifters that have cmake's resolved object list;
	// Phase 3 of the generator-parity uplift sources this from the
	// probe-genex hook's per-target objects.txt file (which cmake
	// emits only for OBJECT_LIBRARY targets at generation time).
	// Empty Objects surfaces as UnsupportedError so the lifter
	// falls back to (b) / legacy.
	case "TARGET_OBJECTS":
		return evalTargetObjects(g, ctx)

	case "TARGET_GENEX_EVAL", "GENEX_EVAL",
		"INSTALL_INTERFACE", "BUILD_INTERFACE", "INSTALL_PREFIX",
		"COMPILE_LANGUAGE", "LINK_LANGUAGE",
		"COMPILE_FEATURES":
		return nil, &UnsupportedError{Op: g.Op, Reason: "target-evaluator-dependent; v1 evaluator does not model this"}
	}
	return nil, &UnsupportedError{Op: g.Op, Reason: "unknown genex op (v1 evaluator only models the configure-time-resolvable subset)"}
}

// evalConfig: $<CONFIG> -> ctx.Config; $<CONFIG:cfg,...> ->
// "1" if ctx.Config matches any case-insensitively, else "0".
// cmake matches case-insensitively per the docs:
// https://cmake.org/cmake/help/latest/manual/cmake-generator-expressions.7.html#genex:CONFIG
func evalConfig(g genexNode, ctx Context) ([]byte, error) {
	if g.Args == nil {
		if ctx.Config == "" {
			return nil, &UnsupportedError{Op: g.Op, Reason: "Context.Config is empty"}
		}
		return []byte(ctx.Config), nil
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range args {
		if strings.EqualFold(a, ctx.Config) {
			return []byte("1"), nil
		}
	}
	return []byte("0"), nil
}

// evalCompilerID consults ctx.CompilerID[ctx.CompilerLanguage]
// when language is set; otherwise picks an arbitrary entry —
// for file(GENERATE) the v1 path doesn't have a per-source
// language, and most cmake projects' C / CXX compiler ids
// match.
func evalCompilerID(g genexNode, ctx Context) ([]byte, error) {
	id, err := pickCompilerID(ctx)
	if err != nil {
		return nil, &UnsupportedError{Op: g.Op, Reason: err.Error()}
	}
	if g.Args == nil {
		return []byte(id), nil
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range args {
		if a == id {
			return []byte("1"), nil
		}
	}
	return []byte("0"), nil
}

func pickCompilerID(ctx Context) (string, error) {
	if ctx.CompilerLanguage != "" {
		if id, ok := ctx.CompilerID[ctx.CompilerLanguage]; ok && id != "" {
			return id, nil
		}
		return "", fmt.Errorf("Context.CompilerID has no entry for language %q", ctx.CompilerLanguage)
	}
	// No language hint — pick any. For typical projects C and
	// CXX agree; refusing because we can't pin a single one
	// would be over-conservative.
	for _, lang := range []string{"C", "CXX", "OBJC", "OBJCXX", "Fortran"} {
		if id, ok := ctx.CompilerID[lang]; ok && id != "" {
			return id, nil
		}
	}
	// Last resort: any entry. Map iteration order is randomized
	// in Go, so sort the keys first to keep this deterministic
	// across runs — a non-deterministic CompilerID pick would
	// flip lifted-output bytes between convert-element-cmake
	// invocations and surface as a flaky srckey.
	keys := make([]string, 0, len(ctx.CompilerID))
	for k := range ctx.CompilerID {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if id := ctx.CompilerID[k]; id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("Context.CompilerID is empty")
}

// evalPlatformID: $<PLATFORM_ID> -> ctx.PlatformID;
// $<PLATFORM_ID:id,...> -> "1"/"0" exact match.
func evalPlatformID(g genexNode, ctx Context) ([]byte, error) {
	if ctx.PlatformID == "" {
		return nil, &UnsupportedError{Op: g.Op, Reason: "Context.PlatformID is empty"}
	}
	if g.Args == nil {
		return []byte(ctx.PlatformID), nil
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range args {
		if a == ctx.PlatformID {
			return []byte("1"), nil
		}
	}
	return []byte("0"), nil
}

// evalCompilerLanguage: $<COMPILER_LANGUAGE:lang,...> -> "1"
// if ctx.CompilerLanguage matches any. v1 requires
// CompilerLanguage to be set — file(GENERATE) callers without
// it get UnsupportedError (rare in practice; file(GENERATE)
// rarely references this).
func evalCompilerLanguage(g genexNode, ctx Context) ([]byte, error) {
	if ctx.CompilerLanguage == "" {
		return nil, &UnsupportedError{Op: g.Op, Reason: "Context.CompilerLanguage is empty"}
	}
	if g.Args == nil {
		return []byte(ctx.CompilerLanguage), nil
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range args {
		if a == ctx.CompilerLanguage {
			return []byte("1"), nil
		}
	}
	return []byte("0"), nil
}

// evalAnd / evalOr: cmake's docs spec the truthy-string set
// as exactly "1", "TRUE", "YES", "Y", "ON" (case-insensitive
// for some, exact for others). v1 follows the strict cmake
// interpretation: only "1" is truthy, anything else (incl.
// "0", "TRUE", empty) is falsy. cmake's actual rules are
// looser but lifting the looser set under (a) risks divergence
// from cmake's evaluation when the operator uses a non-"1"
// truthy form; the conservative interpretation surfaces those
// as UnsupportedError, which falls back to (b) / legacy where
// cmake's literal output bytes win.
func evalAnd(g genexNode, ctx Context) ([]byte, error) {
	for _, arg := range g.Args {
		b, err := evalArg(arg, ctx)
		if err != nil {
			return nil, err
		}
		t, err := bool01(b, g.Op)
		if err != nil {
			return nil, err
		}
		if !t {
			return []byte("0"), nil
		}
	}
	return []byte("1"), nil
}

func evalOr(g genexNode, ctx Context) ([]byte, error) {
	for _, arg := range g.Args {
		b, err := evalArg(arg, ctx)
		if err != nil {
			return nil, err
		}
		t, err := bool01(b, g.Op)
		if err != nil {
			return nil, err
		}
		if t {
			return []byte("1"), nil
		}
	}
	return []byte("0"), nil
}

func evalNot(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 1 {
		return nil, fmt.Errorf("$<NOT:...> requires exactly 1 arg, got %d", len(g.Args))
	}
	b, err := evalArg(g.Args[0], ctx)
	if err != nil {
		return nil, err
	}
	t, err := bool01(b, g.Op)
	if err != nil {
		return nil, err
	}
	if t {
		return []byte("0"), nil
	}
	return []byte("1"), nil
}

func evalIf(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 3 {
		return nil, fmt.Errorf("$<IF:cond,then,else> requires exactly 3 args, got %d", len(g.Args))
	}
	cond, err := evalArg(g.Args[0], ctx)
	if err != nil {
		return nil, err
	}
	t, err := bool01(cond, g.Op)
	if err != nil {
		return nil, err
	}
	if t {
		return evalArg(g.Args[1], ctx)
	}
	return evalArg(g.Args[2], ctx)
}

// evalBool: $<BOOL:str> per cmake docs is "1" if str is a non-
// empty string that doesn't represent zero, false, etc. v1
// keeps the strict interpretation: empty string or "0" → "0",
// everything else → "1". cmake's looser rules are
// UnsupportedError when they'd matter (e.g., "FALSE" / "NO"
// would evaluate to "1" here but "0" in cmake; v1 errs on the
// side of refusal to avoid silent divergence).
func evalBool(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 1 {
		return nil, fmt.Errorf("$<BOOL:...> requires exactly 1 arg, got %d", len(g.Args))
	}
	b, err := evalArg(g.Args[0], ctx)
	if err != nil {
		return nil, err
	}
	s := string(b)
	if s == "" || s == "0" {
		return []byte("0"), nil
	}
	switch strings.ToUpper(s) {
	case "FALSE", "NO", "N", "OFF":
		// cmake-falsy forms beyond "0" — refuse rather than
		// risk divergence with cmake's full ruleset.
		return nil, &UnsupportedError{Op: g.Op, Reason: fmt.Sprintf("non-canonical boolean value %q; v1 only models \"\" / \"0\" → 0 and \"1\" → 1", s)}
	}
	return []byte("1"), nil
}

func evalUpperCase(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 1 {
		return nil, fmt.Errorf("$<UPPER_CASE:...> requires exactly 1 arg, got %d", len(g.Args))
	}
	b, err := evalArg(g.Args[0], ctx)
	if err != nil {
		return nil, err
	}
	return []byte(strings.ToUpper(string(b))), nil
}

func evalLowerCase(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 1 {
		return nil, fmt.Errorf("$<LOWER_CASE:...> requires exactly 1 arg, got %d", len(g.Args))
	}
	b, err := evalArg(g.Args[0], ctx)
	if err != nil {
		return nil, err
	}
	return []byte(strings.ToLower(string(b))), nil
}

func evalStreq(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 2 {
		return nil, fmt.Errorf("$<STREQUAL:s1,s2> requires exactly 2 args, got %d", len(g.Args))
	}
	a, err := evalArg(g.Args[0], ctx)
	if err != nil {
		return nil, err
	}
	b, err := evalArg(g.Args[1], ctx)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(a, b) {
		return []byte("1"), nil
	}
	return []byte("0"), nil
}

// evalLiteralOne: $<1:str> emits str. The dispatcher routes
// `1` as the op name because cmake's `$<<bool>:str>` form
// makes the boolean digit appear in the op slot.
func evalLiteralOne(g genexNode, ctx Context) ([]byte, error) {
	if g.Args == nil {
		// `$<1>` standalone — meaningless in cmake; refuse.
		return nil, &UnsupportedError{Op: g.Op, Reason: "literal `$<1>` without a `:str` arm is not a meaningful genex"}
	}
	// Concatenate all args (joined by commas in cmake's literal
	// semantic when there are multiple, but cmake usually has
	// exactly one). We follow cmake: if multiple args, join with
	// commas — same shape as serializing arg bytes through.
	parts := make([][]byte, len(g.Args))
	for i, a := range g.Args {
		b, err := evalArg(a, ctx)
		if err != nil {
			return nil, err
		}
		parts[i] = b
	}
	return bytes.Join(parts, []byte(",")), nil
}

// evalArg evaluates an arg's node slice and returns the
// concatenated bytes. An empty arg (`$<IF:1,,b>`'s middle
// arg) yields an empty byte slice with no error.
func evalArg(arg []Node, ctx Context) ([]byte, error) {
	if len(arg) == 0 {
		return nil, nil
	}
	return Eval(arg, ctx)
}

// evalArgsToStrings is a convenience for ops that want each
// arg as a string for simple membership tests (CONFIG,
// COMPILER_ID, PLATFORM_ID).
func evalArgsToStrings(args [][]Node, ctx Context) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		b, err := evalArg(a, ctx)
		if err != nil {
			return nil, err
		}
		out[i] = string(b)
	}
	return out, nil
}

// bool01 interprets b as the cmake-canonical "0" / "1" booleans
// this evaluator produces. Anything else (typically because a
// sub-genex evaluator emitted a non-boolean string for use in
// AND/OR/NOT/IF) surfaces as UnsupportedError so the lifter
// falls back rather than guess.
func bool01(b []byte, op string) (bool, error) {
	switch string(b) {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, &UnsupportedError{Op: op, Reason: fmt.Sprintf("non-canonical boolean value %q (expected \"0\" or \"1\")", b)}
}

// evalTargetProperty handles `$<TARGET_PROPERTY:t,prop>`. The
// v1 evaluator models the subset of properties cmake reports
// verbatim from the fileapi codemodel — NAME, TYPE, SOURCES,
// IMPORTED. Properties cmake aggregates from a target's
// dependencies (INTERFACE_INCLUDE_DIRECTORIES,
// INTERFACE_COMPILE_OPTIONS, INTERFACE_LINK_LIBRARIES, ...)
// surface as UnsupportedError until the lifter grows the
// matching aggregation pipeline; the lifter then falls back
// to (b) capture or legacy.
//
// Two-arg form only — the legacy one-arg form
// `$<TARGET_PROPERTY:prop>` (which reads from the "current
// target" at target-evaluator time) has no convert-time
// equivalent for file(GENERATE) templates (file(GENERATE) has
// no current target) and remains UnsupportedError.
func evalTargetProperty(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 2 {
		return nil, &UnsupportedError{
			Op:     g.Op,
			Reason: fmt.Sprintf("expected 2 args (target, property); got %d", len(g.Args)),
		}
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	name := args[0]
	prop := args[1]
	ti, ok := ctx.Targets[name]
	if !ok {
		return nil, &UnsupportedError{
			Op:     g.Op,
			Reason: fmt.Sprintf("no target %q in Context.Targets", name),
		}
	}
	switch prop {
	case "NAME":
		return []byte(name), nil
	case "TYPE":
		if ti.Type == "" {
			return nil, &UnsupportedError{Op: g.Op, Reason: fmt.Sprintf("Context.Targets[%q].Type is empty", name)}
		}
		return []byte(ti.Type), nil
	case "SOURCES":
		return []byte(ti.Sources), nil
	case "IMPORTED":
		if ti.Imported {
			return []byte("TRUE"), nil
		}
		return []byte("FALSE"), nil

	// INTERFACE_* aggregates. cmake's generator-phase evaluator
	// walks the dependency graph and emits the post-walk values;
	// Phase 3's probe-genex hook captures those at generation
	// time and the lifter loads them into TargetInfo here.
	// Empty values are valid (the target has no
	// INTERFACE_<P> set) — distinct from "field not populated"
	// (no probe ran). The Reasons machinery in
	// internal/exportshape (etc.) doesn't apply here; the
	// evaluator can't tell the two cases apart from the struct
	// alone, so empty values just resolve to the empty string
	// (matching cmake's own behavior for an unset INTERFACE_*).
	case "INTERFACE_INCLUDE_DIRECTORIES":
		return []byte(ti.InterfaceIncludeDirectories), nil
	case "INTERFACE_COMPILE_DEFINITIONS":
		return []byte(ti.InterfaceCompileDefinitions), nil
	case "INTERFACE_COMPILE_OPTIONS":
		return []byte(ti.InterfaceCompileOptions), nil
	case "INTERFACE_LINK_LIBRARIES":
		return []byte(ti.InterfaceLinkLibraries), nil
	case "INTERFACE_LINK_OPTIONS":
		return []byte(ti.InterfaceLinkOptions), nil
	}
	return nil, &UnsupportedError{
		Op:     g.Op,
		Reason: fmt.Sprintf("property %q not in the v1 evaluator's supported set (NAME, TYPE, SOURCES, IMPORTED, INTERFACE_INCLUDE_DIRECTORIES, INTERFACE_COMPILE_DEFINITIONS, INTERFACE_COMPILE_OPTIONS, INTERFACE_LINK_LIBRARIES, INTERFACE_LINK_OPTIONS)", prop),
	}
}

// evalTargetObjects handles `$<TARGET_OBJECTS:t>`. Returns
// Context.Targets[t].Objects when populated (single-arg form
// only; the multi-arg form isn't a thing for TARGET_OBJECTS).
// Empty Objects surfaces as UnsupportedError so the lifter falls
// back to (b) / legacy — the typical reason for empty Objects is
// the target isn't an OBJECT_LIBRARY (other target types have no
// per-target object list).
func evalTargetObjects(g genexNode, ctx Context) ([]byte, error) {
	if len(g.Args) != 1 {
		return nil, &UnsupportedError{
			Op:     g.Op,
			Reason: fmt.Sprintf("expected 1 arg (target name); got %d", len(g.Args)),
		}
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	name := args[0]
	ti, ok := ctx.Targets[name]
	if !ok {
		return nil, &UnsupportedError{
			Op:     g.Op,
			Reason: fmt.Sprintf("no target %q in Context.Targets", name),
		}
	}
	if ti.Objects == "" {
		return nil, &UnsupportedError{
			Op:     g.Op,
			Reason: fmt.Sprintf("Context.Targets[%q].Objects is empty (target isn't OBJECT_LIBRARY, or probe-genex hook didn't run)", name),
		}
	}
	return []byte(ti.Objects), nil
}

// evalTargetFile handles `$<TARGET_FILE:t>` — single-arg form
// only (cmake's multi-arg form isn't a thing for TARGET_FILE).
// Resolves to ctx.Targets[t].FileLocation. Empty FileLocation
// surfaces as UnsupportedError so the lifter can fall back to
// (b) / legacy when the target isn't in the captured set or
// the Bazel-time --target-file override didn't fire.
func evalTargetFile(g genexNode, ctx Context) ([]byte, error) {
	return evalTargetFileDerived(g, ctx, "TARGET_FILE", identityPath)
}

// evalTargetFileDerived handles `$<TARGET_FILE:t>` AND its
// six on-disk-path variants (FILE_DIR, FILE_NAME, LINKER_FILE,
// LINKER_FILE_DIR, LINKER_FILE_NAME, SONAME_FILE). All derive
// from ctx.Targets[t].FileLocation; derive picks the
// transformation:
//
//   - identityPath: TARGET_FILE / TARGET_LINKER_FILE /
//     TARGET_SONAME_FILE (Linux v1 aliases).
//   - filepathDir: TARGET_FILE_DIR / TARGET_LINKER_FILE_DIR.
//   - filepathBase: TARGET_FILE_NAME / TARGET_LINKER_FILE_NAME.
//
// opName is the user-facing op label for diagnostics — passed
// in rather than read from g.Op so the TARGET_FILE wrapper
// above can route through this same helper without losing the
// "TARGET_FILE" surface in error messages.
func evalTargetFileDerived(g genexNode, ctx Context, opName string, derive func(string) string) ([]byte, error) {
	if len(g.Args) != 1 {
		return nil, &UnsupportedError{
			Op:     opName,
			Reason: fmt.Sprintf("expected 1 arg (target); got %d", len(g.Args)),
		}
	}
	args, err := evalArgsToStrings(g.Args, ctx)
	if err != nil {
		return nil, err
	}
	name := args[0]
	ti, ok := ctx.Targets[name]
	if !ok {
		return nil, &UnsupportedError{
			Op:     opName,
			Reason: fmt.Sprintf("no target %q in Context.Targets", name),
		}
	}
	if ti.FileLocation == "" {
		return nil, &UnsupportedError{
			Op:     opName,
			Reason: fmt.Sprintf("Context.Targets[%q].FileLocation is empty (lifter didn't capture, or Bazel-time --target-file flag missing)", name),
		}
	}
	return []byte(derive(ti.FileLocation)), nil
}

// identityPath / filepathDir / filepathBase are the three
// derivations evalTargetFileDerived dispatches over. Module-
// level vars so the dispatch table reads cleanly.
var (
	identityPath = func(p string) string { return p }
	filepathDir  = filepath.Dir
	filepathBase = filepath.Base
)
