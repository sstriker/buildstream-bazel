// relax-keeps walks a converted project B and selectively
// removes `# keep` markers from genrules whose `cmd` attribute
// matches an operator-declared pattern in
// `tools/gazelle-rewritable.json`. This is the "best of both
// worlds" lever for continuous-conversion loops:
//
//   - Default state (empty patterns list): every genrule
//     keeps its Phase-7a `# keep` marker, preserving literal-
//     CMake fidelity. Continuous re-conversion runs are
//     byte-stable; the operator's untouched BUILDs round-trip.
//
//   - Operator opts in by adding a pattern entry (e.g.
//     `{"cmd_contains": "protoc"}`): relax-keeps strips the
//     `# keep` from matching genrules, letting downstream
//     `gazelle fix` (with the operator's protoc-aware
//     extension wired into overlay.MODULE.bazel) rewrite the
//     genrule into `proto_library` + `cc_proto_library` on
//     every continuous re-conversion run.
//
// Non-matching genrules keep their `# keep` marker — those
// stay literal until the operator declares a pattern for
// them.
//
// Per Phase 8 of ROADMAP.md.
// Designed to run after build-cc-index and before the
// `bazel run //:gazelle` step of the write-a + Bazel
// driver's Phase 8b gazelle tail.
//
// See ROADMAP.md for the full
// post-conversion + gazelle workflow.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

const (
	exitSuccess = 0
	exitUsage   = 64
	exitTier2   = 65
)

// rewritableConfig is the on-disk shape of
// tools/gazelle-rewritable.json — the operator's declaration
// of which genrule cmd patterns their gazelle setup can
// rewrite. Schema version 1: a flat patterns list, each entry
// with a substring matcher on the genrule's cmd.
type rewritableConfig struct {
	Version  int       `json:"version"`
	Patterns []pattern `json:"patterns"`
}

type pattern struct {
	// Name is a human-facing label for the pattern, e.g.
	// "protoc" or "flatc". Used in log messages and as an
	// operator-readable identifier in the config file.
	Name string `json:"name"`

	// CmdContains is the substring relax-keeps matches
	// against the genrule's cmd attribute. Simple substring
	// match — sufficient for the typical "is this a protoc
	// invocation" question, and easy for operators to
	// reason about without learning a regex dialect.
	// Future versions of the schema may add a regex form;
	// the version field bumps when that lands.
	CmdContains string `json:"cmd_contains"`
}

type args struct {
	root           string
	rewritableFile string
	packages       []string
	verbose        bool
}

func main() {
	a, code := parseArgs(os.Args[1:], os.Stderr)
	if code != exitSuccess {
		os.Exit(code)
	}
	if err := run(a); err != nil {
		fmt.Fprintf(os.Stderr, "relax-keeps: %v\n", err)
		os.Exit(exitTier2)
	}
}

func parseArgs(argv []string, stderr *os.File) (args, int) {
	flags := flag.NewFlagSet("relax-keeps", flag.ContinueOnError)
	flags.SetOutput(stderr)
	a := args{}
	flags.StringVar(&a.root, "root", "", "absolute path to the project-B root (the directory containing MODULE.bazel + tools/)")
	flags.StringVar(&a.rewritableFile, "rewritable", "tools/gazelle-rewritable.json", "path to the operator-owned rewritable-patterns config, relative to --root")
	flags.BoolVar(&a.verbose, "verbose", false, "log every relaxed genrule to stderr")
	if err := flags.Parse(argv); err != nil {
		return a, exitUsage
	}
	a.packages = flags.Args()
	if a.root == "" {
		fmt.Fprintln(stderr, "relax-keeps: --root is required")
		flags.Usage()
		return a, exitUsage
	}
	abs, err := filepath.Abs(a.root)
	if err != nil {
		fmt.Fprintf(stderr, "relax-keeps: resolve --root %q: %v\n", a.root, err)
		return a, exitUsage
	}
	a.root = abs
	return a, exitSuccess
}

func run(a args) error {
	cfg, err := loadConfig(filepath.Join(a.root, a.rewritableFile))
	if err != nil {
		return err
	}
	if len(cfg.Patterns) == 0 {
		// No patterns declared — nothing to relax. The stub
		// default operator state. Exit silently so a CI
		// pipeline running relax-keeps unconditionally
		// produces no noise.
		return nil
	}
	// Targeted vs full-walk: when the caller passes a list
	// of package paths (typically the elements that
	// re-converted on this run — the `$changed` list
	// cmd/stage-b emits), only walk those subtrees. The
	// write-a + Bazel driver's Phase 8b tail uses this for
	// incremental relaxation — O(packages_changed) instead
	// of O(workspace). When no packages are given, fall
	// back to a full project-B walk, used by ad-hoc manual
	// invocations.
	roots := []string{a.root}
	if len(a.packages) > 0 {
		roots = roots[:0]
		for _, pkg := range a.packages {
			roots = append(roots, filepath.Join(a.root, filepath.FromSlash(pkg)))
		}
	}
	relaxed := 0
	for _, r := range roots {
		// Skip roots that don't exist — happens when the
		// caller passes a package path that didn't actually
		// produce output on this run. filepath.WalkDir would
		// error otherwise.
		if _, err := os.Stat(r); os.IsNotExist(err) {
			continue
		}
		n, err := walkAndRelax(a.root, r, cfg.Patterns, a.verbose)
		if err != nil {
			return err
		}
		relaxed += n
	}
	if a.verbose {
		fmt.Fprintf(os.Stderr, "relax-keeps: relaxed %d genrule(s)\n", relaxed)
	}
	return nil
}

// walkAndRelax walks subtree under walkRoot for BUILD.bazel
// files, relaxes matching genrules in each, and returns the
// relaxed-genrule count. rootForRel is the project-B root
// used to compute display-path-relative paths for verbose
// logging (so log lines read like
// "elements/foo/BUILD.bazel" regardless of whether the
// walker entered from the full root or a per-package sub-
// root).
func walkAndRelax(rootForRel, walkRoot string, patterns []pattern, verbose bool) (int, error) {
	relaxed := 0
	err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		if base != "BUILD.bazel" && base != "BUILD" {
			return nil
		}
		rel, err := filepath.Rel(rootForRel, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %v", p, err)
		}
		f, perr := build.Parse(p, body)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "relax-keeps: skipping %s: parse: %v\n", rel, perr)
			return nil
		}
		n := relaxFile(f, patterns, verbose, rel)
		if n == 0 {
			return nil
		}
		relaxed += n
		out := build.Format(f)
		return os.WriteFile(p, out, 0o644)
	})
	return relaxed, err
}

// loadConfig reads + parses tools/gazelle-rewritable.json.
// Returns an empty config (no patterns) when the file
// doesn't exist — the operator's stub default. Hard fail
// on malformed JSON so an operator-introduced syntax error
// doesn't silently disable relaxation.
func loadConfig(path string) (*rewritableConfig, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &rewritableConfig{}, nil
		}
		return nil, fmt.Errorf("read %s: %v", path, err)
	}
	cfg := &rewritableConfig{}
	if err := json.Unmarshal(body, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %v", path, err)
	}
	if cfg.Version != 0 && cfg.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported schema version %d (want 1)", path, cfg.Version)
	}
	return cfg, nil
}

// relaxFile walks every top-level statement in f, finds
// `genrule(...)` calls whose cmd attribute matches any
// pattern's cmd_contains substring, and strips the `# keep`
// suffix marker. Returns the count of genrules relaxed. f
// is mutated in place; callers re-Format and write back.
//
// Idempotent: a genrule that lost its `# keep` on a prior
// relax-keeps run has no marker to strip, so a second run
// counts zero rather than double-acting.
func relaxFile(f *build.File, patterns []pattern, verbose bool, rel string) int {
	relaxed := 0
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		ident, ok := call.X.(*build.Ident)
		if !ok || ident.Name != "genrule" {
			continue
		}
		// Only act on genrules that currently carry the
		// keep marker. Otherwise we'd "relax" rules the
		// operator has hand-removed-then-re-added keeps on.
		keepIdx := -1
		for i, c := range call.Comment().Suffix {
			if strings.TrimSpace(c.Token) == "# keep" {
				keepIdx = i
				break
			}
		}
		if keepIdx < 0 {
			continue
		}
		cmd := stringAttr(call, "cmd")
		if cmd == "" {
			continue
		}
		matched := matchingPattern(cmd, patterns)
		if matched == "" {
			continue
		}
		// Drop the keep marker by removing the matching
		// Comment from Suffix. Preserves any other suffix
		// comments (rare in our emission but possible after
		// operator edits).
		call.Comment().Suffix = append(call.Comment().Suffix[:keepIdx], call.Comment().Suffix[keepIdx+1:]...)
		relaxed++
		if verbose {
			name := stringAttr(call, "name")
			fmt.Fprintf(os.Stderr, "relax-keeps: %s: relaxed genrule %q (pattern %q)\n", rel, name, matched)
		}
	}
	return relaxed
}

// matchingPattern returns the name of the first pattern
// whose CmdContains substring appears in cmd, or "" when no
// pattern matches.
func matchingPattern(cmd string, patterns []pattern) string {
	for _, p := range patterns {
		if p.CmdContains == "" {
			continue
		}
		if strings.Contains(cmd, p.CmdContains) {
			return p.Name
		}
	}
	return ""
}

// stringAttr returns the string-literal value of the named
// keyword argument on call, or "" when absent / non-string.
func stringAttr(call *build.CallExpr, attr string) string {
	for _, a := range call.List {
		assign, ok := a.(*build.AssignExpr)
		if !ok {
			continue
		}
		ident, ok := assign.LHS.(*build.Ident)
		if !ok || ident.Name != attr {
			continue
		}
		if s, ok := assign.RHS.(*build.StringExpr); ok {
			return s.Value
		}
	}
	return ""
}
