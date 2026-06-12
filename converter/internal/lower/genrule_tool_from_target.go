package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// rewriteToolFromTarget walks a genrule cmd token-by-token and
// rewrites every bare reference to a build-dir-relative artifact
// path (e.g. `bin/llvm-min-tblgen`) into the Bazel-native
// `$(location :<target-name>)` form. Returns the rewritten cmd
// plus the set of target names referenced, so the caller can
// populate the genrule's `tools` attribute.
//
// Why this matters: cmake's Ninja-side cmd records artifact paths
// as build-dir-relative literals (`bin/llvm-min-tblgen ...`). At
// action time Bazel has no `bin/llvm-min-tblgen` file in the
// sandbox — the tblgen binary lives under bazel-bin/<pkg>/<name>
// after a build. The `$(location :<name>)` substitution closes
// the gap: Bazel expands it to the actual sandbox path at action
// time, and the `tools = [...]` attribute ensures the binary is
// staged into the action's input closure.
//
// artifactToName maps artifact paths (as cmake records them, i.e.
// build-dir-relative) to the IR target name that produces them.
//
// imports extends the lift to MANIFEST-PROVIDED tools: an ABSOLUTE
// token matching an export's recorded IMPORTED_LOCATION
// (imports.LookupLinkPath — the orchestrator stages each
// IMPORTED_LOCATION_<CONFIG> path there) rewrites to
// `$(execpath <bazel-label>)` with the full label in tools. Without
// this, a genrule driving an imported tool (cmake resolved
// `$<TARGET_FILE:Pkg::tool>` to the host-install absolute path at
// configure time) keeps the raw host path — non-hermetic, and
// invisible under sandboxed /tmp, the same class as the -idirafter
// leak. In-tree lookup wins when both match (it shouldn't — the key
// spaces are build-dir-relative vs absolute).
//
// Conservative tokenisation: splits on shell metacharacter
// boundaries (whitespace, `&`, `|`, `;`, `(`, `)`, backticks,
// dollar). A token matches only when it's an exact key in
// artifactToName / an exact manifest LinkPath — partial-string
// matches (e.g. `prefix/bin/X` containing `bin/X` as a suffix)
// don't rewrite. Conservative because the alternative — substring
// rewrite — would corrupt args like `--toolchain=bin/foo/include`.
func rewriteToolFromTarget(cmd string, artifactToName map[string]string, execArtifacts map[string]bool, imports *manifest.Resolver) (string, []string) {
	if cmd == "" || (len(artifactToName) == 0 && imports.Empty()) {
		return cmd, nil
	}
	var b strings.Builder
	b.Grow(len(cmd))
	seenTools := map[string]bool{}
	var tools []string

	tokStart := 0
	flush := func(end int) {
		if end <= tokStart {
			return
		}
		tok := cmd[tokStart:end]
		// Normalise the leading `./` that cmake's Ninja generator
		// sometimes prepends to make the tool resolve via the
		// current working directory (e.g. `./bin/llvm-lit`). The
		// artifact map keys are plain build-dir-relative paths
		// (`bin/llvm-lit`); strip the `./` before lookup so both
		// forms match.
		emitTool := func(name string) {
			b.WriteString("$(location :")
			b.WriteString(name)
			b.WriteByte(')')
			if !seenTools[name] {
				seenTools[name] = true
				tools = append(tools, ":"+name)
			}
		}
		emitImported := func(label string) {
			b.WriteString("$(execpath ")
			b.WriteString(label)
			b.WriteByte(')')
			if !seenTools[label] {
				seenTools[label] = true
				tools = append(tools, label)
			}
		}
		// resolveImported maps an absolute token onto a manifest
		// export's label via its recorded IMPORTED_LOCATION.
		resolveImported := func(p string) (string, bool) {
			if !filepath.IsAbs(p) {
				return "", false
			}
			if ex := imports.LookupLinkPath(p); ex != nil {
				return ex.BazelLabel, true
			}
			return "", false
		}
		key := strings.TrimPrefix(tok, "./")
		if name, ok := artifactToName[key]; ok {
			emitTool(name)
			return
		}
		if label, ok := resolveImported(tok); ok {
			emitImported(label)
			return
		}
		// `VAR=<artifact-path>` form: a custom command passes the tool as a
		// cmake -D arg, e.g. VTK's `-DEXE_SQLITE3=bin/Debug/sqlitebin-9.4`
		// (libproj hardcodes `$<TARGET_FILE:VTK::sqlitebin>`, an in-tree built
		// executable). The path is embedded after `=`, so the whole-token
		// lookup misses; split on the first `=` and lift just the value when it
		// names a converted target's artifact, keeping the `VAR=` prefix.
		if eq := strings.IndexByte(tok, '='); eq >= 0 {
			val := strings.TrimPrefix(tok[eq+1:], "./")
			if name, ok := artifactToName[val]; ok && val != "" && execArtifacts[val] {
				b.WriteString(tok[:eq+1])
				emitTool(name)
				return
			}
			if label, ok := resolveImported(tok[eq+1:]); ok {
				b.WriteString(tok[:eq+1])
				emitImported(label)
				return
			}
		}
		b.WriteString(tok)
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch c {
		case ' ', '\t', '\n', '&', '|', ';', '(', ')', '`', '$', '"', '\'':
			flush(i)
			b.WriteByte(c)
			tokStart = i + 1
		}
	}
	flush(len(cmd))
	return b.String(), tools
}
