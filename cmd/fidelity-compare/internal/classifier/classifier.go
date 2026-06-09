// Package classifier compares two cc-link artifacts (typically a
// cmake-built libfoo.a vs. a converted-then-bazel-built libfoo.a)
// and classifies each delta per the rules in docs/fidelity-deltas.md
// ("Delta classifier" section).
//
// The classifier is pure: it shells out to `nm` and `strings` to
// extract symbol sets and string tables, then bucket-sorts the
// diff into Benign / Impactful / ConfigurationMismatch categories
// against an operator-supplied allowlist. The CLI wrapper
// (cmd/fidelity-compare/main.go) handles file I/O and exit codes.
package classifier

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Report is the structured result of comparing two artifacts.
// Each delta is bucketed into one of three categories; the
// allowlist (operator-curated per-fixture) lets benign-by-context
// entries pre-empt impactful classification.
type Report struct {
	CMakeArtifact string `json:"cmake_artifact"`
	BazelArtifact string `json:"bazel_artifact"`

	// ExportedBoth is the count of defined-global symbols that
	// appear in both artifacts (the headline fidelity number —
	// the campaign's "105/105 for zlib" claim).
	ExportedBoth int `json:"exported_both"`

	// BenignDeltas catalogs explained differences: distro
	// hardening symbols, template-instantiation pairs, allowlist-
	// suppressed entries, archive-member name suffix differences
	// (.o vs .pic.o). Informational only; doesn't gate the
	// exit code.
	BenignDeltas []Delta `json:"benign_deltas,omitempty"`

	// ImpactfulDeltas catalogs differences that warrant investigation:
	// unexplained symbol-set drops, hermeticity leaks (absolute
	// host paths embedded in the Bazel artifact). Exit code 1 when
	// non-empty.
	ImpactfulDeltas []Delta `json:"impactful_deltas,omitempty"`
}

// Delta is one classified difference between the two artifacts.
type Delta struct {
	// Kind names the category: "fortify-symbol-only-in-cmake",
	// "stack-protector-symbol-only-in-cmake", "template-instantiation-
	// only-in-bazel", "exported-symbol-only-in-cmake", "exported-symbol-
	// only-in-bazel", "absolute-host-path-only-in-bazel", "allowlist-
	// suppressed", "archive-member-suffix-pic", etc.
	Kind string `json:"kind"`

	// Detail is the offending symbol / path / member name.
	Detail string `json:"detail"`
}

// HasImpactful returns true when the report contains at least one
// impactful delta.
func (r *Report) HasImpactful() bool {
	return len(r.ImpactfulDeltas) > 0
}

// FormatForOperator renders the report as a multi-line human-readable
// summary suitable for printing to stderr.
func (r *Report) FormatForOperator() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fidelity-compare: %s vs %s\n", r.CMakeArtifact, r.BazelArtifact)
	fmt.Fprintf(&b, "  exported symbols in both: %d\n", r.ExportedBoth)
	fmt.Fprintf(&b, "  benign deltas: %d\n", len(r.BenignDeltas))
	fmt.Fprintf(&b, "  impactful deltas: %d\n", len(r.ImpactfulDeltas))
	if len(r.ImpactfulDeltas) > 0 {
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "  IMPACTFUL DELTAS (these are bugs or new benign categories to allowlist):")
		for _, d := range r.ImpactfulDeltas {
			fmt.Fprintf(&b, "    - %s: %s\n", d.Kind, d.Detail)
		}
	}
	return b.String()
}

// Compare extracts symbol sets + absolute-path strings from each
// artifact and classifies the deltas. Returns the structured report
// or an error if nm/strings is unavailable or an artifact can't be
// read.
func Compare(cmakePath, bazelPath string, allowed Allowlist) (*Report, error) {
	if _, err := os.Stat(cmakePath); err != nil {
		return nil, fmt.Errorf("stat cmake artifact: %w", err)
	}
	if _, err := os.Stat(bazelPath); err != nil {
		return nil, fmt.Errorf("stat bazel artifact: %w", err)
	}
	cExported, err := exportedSymbols(cmakePath)
	if err != nil {
		return nil, fmt.Errorf("nm cmake exported: %w", err)
	}
	bExported, err := exportedSymbols(bazelPath)
	if err != nil {
		return nil, fmt.Errorf("nm bazel exported: %w", err)
	}
	cWeak, err := weakExportedSymbols(cmakePath)
	if err != nil {
		return nil, fmt.Errorf("nm cmake weak: %w", err)
	}
	bWeak, err := weakExportedSymbols(bazelPath)
	if err != nil {
		return nil, fmt.Errorf("nm bazel weak: %w", err)
	}
	cUndef, err := undefinedSymbols(cmakePath)
	if err != nil {
		return nil, fmt.Errorf("nm cmake undefined: %w", err)
	}
	bUndef, err := undefinedSymbols(bazelPath)
	if err != nil {
		return nil, fmt.Errorf("nm bazel undefined: %w", err)
	}
	bAbsPaths, err := absolutePathStrings(bazelPath)
	if err != nil {
		return nil, fmt.Errorf("strings bazel: %w", err)
	}
	cAbsPaths, err := absolutePathStrings(cmakePath)
	if err != nil {
		return nil, fmt.Errorf("strings cmake: %w", err)
	}

	rep := &Report{CMakeArtifact: cmakePath, BazelArtifact: bazelPath}
	rep.ExportedBoth = countCommon(cExported, bExported)
	classifyExportedDeltas(rep, cExported, bExported, cWeak, bWeak, allowed)
	classifyUndefinedDeltas(rep, cUndef, bUndef, allowed)
	classifyAbsolutePaths(rep, cAbsPaths, bAbsPaths)
	return rep, nil
}

// exportedSymbols returns the set of defined global symbols in an
// artifact via `nm --defined-only -g`. Each entry is the bare symbol
// name; addresses and type letters are stripped.
func exportedSymbols(path string) (map[string]bool, error) {
	cmd := exec.Command("nm", "--defined-only", "-g", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseNmOutput(out.Bytes()), nil
}

// weakExportedSymbols returns the set of WEAK defined-global symbols (nm type
// `W` weak text / `V` weak data) in an artifact. A weak symbol present in only
// ONE artifact is benign: weak symbols (inline functions, implicit template
// instantiations, vague-linkage items) are emitted per-TU ON DEMAND and deduped
// at link, so the optimizer's choice to emit vs elide one — which shifts with
// codegen flags the cc TOOLCHAIN owns (-fPIC / -fstack-protector / frame-pointer
// defaults Bazel adds and cmake doesn't), not anything the converter controls —
// never affects a consumer, which re-emits its own copy. gtest's pthread Mutex /
// ThreadLocal / FilePath weak ctors are exactly this.
func weakExportedSymbols(path string) (map[string]bool, error) {
	cmd := exec.Command("nm", "--defined-only", "-g", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseNmWeak(out.Bytes()), nil
}

// parseNmWeak parses `nm` stdout, returning the names whose type letter marks a
// weak binding (`W`/`V`, and the lowercase forms). nm format is
// `[address] type symbol`, so the type is the second-to-last field.
func parseNmWeak(buf []byte) map[string]bool {
	out := map[string]bool{}
	s := bufio.NewScanner(bytes.NewReader(buf))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || (strings.HasSuffix(line, ":") && !strings.Contains(line, " ")) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[len(fields)-2] {
		case "W", "V", "w", "v":
			out[fields[len(fields)-1]] = true
		}
	}
	return out
}

// undefinedSymbols returns the set of undefined symbol references
// via `nm -u`. The cmake-vs-Bazel undefined-set diff surfaces the
// distro hardening gap (FORTIFY's __*_chk + stack-protector
// canaries appear only in the cmake artifact when the host cc has
// spec-file defaults the hermetic Bazel toolchain doesn't reproduce).
func undefinedSymbols(path string) (map[string]bool, error) {
	cmd := exec.Command("nm", "-u", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseNmOutput(out.Bytes()), nil
}

// parseNmOutput parses `nm` stdout into a set of bare symbol names.
// nm format is `[address] [type] symbol`; we take the last
// whitespace-separated token of each non-empty line.
func parseNmOutput(buf []byte) map[string]bool {
	out := map[string]bool{}
	s := bufio.NewScanner(bytes.NewReader(buf))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		// Skip ar archive-member headers (lines like "foo.o:") —
		// nm prints these between each member's symbol listing.
		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		out[fields[len(fields)-1]] = true
	}
	return out
}

// absolutePathStrings extracts strings that look like absolute
// filesystem paths from an artifact via `strings`. A hermeticity
// leak (the cmake build dir or source tree baked into a Bazel-built
// artifact) shows up here as a string like "/tmp/bazel-..." or
// "/home/user/...". Distro toolchain paths
// ("/usr/lib/gcc/...", "/lib/x86_64-linux-gnu/...") are filtered
// out — those originate in the toolchain, not the converter.
func absolutePathStrings(path string) (map[string]bool, error) {
	cmd := exec.Command("strings", "-n", "8", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return filterHermeticityLeakPaths(out.Bytes()), nil
}

// filterHermeticityLeakPaths keeps only absolute paths whose first
// segment is /tmp, /home, /root, /var, /build, /workspace —
// hermeticity-leak shapes the converter might emit. Distro
// toolchain paths (/usr/..., /lib/...) are toolchain-origin and
// excluded — same artifact bytes appear regardless of how the
// link was driven.
func filterHermeticityLeakPaths(buf []byte) map[string]bool {
	leakPrefixes := []string{"/tmp/", "/home/", "/root/", "/var/", "/build/", "/workspace/"}
	out := map[string]bool{}
	s := bufio.NewScanner(bytes.NewReader(buf))
	for s.Scan() {
		line := s.Text()
		for _, p := range leakPrefixes {
			if strings.HasPrefix(line, p) {
				out[line] = true
				break
			}
		}
	}
	return out
}

// countCommon returns the size of the intersection of two sets.
func countCommon(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

// classifyExportedDeltas bucket-sorts the exported-symbol diff.
// Same-name on both sides → ExportedBoth contribution. Only on one
// side → check allowlist + template-instantiation heuristic +
// fall-through to Impactful.
func classifyExportedDeltas(rep *Report, c, b, cWeak, bWeak map[string]bool, allowed Allowlist) {
	onlyCmake := setSub(c, b)
	onlyBazel := setSub(b, c)
	// Template-instantiation pairs: when both sides have unique
	// C++-mangled (_Z*) symbols sharing a common prefix-of-prefix,
	// the difference is inlining-decision noise from different
	// optimization heuristics. Heuristic: any _Z-mangled name in
	// only-cmake or only-bazel that shares its first 24 chars with
	// at least one _Z-mangled name on the other side is benign.
	cMangled := mangledSymbols(onlyCmake)
	bMangled := mangledSymbols(onlyBazel)
	for sym := range onlyCmake {
		if allowed.Match(sym) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "allowlist-suppressed", Detail: sym})
			continue
		}
		if strings.HasPrefix(sym, "_Z") && hasPrefixPair(sym, bMangled) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "template-instantiation-only-in-cmake", Detail: sym})
			continue
		}
		if isStdlibInternalMangled(sym) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "stdlib-template-instantiation-only-in-cmake", Detail: sym})
			continue
		}
		if cWeak[sym] {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "weak-symbol-only-in-cmake", Detail: sym})
			continue
		}
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "exported-symbol-only-in-cmake", Detail: sym})
	}
	for sym := range onlyBazel {
		if allowed.Match(sym) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "allowlist-suppressed", Detail: sym})
			continue
		}
		if strings.HasPrefix(sym, "_Z") && hasPrefixPair(sym, cMangled) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "template-instantiation-only-in-bazel", Detail: sym})
			continue
		}
		if isStdlibInternalMangled(sym) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "stdlib-template-instantiation-only-in-bazel", Detail: sym})
			continue
		}
		if bWeak[sym] {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "weak-symbol-only-in-bazel", Detail: sym})
			continue
		}
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "exported-symbol-only-in-bazel", Detail: sym})
	}
	sortDeltas(rep.BenignDeltas)
	sortDeltas(rep.ImpactfulDeltas)
}

// classifyUndefinedDeltas surfaces the distro-hardening gap as
// benign (cmake builds against distro cc with spec-file FORTIFY +
// stack-protector defaults; the Bazel hermetic toolchain doesn't
// reproduce those by default). Other undefined-set deltas are
// informational only — they reflect link-time behavior the operator's
// downstream linker resolves, not artifact correctness.
func classifyUndefinedDeltas(rep *Report, c, b map[string]bool, allowed Allowlist) {
	onlyCmake := setSub(c, b)
	onlyBazel := setSub(b, c)
	// Template-pair heuristic on _Z-mangled symbols (same shape
	// as classifyExportedDeltas). Two undefined references that
	// share a long mangled prefix on opposite sides are two
	// instantiations of the same template — the consumer pulled
	// in inline-by-different-rules code paths, but the underlying
	// library API contract is intact.
	cMangled := mangledSymbols(onlyCmake)
	bMangled := mangledSymbols(onlyBazel)
	for sym := range onlyCmake {
		switch {
		case strings.HasSuffix(sym, "_chk"):
			// FORTIFY_SOURCE — cmake's distro toolchain emits
			// these; Bazel's hermetic toolchain doesn't. Benign.
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "fortify-symbol-only-in-cmake", Detail: sym})
		case strings.HasPrefix(sym, "__stack_chk_"):
			// Stack-protector — same distro-vs-hermetic toolchain
			// shape as FORTIFY. Benign.
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "stack-protector-symbol-only-in-cmake", Detail: sym})
		case allowed.Match(sym):
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "allowlist-suppressed-undefined", Detail: sym})
		case isLibcRuntimeHelper(sym):
			// Same toolchain-noise category as fortify/stack-
			// protector; libc/libstdc++ runtime helpers one side
			// inlines and the other references. Direction-
			// agnostic — cmake or bazel could be the inliner
			// depending on builtin-recognition behavior.
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "libc-runtime-helper-only-in-cmake", Detail: sym})
		case strings.HasPrefix(sym, "_Z") && hasPrefixPair(sym, bMangled):
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "template-instantiation-undefined-only-in-cmake", Detail: sym})
		case isStdlibInternalMangled(sym):
			// std::/compiler-internal undefined ref one side inlines
			// and the other references externally — toolchain
			// instantiation variance, not a converter signal (the
			// ABI-regression case the bazel-side guards against is a
			// *project* symbol, which this doesn't match).
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "stdlib-template-instantiation-undefined-only-in-cmake", Detail: sym})
		default:
			rep.ImpactfulDeltas = append(rep.ImpactfulDeltas,
				Delta{Kind: "undefined-symbol-only-in-cmake", Detail: sym})
		}
	}
	// Bazel-only undefined symbols are the consumer's link-time
	// ABI dependency on the library that the cmake-side build
	// doesn't have. Real ABI regressions (e.g. the converter's
	// INTERFACE-library lift emitted a different mangled symbol
	// due to wrong template parameter substitution) surface here
	// as the bazel-side consumer.o referencing a symbol the
	// cmake-side build doesn't need to link against. Classify
	// strictly: allowlist + template-pair heuristic + the
	// FORTIFY-replacement counterpart pair (cmake-side `__X_chk`
	// pairs with bazel-side `X` as the same call un-hardened).
	for sym := range onlyBazel {
		switch {
		case allowed.Match(sym):
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "allowlist-suppressed-undefined", Detail: sym})
		case fortifyCounterpart(sym, onlyCmake):
			// Bazel-side `snprintf` is the same call as cmake-side
			// `__snprintf_chk` — FORTIFY just wrapped cmake's.
			// Benign pair.
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "fortify-counterpart-only-in-bazel", Detail: sym})
		case isLibcRuntimeHelper(sym):
			// Glibc / libstdc++ runtime helpers cmake's distro
			// build inlines or links statically but Bazel's
			// hermetic toolchain references at link time:
			// __tls_get_addr (dynamic TLS resolution),
			// __cxa_atexit / __cxa_thread_atexit (C++ static
			// destructor registration), and friends. Same
			// distro-vs-hermetic toolchain-noise category as
			// fortify/stack-protector; not a converter signal.
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "libc-runtime-helper-only-in-bazel", Detail: sym})
		case strings.HasPrefix(sym, "_Z") && hasPrefixPair(sym, cMangled):
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "template-instantiation-undefined-only-in-bazel", Detail: sym})
		case isStdlibInternalMangled(sym):
			rep.BenignDeltas = append(rep.BenignDeltas,
				Delta{Kind: "stdlib-template-instantiation-undefined-only-in-bazel", Detail: sym})
		default:
			rep.ImpactfulDeltas = append(rep.ImpactfulDeltas,
				Delta{Kind: "undefined-symbol-only-in-bazel", Detail: sym})
		}
	}
}

// isLibcRuntimeHelper reports whether `sym` is a glibc /
// libstdc++ runtime helper that distro toolchains inline,
// builtin-replace, or link statically while hermetic toolchains
// reference at link time (or vice versa — gcc's -fno-builtin
// can flip the direction). Same distro-vs-hermetic toolchain-
// noise category as FORTIFY/stack-protector; classifying these
// as impactful would flag toolchain differences as if they
// were converter bugs.
func isLibcRuntimeHelper(sym string) bool {
	switch sym {
	// libc string/mem builtins — compilers replace these with
	// inline ops at -O2+ when builtin recognition fires, leaving
	// the symbol undefined on one side and defined on the other
	// depending on which side recognised the builtin pattern.
	case "memcpy", "memmove", "memcmp", "memset",
		"strlen", "strcmp", "strncmp", "strcpy", "strncpy",
		"strcat", "strncat", "strchr", "strrchr", "strstr":
		return true
	// libc stdio builtins — the compiler folds printf-family calls into
	// these (printf("…\n")→puts, printf("%c")→putchar, fprintf→fputs/fwrite)
	// at -O2+, so the undefined reference lands on whichever side's builtin
	// recognition fired. Same toolchain-noise class as the str/mem builtins.
	case "puts", "putchar", "fputs", "fputc", "putc", "fwrite":
		return true
	// C++ runtime helpers — static-init guards, atexit,
	// pure-virtual handler. All in libstdc++/libgcc; distro
	// toolchains link these into the consumer when needed,
	// hermetic toolchains reference at link time.
	case "__tls_get_addr", // dynamic TLS resolution
		"__cxa_atexit",        // C++ static destructor registration
		"__cxa_thread_atexit", // C++ thread_local destructor
		"__cxa_finalize",      // C++ shared-lib cleanup
		"__cxa_guard_acquire", // function-static initialisation
		"__cxa_guard_release",
		"__cxa_guard_abort",
		"__cxa_pure_virtual": // pure-virtual call handler
		return true
	}
	// libstdc++ vtables / typeinfo / VTTs for std types — the std::exception
	// family (std::runtime_error, std::bad_alloc, …) plus stream types like
	// std::__cxx11::basic_ostringstream. _ZTV (vtable), _ZTI (typeinfo), _ZTT
	// (VTT), for both direct-std (`St`) and nested-std (`NSt`) names. Emitted
	// by the libstdc++ runtime whenever a consumer references the type; the
	// toolchain provides them at link, so an undefined ref to one is benign.
	for _, p := range []string{
		"_ZTVSt", "_ZTVNSt", "_ZTISt", "_ZTINSt", "_ZTTSt", "_ZTTNSt",
	} {
		if strings.HasPrefix(sym, p) {
			return true
		}
	}
	return false
}

// fortifyCounterpart reports whether `sym` is the unhardened
// counterpart of a `__<sym>_chk` entry in the cmake-only set.
// Used to pair cmake-side `__snprintf_chk` with bazel-side
// `snprintf` as the same call, FORTIFY-wrapped on cmake.
func fortifyCounterpart(sym string, cmakeOnly map[string]bool) bool {
	return cmakeOnly["__"+sym+"_chk"]
}

// classifyAbsolutePaths flags absolute host paths embedded in the
// Bazel artifact that don't appear in the cmake artifact — a
// converter hermeticity leak. Paths that appear in both sides are
// presumed to come from the toolchain (debug info pointing at
// libgcc, etc.) and ignored.
func classifyAbsolutePaths(rep *Report, c, b map[string]bool) {
	for p := range setSub(b, c) {
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas,
			Delta{Kind: "absolute-host-path-only-in-bazel", Detail: p})
	}
}

// Allowlist is the parsed allowlist contents. Symbols matches
// exact-name entries; Prefixes carries `prefix:<mangled-prefix>`
// entries that suppress any symbol starting with the given
// mangled prefix.
//
// The prefix shape (introduced for the nlohmann/json gate)
// closes the "huge namespace of template instantiations" case:
// adding a single `prefix:_ZN8nlohmann16json_abi_v3_11_3`
// entry covers every basic_json template instantiation +
// typeinfo + vtable in one line, instead of listing 1000+
// mangled symbols. Use sparingly — a broad prefix can hide
// real regressions inside the namespace; narrow the prefix as
// far as you can while still covering the noise.
type Allowlist struct {
	Symbols  map[string]bool
	Prefixes []string
}

// Match reports whether sym is allowlisted (either by exact
// match against Symbols or by any prefix match against Prefixes).
func (a Allowlist) Match(sym string) bool {
	if a.Symbols[sym] {
		return true
	}
	for _, p := range a.Prefixes {
		if strings.HasPrefix(sym, p) {
			return true
		}
	}
	return false
}

// LoadAllowlist reads a per-fixture allowlist file. Format:
//
//   - blank lines and '#' comments ignored
//   - `<symbol>` — exact-match entry
//   - `prefix:<mangled-prefix>` — prefix-match entry (matches
//     any symbol starting with <mangled-prefix>)
//
// Empty path yields an empty (always-empty) allowlist.
func LoadAllowlist(path string) (Allowlist, error) {
	out := Allowlist{Symbols: map[string]bool{}}
	if path == "" {
		return out, nil
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	s := bufio.NewScanner(bytes.NewReader(buf))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "prefix:"); ok {
			rest = strings.TrimSpace(rest)
			if rest != "" {
				out.Prefixes = append(out.Prefixes, rest)
			}
			continue
		}
		out.Symbols[line] = true
	}
	return out, nil
}

// setSub returns the set-difference a \ b.
func setSub(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

// mangledSymbols returns the subset of a symbol set that starts
// with `_Z` (the Itanium ABI C++ name-mangling prefix).
func mangledSymbols(s map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range s {
		if strings.HasPrefix(k, "_Z") {
			out[k] = true
		}
	}
	return out
}

// stdlibMangledPrefixes are the Itanium-mangling heads for
// standard-library / compiler-internal names: `St` (std::), the
// `Ss`/`Sa`/`Si`/`So`/`Sd` std substitutions (string / allocator /
// streams), and `__gnu_cxx`. `K`/`V` cover const/volatile members. The
// `_ZZ*` variants are the SAME std names defined in a LOCAL (function/block)
// scope — a lambda or local static INSIDE a std template method (e.g.
// `std::__detail::_Compiler<regex_traits>::_M_expression_term<...>::{lambda}`,
// mangled `_ZZNSt8__detail...`). These are as toolchain-determined as the
// top-level std instantiations, so they classify the same.
var stdlibMangledPrefixes = []string{
	"_ZSt", "_ZNSt", "_ZNKSt", "_ZNVSt",
	"_ZZSt", "_ZZNSt", "_ZZNKSt", "_ZZNVSt",
	"_ZSs", "_ZNSs", "_ZNKSs",
	"_ZSa", "_ZNSa",
	"_ZNSi", "_ZNSo", "_ZNSd",
	"_ZN9__gnu_cxx", "_ZNK9__gnu_cxx", "_ZZN9__gnu_cxx",
}

// isStdlibInternalMangled reports whether sym is a mangled name in a
// standard-library / compiler-internal namespace (std::, __gnu_cxx). Such
// symbols are emitted by the compiler purely as a function of which
// templates the project's sources instantiate; the converter never
// controls them, so an unpaired std-internal symbol is toolchain
// instantiation/inlining variance, not a converter delta. (A converter bug
// that *would* shift std symbols — e.g. dropping an ABI-affecting define —
// also shifts the project's OWN symbols, which stay un-auto-classified, so
// this can't mask a real regression on its own.)
func isStdlibInternalMangled(sym string) bool {
	for _, p := range stdlibMangledPrefixes {
		if strings.HasPrefix(sym, p) {
			return true
		}
	}
	return false
}

// hasPrefixPair returns true if `sym` shares its first 24 chars
// with at least one entry in `peers`. 24 is empirically the
// length of a typical Itanium-mangled function-template head
// (`_ZN<namespace_len><namespace>...`) — long enough to discriminate
// distinct templates, short enough to pair the same template's
// instantiation variants.
func hasPrefixPair(sym string, peers map[string]bool) bool {
	const prefixLen = 24
	if len(sym) < prefixLen {
		return false
	}
	prefix := sym[:prefixLen]
	for p := range peers {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// sortDeltas sorts a Delta slice in-place by (Kind, Detail) for
// stable comparison-report output.
func sortDeltas(s []Delta) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Kind != s[j].Kind {
			return s[i].Kind < s[j].Kind
		}
		return s[i].Detail < s[j].Detail
	})
}
