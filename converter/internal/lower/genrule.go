package lower

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ninja"
)

// codegenContext carries state from genrule recovery and CTest
// classification back into the consuming target's lowering.
type codegenContext struct {
	// Genrules is the list of synthesized ir.Target{Kind: KindGenrule}
	// entries to append to the package.
	Genrules []ir.Target

	// Tests is the list of synthesized ir.Target{Kind: KindCCTest}
	// entries; one per add_test() registration whose COMMAND target
	// matched an EXECUTABLE in this codemodel. Appended to the package
	// after the main target loop alongside Genrules.
	Tests []ir.Target

	// Subs is the list of per-language sub-libraries lower
	// synthesizes when a multi-language target is split into a
	// wrapper + per-language cc_library shape (multi-language
	// structural delta). The wrapper carries the original
	// target's name and Bazel-public surface; each sub-library
	// carries one language's srcs + per-language copts/defines.
	// Appended after Genrules in target-walk order so the
	// rendered BUILD groups them naturally with their parent.
	Subs []ir.Target

	// OutToGenrule maps a package-relative output path to the genrule
	// name that produces it. Used by the consumer side to add
	// has-cmake-codegen and to reference outputs by label.
	OutToGenrule map[string]string

	// SeenBuilds dedupes recovered builds when multiple targets reference
	// the same generated source.
	SeenBuilds map[*ninja.Build]string

	// HeaderWalkCache memoizes filesystem walks of include directories
	// across targets within one lower-element invocation. Keyed on the
	// absolute include-dir path; value is the package-relative header
	// list for that dir. Multiple targets in a project commonly share
	// include roots (`include/`, `src/`); without the cache each
	// target re-walks every shared dir.
	HeaderWalkCache map[string][]string
}

func newCodegenContext() *codegenContext {
	return &codegenContext{
		OutToGenrule:    map[string]string{},
		SeenBuilds:      map[*ninja.Build]string{},
		HeaderWalkCache: map[string][]string{},
	}
}

// recoverGenrule looks up the ninja Build statement that produces the given
// generated source path and lowers it to an ir.Target{Kind: KindGenrule}.
// Returns the package-relative output path to use as the consuming target's
// input, plus the genrule name. If recovery isn't possible (no ninja graph,
// no producing build, refused command shape), returns a typed Tier-1 error.
//
// buildDir is the cmake-side build directory (r.Codemodel.Paths.Build);
// generated source paths in the File API are absolute under it, and ninja's
// build statements are relative to it.
func (cc *codegenContext) recoverGenrule(srcPath, cmakeSrc, buildDir string, g *ninja.Graph) (relOut, name string, err error) {
	relOut, ok := relativeIfInside(buildDir, srcPath)
	if !ok {
		// Generated source outside the build dir is unusual; bail out
		// with a clear failure.
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"generated source %q is outside the build dir %q", srcPath, buildDir)
	}

	if g == nil {
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"target references generated source %q but no build.ninja was provided to recover the producing custom command",
			relOut)
	}

	b := g.BuildFor(relOut)
	if b == nil {
		// Try the explicit-output absolute form.
		b = g.BuildFor(srcPath)
	}
	if b == nil {
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"no ninja build statement produces generated source %q", relOut)
	}

	if b.Rule != "CUSTOM_COMMAND" {
		// Object files etc. — not a custom command. We don't lower these
		// to genrule; they're already in the cc_library compile graph.
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"generated source %q is produced by rule %q, not CUSTOM_COMMAND",
			relOut, b.Rule)
	}

	// Already recovered? Reuse.
	if existingName, ok := cc.SeenBuilds[b]; ok {
		return relOut, existingName, nil
	}

	cmd, ok := ninja.CommandFor(g, b)
	if !ok {
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"could not resolve command for generated source %q", relOut)
	}

	// CMake stuffs the actual command in $COMMAND on the build statement;
	// the rule's command is just `$COMMAND`. CommandFor handles that
	// transparently via scope chain. The literal "cd <dir> &&" prefix
	// gets handled at command translation time.
	if strings.Contains(cmd, "/usr/bin/cmake -P ") || strings.Contains(cmd, "${CMAKE_COMMAND} -P ") {
		return "", "", failure.New(failure.UnsupportedCustomCommandScript,
			"custom command for %q runs `cmake -P script.cmake`; rewrite in a real language", relOut)
	}

	// Sanitize a name from the build statement's first output.
	name = genruleNameFor(b)

	outs := genruleOuts(b, buildDir)
	srcs := genruleSrcs(b, cmakeSrc, buildDir)
	tags := genruleTags(cmd, b, g)

	gen := ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		GenruleCmd:  cmd,
		GenruleOuts: outs,
		Srcs:        srcs,
		Tags:        tags,
		Visibility:  []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, gen)
	cc.SeenBuilds[b] = name
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return relOut, name, nil
}

// genruleNameFor turns the first output path into a Bazel-rule-name-safe
// identifier. `version.h` -> `gen_version_h`; `dir/foo.cc` -> `gen_dir_foo_cc`.
func genruleNameFor(b *ninja.Build) string {
	first := "out"
	if len(b.Outputs) > 0 {
		first = b.Outputs[0]
	}
	first = filepath.ToSlash(first)
	first = strings.TrimPrefix(first, "./")
	var sb strings.Builder
	sb.WriteString("gen_")
	for i := 0; i < len(first); i++ {
		c := first[i]
		switch {
		case (c >= 'a' && c <= 'z'),
			(c >= 'A' && c <= 'Z'),
			(c >= '0' && c <= '9'),
			c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// genruleOuts returns build statement outputs as package-relative paths.
// Implicit outs that resolve to the same file as an explicit out (via the
// `${cmake_ninja_workdir}<name>` redundancy CMake emits) are filtered.
func genruleOuts(b *ninja.Build, buildDir string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, o := range b.Outputs {
		if rel, ok := relativeIfInsideRelaxed(buildDir, o); ok {
			if _, dup := seen[rel]; !dup {
				seen[rel] = struct{}{}
				out = append(out, rel)
			}
		}
	}
	return out
}

// genruleSrcs returns explicit and implicit inputs as package-relative
// paths. CMake records absolute paths in custom-command inputs; we
// relativize against the source root (cmakeSrc) so two inputs with the
// same basename in different subdirectories don't collide.
//
// Inputs that aren't under cmakeSrc fall back to basename — those are
// typically host-leak references the orchestrator's downstream layer
// will re-anchor (or refuse). The fallback is rare and noisy on
// purpose: anything resolving here points at a real concern.
func genruleSrcs(b *ninja.Build, cmakeSrc, buildDir string) []string {
	all := append([]string{}, b.Inputs...)
	all = append(all, b.ImplicitInputs...)

	seen := map[string]struct{}{}
	var out []string
	for _, in := range all {
		key := normalizeInput(in, cmakeSrc, buildDir)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// normalizeInput picks the most-qualified package-relative representation
// of an input path for genrule srcs.
//
//  1. If `in` is under cmakeSrc, return cmakeSrc-relative slash form.
//  2. If under buildDir, return buildDir-relative — same shape genrule
//     outputs use, so an in-element generator binary's output is
//     matchable.
//  3. Otherwise basename, with a comment in the emitted BUILD.bazel
//     that flags the under-qualified entry. (Not implemented as a
//     comment yet; M4.x adds the audit hook.)
func normalizeInput(in, cmakeSrc, buildDir string) string {
	if !filepath.IsAbs(in) {
		return filepath.ToSlash(in)
	}
	if cmakeSrc != "" {
		if rel, ok := relativeIfInside(cmakeSrc, in); ok {
			return rel
		}
	}
	if buildDir != "" {
		if rel, ok := relativeIfInsideRelaxed(buildDir, in); ok {
			return rel
		}
	}
	// Fallback: basename. Documented as a known under-qualification
	// site; M5's converted_pkg_repo layer will need to surface these.
	return filepath.Base(in)
}

// genruleTags computes the cmake-codegen-* tag set for one recovered build.
// See docs/codegen-tags.md for the taxonomy.
func genruleTags(cmd string, b *ninja.Build, g *ninja.Graph) []string {
	tags := []string{"cmake-codegen"}

	driver := extractDriver(cmd)
	tags = append(tags, "cmake-codegen-driver="+driver)

	if hasCmakeE(cmd) {
		tags = append(tags, "cmake-codegen-cmake-e")
	}

	if toolFromTarget(b, g) {
		tags = append(tags, "cmake-codegen-tool-from-target")
	}

	if isSourceOnly(b) {
		tags = append(tags, "cmake-codegen-source-only")
	}

	sort.Strings(tags)
	return tags
}

// extractDriver returns the binary name the command actually invokes. Strips
// `cd <dir> &&` prefix and a small recognizer list of wrappers.
//
// Falls back to "unknown" — never empty — so the driver= facet is always
// present in queries.
func extractDriver(cmd string) string {
	// Strip a leading `cd <dir> && `. ninja-emitted cmake commands almost
	// always start with this.
	if i := strings.Index(cmd, " && "); i > 0 && strings.HasPrefix(cmd, "cd ") {
		cmd = cmd[i+4:]
	}
	cmd = strings.TrimSpace(cmd)

	tokens := splitShellTokens(cmd)
	wrappers := map[string]bool{
		"env":     true,
		"sh":      true,
		"bash":    true,
		"taskset": true,
		"nice":    true,
		"ionice":  true,
	}
	for len(tokens) > 0 {
		first := tokens[0]
		base := filepath.Base(first)
		if wrappers[base] {
			// env may carry KEY=VAL pairs and -i/-u flags before the real
			// command; we strip lazily by skipping tokens starting with
			// '-' or containing '=' until a clean argv0 appears.
			tokens = tokens[1:]
			for len(tokens) > 0 {
				t := tokens[0]
				if strings.HasPrefix(t, "-") || strings.Contains(t, "=") {
					tokens = tokens[1:]
					continue
				}
				break
			}
			continue
		}
		// `sh -c "<cmd>"` is a special wrapper: we'd need to reparse the
		// quoted string. Keep "sh" as the driver to surface that we
		// didn't drill in; M2 audit can flag these.
		if base == "" {
			return "unknown"
		}
		return base
	}
	return "unknown"
}

// splitShellTokens is a small tokenizer for shell-style commands. Honors '
// and " quoting and \-escapes. Not POSIX-complete; sufficient for the
// command shapes CMake's CUSTOM_COMMAND emits.
func splitShellTokens(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			if c == '\\' && i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case ' ', '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// hasCmakeE returns true if the command invokes a cmake -E sub-tool that we
// translate to a native Bazel idiom.
func hasCmakeE(cmd string) bool {
	for _, tok := range []string{
		"/usr/bin/cmake -E ",
		"${CMAKE_COMMAND} -E ",
		" cmake -E ",
	} {
		if strings.Contains(cmd, tok) {
			return true
		}
	}
	return false
}

// toolFromTarget returns true if the command's driver tool is itself the
// output of another build statement in the graph (i.e. an in-codebase
// generator binary).
func toolFromTarget(b *ninja.Build, g *ninja.Graph) bool {
	cmd, ok := ninja.CommandFor(g, b)
	if !ok {
		return false
	}
	driver := extractDriver(cmd)
	if driver == "unknown" {
		return false
	}
	// Try the basename first (driver is a filename); look up any output
	// in the index whose basename matches.
	for out := range g.OutputIndex {
		if filepath.Base(out) == driver {
			return true
		}
	}
	return false
}

// isSourceOnly returns true if the build statement's outputs are all source-
// or header-shaped paths (used as srcs/hdrs by a downstream cc rule). The
// converter doesn't have full transitive consumer info at this point; we
// approximate by extension.
func isSourceOnly(b *ninja.Build) bool {
	if len(b.Outputs) == 0 {
		return false
	}
	for _, o := range b.Outputs {
		ext := strings.ToLower(path.Ext(o))
		switch ext {
		case ".c", ".cc", ".cpp", ".cxx",
			".h", ".hh", ".hpp", ".hxx", ".inl",
			".s", ".S",
			".y", ".l":
		default:
			return false
		}
	}
	return true
}

// relativeIfInsideRelaxed is like relativeIfInside but accepts equality (the
// path itself being the root) — useful for build-statement outputs that are
// sometimes the whole build dir's relative path.
func relativeIfInsideRelaxed(root, abs string) (string, bool) {
	if !filepath.IsAbs(abs) {
		// Already relative — assume relative to the build dir, which is
		// what ninja outputs are.
		return filepath.ToSlash(abs), true
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}
