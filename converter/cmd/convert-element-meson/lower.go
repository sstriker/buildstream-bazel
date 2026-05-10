// Lowering meson introspection into the IR consumed by
// converter/internal/emit/bazel.
//
// Per-target translation:
//
//   - `static library`  → cc_library (linkstatic=true)
//   - `shared library`  → cc_library
//   - `both libraries`  → cc_library (Bazel toolchain decides static/shared)
//   - `executable`      → cc_binary
//   - `custom`          → genrule (best-effort lift; refuses on opaque shapes)
//   - `run`             → silently skipped (developer-convenience target;
//     no consumer-visible artifact, no Bazel analog).
//   - `jar`             → typed Tier-1 refusal (JVM toolchain not modeled).
//
// Per-target_sources:
//
//   - parameters split into Includes (`-I<dir>`), Defines (`-D…`), Copts (rest).
//   - Includes pointing at the build dir are dropped (the in-source-tree
//     ones are the ones consumers care about — meson stages a per-target
//     `<build>/<name>.<sfx>.p/` dir as the first include for include-order
//     reasons we don't replicate in Bazel).
//   - Sources are projected from absolute → source-root-relative.
//
// Per-target dependencies:
//
//   - `depends` (in-project target IDs) become `:<name>` deps.
//   - `dependencies` (external) resolve via the imports manifest if
//     supplied; otherwise produce a typed `unresolved-meson-dependency`
//     refusal.
package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

// LowerOptions tunes the lowering pass.
type LowerOptions struct {
	// SourceRoot is the absolute path of the meson source tree.
	// Sources outside this prefix surface as a refusal.
	SourceRoot string

	// BuildDir is the absolute path meson configured against. Used
	// to filter out build-dir-rooted include paths from the per-
	// target parameters list.
	BuildDir string

	// Imports, when non-nil, resolves cross-element external
	// dependencies (`dependency('foo')`) onto Bazel labels.
	Imports *manifest.Resolver
}

// Lower translates the introspection bundle into an ir.Package
// ready for converter/internal/emit/bazel.Emit. Returns a typed
// failure (Tier-1 code) on unsupported shapes, or a generic error
// on internal mismatches.
func Lower(intro *Introspect, opts LowerOptions) (*ir.Package, error) {
	pkg := &ir.Package{
		Name:       intro.ProjectInfo.Name,
		SourceRoot: opts.SourceRoot,
	}

	// Map target ID → in-project Bazel label so the depends list
	// resolves. meson IDs follow `<name>@<sfx>` where sfx is sta/
	// sha/exe/cus etc.; we key the map by both ID and name to be
	// permissive (some meson versions have inconsistencies).
	targetByID := map[string]string{}
	targetByName := map[string]string{}
	// Map archive / shared-library output basename
	// (e.g. "libhello.a", "libfoo.so") to the same label, used
	// when a target's linker entry references the dep by output
	// path rather than by name (`link_with:` propagates as a
	// `libfoo.a` linker arg, NOT a `depends:` entry).
	targetByArchive := map[string]string{}
	for _, t := range intro.Targets {
		if t.Name == "" {
			continue
		}
		// Mirror meson's name preservation; Bazel labels accept
		// the same character set meson uses for target names
		// (alphanumerics, _, -, +) so no remapping required for
		// the FDSDK shape.
		label := ":" + t.Name
		targetByID[t.ID] = label
		targetByName[t.Name] = label
		for _, fn := range t.Filename {
			targetByArchive[filepath.Base(fn)] = label
		}
	}

	// Index dependencies by name for ad-hoc lookup. meson's
	// intro-dependencies.json is the authoritative resolved list;
	// we use it to attach compile/link args verbatim when an
	// external dep isn't bound via the imports manifest but is
	// known to the build (e.g., `threads`/`pthread`).
	depByName := map[string]*Dependency{}
	for i := range intro.Dependencies {
		d := &intro.Dependencies[i]
		depByName[d.Name] = d
	}

	for _, t := range intro.Targets {
		if t.Subproject != nil && *t.Subproject != "" {
			return nil, newFailure(unsupportedMesonSubproject,
				"target %q lives in subproject %q (meson subprojects are not supported in v1)",
				t.Name, *t.Subproject)
		}
		switch t.Type {
		case "static library", "shared library", "both libraries":
			tgt, err := lowerLibrary(t, targetByID, targetByName, targetByArchive, depByName, opts)
			if err != nil {
				return nil, err
			}
			pkg.Targets = append(pkg.Targets, tgt)
		case "executable":
			tgt, err := lowerExecutable(t, targetByID, targetByName, targetByArchive, depByName, opts)
			if err != nil {
				return nil, err
			}
			pkg.Targets = append(pkg.Targets, tgt)
		case "custom":
			tgt, err := lowerCustom(t, opts)
			if err != nil {
				return nil, err
			}
			pkg.Targets = append(pkg.Targets, tgt)
		case "run":
			// `run_target` is a developer convenience (meson's
			// version of `add_custom_target(... CONFIGURE_OUTPUT)`).
			// Skip rather than refuse — no consumer-visible
			// artifact, no Bazel analog needed.
			continue
		case "jar":
			return nil, newFailure(unsupportedMesonTargetType,
				"target %q has type %q (java rules not modeled in v1)", t.Name, t.Type)
		default:
			return nil, newFailure(unsupportedMesonTargetType,
				"target %q has unknown type %q", t.Name, t.Type)
		}
	}
	return pkg, nil
}

// lowerLibrary handles static / shared / both libraries.
func lowerLibrary(t Target, byID, byName, byArchiveBasename map[string]string, deps map[string]*Dependency, opts LowerOptions) (ir.Target, error) {
	out := ir.Target{
		Name:       t.Name,
		Kind:       ir.KindCCLibrary,
		Linkstatic: t.Type == "static library",
	}
	if len(t.Filename) > 0 {
		out.ArtifactName = filepath.Base(t.Filename[0])
	}
	if t.Installed && len(t.InstallFilename) > 0 {
		out.InstallDest = filepath.Dir(t.InstallFilename[0])
	}
	if err := fillSourcesAndFlags(&out, t, byArchiveBasename, opts); err != nil {
		return ir.Target{}, err
	}
	if err := fillDeps(&out, t, byID, byName, deps, opts); err != nil {
		return ir.Target{}, err
	}
	out.Visibility = []string{"//visibility:public"}
	return out, nil
}

// lowerExecutable handles type=executable.
func lowerExecutable(t Target, byID, byName, byArchiveBasename map[string]string, deps map[string]*Dependency, opts LowerOptions) (ir.Target, error) {
	out := ir.Target{
		Name: t.Name,
		Kind: ir.KindCCBinary,
	}
	if len(t.Filename) > 0 {
		out.ArtifactName = filepath.Base(t.Filename[0])
	}
	if err := fillSourcesAndFlags(&out, t, byArchiveBasename, opts); err != nil {
		return ir.Target{}, err
	}
	if err := fillDeps(&out, t, byID, byName, deps, opts); err != nil {
		return ir.Target{}, err
	}
	out.Visibility = []string{"//visibility:public"}
	return out, nil
}

// lowerCustom handles type=custom (custom_target). Always
// returns either a non-nil ir.Target (lift succeeded) or a
// typed unsupported-meson-custom-target failure.
//
// Liftable shape: single target_sources entry, single-argv
// command with at most one input and at least one output, no
// @-substitutions other than standalone @INPUT@ / @OUTPUT@.
// Anything more elaborate (multi-group target_sources, multi-
// step shell pipelines, host probes via run_command, or
// @CURRENT_SOURCE_DIR@ usage) refuses with a typed Tier-1
// failure — there's no silent-skip path; refused targets
// surface to the operator via failure.json.
func lowerCustom(t Target, opts LowerOptions) (ir.Target, error) {
	if len(t.TargetSources) == 0 {
		return ir.Target{}, newFailure(unsupportedMesonCustomTarget,
			"custom target %q has no target_sources", t.Name)
	}
	if len(t.TargetSources) != 1 {
		// meson's custom_target() emits multiple target_sources
		// entries when COMMAND chains span groups (multi-step
		// shell pipelines, mixed-language inputs, etc.) — a
		// shape v1 doesn't model. Refusing here keeps us from
		// silently dropping the second-and-later entries' inputs/
		// commands and rendering a genrule that doesn't match
		// the meson target's actual recipe.
		return ir.Target{}, newFailure(unsupportedMesonCustomTarget,
			"custom target %q has %d target_sources entries (multi-group / multi-COMMAND custom targets aren't lifted in v1)",
			t.Name, len(t.TargetSources))
	}
	src := t.TargetSources[0]
	if len(src.Compiler) == 0 {
		return ir.Target{}, newFailure(unsupportedMesonCustomTarget,
			"custom target %q has empty command", t.Name)
	}
	for _, arg := range src.Compiler {
		if strings.Contains(arg, "@CURRENT_SOURCE_DIR@") ||
			strings.Contains(arg, "@CURRENT_BUILD_DIR@") ||
			strings.Contains(arg, "@PRIVATE_DIR@") ||
			strings.Contains(arg, "@SOURCE_ROOT@") ||
			strings.Contains(arg, "@BUILD_ROOT@") ||
			strings.Contains(arg, "@DEPFILE@") ||
			strings.Contains(arg, "@OUTDIR@") {
			return ir.Target{}, newFailure(unsupportedMesonCustomTarget,
				"custom target %q uses unsupported substitution in command: %q",
				t.Name, arg)
		}
	}

	srcs, err := relativizeSources(src.Sources, opts.SourceRoot)
	if err != nil {
		return ir.Target{}, err
	}
	outs := make([]string, 0, len(t.Filename))
	for _, fn := range t.Filename {
		outs = append(outs, filepath.Base(fn))
	}
	if len(outs) == 0 {
		return ir.Target{}, newFailure(unsupportedMesonCustomTarget,
			"custom target %q produces no outputs", t.Name)
	}

	cmd, err := renderCustomCmd(src.Compiler, srcs, outs)
	if err != nil {
		return ir.Target{}, fmt.Errorf("custom target %q: %w", t.Name, err)
	}
	return ir.Target{
		Name:        t.Name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleOuts: outs,
		GenruleCmd:  cmd,
		Visibility:  []string{"//visibility:public"},
		Tags:        []string{"meson-codegen-custom-target"},
	}, nil
}

// renderCustomCmd substitutes meson's @INPUT@ / @OUTPUT@ tokens
// for Bazel's $(SRCS) / $(OUTS). v1 lifts only the single-input /
// single-output / standalone-token shape; everything else refuses
// to keep the rendered genrule sound.
//
// Refusal rules:
//   - Any argv element containing `@INPUT` or `@OUTPUT` that isn't
//     EXACTLY `@INPUT@` / `@OUTPUT@` (so embedded forms like
//     `--in=@INPUT@` and indexed forms like `@INPUT0@` both
//     refuse; substituting them blindly would either break the
//     genrule's $(location ...) interpolation or generate an
//     argv that doesn't match the genrule's declared inputs/
//     outputs).
//   - The standalone `@INPUT@` token requires exactly one source;
//     `@OUTPUT@` requires exactly one output.
//   - The argv must mention `@INPUT@` (when srcs is non-empty)
//     and `@OUTPUT@` (always), so the rendered genrule cmd
//     actually references its declared inputs/outputs. Otherwise
//     Bazel would build a genrule whose cmd ignores its srcs/outs
//     — silently broken at action time.
func renderCustomCmd(argv, srcs, outs []string) (string, error) {
	out := make([]string, 0, len(argv))
	sawInput := false
	sawOutput := false
	for _, arg := range argv {
		switch arg {
		case "@INPUT@":
			if len(srcs) != 1 {
				return "", newFailure(unsupportedMesonCustomTarget,
					"@INPUT@ used but %d source(s)", len(srcs))
			}
			out = append(out, "$(location "+srcs[0]+")")
			sawInput = true
		case "@OUTPUT@":
			if len(outs) != 1 {
				return "", newFailure(unsupportedMesonCustomTarget,
					"@OUTPUT@ used but %d output(s)", len(outs))
			}
			out = append(out, "$(location "+outs[0]+")")
			sawOutput = true
		default:
			if strings.Contains(arg, "@INPUT") || strings.Contains(arg, "@OUTPUT") {
				return "", newFailure(unsupportedMesonCustomTarget,
					"command argv contains unsupported meson substitution: %q (only standalone @INPUT@ / @OUTPUT@ are lifted in v1)", arg)
			}
			out = append(out, shellQuote(arg))
		}
	}
	if len(srcs) > 0 && !sawInput {
		return "", newFailure(unsupportedMesonCustomTarget,
			"command argv has %d source(s) but doesn't reference @INPUT@; rendered genrule would ignore its declared inputs",
			len(srcs))
	}
	if !sawOutput {
		return "", newFailure(unsupportedMesonCustomTarget,
			"command argv doesn't reference @OUTPUT@; rendered genrule would ignore its declared outputs")
	}
	return strings.Join(out, " "), nil
}

// shellQuote wraps an argv element in single quotes for safe
// embedding into the genrule's cmd (Bazel runs the cmd under
// `/bin/sh -c`). The genrule cmd's $(location ...) substitutions
// are emitted unquoted by renderCustomCmd; everything else flows
// through this helper, which:
//
//   - Returns plain alphanumeric / `_` / `-` / `.` / `/` / `=`
//     args verbatim (the common shape — file paths, simple
//     identifiers, `--flag=value`). Keeps the rendered genrule
//     readable in the BUILD output.
//   - Single-quotes everything else, escaping embedded single
//     quotes via the standard `'\”` dance. This covers the full
//     POSIX shell metacharacter set (`;`, `|`, `&`, `<`, `>`,
//     `(`, `)`, newline, `$`, “ ` “, `\`, glob chars,
//     whitespace) without enumerating each one — anything that
//     isn't in the safe set goes through quoting.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !isShellSafeRune(r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

func isShellSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '_', '-', '.', '/', '=', '+', ':', ',', '@', '%':
		return true
	}
	return false
}

// fillSourcesAndFlags walks target_sources, pulling the cc-shaped
// entry and projecting it onto the IR target's Srcs / Includes /
// Defines / Copts. Linker entries are forwarded to fillLinkInfo
// where they're decomposed into LinkOpts + in-project Deps.
func fillSourcesAndFlags(out *ir.Target, t Target, byArchive map[string]string, opts LowerOptions) error {
	for _, src := range t.TargetSources {
		if src.IsLinker() {
			// `ar` / `gcc-ar` linker entries belong to static-
			// library archive steps; their `parameters` are
			// archiver flags (e.g. "csrD"), not linker flags.
			// Skip them — Bazel's cc_library handles archiving
			// internally.
			if isArchiverLinker(src.Linker) {
				continue
			}
			fillLinkInfo(out, src, byArchive, opts)
			continue
		}
		if !src.IsCompile() {
			continue
		}
		// We only model the host-machine compile group in v1.
		// build-machine entries (cross-builds) are rare in FDSDK
		// and need a different lowering strategy (separate
		// toolchain selection). Refuse.
		if src.Machine != "" && src.Machine != "host" {
			return newFailure(unsupportedMesonCrossCompile,
				"target %q has machine=%q (cross/build-machine compile groups not modeled)",
				t.Name, src.Machine)
		}
		srcs, err := relativizeSources(src.Sources, opts.SourceRoot)
		if err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		out.Srcs = append(out.Srcs, srcs...)
		// Generated sources: meson can stage build-time-generated
		// files into a target. v1 doesn't support these — Bazel
		// would need the producer wired explicitly. Refuse loudly
		// so operators see the gap rather than getting a build
		// that compiles too few files.
		if len(src.GeneratedSources) > 0 {
			return newFailure(unsupportedMesonGeneratedSources,
				"target %q references %d generated source(s); v1 doesn't yet wire custom_target outputs into target srcs",
				t.Name, len(src.GeneratedSources))
		}
		applyCompileParameters(out, src.Parameters, opts)
		if out.LinkLanguage == "" {
			out.LinkLanguage = src.Language
		}
	}
	return nil
}

// applyCompileParameters splits a compile argv into Includes (-I…),
// Defines (-D…), and Copts (everything else). meson injects build-
// dir-rooted includes (`-I<bd>/<name>.<sfx>.p`) that are useless to
// Bazel; those get filtered when opts.BuildDir is set.
func applyCompileParameters(out *ir.Target, params []string, opts LowerOptions) {
	for _, p := range params {
		switch {
		case strings.HasPrefix(p, "-I"):
			path := strings.TrimPrefix(p, "-I")
			if opts.BuildDir != "" && isUnderDir(path, opts.BuildDir) {
				continue
			}
			rel := projectInclude(path, opts.SourceRoot)
			if rel == "" {
				// External include (system / sysroot). Keep
				// as a Copt so the toolchain picks it up;
				// `includes` only accepts package-relative
				// paths.
				out.Copts = append(out.Copts, p)
				continue
			}
			if rel == "." {
				// `-I<source-root>` is the source-tree-root
				// include meson injects unconditionally.
				// Bazel implicitly puts the package on the
				// include path, so emitting "." in `includes`
				// would just inflate every BUILD with a
				// no-op entry. Drop it.
				continue
			}
			out.Includes = appendUnique(out.Includes, rel)
		case strings.HasPrefix(p, "-D"):
			out.Defines = appendUnique(out.Defines, strings.TrimPrefix(p, "-D"))
		case isToolchainHandledFlag(p):
			// Bazel's cc toolchain emits the canonical form of
			// these flags; preserving the meson copy would
			// duplicate them in every cc_* rule's copts. Drop.
			continue
		default:
			out.Copts = append(out.Copts, p)
		}
	}
}

// isToolchainHandledFlag returns true for flags Bazel's cc toolchain
// emits unconditionally (so meson's verbatim copy adds noise without
// changing semantics).
func isToolchainHandledFlag(p string) bool {
	switch p {
	case "-fPIC", "-fpic", "-fPIE", "-fpie",
		"-fdiagnostics-color=always",
		"-fdiagnostics-color=auto",
		"-fdiagnostics-color=never":
		return true
	}
	return false
}

// fillLinkInfo handles a non-archiver linker entry's parameters.
// Bare `lib<name>.a` / `lib<name>.so` references — meson's encoding
// of `link_with: <foo>` — resolve into Deps via the byArchive map.
// Toolchain-injected flags are dropped; everything else passes
// through to LinkOpts.
func fillLinkInfo(out *ir.Target, src TargetSource, byArchive map[string]string, opts LowerOptions) {
	for _, p := range src.Parameters {
		if isMesonInjectedLinkFlag(p) {
			continue
		}
		base := filepath.Base(p)
		if strings.HasSuffix(base, ".a") || strings.HasSuffix(base, ".so") || soVersionedSuffix(base) {
			if lab, ok := byArchive[base]; ok {
				out.Deps = appendUnique(out.Deps, lab)
				continue
			}
			// Out-of-tree archive — likely the imports
			// manifest's responsibility (kind:cmake handles
			// the same shape via LookupLinkPath). v1 keeps
			// it as a LinkOpt so the link still resolves at
			// least when the archive is on the system path.
		}
		out.LinkOpts = append(out.LinkOpts, p)
	}
}

func isArchiverLinker(linker []string) bool {
	if len(linker) == 0 {
		return false
	}
	bin := filepath.Base(linker[0])
	switch bin {
	case "ar", "gcc-ar", "llvm-ar", "lib":
		return true
	}
	return false
}

// soVersionedSuffix recognises shared-object basenames that carry a
// SONAME version (libfoo.so.1, libfoo.so.1.2.3). Returns true when
// the basename's `.so` is followed by a `.<digit>` chain.
func soVersionedSuffix(base string) bool {
	idx := strings.Index(base, ".so")
	if idx < 0 {
		return false
	}
	rest := base[idx+len(".so"):]
	if rest == "" {
		return false
	}
	if rest[0] != '.' {
		return false
	}
	for _, r := range rest[1:] {
		if !(r == '.' || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func isMesonInjectedLinkFlag(p string) bool {
	switch p {
	case "-Wl,--as-needed",
		"-Wl,--no-undefined",
		"-shared",
		"-fPIC":
		return true
	}
	if strings.HasPrefix(p, "-Wl,-soname,") {
		return true
	}
	return false
}

// fillDeps walks the in-project `depends` list and the external
// `dependencies` list, populating out.Deps. Bare-archive references
// (`libfoo.a` / `libfoo.so` / SONAME-versioned shapes) in the
// linker entry's parameters are matched against in-project archive
// outputs by basename in fillLinkInfo. v1 doesn't yet parse `-l<name>`
// linker args; cross-element link resolution for system libraries is
// expected to flow through the `dependencies` field
// (intro-dependencies.json's `link_args`) rather than raw `-l`
// fragments. If a fixture exposes raw `-l` references that need
// imports-manifest recovery (mirroring kind:cmake's
// LookupLinkLibrary), wire it here.
func fillDeps(out *ir.Target, t Target, byID, byName map[string]string, deps map[string]*Dependency, opts LowerOptions) error {
	// In-project deps via `depends` (preferred — meson IDs are
	// authoritative) or by name when an ID isn't recognized.
	for _, d := range t.Depends {
		if lab, ok := byID[d]; ok {
			out.Deps = appendUnique(out.Deps, lab)
			continue
		}
		// meson sometimes puts plain target names here (older
		// versions, library-of-libraries patterns). Fall back to
		// the by-name index.
		if lab, ok := byName[d]; ok {
			out.Deps = appendUnique(out.Deps, lab)
			continue
		}
		return fmt.Errorf("target %q: depends entry %q not found in target list",
			t.Name, d)
	}

	// External deps via `dependencies` (names like `glib-2.0`).
	// Resolution order:
	//   1. imports manifest LookupCMakeTarget(name) — the cross-
	//      element bind; mirrors how kind:cmake wires deps
	//      through the orchestrator-emitted manifest.
	//   2. imports manifest LookupCMakeTarget(name + "::" + name)
	//      — the convention bind kind:cmake's writeCmakeImports-
	//      Manifest defaults to.
	//   3. external dep is internally resolved by meson without
	//      a Bazel binding (e.g., `threads` -> `-pthread`); fold
	//      its compile_args / link_args into Copts / LinkOpts.
	//   4. otherwise refuse with unresolved-meson-dependency.
	for _, name := range t.Dependencies {
		if opts.Imports != nil {
			if ex := opts.Imports.LookupCMakeTarget(name); ex != nil {
				out.Deps = appendUnique(out.Deps, ex.BazelLabel)
				continue
			}
			if ex := opts.Imports.LookupCMakeTarget(name + "::" + name); ex != nil {
				out.Deps = appendUnique(out.Deps, ex.BazelLabel)
				continue
			}
		}
		if d, ok := deps[name]; ok && (len(d.CompileArgs) > 0 || len(d.LinkArgs) > 0) {
			// "Built-in" dep we can fold inline. Most of these
			// carry pure flags (e.g., threads → -pthread) and
			// don't need a separate Bazel target.
			for _, a := range d.CompileArgs {
				out.Copts = appendUnique(out.Copts, a)
			}
			for _, a := range d.LinkArgs {
				out.LinkOpts = append(out.LinkOpts, a)
			}
			continue
		}
		return newFailure(unresolvedMesonDependency,
			"target %q: dependency %q not bound by imports manifest and not folded by meson",
			t.Name, name)
	}
	sort.Strings(out.Deps)
	return nil
}

// projectInclude maps an absolute include path to a source-root-relative
// directory, or "" if path is outside the source root.
func projectInclude(path, sourceRoot string) string {
	if sourceRoot == "" {
		return ""
	}
	if path == sourceRoot {
		return "."
	}
	if strings.HasPrefix(path, sourceRoot+string(filepath.Separator)) {
		return path[len(sourceRoot)+1:]
	}
	return ""
}

// relativizeSources projects each absolute path to source-root-relative.
// Sources outside the source tree (e.g. ones from a subproject) refuse.
func relativizeSources(srcs []string, sourceRoot string) ([]string, error) {
	out := make([]string, 0, len(srcs))
	for _, p := range srcs {
		if !filepath.IsAbs(p) {
			out = append(out, p)
			continue
		}
		if sourceRoot == "" {
			return nil, fmt.Errorf("absolute source path %q with no --source-root", p)
		}
		if p == sourceRoot {
			return nil, fmt.Errorf("source path equals source root: %q", p)
		}
		if !strings.HasPrefix(p, sourceRoot+string(filepath.Separator)) {
			return nil, newFailure(unsupportedMesonSubproject,
				"source path %q lies outside source root %q", p, sourceRoot)
		}
		out = append(out, p[len(sourceRoot)+1:])
	}
	return out, nil
}

// isUnderDir reports whether path is dir itself or a descendant
// of dir. Plain `strings.HasPrefix(path, dir)` would match sibling
// paths (`/tmp/bd2/include` falsely matches `/tmp/bd`); the
// separator-anchored check rules them out. Both inputs are passed
// through filepath.Clean so trailing slashes / repeated separators
// don't sneak past the comparison.
func isUnderDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	cp := filepath.Clean(path)
	cd := filepath.Clean(dir)
	if cp == cd {
		return true
	}
	return strings.HasPrefix(cp, cd+string(filepath.Separator))
}

func appendUnique(in []string, item string) []string {
	for _, e := range in {
		if e == item {
			return in
		}
	}
	return append(in, item)
}
