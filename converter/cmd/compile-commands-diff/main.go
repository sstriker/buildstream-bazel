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
// unit: the set of macro defines, the language standard, and the RAW include
// dirs (normalized to a common source-relative space at diff time — see
// normalizeInclude).
type tuFacts struct {
	Defines    map[string]bool
	Std        string
	IncludeDir map[string]bool // raw -I/-isystem dirs as they appeared in argv
	Copts      map[string]bool // project-signal compile flags (toolchain/build-mode defaults filtered)
}

// normOpts carries the prefixes needed to translate cmake (absolute host) and
// Bazel (exec-root-relative) include dirs into one comparable space.
type normOpts struct {
	cmakeSrc   string // cmake source root, e.g. /tmp/zlib
	cmakeBuild string // cmake build dir, e.g. /tmp/cc-lens-zlib/cmake
	bazelPkg   string // converted element package, e.g. elements/zlib
}

// normalizeInclude maps a raw include dir (from either cmake's absolute host
// paths or Bazel's exec-root-relative paths) to a canonical key so the two
// sides diff meaningfully:
//
//	<src-relative>   a source-tree dir (cmake: under cmakeSrc; bazel: under the
//	                 element package) — e.g. "include", "." (the package root)
//	gen:<rel>        a generated/build-dir include (cmake: under cmakeBuild;
//	                 bazel: under bazel-out/<cfg>/bin/<pkg>)
//	sys:<base>      a system dir (/usr, /lib, …) — toolchain-supplied
//	ext:<tail>      anything else (host/external prefix) — keyed by a stable tail
//
// Empty result means "drop" (an unkeyable dir). The mapping is intentionally
// lossy on the gen: side (build-dir layouts differ) but exact on source
// includes, which is where header-search fidelity actually matters.
func normalizeInclude(dir string, o normOpts) string {
	dir = strings.TrimRight(strings.TrimSpace(dir), "/")
	if dir == "" {
		return ""
	}
	if filepath.IsAbs(dir) {
		if o.cmakeBuild != "" {
			if rel, ok := relUnder(o.cmakeBuild, dir); ok {
				return "gen:" + rel
			}
		}
		if o.cmakeSrc != "" {
			if rel, ok := relUnder(o.cmakeSrc, dir); ok {
				return rel
			}
		}
		if strings.HasPrefix(dir, "/usr/") || strings.HasPrefix(dir, "/lib") || dir == "/usr/include" {
			return "sys:" + filepath.Base(dir)
		}
		return "ext:" + filepath.Base(dir)
	}
	// Bazel exec-root-relative forms.
	if rest, ok := stripBazelOut(dir); ok {
		// bazel-out/<cfg>/bin/<pkg>/<rel> → gen:<rel>
		if o.bazelPkg != "" {
			if rel, ok := relUnder(o.bazelPkg, rest); ok {
				return "gen:" + rel
			}
		}
		return "gen:" + rest
	}
	if o.bazelPkg != "" {
		if rel, ok := relUnder(o.bazelPkg, dir); ok {
			return rel
		}
	}
	if strings.HasPrefix(dir, "external/") {
		return "ext:" + filepath.Base(dir)
	}
	return dir
}

// relUnder returns p relative to base when p is base or under it. "." for an
// exact match (the package/build root itself).
func relUnder(base, p string) (string, bool) {
	base = strings.TrimRight(base, "/")
	if p == base {
		return ".", true
	}
	if strings.HasPrefix(p, base+"/") {
		return p[len(base)+1:], true
	}
	return "", false
}

// stripBazelOut removes a leading bazel-out/<config>/bin (or /genfiles) segment,
// returning the package-rooted remainder.
func stripBazelOut(p string) (string, bool) {
	if !strings.HasPrefix(p, "bazel-out/") {
		return "", false
	}
	parts := strings.SplitN(p, "/", 4) // bazel-out, <cfg>, bin, <rest>
	if len(parts) < 4 {
		return "", true
	}
	return parts[3], true
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
	cmakeSrc := flag.String("cmake-src", "", "cmake source root (for include normalization, e.g. /tmp/zlib)")
	cmakeBuild := flag.String("cmake-build", "", "cmake build dir (for include normalization, e.g. /tmp/zbuild)")
	bazelPkg := flag.String("bazel-package", "", "converted element package (for include normalization, e.g. elements/zlib)")
	cmakeReply := flag.String("cmake-codemodel", "", "cmake File API reply dir (for link-ORDER check; e.g. <build>/.cmake/api/v1/reply)")
	aqueryLink := flag.String("aquery-link", "", "path to `bazel aquery --output=jsonproto mnemonic(CppLink,//...)` JSON (link-ORDER check)")
	flag.Parse()
	o := normOpts{cmakeSrc: *cmakeSrc, cmakeBuild: *cmakeBuild, bazelPkg: *bazelPkg}

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

	rep := diff(cmakeTUs, bazelTUs, o)
	rep.print(os.Stdout)
	if *jsonOut != "" {
		if err := rep.writeJSON(*jsonOut); err != nil {
			fmt.Fprintf(os.Stderr, "compile-commands-diff: write %s: %v\n", *jsonOut, err)
		}
	}

	// Optional link-ORDER check (Q2): only when both cmake codemodel + CppLink
	// aquery are supplied. Report-only, like the compile diff.
	if *cmakeReply != "" && *aqueryLink != "" {
		lrep, err := linkOrderDiff(*cmakeReply, *aqueryLink)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compile-commands-diff: link-order: %v\n", err)
		} else {
			lrep.print(os.Stdout)
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

// interestingCopt reports whether a compile flag is a PROJECT-authored
// semantic flag worth diffing — as opposed to a toolchain default, a build-mode
// flag, or a structural arg that differs between cmake's gcc driver and Bazel's
// hermetic toolchain for reasons unrelated to conversion fidelity. Kept (high
// signal): -fvisibility=, -fno-rtti, -fno-exceptions, -fopenmp, -march/-msse/
// -mavx, -pthread, -fPIC is borderline but build-mode so dropped. Dropped:
// optimization (-O*), debug (-g*), warnings (-W/-w*), hardening + dep-file +
// random-seed + PIC/PIE toolchain defaults, and the -D/-I/-isystem/-std flags
// extracted separately.
func interestingCopt(a string) bool {
	if !strings.HasPrefix(a, "-") {
		return false
	}
	switch a {
	case "-c", "-o", "-S", "-E", "-MD", "-MMD", "-MF", "-MT", "-MQ", "-MP":
		return false
	}
	for _, p := range []string{
		"-D", "-I", "-isystem", "-iquote", "-std=", // extracted elsewhere
		"-O", "-g", "-W", "-w", // optimization / debug / warnings (toolchain + build-mode noise)
		"-fstack-protector", "-fno-omit-frame-pointer", "-fno-canonical-system-headers",
		"-fPIC", "-fPIE", "-fpic", "-fpie",
		"-U_FORTIFY_SOURCE", "-D_FORTIFY_SOURCE", "-frandom-seed", "-fdiagnostics",
		"-MF", "-MT", "-MQ",
	} {
		if strings.HasPrefix(a, p) {
			return false
		}
	}
	return true
}

// factsFromArgv extracts the path-independent compile facts from a compile argv.
func factsFromArgv(argv []string) tuFacts {
	f := tuFacts{Defines: map[string]bool{}, IncludeDir: map[string]bool{}, Copts: map[string]bool{}}
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
				f.IncludeDir[d] = true // raw; normalized at diff time
			}
		case a == "-isystem", a == "-iquote":
			if i+1 < len(argv) {
				i++
				f.IncludeDir[argv[i]] = true
			}
		case strings.HasPrefix(a, "-isystem"):
			if d := strings.TrimPrefix(a, "-isystem"); d != "" {
				f.IncludeDir[d] = true
			}
		case strings.HasPrefix(a, "-iquote"):
			if d := strings.TrimPrefix(a, "-iquote"); d != "" {
				f.IncludeDir[d] = true
			}
		default:
			if interestingCopt(a) {
				f.Copts[a] = true
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
	IncludeMismatch map[string]diffSet   `json:"include_mismatch"` // TU → source-include delta
	CoptMismatch    map[string]diffSet   `json:"copt_mismatch"`    // TU → project-copt delta
}

type diffSet struct {
	MissingInBazel []string `json:"missing_in_bazel"` // cmake has, bazel lacks
	ExtraInBazel   []string `json:"extra_in_bazel"`   // bazel has, cmake lacks
}

func diff(cmake, bazel map[string]tuFacts, o normOpts) *report {
	r := &report{
		DefineMismatch:  map[string]diffSet{},
		StdMismatch:     map[string][2]string{},
		IncludeMismatch: map[string]diffSet{},
		CoptMismatch:    map[string]diffSet{},
	}
	normSet := func(raw map[string]bool) map[string]bool {
		out := map[string]bool{}
		for d := range raw {
			if k := normalizeInclude(d, o); k != "" {
				out[k] = true
			}
		}
		return out
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
		// Compare SOURCE-relative include dirs (high-confidence: a real
		// header-search divergence). gen:/sys:/ext: keys reflect build-layout
		// and toolchain differences (cmake build-dir vs bazel-out, Bazel's
		// hermetic toolchain externals) that legitimately differ even when the
		// converted graph is correct, so they're bucketed as informational, not
		// a headline mismatch.
		miss, extra := setDelta(srcOnly(normSet(cf.IncludeDir)), srcOnly(normSet(bf.IncludeDir)))
		if len(miss)+len(extra) > 0 {
			r.IncludeMismatch[k] = diffSet{MissingInBazel: miss, ExtraInBazel: extra}
		}
		if cmiss, cextra := setDelta(cf.Copts, bf.Copts); len(cmiss)+len(cextra) > 0 {
			r.CoptMismatch[k] = diffSet{MissingInBazel: cmiss, ExtraInBazel: cextra}
		}
	}
	return r
}

// srcOnly keeps only source-relative include keys (dropping the gen:/sys:/ext:
// build-layout/toolchain buckets) so the include diff is high-confidence.
func srcOnly(s map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range s {
		if strings.HasPrefix(k, "gen:") || strings.HasPrefix(k, "sys:") || strings.HasPrefix(k, "ext:") {
			continue
		}
		out[k] = true
	}
	return out
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
	fmt.Fprintf(w, "  DEFINE mismatches: %d   -std mismatches: %d   include-dir mismatches: %d   copt mismatches: %d\n",
		len(r.DefineMismatch), len(r.StdMismatch), len(r.IncludeMismatch), len(r.CoptMismatch))
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
	if len(r.IncludeMismatch) > 0 {
		// Normalized to a common source-relative space (gen:/sys:/ext: prefixes
		// mark generated/system/external roots). Source-relative deltas are
		// high-confidence; gen:/sys: deltas reflect build-layout differences and
		// are lower-confidence.
		fmt.Fprintln(w, "\n-- include-dir mismatches (normalized; cmake vs bazel) --")
		printDeltas(w, r.IncludeMismatch)
	}
	if len(r.CoptMismatch) > 0 {
		// Project-authored compile flags (toolchain + build-mode defaults
		// filtered — see interestingCopt). A delta here is a real semantic
		// divergence (a -fvisibility/-fno-rtti/-fopenmp/-march cmake applies
		// that the conversion dropped, or vice versa).
		fmt.Fprintln(w, "\n-- copt mismatches (project flags; cmake vs bazel) --")
		printDeltas(w, r.CoptMismatch)
	}
}

func printDeltas(w *os.File, m map[string]diffSet) {
	for _, k := range sortedKeys(m) {
		d := m[k]
		fmt.Fprintf(w, "  %s\n", k)
		if len(d.MissingInBazel) > 0 {
			fmt.Fprintf(w, "      missing in bazel: %s\n", strings.Join(d.MissingInBazel, " "))
		}
		if len(d.ExtraInBazel) > 0 {
			fmt.Fprintf(w, "      extra in bazel:   %s\n", strings.Join(d.ExtraInBazel, " "))
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
