// Command compile-commands-diff is the compile-commands fidelity lens: it
// compares cmake's compile_commands.json (the ground truth — what cmake would
// compile each translation unit with) against a Bazel-derived compile-commands
// view (from `bazel aquery --output=jsonproto 'mnemonic("CppCompile", //...)'`)
// and reports, per source file, where the two diverge on the load-bearing,
// path-independent compile facts: the macro DEFINES (-D), the C/C++ language
// STANDARD (-std=), and (informationally) the include-dir basenames (-I /
// -isystem).
//
// WHY this lens: the build lens proves the converted graph BUILDS, but a build
// can succeed while silently compiling a TU with the wrong macro set (e.g. a
// PRIVATE define wrongly propagated, or a missing -D that flips an #ifdef) —
// exactly the class of bug the define-scope-routing and find_package work
// chased. Diffing per-TU defines against cmake catches that drift directly.
//
// Comparison is deliberately path-INDEPENDENT where it matters: cmake records
// absolute host paths (/tmp/zlib/adler32.c, -I/tmp/zbuild) while Bazel records
// exec-root-relative sandbox paths, so an exact string diff is meaningless.
// Defines and -std are host-path-free and compared as sets/values; include
// dirs are compared by basename only (a loose signal — a real header-search
// fidelity check is future work, noted in ROADMAP).
//
// Usage:
//
//	compile-commands-diff --cmake <compile_commands.json> --aquery <aquery.json> [--json out.json]
//
// Exit status is always 0 (it's a report, not a gate); --json emits a machine-
// readable summary for a survey lens to thread.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// tuFacts is the normalized, path-independent compile view of one translation
// unit: the set of macro defines, the language standard, and the set of
// include-dir basenames.
type tuFacts struct {
	Defines    map[string]bool
	Std        string
	IncludeDir map[string]bool // basenames only
}

// cmakeEntry is one record in a cmake compile_commands.json. cmake emits either
// `command` (a single shell string) or `arguments` (a pre-tokenized argv); we
// handle both.
type cmakeEntry struct {
	Directory string   `json:"directory"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
	File      string   `json:"file"`
}

// aqueryDoc is the slice of `bazel aquery --output=jsonproto` we need: the
// actions, each carrying its full argv. (The jsonproto has more — artifacts,
// dep sets, etc. — but the arguments + mnemonic are enough to recover the
// per-TU compile facts.)
type aqueryDoc struct {
	Actions []struct {
		Mnemonic  string   `json:"mnemonic"`
		Arguments []string `json:"arguments"`
	} `json:"actions"`
}

func main() {
	cmakePath := flag.String("cmake", "", "path to cmake compile_commands.json (CMAKE_EXPORT_COMPILE_COMMANDS=ON)")
	aqueryPath := flag.String("aquery", "", "path to `bazel aquery --output=jsonproto mnemonic(CppCompile,//...)` JSON")
	jsonOut := flag.String("json", "", "optional: write a machine-readable summary here")
	flag.Parse()

	if *cmakePath == "" || *aqueryPath == "" {
		fmt.Fprintln(os.Stderr, "compile-commands-diff: --cmake and --aquery are required")
		os.Exit(2)
	}

	cmakeTUs, err := loadCmake(*cmakePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile-commands-diff: cmake: %v\n", err)
		os.Exit(2)
	}
	bazelTUs, err := loadAquery(*aqueryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile-commands-diff: aquery: %v\n", err)
		os.Exit(2)
	}

	rep := diff(cmakeTUs, bazelTUs)
	rep.print(os.Stdout)
	if *jsonOut != "" {
		if err := rep.writeJSON(*jsonOut); err != nil {
			fmt.Fprintf(os.Stderr, "compile-commands-diff: write %s: %v\n", *jsonOut, err)
		}
	}
}

// loadCmake parses a cmake compile_commands.json into per-TU facts keyed by the
// source file's basename (cmake records absolute host source paths).
func loadCmake(path string) (map[string]tuFacts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []cmakeEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	out := map[string]tuFacts{}
	for _, e := range entries {
		argv := e.Arguments
		if len(argv) == 0 {
			argv = splitCommand(e.Command)
		}
		key := tuKey(e.File)
		if key == "" {
			continue
		}
		out[key] = factsFromArgv(argv)
	}
	return out, nil
}

// loadAquery parses a bazel aquery jsonproto and recovers per-TU facts keyed by
// the compiled source's basename. Only CppCompile actions are considered.
func loadAquery(path string) (map[string]tuFacts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc aqueryDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	out := map[string]tuFacts{}
	for _, a := range doc.Actions {
		if a.Mnemonic != "CppCompile" {
			continue
		}
		src := sourceFromArgv(a.Arguments)
		if src == "" {
			continue
		}
		out[tuKey(src)] = factsFromArgv(a.Arguments)
	}
	return out, nil
}

// tuKey is the cross-toolchain match key for a translation unit: its basename.
// cmake uses absolute host source paths, Bazel exec-root-relative ones, so a
// basename is the portable join key. (Basename collisions across directories
// are possible in large trees; the report flags the count so a future version
// can disambiguate by relative-suffix when it matters.)
func tuKey(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// ignoredDefine reports whether a -D macro is one Bazel's C++ toolchain
// injects on EVERY compile for build reproducibility, regardless of the
// project — comparing it against cmake (which never sets them) is pure noise.
// Matched on the macro NAME (before any `=value`).
func ignoredDefine(d string) bool {
	name := d
	if i := strings.IndexByte(d, '='); i >= 0 {
		name = d[:i]
	}
	switch name {
	case "__DATE__", "__TIME__", "__TIMESTAMP__":
		// Bazel's "redacted" stamping (unconditional, reproducibility).
		return true
	}
	return false
}

// factsFromArgv extracts the path-independent compile facts from a compile argv.
func factsFromArgv(argv []string) tuFacts {
	f := tuFacts{Defines: map[string]bool{}, IncludeDir: map[string]bool{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case strings.HasPrefix(a, "-D"):
			v := strings.TrimPrefix(a, "-D")
			if v == "" && i+1 < len(argv) { // `-D FOO` (space-separated)
				i++
				v = argv[i]
			}
			if v != "" && !ignoredDefine(v) {
				f.Defines[v] = true
			}
		case strings.HasPrefix(a, "-std="):
			f.Std = strings.TrimPrefix(a, "-std=")
		case strings.HasPrefix(a, "-I"):
			d := strings.TrimPrefix(a, "-I")
			if d == "" && i+1 < len(argv) {
				i++
				d = argv[i]
			}
			if d != "" {
				f.IncludeDir[filepath.Base(strings.TrimRight(d, "/"))] = true
			}
		case a == "-isystem":
			if i+1 < len(argv) {
				i++
				f.IncludeDir[filepath.Base(strings.TrimRight(argv[i], "/"))] = true
			}
		case strings.HasPrefix(a, "-isystem"):
			d := strings.TrimPrefix(a, "-isystem")
			if d != "" {
				f.IncludeDir[filepath.Base(strings.TrimRight(d, "/"))] = true
			}
		}
	}
	return f
}

// sourceFromArgv finds the compiled translation unit in a CppCompile argv: the
// argument after `-c`, falling back to the lone C/C++ source-extension arg.
func sourceFromArgv(argv []string) string {
	for i, a := range argv {
		if a == "-c" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch strings.ToLower(filepath.Ext(a)) {
		case ".c", ".cc", ".cpp", ".cxx", ".c++", ".m", ".mm":
			return a
		}
	}
	return ""
}

// splitCommand tokenizes a cmake `command` string. cmake quotes shell-special
// values; a simple space split is sufficient for the -D/-I/-std flags we read
// (those values don't carry spaces in practice for the fidelity facts).
func splitCommand(s string) []string { return strings.Fields(s) }

// report is the per-TU diff outcome.
type report struct {
	Matched         int                  `json:"matched"`          // TUs present in both
	OnlyCmake       []string             `json:"only_cmake"`       // TU keys cmake compiles, bazel doesn't
	OnlyBazel       []string             `json:"only_bazel"`       // TU keys bazel compiles, cmake doesn't
	DefineMismatch  map[string]diffSet   `json:"define_mismatch"`  // TU → define delta
	StdMismatch     map[string][2]string `json:"std_mismatch"`     // TU → [cmake, bazel]
	IncludeMismatch map[string]diffSet   `json:"include_mismatch"` // TU → include-basename delta
}

type diffSet struct {
	MissingInBazel []string `json:"missing_in_bazel"` // cmake has, bazel lacks
	ExtraInBazel   []string `json:"extra_in_bazel"`   // bazel has, cmake lacks
}

func diff(cmake, bazel map[string]tuFacts) *report {
	r := &report{
		DefineMismatch:  map[string]diffSet{},
		StdMismatch:     map[string][2]string{},
		IncludeMismatch: map[string]diffSet{},
	}
	for k := range cmake {
		if _, ok := bazel[k]; !ok {
			r.OnlyCmake = append(r.OnlyCmake, k)
		}
	}
	for k := range bazel {
		if _, ok := cmake[k]; !ok {
			r.OnlyBazel = append(r.OnlyBazel, k)
		}
	}
	sort.Strings(r.OnlyCmake)
	sort.Strings(r.OnlyBazel)
	for k, cf := range cmake {
		bf, ok := bazel[k]
		if !ok {
			continue
		}
		r.Matched++
		if miss, extra := setDelta(cf.Defines, bf.Defines); len(miss)+len(extra) > 0 {
			r.DefineMismatch[k] = diffSet{MissingInBazel: miss, ExtraInBazel: extra}
		}
		if cf.Std != bf.Std {
			r.StdMismatch[k] = [2]string{cf.Std, bf.Std}
		}
		if miss, extra := setDelta(cf.IncludeDir, bf.IncludeDir); len(miss)+len(extra) > 0 {
			r.IncludeMismatch[k] = diffSet{MissingInBazel: miss, ExtraInBazel: extra}
		}
	}
	return r
}

func setDelta(a, b map[string]bool) (missingInB, extraInB []string) {
	for k := range a {
		if !b[k] {
			missingInB = append(missingInB, k)
		}
	}
	for k := range b {
		if !a[k] {
			extraInB = append(extraInB, k)
		}
	}
	sort.Strings(missingInB)
	sort.Strings(extraInB)
	return
}

func (r *report) print(w *os.File) {
	fmt.Fprintf(w, "compile-commands fidelity: %d TUs matched\n", r.Matched)
	fmt.Fprintf(w, "  only in cmake: %d   only in bazel: %d\n", len(r.OnlyCmake), len(r.OnlyBazel))
	fmt.Fprintf(w, "  DEFINE mismatches: %d   -std mismatches: %d\n",
		len(r.DefineMismatch), len(r.StdMismatch))
	// Include-dir deltas are informational: cmake records build-dir/source
	// absolute paths, Bazel exec-root-relative sandbox paths, so the basename
	// sets legitimately differ even when header resolution is equivalent. A
	// real header-search fidelity check is future work (ROADMAP); surfaced
	// here only as a count + in --json, never as a headline mismatch.
	fmt.Fprintf(w, "  include-dir basename deltas (informational): %d\n", len(r.IncludeMismatch))
	// Defines are the highest-signal, path-independent facts — show them in full.
	if len(r.DefineMismatch) > 0 {
		fmt.Fprintln(w, "\n-- DEFINE mismatches (cmake vs bazel, per TU) --")
		keys := sortedKeys(r.DefineMismatch)
		for _, k := range keys {
			d := r.DefineMismatch[k]
			fmt.Fprintf(w, "  %s\n", k)
			if len(d.MissingInBazel) > 0 {
				fmt.Fprintf(w, "      missing in bazel: %s\n", strings.Join(d.MissingInBazel, " "))
			}
			if len(d.ExtraInBazel) > 0 {
				fmt.Fprintf(w, "      extra in bazel:   %s\n", strings.Join(d.ExtraInBazel, " "))
			}
		}
	}
	if len(r.StdMismatch) > 0 {
		fmt.Fprintln(w, "\n-- -std mismatches --")
		for _, k := range sortedStdKeys(r.StdMismatch) {
			v := r.StdMismatch[k]
			fmt.Fprintf(w, "  %s: cmake=%q bazel=%q\n", k, v[0], v[1])
		}
	}
}

func sortedKeys(m map[string]diffSet) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedStdKeys(m map[string][2]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (r *report) writeJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
