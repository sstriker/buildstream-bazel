package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakeargv"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// backtraceRecoverLinkScope walks every TargetDependency's backtrace
// in r.Targets, reads the originating cmake source file via
// cmakeargv, and extracts the PUBLIC / PRIVATE / INTERFACE keyword
// for each (target, dep) pair. Phase 1 task 1's recovery half.
//
// Returns the same shape lower's traceLinkScope uses
// (target → lib → keyword), so the existing routing in lowerTarget
// can fall back from trace to backtrace transparently — or use
// backtrace when trace is unavailable (offline replay, cmake run
// without --trace-expand, etc.).
//
// Robustness over trace-based recovery:
//
//   - Always available — backtraceGraph is part of codemodel-v2;
//     no --trace-expand dependency.
//   - Survives macro expansion. cmake's BacktraceGraph carries the
//     full call stack; we walk to the OUTERMOST user-source frame
//     (skipping system / wrapper-module frames) so the recovered
//     argv reflects the user's call shape, not the macro's
//     internal target_link_libraries.
//   - File-cached: per-(file, line, command) tuple is read at most
//     once; many targets call into the same call sites.
//
// Best-effort: source files that can't be opened, calls whose
// argv shape doesn't match the expected (target, [keyword, dep…])
// pattern, and dep names that appear inside `${VAR}` (literal
// preserved by cmakeargv but not matchable) all surface as
// missing entries — the caller's fallback path (trace or "no
// keyword recovered, treat as PUBLIC default") fires.
//
// Returns nil when r has no targets or every target's backtrace
// is incomplete (cmake < 3.21).
func backtraceRecoverLinkScope(r *fileapi.Reply) map[string]map[string]string {
	if r == nil || len(r.Targets) == 0 {
		return nil
	}
	// Per-call-site cache: (file, line) → parsed argv. Many
	// projects call target_link_libraries multiple times for the
	// same target across nested macros; the unique call-site set
	// is much smaller than the dep count.
	type cacheKey struct {
		file string
		line int
	}
	cache := map[cacheKey][]string{}

	scope := map[string]map[string]string{}
	for _, t := range r.Targets {
		if t.IsGeneratorProvided {
			continue
		}
		for _, dep := range t.Dependencies {
			if dep.Backtrace <= 0 || dep.Backtrace >= len(t.BacktraceGraph.Nodes) {
				continue
			}
			// Walk to the outermost user-source frame —
			// skipping cmake-internal modules (which live
			// under /usr/share/cmake-* etc.) so the recovered
			// argv reflects the user's call, not a macro
			// expansion's internal call.
			file, line, command := outermostUserFrame(t.BacktraceGraph, dep.Backtrace)
			if file == "" || line <= 0 || command == "" {
				continue
			}
			// Only target_link_libraries (and its CMakeFamily
			// variants like target_link_directories) carry the
			// keyword we're interested in; everything else gets
			// no scope recovery from this pass.
			if !isTLLLike(command) {
				continue
			}

			key := cacheKey{file: file, line: line}
			args, ok := cache[key]
			if !ok {
				call, err := cmakeargv.ReadCall(file, line, command)
				if err != nil {
					// File missing / parse failure: best-effort,
					// skip the dep — the trace-based fallback or
					// the default-PUBLIC routing fires for it.
					cache[key] = nil
					continue
				}
				args = call.Args
				cache[key] = args
			}
			if len(args) == 0 {
				continue
			}
			// First arg is the target name; the rest cycle
			// through (keyword, dep, dep, …, keyword, dep, …)
			// per cmake's target_link_libraries shape. Find
			// the keyword in effect when this dep was passed.
			depKeyword := findKeywordForLib(args[1:], dep.Id)
			if depKeyword == "" {
				continue
			}
			if scope[t.Name] == nil {
				scope[t.Name] = map[string]string{}
			}
			// First-write-wins: same lib mentioned in two
			// arms keeps the upstream-most keyword (matches
			// the trace-based recovery's policy).
			libName := depLibName(dep)
			if libName != "" {
				if _, present := scope[t.Name][libName]; !present {
					scope[t.Name][libName] = depKeyword
				}
			}
		}
	}
	if len(scope) == 0 {
		return nil
	}
	return scope
}

// outermostUserFrame walks the backtrace chain from the given node
// up to the root, returning the outermost frame whose file is not
// a cmake-internal path. Falls back to the recorded frame itself
// when no user frame is found (cmake-internal modules calling
// each other only).
func outermostUserFrame(g fileapi.BacktraceGraph, start int) (file string, line int, command string) {
	cur := start
	var lastUserFile string
	var lastUserLine int
	var lastUserCmd string
	for cur > 0 && cur < len(g.Nodes) {
		node := g.Nodes[cur]
		var fname, cmd string
		if node.File >= 0 && node.File < len(g.Files) {
			fname = g.Files[node.File]
		}
		if node.Command >= 0 && node.Command < len(g.Commands) {
			cmd = g.Commands[node.Command]
		}
		if fname != "" && !isCMakeInternalPath(fname) {
			lastUserFile = fname
			lastUserLine = node.Line
			lastUserCmd = cmd
		}
		if node.Parent == nil {
			break
		}
		cur = *node.Parent
	}
	if lastUserFile != "" {
		return lastUserFile, lastUserLine, lastUserCmd
	}
	// Fallback: use the start node verbatim.
	if start > 0 && start < len(g.Nodes) {
		n := g.Nodes[start]
		if n.File >= 0 && n.File < len(g.Files) {
			file = g.Files[n.File]
		}
		if n.Command >= 0 && n.Command < len(g.Commands) {
			command = g.Commands[n.Command]
		}
		line = n.Line
	}
	return file, line, command
}

// isCMakeInternalPath identifies frames that originate inside
// cmake's bundled modules rather than user CMakeLists. Matches
// the typical install layouts; conservative on unknown patterns
// (returns false → treat as user code, which is the safer
// default for the recovery's outermost-frame walk).
func isCMakeInternalPath(p string) bool {
	if p == "" {
		return false
	}
	return strings.Contains(p, "/share/cmake-") ||
		strings.Contains(p, "/cmake/Modules/") ||
		strings.Contains(p, "/usr/local/share/cmake/") ||
		strings.Contains(p, "/Cellar/cmake/") || // Homebrew on macOS
		strings.HasSuffix(p, "CMakeSystem.cmake") ||
		strings.HasSuffix(p, "CMakeDetermineSystem.cmake") ||
		strings.HasSuffix(p, "CMakeTestCompiler.cmake")
}

// isTLLLike reports whether the command name carries
// PUBLIC/PRIVATE/INTERFACE keyword arguments in the
// target_link_libraries family.
func isTLLLike(cmd string) bool {
	switch strings.ToLower(cmd) {
	case "target_link_libraries",
		"target_link_directories",
		"target_link_options",
		"target_compile_definitions",
		"target_compile_options",
		"target_compile_features",
		"target_include_directories",
		"target_precompile_headers",
		"target_sources":
		return true
	}
	return false
}

// findKeywordForLib walks the post-target argv and returns the
// keyword (PUBLIC/PRIVATE/INTERFACE) in effect when the
// dependency named depId was passed.
//
// cmake's target_link_libraries(target [scope] dep1 [scope] dep2 …)
// shape: scope keywords are sticky — once a PUBLIC is seen, every
// following dep is PUBLIC until a different keyword resets the
// scope. The legacy positional form (no keyword at all) is treated
// as PUBLIC per cmake's documented default.
//
// depId comes from fileapi.TargetDependency.Id, which uses the
// `<targetName>::@` form for user-defined targets and the bare
// name for find_package imports. We strip the `::@` suffix to
// match the literal in the argv.
func findKeywordForLib(args []string, depId string) string {
	want := depLibNameFromId(depId)
	if want == "" {
		return ""
	}
	current := "PUBLIC" // legacy default
	for _, a := range args {
		switch strings.ToUpper(a) {
		case "PUBLIC", "PRIVATE", "INTERFACE":
			current = strings.ToUpper(a)
			continue
		}
		if a == want {
			return current
		}
		// Some cmake idioms wrap deps in $<BUILD_INTERFACE:...> or
		// $<INSTALL_INTERFACE:...>; recover the inner literal so
		// the recovery still fires for those shapes.
		if inner := stripGenexWrapper(a); inner == want {
			return current
		}
	}
	return ""
}

// depLibName returns the dep's user-visible library name for the
// scope lookup. fileapi.TargetDependency carries Id; we strip the
// cmake-internal `::@` suffix to match the literal in the argv.
func depLibName(dep fileapi.TargetDependency) string {
	return depLibNameFromId(dep.Id)
}

func depLibNameFromId(id string) string {
	if id == "" {
		return ""
	}
	// cmake's internal id form: `<name>::@<hash>`. Strip from `::`
	// onwards.
	if i := strings.Index(id, "::"); i >= 0 {
		return id[:i]
	}
	return id
}

// stripGenexWrapper unwraps `$<BUILD_INTERFACE:inner>` /
// `$<INSTALL_INTERFACE:inner>` to inner, leaving everything else
// untouched. Used so target_link_libraries deps inside an
// interface-gated genex still match the literal lookup.
func stripGenexWrapper(arg string) string {
	const buildPrefix = "$<BUILD_INTERFACE:"
	const installPrefix = "$<INSTALL_INTERFACE:"
	if strings.HasPrefix(arg, buildPrefix) && strings.HasSuffix(arg, ">") {
		return arg[len(buildPrefix) : len(arg)-1]
	}
	if strings.HasPrefix(arg, installPrefix) && strings.HasSuffix(arg, ">") {
		return arg[len(installPrefix) : len(arg)-1]
	}
	return arg
}
