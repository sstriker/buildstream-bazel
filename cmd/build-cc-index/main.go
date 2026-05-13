// build-cc-index walks a converted Bazel project's BUILD.bazel
// files and emits the two metadata files gazelle reads to drive
// header-scan / module-name dep resolution:
//
//   - cc_index.json: header path → Bazel label, sourced from every
//     emitted cc_library's hdrs (and the .h/.hpp/.hxx subset of
//     its srcs — the codemodel sometimes lists private headers
//     in srcs, and gazelle's header-scan resolver consults the
//     index for ANY #include line regardless of public/private
//     intent, so widening the index entries from srcs-side
//     captures pre-existing under-reporting cheaply). The
//     emitted label is `//<package>:<target_name>` where
//     <package> is the BUILD file's path relative to --root.
//
//   - python_modules.json: dist-name → Bazel label, sourced from
//     every py_binary's name (matching pyproject.toml's
//     [project.scripts] entry name) and every py_library's name
//     when the name is a valid Python distribution name. Mirrors
//     rules_python gazelle plugin's `modules_mapping.json`
//     schema (flat top-level object).
//
// Designed to run AFTER write-a + per-element conversion has
// produced the staged BUILD.bazel files in project B. Invoked
// once per project-B render; output is deterministic and
// idempotent.
//
// Per Phase 7c of docs/design/build-output-conventions.md. The
// stub `{}` files Phase 7b wrote get rewritten in place — the
// stable file paths the MODULE.bazel `# gazelle:cc_indexfile` /
// `# gazelle:python_module_mapping` directives reference don't
// change.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

const (
	exitSuccess = 0
	exitUsage   = 64
	exitTier2   = 65
)

type args struct {
	root         string
	outCCIndex   string
	outPyModules string
}

func main() {
	a, code := parseArgs(os.Args[1:], os.Stderr)
	if code != exitSuccess {
		os.Exit(code)
	}
	if err := run(a); err != nil {
		fmt.Fprintf(os.Stderr, "build-cc-index: %v\n", err)
		os.Exit(exitTier2)
	}
}

func parseArgs(argv []string, stderr *os.File) (args, int) {
	flags := flag.NewFlagSet("build-cc-index", flag.ContinueOnError)
	flags.SetOutput(stderr)
	a := args{}
	flags.StringVar(&a.root, "root", "", "absolute path to the project-B root (the directory containing MODULE.bazel and elements/)")
	flags.StringVar(&a.outCCIndex, "out-cc-index", "tools/cc_index.json", "destination path for the cc header → label map, relative to --root")
	flags.StringVar(&a.outPyModules, "out-python-modules", "tools/python_modules.json", "destination path for the python dist-name → label map, relative to --root")
	if err := flags.Parse(argv); err != nil {
		return a, exitUsage
	}
	if a.root == "" {
		fmt.Fprintln(stderr, "build-cc-index: --root is required")
		flags.Usage()
		return a, exitUsage
	}
	abs, err := filepath.Abs(a.root)
	if err != nil {
		fmt.Fprintf(stderr, "build-cc-index: resolve --root %q: %v\n", a.root, err)
		return a, exitUsage
	}
	a.root = abs
	return a, exitSuccess
}

func run(a args) error {
	ccIndex := map[string]string{}
	pyMods := map[string]string{}

	err := filepath.WalkDir(a.root, func(p string, d fs.DirEntry, err error) error {
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
		// Skip the project-B-root BUILD.bazel — has no
		// per-element rules; just a comment-only file.
		rel, err := filepath.Rel(a.root, p)
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if pkg == "." {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %v", p, err)
		}
		f, perr := build.Parse(p, body)
		if perr != nil {
			// Skip unparseable BUILDs rather than fail the
			// whole walk — the file may be hand-edited by an
			// operator. Index entries from other packages
			// still get recorded.
			fmt.Fprintf(os.Stderr, "build-cc-index: skipping %s: parse: %v\n", rel, perr)
			return nil
		}
		harvestPackage(f, pkg, ccIndex, pyMods)
		return nil
	})
	if err != nil {
		return err
	}

	if err := writeJSON(filepath.Join(a.root, a.outCCIndex), ccIndex); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(a.root, a.outPyModules), pyMods); err != nil {
		return err
	}
	return nil
}

// harvestPackage walks every rule in f and records the header
// → label / dist → label entries into the per-call maps. pkg
// is the package's path relative to the project-B root, in
// slash form (e.g. "elements/components/hello"). Both maps
// are keyed for first-write-wins semantics — duplicate header
// paths across packages (rare in practice; would imply two
// targets exporting the same header) keep the alphabetically-
// earlier package's claim, matching gazelle_cc's own first-
// declared-wins resolution shape.
func harvestPackage(f *build.File, pkg string, ccIndex, pyMods map[string]string) {
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		kind := callRuleKind(call)
		name := stringArg(call, "name")
		if name == "" {
			continue
		}
		label := canonicalLabel(pkg, name)
		switch kind {
		case "cc_library":
			for _, h := range stringListArg(call, "hdrs") {
				rec(ccIndex, path.Join(pkg, h), label)
			}
			for _, s := range stringListArg(call, "srcs") {
				if isHeaderPath(s) {
					rec(ccIndex, path.Join(pkg, s), label)
				}
			}
		case "cc_test", "cc_binary":
			// cc_test / cc_binary aren't sources of public
			// headers (they're consumers), but their srcs may
			// include private headers that the index can
			// resolve back to. Skip srcs-side harvesting on
			// non-library kinds — gazelle's expectation is
			// that resolution goes to library labels, not
			// binary/test labels.
		case "cc_import":
			// cc_import doesn't carry a hdrs attribute in
			// stock rules_cc; its includes attribute (when
			// emitted via the Phase 2 wrap) carries
			// directories not files. Skip — operators
			// wanting cross-element header resolution
			// against a cc_import should wrap it in a
			// cc_library with strip_include_prefix.
		case "py_binary":
			// Phase 5's strict-shape py_binary has main +
			// srcs pointing at the module file. The script
			// name IS the dist-name an operator imports as
			// (per [project.scripts]). Record name → label.
			pyMods[name] = label
		case "py_library":
			// py_library names are dist-names too (rules_python
			// gazelle plugin's convention). Record so a
			// cross-element `import <dist_name>` resolves.
			pyMods[name] = label
		}
	}
}

// canonicalLabel returns the short form of a Bazel label:
// `//pkg:pkg` collapses to `//pkg` when the target name
// matches the package's basename. Matches buildifier /
// gazelle's own label-canonicalization rule (Phase 3 already
// applies this same shortening to the cc emitter's output).
func canonicalLabel(pkg, target string) string {
	base := path.Base(pkg)
	if base == target {
		return "//" + pkg
	}
	return "//" + pkg + ":" + target
}

// isHeaderPath reports whether s is a C/C++ header by
// extension. Mirrors the conventional .h / .hpp / .hxx / .hh
// / .h++ / .H set; cmake codemodel surfaces these in
// target.sources for include-only files.
func isHeaderPath(s string) bool {
	switch strings.ToLower(filepath.Ext(s)) {
	case ".h", ".hpp", ".hxx", ".hh", ".h++":
		return true
	}
	return false
}

// rec inserts k→v into m on first-write. Later writes are
// dropped silently — duplicate header paths across packages
// keep the alphabetically-earlier (first-walked) claim.
func rec(m map[string]string, k, v string) {
	if _, ok := m[k]; ok {
		return
	}
	m[k] = v
}

// callRuleKind returns the rule kind for a CallExpr or "" if
// the function position isn't a bare Ident.
func callRuleKind(call *build.CallExpr) string {
	if ident, ok := call.X.(*build.Ident); ok {
		return ident.Name
	}
	return ""
}

// stringArg returns the string-literal value of the named
// keyword argument, or "" when absent / non-string.
func stringArg(call *build.CallExpr, attr string) string {
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

// stringListArg returns the contents of a list-valued
// keyword argument as a Go slice. Non-string entries are
// silently dropped; absent or non-list returns nil.
func stringListArg(call *build.CallExpr, attr string) []string {
	for _, a := range call.List {
		assign, ok := a.(*build.AssignExpr)
		if !ok {
			continue
		}
		ident, ok := assign.LHS.(*build.Ident)
		if !ok || ident.Name != attr {
			continue
		}
		list, ok := assign.RHS.(*build.ListExpr)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(list.List))
		for _, item := range list.List {
			if s, ok := item.(*build.StringExpr); ok {
				out = append(out, s.Value)
			}
		}
		return out
	}
	return nil
}

// writeJSON marshals m to a deterministic JSON object
// (alphabetical keys) and writes it to path. Creates parent
// directories as needed.
func writeJSON(p string, m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(m))
	for _, k := range keys {
		ordered[k] = m[k]
	}
	// json.Marshal sorts maps alphabetically by default, so
	// the intermediate sort is for the sake of the loop
	// below; the on-disk output is deterministic.
	body, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(p, body, 0o644)
}
