// convert-element-trace is the trace-driven Bazel converter shared
// by every kind:* element whose build runs through cc/ar via some
// driver — autotools / make / manual / script / makemaker /
// modulebuild today. Project B runs the build under a process
// tracer; the trace gets registered (CAS-keyed by srckey) and
// read back by project A's render in round 2. With the trace in
// hand, the element converts to native cc_library / cc_binary
// targets instead of the opaque install_tree.tar genrule.
//
// The binary used to live under cmd/convert-element-autotools/
// when only autotools opted into the trace-driven path. The
// converter logic was always kind-agnostic — it operates on
// cc/ar execve events, not on autotools-specific patterns — so
// the rename to "trace" reflects what the code actually does.
//
// Scope:
//   - Input: a strace text-format trace file (see --trace).
//   - Trace event filter: top-level compiler-driver execve calls
//     (cc / gcc / g++ / clang / c++) and `ar` archive calls.
//     gcc-internal cc1 / as / collect2 / ld sub-invocations are
//     filtered out — they're a portable re-emission of the
//     driver call.
//   - Cross-event correlation: a compile-only `cc -c -o x.o x.c`
//     event paired with an archive `ar rcs libfoo.a x.o y.o`
//     pairs the .o → .c map back to source files for the
//     archive's cc_library{srcs=[...]}. Link events that
//     consume the same archive resolve `-lfoo` → `:foo`.
//   - Optional `--make-db` hint: when the build invokes `make`,
//     a `make -np` dump captured alongside the trace gives
//     structural Makefile context (target names, recipes,
//     variables). For non-make-driven kinds (manual / script)
//     this stays unset and the trace alone drives conversion.
//   - Output: BUILD.bazel.out with one cc_library per recovered
//     archive plus one cc_binary per recovered link.
//
// Cross-element dep resolution (link command's `-l<lib>`
// referencing system / out-of-tree libraries) is the next slice
// — it needs the imports manifest, mirroring the cmake STATIC
// IMPORTED dep recovery path.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/emit/bazel"
	"github.com/sstriker/cmake-to-bazel/converter/ir"
	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

func main() {
	tracePath := flag.String("trace", "", "path to strace text-format output (`-f -e trace=execve -s 4096 -o <path>`). Required unless --trace-dir is set.")
	traceDir := flag.String("trace-dir", "", "alternative to --trace + --make-db: a directory containing trace.log + make-db.txt (typically materialized from @trace_<key>//:trace by the round-2 converter genrule). Empty / non-existent / missing-trace.log ⇒ converter emits a placeholder BUILD.bazel.out (the round-2 boot phase signal: no published trace yet, project B keeps using its coarse install genrule until pass-3 publishes one).")
	outBuild := flag.String("out-build", "", "path to write BUILD.bazel.out")
	outIRJSON := flag.String("out-ir-json", "", "optional: path to write the recovered ir.Package as JSON, parallel to convert-element's --out-ir-json. Drives the orchestrator's per-element multi-platform fold for round-2 trace-driven kinds; ignored by single-platform flows. The legacy BUILD.bazel.out at --out-build is emitted independently.")
	importsPath := flag.String("imports-manifest", "", "optional: path to imports.json mapping cross-element link libraries to Bazel labels")
	makeDBPath := flag.String("make-db", "", "optional: path to `make -np` dump for post-build Makefile structural hints (target names, recipes, variables)")
	outMapping := flag.String("out-install-mapping", "", "optional: path to write install-mapping.json sidecar (source → install-tree dest map; consumed by Phase 4 typed-filegroup work)")
	generatedHeadersPath := flag.String("generated-headers", "", "optional: path to a newline-separated list of build-time-generated headers (config.h-style outputs of AC_CONFIG_HEADERS / yacc / etc.); added to every cc_library's hdrs so the BUILD manifest documents the dep")
	flag.Parse()

	if *outBuild == "" {
		fmt.Fprintln(os.Stderr, "convert-element-trace: --out-build is required")
		os.Exit(2)
	}

	// --trace-dir mode dispatch:
	//   - dir empty / missing / no trace.log inside ⇒ no published
	//     trace yet (round-2 boot). Emit a placeholder
	//     BUILD.bazel.out and exit cleanly. The converter genrule
	//     in project A still has a real output; project B keeps
	//     using its coarse install genrule (which is what the
	//     round-1 build path wires in handler_autotools_native.go,
	//     unchanged by this dispatch).
	//   - dir non-empty + trace.log present ⇒ thread the dir's
	//     trace.log + make-db.txt into the existing fine-mode flow
	//     (so callers can choose between flag-based and dir-based
	//     wiring without duplicating the parse path).
	if *traceDir != "" {
		if _, err := os.Stat(filepath.Join(*traceDir, "trace.log")); err != nil {
			if err := writePlaceholderBuild(*outBuild); err != nil {
				fmt.Fprintf(os.Stderr, "convert-element-trace: write placeholder: %v\n", err)
				os.Exit(1)
			}
			// Multi-platform fold contract: the converter genrule
			// must produce ir.json whenever --out-ir-json is set
			// (Bazel's declared-outputs invariant), even in the
			// round-2 boot phase where no trace is published yet.
			// An empty ir.Package matches the placeholder
			// BUILD.bazel — the fold composes empties to empty.
			if *outIRJSON != "" {
				if err := writePlaceholderIRJSON(*outIRJSON); err != nil {
					fmt.Fprintf(os.Stderr, "convert-element-trace: write placeholder ir.json: %v\n", err)
					os.Exit(1)
				}
			}
			return
		}
		if *tracePath == "" {
			*tracePath = filepath.Join(*traceDir, "trace.log")
		}
		if *makeDBPath == "" {
			candidate := filepath.Join(*traceDir, "make-db.txt")
			if _, err := os.Stat(candidate); err == nil {
				*makeDBPath = candidate
			}
		}
	}

	if *tracePath == "" {
		fmt.Fprintln(os.Stderr, "convert-element-trace: --trace (or non-empty --trace-dir) is required")
		os.Exit(2)
	}

	traceFile, err := os.Open(*tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-trace: open trace: %v\n", err)
		os.Exit(1)
	}
	defer traceFile.Close()

	var imports *manifest.Resolver
	if *importsPath != "" {
		imports, err = manifest.Load(*importsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: load imports manifest: %v\n", err)
			os.Exit(1)
		}
	}

	var makeDB *MakeDB
	if *makeDBPath != "" {
		body, err := os.ReadFile(*makeDBPath)
		if err == nil {
			// Tolerate empty / missing make-db files: the
			// genrule's `make -np` capture redirects stderr
			// and uses `|| true` so the artifact may be
			// absent or empty on healthy builds where make
			// dislikes the dry run. Treat parse failures the
			// same — convert-element-trace should still
			// emit a valid BUILD.bazel.out from the trace
			// alone.
			makeDB = parseMakeDB(body)
		}
	}
	var generatedHeaders []string
	if *generatedHeadersPath != "" {
		body, err := os.ReadFile(*generatedHeadersPath)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "convert-element-trace: read generated-headers: %v\n", err)
			os.Exit(1)
		}
		generatedHeaders = parseGeneratedHeaderList(body)
	}

	events := parseTrace(traceFile)
	graph := correlate(events)
	rules := recoveredRules(graph, imports, makeDB, generatedHeaders)
	out := renderRules(rules)

	// Per-element multi-platform fold for round-2 trace-driven
	// kinds (ROADMAP.md "Per-platform fold"): when --out-ir-json
	// is set, ship the recovered rules as an ir.Package JSON
	// alongside the rendered BUILD.bazel. The orchestrator's
	// fold consumes one ir.json per (element, platform) cell
	// and composes them via fold-element. Single-platform
	// invocations leave --out-ir-json unset; the BUILD.bazel.out
	// remains the canonical artifact. We share the recovered
	// rules slice with renderRules above so buildRules +
	// generated-headers fold runs once per action.
	if *outIRJSON != "" {
		pkg := toIR(rules)
		body, err := json.MarshalIndent(pkg, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: marshal ir.Package: %v\n", err)
			os.Exit(1)
		}
		if err := os.MkdirAll(filepath.Dir(*outIRJSON), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: mkdir ir.json: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*outIRJSON, append(body, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: write ir.json: %v\n", err)
			os.Exit(1)
		}
	}

	// Install-mapping sidecar: when --out-install-mapping is
	// set AND we have a make-db, parse the install: recipe and
	// emit the source → install-tree-dest map. Today's make-db
	// integration; future work cross-validates trace flags
	// against per-target Makefile vars (slice (2)) and uses
	// Makefile target names where the trace's `-o` differs
	// (slice (3)).
	if *outMapping != "" && makeDB != nil {
		mapping := buildInstallMapping(makeDB, buildRules(graph, imports, makeDB))
		body, err := renderInstallMappingJSON(mapping)
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: render install-mapping: %v\n", err)
			os.Exit(1)
		}
		if body == nil {
			// No install mapping to record — write an empty
			// version-1 envelope so the Bazel output exists
			// (genrule must produce every declared output).
			body = []byte(`{"version":1,"mappings":[]}` + "\n")
		}
		if err := os.MkdirAll(filepath.Dir(*outMapping), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: mkdir mapping: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*outMapping, body, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-trace: write mapping: %v\n", err)
			os.Exit(1)
		}
	}

	if err := os.MkdirAll(filepath.Dir(*outBuild), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-trace: mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outBuild, []byte(out), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-trace: write: %v\n", err)
		os.Exit(1)
	}
}

// writePlaceholderIRJSON emits the empty ir.Package the round-2
// converter genrule produces when --out-ir-json is set and no
// trace is published yet (the boot phase). Sibling of
// writePlaceholderBuild — the converter genrule must satisfy
// every declared output, and an empty ir.Package matches the
// placeholder BUILD.bazel's "no recoverable targets" semantic.
// fold-element composes empties to empty without complaint.
func writePlaceholderIRJSON(outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(ir.Package{}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, append(body, '\n'), 0o644)
}

// recoveredRules returns the same CCRule slice emitBuild
// internally renders — buildRules' output with the (uniformly
// applied) generated-headers list folded into each rule's Hdrs.
// Factored out so the --out-ir-json path can serialize the
// identical post-fold ruleset without duplicating the
// generated-headers logic.
func recoveredRules(g *Graph, imports *manifest.Resolver, makeDB *MakeDB, generatedHeaders []string) []CCRule {
	rules := buildRules(g, imports, makeDB)
	if len(generatedHeaders) > 0 {
		hdrs := stableUnique(generatedHeaders)
		for i := range rules {
			rules[i].Hdrs = hdrs
		}
	}
	return rules
}

// toIR maps the trace converter's local CCRule slice to the
// shared ir.Package shape consumed by converter/elementfold.
// Single-platform conversions don't need this — emitBuild renders
// BUILD.bazel.out directly — but the multi-platform fold for
// round-2 trace-driven kinds composes N per-platform ir.Package
// JSONs via fold-element, which only understands the shared IR.
//
// Mapping:
//   - "cc_library" CCRule → ir.KindCCLibrary target with
//     Linkstatic=true (mirrors emitBuild's "linkstatic = True"
//     line on every cc_library it emits).
//   - "cc_binary" CCRule → ir.KindCCBinary target.
//   - Every target gets Visibility = ["//visibility:public"]
//     to match emitBuild's blanket visibility on every rule.
//
// Package.Name and Package.SourceRoot are left empty: the
// trace-driven converter has no project() name and the
// orchestrator's fold doesn't need them (fold-element keys by
// cell name, not by Package.Name).
func toIR(rules []CCRule) ir.Package {
	pkg := ir.Package{}
	if len(rules) == 0 {
		return pkg
	}
	pkg.Targets = make([]ir.Target, 0, len(rules))
	for _, r := range rules {
		t := ir.Target{
			Name:       r.Name,
			Srcs:       append([]string(nil), r.Srcs...),
			Hdrs:       append([]string(nil), r.Hdrs...),
			Copts:      append([]string(nil), r.Copts...),
			Defines:    append([]string(nil), r.Defines...),
			Deps:       append([]string(nil), r.Deps...),
			Visibility: []string{"//visibility:public"},
		}
		switch r.RuleKind {
		case "cc_library":
			t.Kind = ir.KindCCLibrary
			t.Linkstatic = true
		case "cc_binary":
			t.Kind = ir.KindCCBinary
		default:
			t.Kind = ir.KindUnknown
		}
		pkg.Targets = append(pkg.Targets, t)
	}
	return pkg
}

// writePlaceholderBuild emits the empty BUILD.bazel.out the
// round-2 converter genrule produces during the boot phase: no
// trace published yet, so no cc_library / cc_binary is recoverable
// from this srckey. The file's existence still satisfies the
// converter genrule's declared output; project B's coarse install
// genrule (rendered separately by handler_autotools_native.go
// when round-2 is enabled) remains the buildable target until
// trace-publish lands an AC entry.
func writePlaceholderBuild(outBuild string) error {
	if err := os.MkdirAll(filepath.Dir(outBuild), 0o755); err != nil {
		return err
	}
	const body = `# Generated by convert-element-trace.
# Round-2 boot phase: no trace published for this srckey yet.
# Build project B's coarse install genrule
# (//elements/<elem>:<elem>_install) to materialize the install
# tree + publish a trace; subsequent renders of project A will
# see an AC hit and emit fine-grained cc rules here.
`
	return os.WriteFile(outBuild, []byte(body), 0o644)
}

// EventKind classifies a trace event by what it produces.
//
//   - EventCompile: `cc -c ... -o X.o Y.c` — one source → one
//     object. Copts on the event are the remaining argv flags.
//   - EventLink: `cc ... -o BIN A.c|A.o ... [-lLIB]...` — produces
//     a binary or shared object. May mix .c and .o inputs.
//   - EventArchive: `ar rcs libNAME.a A.o B.o ...` — produces a
//     static library from object files.
type EventKind int

const (
	EventCompile EventKind = iota
	EventLink
	EventArchive
)

// Event is the typed trace event we extract from a single
// compiler-driver / archiver execve call. Source paths are
// preserved verbatim from argv; the correlation pass normalizes
// to basenames where matching by basename is safer than matching
// by absolute path (which is build-dir-dependent).
type Event struct {
	Kind    EventKind
	Cwd     string   // tracee cwd at exec time ("" when build-tracer didn't capture it)
	Out     string   // output path (-o argument or ar's first positional)
	Srcs    []string // .c/.cc/.cpp/.cxx/.c++/.C input args (compile + link)
	Objs    []string // .o input args (link + archive)
	Libs    []string // -l<name> args (link)
	Copts   []string // remaining flags, sorted + deduped (excludes default-toolchain + -D / -L / -I)
	Defines []string // -D<name>[=<val>] args, sorted + deduped (excludes -DNDEBUG)
}

// parseTrace walks an strace text-format trace, returning the
// sequence of recognized Events. Unknown / uninteresting lines
// are silently skipped.
func parseTrace(r *os.File) []Event {
	var out []Event
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		cwd := parseCwdAnnotation(line) // "" when absent
		argv, ok := parseExecveLine(line)
		if !ok {
			continue
		}
		ev, ok := classifyArgv(argv)
		if !ok {
			continue
		}
		ev.Cwd = cwd
		out = append(out, ev)
	}
	return out
}

// parseCwdAnnotation extracts the build-tracer-extension
// `cwd:"..."` annotation that may appear between the (already-
// stripped) pid prefix and the `execve(...)` call. Returns ""
// when the annotation isn't present (older build-tracer
// versions, or strace-fallback traces that didn't capture cwd).
//
// Format from build-tracer's emitExecve:
//
//	cwd:"/build/root/lib"  execve(...) = 0
//
// We don't unescape strace's quoting beyond the simplest \"
// case because real cwd paths are POSIX paths without embedded
// quotes; if a path contains \", the converter falls back to
// the empty-cwd behavior (basename-only correlation, the
// pre-cwd default).
func parseCwdAnnotation(line string) string {
	idx := strings.Index(line, "cwd:\"")
	if idx < 0 {
		return ""
	}
	// Make sure cwd: appears BEFORE execve(, not embedded
	// somewhere in argv.
	exec := strings.Index(line, "execve(")
	if exec < 0 || idx >= exec {
		return ""
	}
	rest := line[idx+len("cwd:"):] // includes the leading "
	end := indexAfterQuoted(rest, 0)
	if end < 0 {
		return ""
	}
	// rest[1:end] is the content between the quotes.
	// indexAfterQuoted returns the index of the closing quote.
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(rest[1:end])
}

// classifyArgv routes a single execve argv to the right Event
// constructor (or returns ok=false for argv we don't lower).
// Compiler-driver argv → compile or link event. Archiver argv →
// archive event. gcc-internal sub-invocations (cc1 / as /
// collect2 / ld) → filtered out (the user-facing driver argv
// is the canonical record).
func classifyArgv(argv []string) (Event, bool) {
	if len(argv) == 0 {
		return Event{}, false
	}
	bin := filepath.Base(argv[0])
	switch bin {
	case "cc", "gcc", "g++", "clang", "clang++", "c++", "cxx":
		return classifyCompilerDriver(argv)
	case "ar":
		return classifyArchiver(argv)
	}
	return Event{}, false
}

// classifyCompilerDriver inspects a cc/gcc/g++/clang argv and
// returns either an EventCompile (compile-only `-c`) or
// EventLink (compile-and-link / link-only). Returns ok=false
// for invocations we can't sensibly classify (e.g., `cc
// --version`).
//
// Flag handling:
//   - `-D<name>[=<val>]` lands in Defines, not Copts. Bazel's
//     cc_library / cc_binary surface defines as a first-class
//     attribute; emitting them as `-D...` in copts would be
//     redundant.
//   - `-O<N>` / `-Os` / `-Og` / `-g[N]` / `-DNDEBUG` /
//     `-fPIC` / `-fpic` / `-fPIE` / `-fpie` are stripped:
//     Bazel's cc_toolchain provides these per compilation
//     mode (opt / dbg / fastbuild) and per linkstatic shape;
//     copying them onto every target would override the
//     user's `bazel build -c dbg` intent.
//   - `-L<dir>` / `-I<dir>` stripped: build-dir-specific
//     paths; Bazel's includes / deps attributes carry the
//     equivalents at higher granularity.
//   - Everything else with a `-` prefix passes through as
//     copts. Distro-specific hardening flags
//     (`-fstack-protector-strong`, `-fcf-protection`,
//     `-D_FORTIFY_SOURCE=N`, etc.) are intentionally
//     preserved — those carry build-time intent the
//     user-side cc_toolchain may not replicate.
func classifyCompilerDriver(argv []string) (Event, bool) {
	var srcs, objs, libs, copts, defines []string
	output := ""
	compileOnly := false
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-c":
			compileOnly = true
		case a == "-o" && i+1 < len(argv):
			output = argv[i+1]
			i++
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			output = a[2:]
		case strings.HasPrefix(a, "-l") && len(a) > 2:
			libs = append(libs, a[2:])
		case strings.HasPrefix(a, "-D") && len(a) > 2:
			// -DNDEBUG is one of the stripped defaults — Bazel's
			// opt mode defines it. Other -D<name>[=<val>] entries
			// land on the Bazel rule's defines attribute.
			d := a[2:]
			if d == "NDEBUG" {
				continue
			}
			defines = append(defines, d)
		case isSourceFile(a):
			srcs = append(srcs, a)
		case isObjectFile(a):
			objs = append(objs, a)
		default:
			if strings.HasPrefix(a, "-") {
				// Drop -L<dir> and -I<dir> flags from copts —
				// they're build-dir-specific paths and bazel's
				// includes/deps attributes carry the equivalents
				// at higher granularity. Default-toolchain
				// flags (-O2, -fPIC, -g, ...) are kept here;
				// the strip happens later in buildRules with
				// make-db awareness so per-target overrides
				// (`hotloop.o: CFLAGS += -O2`) survive.
				if !strings.HasPrefix(a, "-L") && !strings.HasPrefix(a, "-I") {
					copts = append(copts, a)
				}
			}
		}
	}
	if output == "" {
		return Event{}, false
	}
	// Shared-library link outputs (real libtool's `cc -shared
	// -o libfoo.so.0.0.0` step) are skipped: Bazel's cc_library
	// rule already produces the matching shared object at link
	// time alongside the static archive. Emitting these as
	// EventLink would create a spurious `cc_binary` named
	// libfoo.so.0.0.0 that collides with — and adds nothing
	// over — the cc_library recovered from the matching .a.
	// We also skip libtool's wrapper-mediated `-o foo.lo`
	// outputs: `.lo` is libtool's own metadata file, never a
	// real artifact, and the inner cc invocation that does the
	// real compile (`-o .libs/foo.o` / `-o foo.o`) is captured
	// independently.
	if isSharedLibraryOutput(output) || isLibtoolObject(output) {
		return Event{}, false
	}
	if compileOnly {
		// Compile-only invocations should have exactly one
		// source and an output ending in .o. Tolerate
		// non-.o outputs (some Makefiles use unusual
		// extensions); skip if zero srcs.
		if len(srcs) == 0 {
			return Event{}, false
		}
		return Event{
			Kind:    EventCompile,
			Out:     output,
			Srcs:    srcs,
			Copts:   stableUnique(copts),
			Defines: stableUnique(defines),
		}, true
	}
	// Not -c: compile-and-link or link-only.
	if len(srcs)+len(objs) == 0 {
		return Event{}, false
	}
	return Event{
		Kind:    EventLink,
		Out:     output,
		Srcs:    srcs,
		Objs:    objs,
		Libs:    stableUnique(libs),
		Copts:   stableUnique(copts),
		Defines: stableUnique(defines),
	}, true
}

// isSharedLibraryOutput recognizes the output forms libtool's
// link mode produces for shared libraries: bare `libfoo.so` /
// `libfoo.dylib`, versioned `libfoo.so.0` / `libfoo.so.0.0.0`,
// and the build-tree `.libs/`-prefixed variants. Bazel's
// cc_library covers the equivalent on the consumer side, so
// these events don't need to round-trip through the converter.
func isSharedLibraryOutput(p string) bool {
	base := filepath.Base(p)
	if strings.HasSuffix(base, ".dylib") {
		return true
	}
	// `.so` may be followed by version segments (`.so.0.0.0`).
	// Find the first `.so` boundary and require the remainder
	// to be either empty or `.<digits>(.<digits>)*`.
	idx := strings.Index(base, ".so")
	if idx < 0 {
		return false
	}
	rest := base[idx+len(".so"):]
	if rest == "" {
		return true
	}
	// Versioned shared libs have the shape `.so.<digits>(.<digits>)*`.
	// rest begins with a leading "." that we strip before walking
	// the numeric segments.
	if !strings.HasPrefix(rest, ".") {
		return false
	}
	for _, seg := range strings.Split(rest[1:], ".") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// isLibtoolObject covers libtool's `.lo` metadata file path
// (compile mode) and `.la` text-archive path (link mode).
// Neither is a real compile / link artifact — both are
// libtool-internal bookkeeping written by the libtool shell
// script. The actual cc invocations under the wrapper produce
// `.libs/foo.o` / `.libs/libfoo.so` / `.libs/libfoo.a` and
// are captured in the trace independently.
func isLibtoolObject(p string) bool {
	switch filepath.Ext(p) {
	case ".lo", ".la":
		return true
	}
	return false
}

// perTargetIntentFlags returns the set of flag tokens that
// the make-db's TargetVars[<target>] entries declare for this
// target. The Makefile's per-target CFLAGS / CXXFLAGS /
// CPPFLAGS assignments are the user's intent signal — flags
// added there should survive the default-toolchain strip even
// if Bazel's cc_toolchain would otherwise provide an
// equivalent (the canonical case: `hotloop.o: CFLAGS += -O2`
// for a hot path even when global CFLAGS = -O0).
//
// Returns an empty set when makeDB is nil, when no entries
// match, or when the entries' values don't look like flag
// tokens (e.g., `obj1.o: CC = clang` — that's a different
// kind of override and doesn't produce strip-relevant flags).
func perTargetIntentFlags(makeDB *MakeDB, target string) map[string]bool {
	if makeDB == nil {
		return nil
	}
	tvs := makeDB.TargetVars[target]
	if len(tvs) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, tv := range tvs {
		switch tv.Name {
		case "CFLAGS", "CXXFLAGS", "CPPFLAGS", "CCASFLAGS":
		default:
			continue
		}
		for _, tok := range strings.Fields(tv.Value) {
			out[tok] = true
		}
	}
	return out
}

// stripDefaultsRespectingIntent walks copts; default-toolchain
// flags survive only when the per-target intent set declares
// them. Flag order is preserved.
func stripDefaultsRespectingIntent(copts []string, intent map[string]bool) []string {
	if len(copts) == 0 {
		return nil
	}
	out := make([]string, 0, len(copts))
	for _, c := range copts {
		if isDefaultToolchainFlag(c) && !intent[c] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// isDefaultToolchainFlag reports whether a copt is one Bazel's
// stock cc_toolchain provides per compilation mode / target
// shape. Stripping these from converter output prevents the
// converted Bazel rules from overriding the user's
// `bazel build -c dbg|opt|fastbuild` intent and their
// linkstatic-vs-shared choice.
//
// The list is intentionally narrow: only flags every
// modern cc_toolchain (rules_cc default + sensible vendored
// configs) carries, where the autotools-emitted version
// would be redundant rather than additive.
func isDefaultToolchainFlag(flag string) bool {
	switch flag {
	case "-fPIC", "-fpic", "-fPIE", "-fpie":
		return true
	}
	// Optimization level: -O0, -O1, -O2, -O3, -Os, -Og, -Ofast.
	if strings.HasPrefix(flag, "-O") && len(flag) > 1 {
		return true
	}
	// Debug info: -g, -g0..-g3, -gdwarf etc.
	if flag == "-g" || strings.HasPrefix(flag, "-g") && isDebugInfoFlag(flag) {
		return true
	}
	return false
}

// isDebugInfoFlag matches the family of `-g*` flags that fall
// under Bazel's debug-info handling: -g, -g0..-g3, -ggdb, etc.
// Carved out as a separate predicate because gcc has many
// `-g`-prefixed flags that are NOT debug-info (e.g., -gz,
// -gsplit-dwarf), and we err on the conservative side —
// strip the universal forms, preserve the rest.
func isDebugInfoFlag(flag string) bool {
	switch flag {
	case "-g", "-g0", "-g1", "-g2", "-g3", "-ggdb", "-ggdb0", "-ggdb1", "-ggdb2", "-ggdb3":
		return true
	}
	return false
}

// classifyArchiver inspects an `ar` argv. Recognizes the
// canonical autotools `ar rcs lib<NAME>.a <obj> <obj>...`
// shape; other shapes (extract, list, replace-only) return
// ok=false. The first positional after the operation flags is
// the archive output; everything after that is .o inputs.
func classifyArchiver(argv []string) (Event, bool) {
	if len(argv) < 3 {
		return Event{}, false
	}
	// ar's first non-binary arg is the operation flag set
	// (e.g., "rcs", "rv"). Skip it; verify it includes a
	// create-or-replace operation ('r' or 'q').
	flags := argv[1]
	if !strings.ContainsAny(flags, "rq") {
		return Event{}, false
	}
	output := argv[2]
	if !strings.HasSuffix(output, ".a") {
		return Event{}, false
	}
	var objs []string
	for _, a := range argv[3:] {
		if isObjectFile(a) {
			objs = append(objs, a)
		}
	}
	if len(objs) == 0 {
		return Event{}, false
	}
	return Event{
		Kind: EventArchive,
		Out:  output,
		Objs: objs,
	}, true
}

func isSourceFile(p string) bool {
	switch filepath.Ext(p) {
	case ".c", ".cc", ".cpp", ".cxx", ".C", ".c++":
		return true
	case ".S", ".s":
		// Assembler sources. `.S` is preprocessed-then-assembled
		// (cpp invoked first); `.s` is assembled directly. cc
		// handles both natively and Bazel's cc_library accepts
		// them in srcs alongside `.c`. Real autotools projects
		// (notably libffi's `src/<arch>/sysv.S`) wire arch-
		// specific assembly via configure.host; without `.S` in
		// the source-file allowlist these compile events get
		// silently dropped during classifyArgv.
		return true
	}
	return false
}

func isObjectFile(p string) bool {
	return filepath.Ext(p) == ".o"
}

// Graph is the correlated view of the trace events: which
// archives consume which compile-output objects, which links
// consume which archives. The emitter walks Graph to produce
// cc_library / cc_binary rules.
type Graph struct {
	// objByCwdPath maps `<cwd>/<out>` (the compile event's
	// cwd at exec time joined with its -o output path) to the
	// producing event. Preferred when both the producer and a
	// consumer event have cwd captured — disambiguates
	// recursive-make SUBDIRS where lib/foo.o and src/foo.o
	// would otherwise look identical to objByPath / objByBasename.
	objByCwdPath map[string]Event
	// objByPath maps the compile event's exact output path
	// (e.g., "foo.o", ".libs/foo.o") to the producing event.
	// Used as a fallback when cwd isn't available; libtool-
	// style dual-compile patterns also rely on this tier
	// (`foo.o` vs `.libs/foo.o` — different Out paths).
	objByPath map[string]Event
	// objByBasename is the last-resort fallback for traces
	// where neither side has reliable path information.
	// Last-write-wins on duplicate basenames.
	objByBasename map[string]Event
	// libByBaseName maps a stripped library basename
	// (libfoo.a → "foo") to the archive event that produced it.
	// Used to resolve link events' -l<name> args to local
	// cc_library labels.
	libByBaseName map[string]Event
	// archives are the recovered archive events in
	// trace-arrival order; emit walks them deterministically.
	archives []Event
	// links are the recovered link events in trace-arrival
	// order.
	links []Event
}

// lookupCompile resolves an object path (as it appears in an
// archive's input list or a link command's `.o` arg) to the
// compile event that produced it. The lookup is three-tier:
//
//  1. (cwd, out) exact match — preferred when both events
//     have a cwd captured. Disambiguates same-basename
//     compiles from recursive-make SUBDIRS where lib/foo.o
//     and src/foo.o would otherwise collide.
//  2. ev.Out exact match — falls back when the consumer
//     event (archive / link) has no cwd OR the producer
//     compile event had no cwd. Also keeps libtool-style
//     dual-compiles distinguishable: `foo.o` resolves to
//     the non-PIC compile, `.libs/foo.o` to the PIC one.
//  3. basename match — last-resort for traces where neither
//     side has reliable path information.
//
// `cwd` is the consumer event's cwd; an archive in lib/Makefile
// references "foo.o" relative to lib/, so we compose
// `(consumerCwd, obj)` for the (1) lookup.
func (g *Graph) lookupCompile(consumerCwd, obj string) (Event, bool) {
	if consumerCwd != "" {
		key := consumerCwd + string(filepath.Separator) + obj
		if ev, ok := g.objByCwdPath[key]; ok {
			return ev, true
		}
	}
	if ev, ok := g.objByPath[obj]; ok {
		return ev, true
	}
	ev, ok := g.objByBasename[filepath.Base(obj)]
	return ev, ok
}

func correlate(events []Event) *Graph {
	g := &Graph{
		objByCwdPath:  map[string]Event{},
		objByPath:     map[string]Event{},
		objByBasename: map[string]Event{},
		libByBaseName: map[string]Event{},
	}
	for _, ev := range events {
		switch ev.Kind {
		case EventCompile:
			if ev.Cwd != "" {
				g.objByCwdPath[ev.Cwd+string(filepath.Separator)+ev.Out] = ev
			}
			g.objByPath[ev.Out] = ev
			g.objByBasename[filepath.Base(ev.Out)] = ev
		case EventArchive:
			g.archives = append(g.archives, ev)
			g.libByBaseName[stripLibPrefixSuffix(ev.Out)] = ev
		case EventLink:
			g.links = append(g.links, ev)
		}
	}
	return g
}

// stripLibPrefixSuffix turns `lib<name>.a` (or `<dir>/lib<name>.a`)
// into `<name>` — the form used in `-l<name>` link flags and
// the natural Bazel rule name for the archive.
func stripLibPrefixSuffix(p string) string {
	base := filepath.Base(p)
	base = strings.TrimSuffix(base, ".a")
	return strings.TrimPrefix(base, "lib")
}

// CCRule is the spike's IR target: emitted as either
// `cc_library` or `cc_binary` based on the producing event.
type CCRule struct {
	RuleKind string // "cc_library" or "cc_binary"
	Name     string
	Srcs     []string // sorted
	Hdrs     []string // sorted (build-time-generated headers; AC_CONFIG_HEADERS-style config.h)
	Copts    []string // sorted, deduped
	Defines  []string // sorted, deduped (-D<name>[=<val>] from compile events)
	Deps     []string // sorted (in-tree library labels like ":foo")
}

// emitBuild renders BUILD.bazel.out from the correlated graph.
// One cc_library per archive (sources from constituent .o
// compiles), one cc_binary per link (sources from direct .c
// args + objects' compile sources, deps from `-l<name>` mapped
// to local archives or — via the optional imports manifest —
// to cross-element Bazel labels). Order is stable: libraries
// first sorted by name, then binaries sorted by name.
// emitBuild renders the recovered ruleset as a BUILD.bazel
// string by routing through converter/emit/bazel — the
// same renderer the cmake / meson / fold paths use. Folding to
// the shared renderer (rather than the previous inline format-
// string emit) unifies attribute order, load-line shape,
// `package(default_visibility=...)` header, and per-rule
// visibility elision across every cc-emitting path in the
// repo. Documented in docs/design/build-output-conventions.md
// Phase 1.
func emitBuild(g *Graph, imports *manifest.Resolver, makeDB *MakeDB, generatedHeaders []string) string {
	rules := recoveredRules(g, imports, makeDB, generatedHeaders)
	return renderRules(rules)
}

// renderRules serialises a pre-computed []CCRule slice to
// BUILD.bazel.out text via converter/emit/bazel.Emit
// against toIR(rules). Factored out from emitBuild so the
// --out-ir-json path (which also needs the same post-fold
// ruleset) shares the buildRules + generated-headers
// computation, and so tests can assert the rendering shape
// without rebuilding the trace graph.
//
// On Emit error (only reachable on a template-execution
// pathology Emit's own templates make impossible in practice),
// we fall back to a minimal header so the caller still
// receives a writable BUILD body. The error surface is
// intentionally swallowed here because emitBuild's signature
// is `string` — the trace converter exits 65 on lowering
// failures via main.go's other paths if the IR itself is
// malformed.
func renderRules(rules []CCRule) string {
	pkg := toIR(rules)
	body, err := bazel.Emit(&pkg)
	if err != nil {
		return fmt.Sprintf("# convert-element-trace: emit failed: %v\n", err)
	}
	return string(body)
}

// parseGeneratedHeaderList reads the body of the
// generated-headers list file (one path per line; blank lines
// and `#`-prefixed comments tolerated) and returns the
// non-empty entries. The pipeline emits this file by snapshotting
// the build dir's `*.h` set before configure and after make,
// then running `comm -13` on the sorted lists; the result is
// the set of headers created by the build (config.h-style
// outputs of AC_CONFIG_HEADERS, yacc parser headers, etc.).
func parseGeneratedHeaderList(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `find . -type f -name '*.h'` emits paths like
		// `./config.h`. Drop the leading `./` so the BUILD
		// reference is package-relative.
		out = append(out, strings.TrimPrefix(line, "./"))
	}
	return out
}

func buildRules(g *Graph, imports *manifest.Resolver, makeDB *MakeDB) []CCRule {
	var libs, bins []CCRule
	// Archives → cc_library.
	for _, a := range g.archives {
		var srcs, copts, defines []string
		for _, obj := range a.Objs {
			c, ok := g.lookupCompile(a.Cwd, obj)
			if !ok {
				continue
			}
			srcs = append(srcs, c.Srcs...)
			// Per-target strip: keep flags the Makefile's
			// per-target CFLAGS declared (intent), strip the
			// rest of the default-toolchain flags.
			intent := perTargetIntentFlags(makeDB, filepath.Base(obj))
			copts = append(copts, stripDefaultsRespectingIntent(c.Copts, intent)...)
			defines = append(defines, c.Defines...)
		}
		if len(srcs) == 0 {
			continue
		}
		libs = append(libs, CCRule{
			RuleKind: "cc_library",
			Name:     stripLibPrefixSuffix(a.Out),
			Srcs:     stableUnique(srcs),
			Copts:    stableUnique(copts),
			Defines:  stableUnique(defines),
		})
	}
	// Links → cc_binary.
	for _, l := range g.links {
		var srcs, copts, defines, deps []string
		// Direct .c args carry their own copts/defines (event-level).
		// Compile-and-link invocations get the binary's name as
		// the "target" key for per-target intent lookup.
		srcs = append(srcs, l.Srcs...)
		linkIntent := perTargetIntentFlags(makeDB, filepath.Base(l.Out))
		copts = append(copts, stripDefaultsRespectingIntent(l.Copts, linkIntent)...)
		defines = append(defines, l.Defines...)
		// Each .o input expands to its compile event's source +
		// copts + defines (last-write-wins on duplicate basenames).
		for _, obj := range l.Objs {
			c, ok := g.lookupCompile(l.Cwd, obj)
			if !ok {
				continue
			}
			srcs = append(srcs, c.Srcs...)
			objIntent := perTargetIntentFlags(makeDB, filepath.Base(obj))
			copts = append(copts, stripDefaultsRespectingIntent(c.Copts, objIntent)...)
			defines = append(defines, c.Defines...)
		}
		// `-l<name>` arg resolves in two stages:
		//   1. If the trace also produced an archive
		//      `lib<name>.a`, link against the in-tree
		//      cc_library (`:<name>`).
		//   2. Otherwise fall back to the imports manifest's
		//      LookupLinkLibrary, which carries cross-element
		//      and system-library mappings (e.g. `-lz` →
		//      `//elements/zlib:zlib`). Mirrors lower's cmake
		//      STATIC IMPORTED dep recovery.
		// Unresolved names are dropped silently — the link
		// command's standard libs (`-lc`, `-lgcc_s`) typically
		// flow via Bazel's cc_toolchain, not user-visible deps.
		for _, lib := range l.Libs {
			if _, ok := g.libByBaseName[lib]; ok {
				deps = append(deps, ":"+lib)
				continue
			}
			if ex := imports.LookupLinkLibrary(lib); ex != nil {
				deps = append(deps, ex.BazelLabel)
			}
		}
		if len(srcs) == 0 {
			continue
		}
		bins = append(bins, CCRule{
			RuleKind: "cc_binary",
			Name:     filepath.Base(l.Out),
			Srcs:     stableUnique(srcs),
			Copts:    stableUnique(copts),
			Defines:  stableUnique(defines),
			Deps:     stableUnique(deps),
		})
	}
	sort.Slice(libs, func(i, j int) bool { return libs[i].Name < libs[j].Name })
	sort.Slice(bins, func(i, j int) bool { return bins[i].Name < bins[j].Name })
	return append(libs, bins...)
}

// stableUnique sorts and dedupes a slice of strings.
func stableUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	return dedup(cp)
}

func dedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := []string{in[0]}
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// parseExecveLine returns the argv from an strace `execve(...)`
// line when the call succeeded. Lines from `strace -f -e
// trace=execve` look like:
//
//	1234  execve("/usr/bin/cc", ["cc", "-O2", "-o", "greet", "greet.c"], 0x... /* N vars */) = 0
//
// We tolerate the optional PID prefix and reject failed
// execves (return value != 0) as well as `<unfinished ...>`
// continuation fragments.
func parseExecveLine(line string) ([]string, bool) {
	idx := strings.Index(line, "execve(")
	if idx < 0 {
		return nil, false
	}
	// Trailing `= 0` (or other return code). Bail on failures
	// and on the unfinished-call resumption fragments strace
	// emits when other syscalls interleave.
	if len(line) < 12 {
		return nil, false
	}
	tail := line[len(line)-12:]
	if !strings.Contains(tail, "= 0") {
		return nil, false
	}
	if strings.Contains(line, "<unfinished") || strings.Contains(line, "resumed>") {
		return nil, false
	}
	rest := line[idx+len("execve("):]
	pathEnd := indexAfterQuoted(rest, 0)
	if pathEnd < 0 {
		return nil, false
	}
	rest = rest[pathEnd+1:]
	rest = strings.TrimLeft(rest, ", ")
	if !strings.HasPrefix(rest, "[") {
		return nil, false
	}
	rest = rest[1:]
	end := indexUnescapedClose(rest, ']')
	if end < 0 {
		return nil, false
	}
	return parseArgvBlock(rest[:end]), true
}

func indexAfterQuoted(s string, start int) int {
	if start >= len(s) || s[start] != '"' {
		return -1
	}
	i := start + 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i
		}
		i++
	}
	return -1
}

func indexUnescapedClose(s string, close byte) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			j := indexAfterQuoted(s, i)
			if j < 0 {
				return -1
			}
			i = j
		case close:
			return i
		}
	}
	return -1
}

func parseArgvBlock(block string) []string {
	var out []string
	i := 0
	for i < len(block) {
		for i < len(block) && (block[i] == ' ' || block[i] == ',') {
			i++
		}
		if i >= len(block) {
			break
		}
		if block[i] != '"' {
			break
		}
		end := indexAfterQuoted(block, i)
		if end < 0 {
			break
		}
		out = append(out, unescapeStraceString(block[i+1:end]))
		i = end + 1
	}
	return out
}

func unescapeStraceString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		next := s[i]
		switch next {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(next)
		}
	}
	return b.String()
}
