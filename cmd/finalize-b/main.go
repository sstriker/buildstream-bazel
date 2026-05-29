// finalize-b is the deliverable-handover step for the
// cross-element configure-step bootstrap. It takes a converged
// project B at `--in <path>` and writes a stripped standalone
// Bazel project at `--out <dest>`:
//
//   - Removes dead `<elem>_trace_build` genrule targets — those
//     whose element has fine-grained cc rules (cc_library /
//     cc_binary / cc_import emitted by the converter) and where
//     no consumer references the install_tree.tar filegroup. The
//     trace_build was conversion-era scaffolding to publish
//     traces between rounds; once the converged shape has fine
//     rules and consumers point at them, the genrule is just
//     dead bytes that re-run an expensive build on every
//     re-pull.
//   - Drops conversion-era intermediate filegroups
//     (:install_tree.tar, :cmake_config_bundle, :pkg_config_bundle,
//     :build_bazel) where no surviving target references them.
//     These were emitted to wire the round-2 publish/lookup
//     contract; the finalized project has no use for them.
//   - Prunes the `bazel_dep(name = "rules_buildstream_bazel")` +
//     `local_path_override(...)` block from MODULE.bazel once no
//     surviving target in the finalized project references the
//     package (via load("@rules_buildstream_bazel//..."),
//     trace_load(...), or similar). Pure-cmake projects where
//     every element converted cleanly end up with no rules_buildstream_bazel
//     reference; the bazel_dep gets removed.
//
// Out of scope for v1: pruning the `tools/build-tracer`,
// `tools/trace-publish`, `tools/trace-lookup` exports from
// `tools/BUILD.bazel`. The current strip path only fires when
// the BUILD has fine cc rules, which `tools/BUILD.bazel`
// typically doesn't — that pruning needs a separate, tag-based
// or whitelist-based code path. Operators can hand-remove the
// tools/ exports + binaries once they're confident no element
// re-publishes a trace.
//
// The tool is **idempotent** — running it on an already-
// finalized project is a no-op (no trace_build targets to
// remove, no rules_buildstream_bazel dep to prune). And it's
// **reversible** by virtue of being non-destructive: the
// --in directory is never modified; --out is written from
// scratch.
//
// Usage:
//
//	finalize-b --in <converged-B> --out <stripped-B>
//
// Both paths must be absolute. --out must not already exist.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

func main() {
	log.SetFlags(0)
	in := flag.String("in", "", "absolute path to the converged project B")
	out := flag.String("out", "", "absolute path to write the stripped standalone Bazel project. Must not already exist.")
	flag.Parse()
	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if !filepath.IsAbs(*in) || !filepath.IsAbs(*out) {
		log.Fatalf("--in and --out must be absolute paths")
	}
	if _, err := os.Stat(*out); err == nil {
		log.Fatalf("--out %q already exists; finalize-b refuses to overwrite", *out)
	}
	if err := run(*in, *out); err != nil {
		log.Fatalf("finalize-b: %v", err)
	}
}

// run reads project B at inDir, walks per-element BUILDs,
// computes the cleanup plan, and writes the result to outDir.
// Walks the source tree, copies non-BUILD files verbatim, and
// processes BUILD.bazel files through the per-package cleanup
// pass.
func run(inDir, outDir string) error {
	// First pass: walk per-element BUILDs, parse them, decide
	// per-element whether the trace_build genrule is dead.
	// Track which BUILDs would still reference @rules_buildstream_bazel
	// after the cleanup so MODULE.bazel can prune the bazel_dep
	// if it's no longer used.
	plan, err := computeCleanupPlan(inDir)
	if err != nil {
		return fmt.Errorf("compute cleanup plan: %w", err)
	}

	// Second pass: walk again, this time copying files into
	// outDir. BUILD.bazel files go through cleanupBuild;
	// everything else copies verbatim. MODULE.bazel goes through
	// cleanupModule with the plan in hand.
	return filepath.Walk(inDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(inDir, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(outDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, info.Mode())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch filepath.Base(path) {
		case "BUILD.bazel":
			cleaned, err := cleanupBuild(body, rel)
			if err != nil {
				return fmt.Errorf("clean BUILD %s: %w", rel, err)
			}
			body = cleaned
		case "MODULE.bazel":
			if rel == "MODULE.bazel" {
				cleaned, err := cleanupModule(body, plan)
				if err != nil {
					return fmt.Errorf("clean MODULE.bazel: %w", err)
				}
				body = cleaned
			}
		}
		return os.WriteFile(dst, body, info.Mode())
	})
}

// cleanupPlan tracks workspace-level facts the finalize pass
// needs to make decisions that look beyond a single BUILD file:
//
//   - rulesPackageStillUsed: whether any per-element BUILD's
//     POST-CLEANUP body still references @rules_buildstream_bazel
//     (via a load() statement or via a still-living trace_load
//     target). When false, the MODULE.bazel cleanup prunes the
//     bazel_dep + local_path_override block.
type cleanupPlan struct {
	rulesPackageStillUsed bool
}

// computeCleanupPlan does a dry-run pass over the project tree
// to gather the cross-package facts cleanupBuild + cleanupModule
// need. Functionally equivalent to running cleanupBuild on each
// BUILD and inspecting the post-cleanup shape — but doing it
// twice (once for the plan, once for the write) keeps the
// per-file passes pure (no global side effects) and the result
// is byte-stable.
func computeCleanupPlan(inDir string) (*cleanupPlan, error) {
	plan := &cleanupPlan{}
	err := filepath.Walk(inDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "BUILD.bazel" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(inDir, path)
		cleaned, err := cleanupBuild(body, rel)
		if err != nil {
			return err
		}
		if mentionsRulesPackage(cleaned) {
			plan.rulesPackageStillUsed = true
		}
		return nil
	})
	return plan, err
}

// mentionsRulesPackage reports whether a (cleaned) BUILD body
// still references @rules_buildstream_bazel. We pattern-match
// the textual form rather than re-parsing — load() statements
// and bazel-dep references in BUILD files are visible as
// literal strings.
func mentionsRulesPackage(body []byte) bool {
	return strings.Contains(string(body), "@rules_buildstream_bazel//")
}

// cleanupBuild parses a per-package BUILD.bazel and removes:
//
//   - trace_load targets (no longer needed once the converged
//     fine cc rules don't reference them — and they never
//     directly do; the converter genrule consumed the trace
//     filegroup, not the cc rules)
//   - trace_build genrules (the install/publish action; dead
//     once the per-element fine cc rules exist)
//   - conversion-era intermediate filegroups
//     (`:install_tree.tar`, `:cmake_config_bundle`,
//     `:pkg_config_bundle`, `:build_bazel`) when no surviving
//     target references them. These were emitted to wire the
//     round-2 publish/lookup contract; once trace_build is
//     gone, they have no purpose.
//
// Decision rule for whether to strip vs preserve a given
// element's trace_build: if the BUILD has at least one
// fine-grained `cc_library` / `cc_binary` / `cc_import` rule,
// the element has converged and the trace_build is dead. If the
// BUILD's only rules are the conversion-era genrules (trace_build
// + the bundle filegroups), the element hasn't converged — keep
// everything.
//
// Also drops the `load("@rules_buildstream_bazel//rules:traces.bzl", ...)`
// and `load("@rules_buildstream_bazel//rules:zero_files.bzl", ...)`
// load() statements when no surviving rule in the BUILD uses
// them (i.e. once trace_load + zero_files targets are gone).
// elemInstallName returns the per-platform install-root select()
// filegroup name for the element whose BUILD.bazel is at relPath
// (e.g. "elements/foo/BUILD.bazel" -> "foo_install"). The
// multi-platform round-2 fan-out emits this filegroup; it's
// conversion-era scaffolding stripped once the element converges.
func elemInstallName(relPath string) string {
	dir := filepath.Dir(relPath)
	base := filepath.Base(dir)
	if base == "." || base == string(filepath.Separator) || base == "" {
		// No element segment to derive a name from; return a
		// sentinel that won't match any real filegroup name.
		return "\x00"
	}
	return base + "_install"
}

func cleanupBuild(body []byte, relPath string) ([]byte, error) {
	f, err := build.Parse(relPath, body)
	if err != nil {
		// If the file isn't parseable, leave it untouched —
		// finalize-b shouldn't make destructive changes to
		// files it can't reason about.
		return body, nil
	}

	// hasFineCC = "has cc rules NOT tagged as fallback-shape
	// stubs." kind:cmake / kind:meson Phase B fallback BUILDs
	// emit cc_import + sh_binary referencing paths the
	// _install_tree_extract genrule produces from
	// install_tree.tar — those cc_import calls are interlocked
	// with the trace_build scaffolding, not converged
	// replacements. Stripping the scaffolding under them leaves
	// dangling label refs. The codegen-target-fallback tag
	// (cmake-codegen-target-fallback / meson-codegen-target-fallback)
	// is the renderer's marker for "this is a fallback stub,
	// not a converted rule."
	hasFineCC := false
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		name, ok := callIdent(call)
		if !ok {
			continue
		}
		switch name {
		case "cc_library", "cc_binary", "cc_import", "cc_test":
			if hasStringInListAttr(call, "tags", "cmake-codegen-target-fallback") ||
				hasStringInListAttr(call, "tags", "meson-codegen-target-fallback") {
				continue
			}
			hasFineCC = true
		}
	}

	// If the element hasn't converged (no fine cc rules), don't
	// touch this BUILD. The trace_build + filegroups are still
	// load-bearing.
	if !hasFineCC {
		return body, nil
	}

	// Element has converged. Strip the conversion-era scaffolding.
	stripped := make([]build.Expr, 0, len(f.Stmt))
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			stripped = append(stripped, stmt)
			continue
		}
		ruleKind, _ := callIdent(call)
		nameAttr := stringAttr(call, "name")

		// Strip trace_load + zero_files targets entirely.
		if ruleKind == "trace_load" || ruleKind == "zero_files" {
			continue
		}
		// Strip the trace_build install rule. The recognition is by
		// the `tags = ["trace_build"]` attribute the round-2
		// install templates emit (set by handler_pipeline.go's
		// IsTraceBuild flag + the standalone cmake/meson round-2
		// templates' literal `tags = ["trace_build"]`). The install
		// rule is now a pipeline_install (TreeArtifact) rather than
		// a genrule; match on the tag regardless of rule kind so a
		// future rename of the install rule keeps converging.
		if hasStringInListAttr(call, "tags", "trace_build") {
			continue
		}
		// Strip conversion-era intermediate filegroups that
		// nothing in this BUILD references. The match is by
		// well-known target names; surviving consumers (if any)
		// would have produced fine cc rules that don't need
		// them, so the filegroups dangle. "<elem>_install" is the
		// per-platform install-root select() filegroup the
		// multi-platform fan-out emits (the TreeArtifact-era
		// successor to the old "install_tree.tar" filegroup).
		if ruleKind == "filegroup" {
			switch nameAttr {
			case "install_tree.tar", "cmake_config_bundle",
				"pkg_config_bundle", "build_bazel", elemInstallName(relPath):
				continue
			}
		}
		// Strip the round-2 execute-process-fallback pick_file
		// stubs (and the old extract genrule, for forward-compat).
		// They project files out of the now-stripped trace_build
		// install root; once fine cc rules exist they dangle.
		if hasStringInListAttr(call, "tags", "cmake-codegen-execute-process-fallback-extract") ||
			hasStringInListAttr(call, "tags", "meson-codegen-target-fallback-extract") {
			continue
		}
		stripped = append(stripped, stmt)
	}
	f.Stmt = stripped

	// Drop load() statements whose imported names are no longer
	// referenced. This catches the load("@rules_buildstream_bazel//rules:...")
	// lines that loaded trace_load / zero_files.
	f.Stmt = pruneUnusedLoads(f.Stmt)

	return build.Format(f), nil
}

// pruneUnusedLoads drops load() statements where every imported
// name has no remaining call site in the statement list. A name
// is "referenced" if it appears as the callee of any CallExpr or
// as an identifier in any other position; we just check for any
// CallExpr whose function-name matches.
func pruneUnusedLoads(stmts []build.Expr) []build.Expr {
	// Collect every callee name in the post-strip statement list.
	calleeNames := map[string]bool{}
	build.Walk(&build.File{Stmt: stmts}, func(expr build.Expr, stack []build.Expr) {
		call, ok := expr.(*build.CallExpr)
		if !ok {
			return
		}
		if name, ok := callIdent(call); ok {
			calleeNames[name] = true
		}
	})

	out := make([]build.Expr, 0, len(stmts))
	for _, stmt := range stmts {
		load, ok := stmt.(*build.LoadStmt)
		if !ok {
			out = append(out, stmt)
			continue
		}
		// Filter the load's import list to names still
		// referenced. If empty after filtering, drop the load
		// entirely.
		var keepTo []*build.Ident
		var keepFrom []*build.Ident
		for i, to := range load.To {
			if calleeNames[to.Name] {
				keepTo = append(keepTo, to)
				keepFrom = append(keepFrom, load.From[i])
			}
		}
		if len(keepTo) == 0 {
			continue
		}
		load.To = keepTo
		load.From = keepFrom
		out = append(out, load)
	}
	return out
}

// cleanupModule processes the project's MODULE.bazel. When the
// plan reports the rules package is no longer used, the
// bazel_dep + local_path_override block referencing
// `rules_buildstream_bazel` is removed.
func cleanupModule(body []byte, plan *cleanupPlan) ([]byte, error) {
	if plan.rulesPackageStillUsed {
		return body, nil
	}
	f, err := build.Parse("MODULE.bazel", body)
	if err != nil {
		return body, nil
	}
	out := make([]build.Expr, 0, len(f.Stmt))
	for _, stmt := range f.Stmt {
		call, ok := stmt.(*build.CallExpr)
		if !ok {
			out = append(out, stmt)
			continue
		}
		ruleKind, _ := callIdent(call)
		nameAttr := stringAttr(call, "name")
		moduleNameAttr := stringAttr(call, "module_name")
		// Drop the bazel_dep on rules_buildstream_bazel.
		if ruleKind == "bazel_dep" && nameAttr == "rules_buildstream_bazel" {
			continue
		}
		// Drop the matching local_path_override.
		if ruleKind == "local_path_override" && moduleNameAttr == "rules_buildstream_bazel" {
			continue
		}
		out = append(out, stmt)
	}
	f.Stmt = out
	return build.Format(f), nil
}

// callIdent returns the called identifier's name (e.g. "genrule",
// "cc_library"). Returns false when the callee isn't a plain
// Ident (e.g. dotted access like `foo.bar()` — rare in BUILD
// files).
func callIdent(call *build.CallExpr) (string, bool) {
	id, ok := call.X.(*build.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// stringAttr returns the literal string value of attr `name` on
// call. Returns "" when the attr is absent or isn't a literal
// string. The call's attrs are kwarg expressions like
// `name = "foo"`.
func stringAttr(call *build.CallExpr, attr string) string {
	for _, arg := range call.List {
		bin, ok := arg.(*build.AssignExpr)
		if !ok {
			continue
		}
		lhs, ok := bin.LHS.(*build.Ident)
		if !ok || lhs.Name != attr {
			continue
		}
		str, ok := bin.RHS.(*build.StringExpr)
		if !ok {
			continue
		}
		return str.Value
	}
	return ""
}

// hasStringInListAttr reports whether call's `attr` list-typed
// argument contains the given string literal. Used to recognise
// `tags = ["trace_build"]` even when other tags coexist.
func hasStringInListAttr(call *build.CallExpr, attr, needle string) bool {
	for _, arg := range call.List {
		bin, ok := arg.(*build.AssignExpr)
		if !ok {
			continue
		}
		lhs, ok := bin.LHS.(*build.Ident)
		if !ok || lhs.Name != attr {
			continue
		}
		list, ok := bin.RHS.(*build.ListExpr)
		if !ok {
			continue
		}
		for _, e := range list.List {
			s, ok := e.(*build.StringExpr)
			if !ok {
				continue
			}
			if s.Value == needle {
				return true
			}
		}
	}
	return false
}
