// Package verify cross-checks the converter's lowering output against
// independent witnesses cmake produced. The intent is to catch
// regressions that schema-version checks alone miss — e.g., a
// flag-classification bug that silently drops a -D, or an include
// that didn't make it from CompileGroup into the IR.
//
// compile_commands.json is the strongest witness available: it carries
// the fully-expanded per-source compile command, including resolved
// generator expressions and all -I / -D / flags, as cmake would
// actually invoke the compiler. It is a separate code path from the
// codemodel-v2 File API the converter normally consumes, so a
// disagreement between the two surfaces a real lowering issue.
//
// The diff is intentionally a *set* comparison over -D macros and -I
// include directories. Token order, optimisation flags, and
// per-toolchain quirks are out of scope; we only flag adds/drops to
// keep false-positive noise low. Mismatches are reported as warnings
// (the caller decides whether to surface them as stderr lines or as
// a structured report file).
package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/ir"
)

// Mismatch is one disagreement between compile_commands.json and the
// IR for a single source file.
type Mismatch struct {
	// File is the package-relative source path (e.g. "src/foo.c").
	File string `json:"file"`
	// Target is the IR target that owns File, or "" if File appears
	// in compile_commands.json but no IR target claims it.
	Target string `json:"target,omitempty"`
	// Kind identifies the diff: "missing-define", "extra-define",
	// "missing-include", "extra-include", or "orphan-source".
	Kind string `json:"kind"`
	// Detail is the offending token (a -D macro or -I path).
	Detail string `json:"detail,omitempty"`
}

// Report is the structured form of Verify's output. Empty Mismatches
// means the IR matched compile_commands.json on the dimensions we
// check.
type Report struct {
	// Source is the absolute path to the compile_commands.json that
	// produced this report. Recorded so external readers can correlate
	// it with the cmake build dir.
	Source string `json:"source"`
	// Mismatches is the per-source diff list, sorted by file then
	// kind for stable output.
	Mismatches []Mismatch `json:"mismatches"`
}

// compileCmd mirrors one entry in compile_commands.json. CMake's
// generator emits the "command" form (single string) on most
// platforms; "arguments" (string array) is the alternative shape some
// generators produce. We accept both.
type compileCmd struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Command   string   `json:"command,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
}

// Verify reads compile_commands.json at path and compares its
// per-source compile flags against pkg's IR. sourceRoot is the
// absolute path the IR's source-relative paths resolve against.
//
// Returns a Report; the caller decides how to surface it. Missing
// compile_commands.json is not an error — we return an empty report
// (the converter ran in --reply-dir mode against a fixture that
// didn't include it).
func Verify(path string, pkg *ir.Package, sourceRoot string) (*Report, error) {
	rep := &Report{Source: path}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return nil, fmt.Errorf("verify: read %s: %w", path, err)
	}
	var cmds []compileCmd
	if err := json.Unmarshal(body, &cmds); err != nil {
		return nil, fmt.Errorf("verify: parse %s: %w", path, err)
	}
	// filepath.Rel requires both args to be absolute or both relative;
	// callers can pass either, so normalize once here. Without this,
	// every -I comparison degrades to the absolute-path fallback and
	// every IR include surfaces as a "missing" mismatch.
	absSourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("verify: abs sourceRoot %q: %w", sourceRoot, err)
	}
	sourceRoot = absSourceRoot

	// Build a source-path -> target index for O(1) lookup. Sources are
	// stored in the IR as package-relative slash paths; we normalise
	// compile_commands' absolute paths to the same form.
	srcToTarget := map[string]*ir.Target{}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		for _, s := range t.Srcs {
			srcToTarget[filepath.ToSlash(s)] = t
		}
	}

	for _, c := range cmds {
		rel, ok := relUnder(sourceRoot, c.File)
		if !ok {
			// File outside source root — generated source under build/,
			// or a vendored toolchain bit. We have no IR target to compare
			// against; skip rather than warn (false-positive prone).
			continue
		}
		t, ok := srcToTarget[rel]
		if !ok {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				File: rel,
				Kind: "orphan-source",
			})
			continue
		}

		gotDefs, gotIncs := extractDefsAndIncs(c, sourceRoot)
		wantDefs, wantIncs := irDefsAndIncs(t, sourceRoot)

		for d := range diffSet(gotDefs, wantDefs) {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				File: rel, Target: t.Name, Kind: "missing-define", Detail: d,
			})
		}
		for d := range diffSet(wantDefs, gotDefs) {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				File: rel, Target: t.Name, Kind: "extra-define", Detail: d,
			})
		}
		for i := range diffSet(gotIncs, wantIncs) {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				File: rel, Target: t.Name, Kind: "missing-include", Detail: i,
			})
		}
		for i := range diffSet(wantIncs, gotIncs) {
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				File: rel, Target: t.Name, Kind: "extra-include", Detail: i,
			})
		}
	}

	sort.Slice(rep.Mismatches, func(i, j int) bool {
		a, b := rep.Mismatches[i], rep.Mismatches[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Detail < b.Detail
	})
	return rep, nil
}

// extractDefsAndIncs walks a compile_commands entry's tokens and
// returns sets of -D macros and -I directories (the latter as
// source-root-relative paths when possible, absolute otherwise).
//
// Splitting "command" on whitespace is a deliberate simplification —
// quoting subtleties (e.g. -DFOO="bar baz") would need a real shell
// tokenizer to handle precisely, but the percentage of compile flags
// that depend on that subtlety is small and false positives are
// caller-decided warnings, not errors.
func extractDefsAndIncs(c compileCmd, sourceRoot string) (defs, incs map[string]bool) {
	defs = map[string]bool{}
	incs = map[string]bool{}

	tokens := c.Arguments
	if len(tokens) == 0 && c.Command != "" {
		tokens = strings.Fields(c.Command)
	}

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		case strings.HasPrefix(tok, "-D"):
			defs[strings.TrimPrefix(tok, "-D")] = true
		case strings.HasPrefix(tok, "-I"):
			rest := strings.TrimPrefix(tok, "-I")
			if rest == "" && i+1 < len(tokens) {
				rest = tokens[i+1]
				i++
			}
			incs[normIncludeDir(rest, c.Directory, sourceRoot)] = true
		case tok == "-isystem" && i+1 < len(tokens):
			incs[normIncludeDir(tokens[i+1], c.Directory, sourceRoot)] = true
			i++
		}
	}
	return defs, incs
}

// irDefsAndIncs returns the same {-D, -I} sets reconstructed from the
// IR target. Both sides go through normIncludeDir so an IR-relative
// "include" and a compile_commands absolute "/abs/.../include"
// collapse to the same key.
func irDefsAndIncs(t *ir.Target, sourceRoot string) (defs, incs map[string]bool) {
	defs = map[string]bool{}
	incs = map[string]bool{}
	for _, d := range t.Defines {
		defs[d] = true
	}
	for _, inc := range t.Includes {
		incs[normIncludeDir(inc, sourceRoot, sourceRoot)] = true
	}
	// Copts can carry -I and -D entries too (PRIVATE includes flow
	// through as -I in copts, see lower.go). Walk them and harvest
	// the same way as compile_commands.
	for i := 0; i < len(t.Copts); i++ {
		tok := t.Copts[i]
		switch {
		case strings.HasPrefix(tok, "-D"):
			defs[strings.TrimPrefix(tok, "-D")] = true
		case strings.HasPrefix(tok, "-I"):
			rest := strings.TrimPrefix(tok, "-I")
			if rest == "" && i+1 < len(t.Copts) {
				rest = t.Copts[i+1]
				i++
			}
			incs[normIncludeDir(rest, sourceRoot, sourceRoot)] = true
		}
	}
	return defs, incs
}

// normIncludeDir resolves an -I path to a stable comparison form.
// Paths under sourceRoot become slash-form package-relative; paths
// outside (toolchain dirs, build dir, abs prefixes) stay absolute
// and slash-form.
func normIncludeDir(p, baseDir, sourceRoot string) string {
	if !filepath.IsAbs(p) && baseDir != "" {
		p = filepath.Join(baseDir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	if rel, err := filepath.Rel(sourceRoot, abs); err == nil &&
		!strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// diffSet returns elements in a not in b.
func diffSet(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

// relUnder returns the slash-form path of file relative to root, or
// ("", false) if file isn't inside root.
func relUnder(root, file string) (string, bool) {
	absFile, err := filepath.Abs(file)
	if err != nil {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absRoot, absFile)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// FormatMismatches returns a human-readable rendering of rep suitable
// for stderr. One line per mismatch; an empty list returns "".
func FormatMismatches(rep *Report) string {
	if rep == nil || len(rep.Mismatches) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range rep.Mismatches {
		fmt.Fprintf(&b, "verify: %s [%s] %s", m.File, m.Kind, m.Detail)
		if m.Target != "" {
			fmt.Fprintf(&b, " (target %s)", m.Target)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
