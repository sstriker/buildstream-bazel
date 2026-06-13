package shadow

import (
	"regexp"
	"strings"
)

// ExtractVariableReads walks a NON-EXPANDED cmake trace (`cmake --trace`,
// not `--trace-expand`) and returns the set of variable names the
// configure READS anywhere after capture. The dead-capture analysis
// (lower's execute_process recovery) uses it to distinguish a capture
// that feeds configure logic from one that exists only to SILENCE
// console output — the ubiquitous
// `execute_process(... OUTPUT_VARIABLE _quiet ERROR_VARIABLE _quiet)`
// idiom — which can safely be treated as if the keyword were absent.
//
// Read shapes counted (deliberately conservative — over-counting a read
// only KEEPS a refusal, never widens a lift):
//
//   - `${NAME}` occurrences in ANY argument of ANY command. The
//     non-expanded trace records arguments verbatim, so every textual
//     dereference is visible. ($ENV{...} and $CACHE{...} are different
//     namespaces and excluded; a generator-expression `$<...>` carries
//     no `{`, so it can't false-positive here.)
//   - Bare identifier arguments of the auto-dereferencing commands —
//     if() / elseif() / while() (`if(NOT _rc EQUAL 0)`), foreach()
//     (`foreach(x IN LISTS items)`), and the name-taking readers
//     list() / math() / separate_arguments() — where cmake reads a
//     variable WITHOUT `${}`. Every identifier-shaped token in those
//     commands counts, operators and keywords included when they
//     happen to match a variable name; that over-approximation is the
//     conservative direction.
//
// Variable INDIRECTION (`${${prefix}_out}`) appears textually as the
// inner name only; the outer composite read is invisible. A capture
// consumed solely through indirection would therefore look dead. The
// composite name still contains a `${` boundary in the trace, so the
// inner variable IS counted — and captures are concrete names, so the
// gap requires the capture name itself to be composed, which the
// classifiers' expanded-trace view records concretely. Accepted
// residual risk, mirrored in the dead-capture call sites' docs.
func ExtractVariableReads(traceRaw []byte) map[string]bool {
	reads := map[string]bool{}
	for _, ev := range ParseTrace(traceRaw) {
		for _, a := range ev.Args {
			for _, m := range variableRefPattern.FindAllStringSubmatch(a, -1) {
				reads[m[1]] = true
			}
		}
		if autoDerefCommands[strings.ToLower(ev.Cmd)] {
			for _, a := range ev.Args {
				if identifierPattern.MatchString(a) {
					reads[a] = true
				}
			}
		}
	}
	return reads
}

// variableRefPattern matches a `${NAME}` dereference. cmake variable
// names are nearly unrestricted; this covers the names that occur in
// practice (alnum, underscore, dash, dot, slash, plus). A leading
// `$ENV{` / `$CACHE{` prefix is excluded by requiring the character
// before `{` to be `$` directly.
var variableRefPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_.+/-]+)\}`)

// identifierPattern matches a bare token an auto-dereferencing command
// would treat as a variable name.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.+-]*$`)

// autoDerefCommands are the commands whose bare identifier arguments
// dereference variables without `${}`.
var autoDerefCommands = map[string]bool{
	"if":                 true,
	"elseif":             true,
	"while":              true,
	"foreach":            true,
	"list":               true,
	"math":               true,
	"separate_arguments": true,
}
