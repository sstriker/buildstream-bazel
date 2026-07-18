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
// imports ALSO carries the `tools` map (manifest.Resolver.LookupTool) — the
// home for host codegen tools with NO native rule (a project's python/perl
// generator, flatc, thrift, an absolute-path script). A token matching a tool
// by absolute path or driver basename rewrites to `$(execpath <label>)` with
// the label in tools, exactly like the imported-library channel. This is the
// channel that makes BOTH genrule paths (ninja + standalone) hermeticize a
// basename-driven host tool, which neither could do through LinkPaths alone.
//
// hostPrefix mirrors the link-fragment channel's pre-lookup rewrite
// (lower.go's hostPrefix→manifestPrefixAnchor remap): orchestrator-
// emitted manifests key link_paths in the ANCHORED /opt/prefix/ form
// while cmake resolved the tool against the REAL synth-prefix dir, so
// the raw token must remap onto the anchor before LookupLinkPath or
// the lookup misses in exactly the orchestrated flow. The verbatim
// token is tried first — hand-written manifests carry literal
// absolute paths.
//
// Conservative tokenisation: splits on shell metacharacter
// boundaries (whitespace, `&`, `|`, `;`, `(`, `)`, backticks,
// dollar). A token matches only when it's an exact key in
// artifactToName / an exact manifest LinkPath — partial-string
// matches (e.g. `prefix/bin/X` containing `bin/X` as a suffix)
// don't rewrite. Conservative because the alternative — substring
// rewrite — would corrupt args like `--toolchain=bin/foo/include`.
// toolchainToolPaths carries the project's toolchain tool paths (from the CMake
// cache: CMAKE_C_COMPILER / CMAKE_CXX_COMPILER / CMAKE_AR / CMAKE_NM). A genrule
// tool token equal to one of them is routed to the Bazel cc_toolchain make-var
// instead of a non-hermetic prebuilt lift, so a custom command that invokes the
// compiler/archiver/nm runs through the same hermetic, config-correct toolchain
// as normal compile/link actions.
type toolchainToolPaths struct {
	cCompiler   string
	cxxCompiler string
	cxxSibling  bool // dirname(cCompiler) == dirname(cxxCompiler)
	ar          string
	nm          string
}

func (tc toolchainToolPaths) empty() bool {
	return tc.cCompiler == "" && tc.cxxCompiler == "" && tc.ar == "" && tc.nm == ""
}

// toolchainTools projects the codegenContext's recorded toolchain tool paths,
// computing the C++/C sibling relationship the C++-driver derivation needs.
func (cc *codegenContext) toolchainTools() toolchainToolPaths {
	if cc == nil {
		return toolchainToolPaths{}
	}
	return toolchainToolPaths{
		cCompiler:   cc.CCompiler,
		cxxCompiler: cc.CxxCompiler,
		cxxSibling:  cc.CCompiler != "" && cc.CxxCompiler != "" && filepath.Dir(cc.CCompiler) == filepath.Dir(cc.CxxCompiler),
		ar:          cc.ARTool,
		nm:          cc.NMTool,
	}
}

// currentCcToolchain is the make-var-supplying toolchain a genrule must declare
// to expand $(CC) / $(AR) / $(NM).
const currentCcToolchain = "@bazel_tools//tools/cpp:current_cc_toolchain"

// toolchainMakeVar returns the cc_toolchain make-var substitution for a tool
// token that equals one of the project's toolchain tools, plus the toolchain the
// genrule must declare to expand it. C -> $(CC); AR/NM -> $(AR)/$(NM). The C++
// driver has no $(CXX) make-var, so when it is a SIBLING of the C compiler
// (same directory) it is derived at ACTION time as $$(dirname $(CC))/<cxx-base>
// — config-correct, since $(CC) follows whatever C compiler the selected
// toolchain provides and the C++ driver sits beside it. A non-sibling C++
// compiler (a wrapper, a split install) returns ok=false so the caller keeps the
// prebuilt lift (non-hermetic but correct).
func (tc toolchainToolPaths) toolchainMakeVar(tok string) (repl, toolchain string, ok bool) {
	switch {
	case tc.cCompiler != "" && tok == tc.cCompiler:
		return "$(CC)", currentCcToolchain, true
	case tc.cxxCompiler != "" && tok == tc.cxxCompiler:
		if tc.cxxSibling {
			return "$$(dirname $(CC))/" + filepath.Base(tc.cxxCompiler), currentCcToolchain, true
		}
		return "", "", false
	case tc.ar != "" && tok == tc.ar:
		return "$(AR)", currentCcToolchain, true
	case tc.nm != "" && tok == tc.nm:
		return "$(NM)", currentCcToolchain, true
	}
	return "", "", false
}

// resolveImportedToolPath maps an absolute token onto a manifest export's label
// via its recorded IMPORTED_LOCATION: the verbatim token first (hand-written
// manifests), then the hostPrefix→anchor remapped form (orchestrator-emitted
// manifests key link_paths in the ManifestPrefixAnchor form).
func resolveImportedToolPath(p string, imports *manifest.Resolver, hostPrefix string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	if ex := imports.LookupLinkPath(p); ex != nil {
		return ex.BazelLabel, true
	}
	if hostPrefix != "" && strings.HasPrefix(p, hostPrefix+string(filepath.Separator)) {
		if ex := imports.LookupLinkPath(manifestPrefixAnchor + p[len(hostPrefix)+1:]); ex != nil {
			return ex.BazelLabel, true
		}
	}
	return "", false
}

// liftKeyedToolToken handles the `VAR=<tool-path>` form — a custom command
// passing the tool as a cmake -D arg (VTK's `-DEXE_SQLITE3=bin/Debug/sqlitebin`,
// where libproj hardcodes `$<TARGET_FILE:VTK::sqlitebin>`). Splits on the first
// `=` and lifts the value when it names a converted target's artifact, an
// imported IMPORTED_LOCATION, or a manifest tool — keeping the `VAR=` prefix.
// Returns (prefix, in-tree target name OR "", imported/tool label OR "", ok):
// exactly one of name/label is set when ok.
func liftKeyedToolToken(tok string, artifactToName map[string]string, execArtifacts map[string]bool, imports *manifest.Resolver, resolveImported func(string) (string, bool)) (prefix, name, label string, ok bool) {
	eq := strings.IndexByte(tok, '=')
	if eq < 0 {
		return "", "", "", false
	}
	prefix = tok[:eq+1]
	val := strings.TrimPrefix(tok[eq+1:], "./")
	if n, has := artifactToName[val]; has && val != "" && execArtifacts[val] {
		return prefix, n, "", true
	}
	if l, has := resolveImported(tok[eq+1:]); has {
		return prefix, "", l, true
	}
	if l, has := imports.LookupTool(val); has && val != "" {
		return prefix, "", l, true
	}
	return "", "", "", false
}

func rewriteToolFromTarget(cmd string, artifactToName map[string]string, execArtifacts map[string]bool, imports *manifest.Resolver, hostPrefix string, tc toolchainToolPaths) (string, []string, []string) {
	if cmd == "" || (len(artifactToName) == 0 && imports.Empty() && !imports.HasTools() && tc.empty()) {
		return cmd, nil, nil
	}
	var b strings.Builder
	b.Grow(len(cmd))
	seenTools := map[string]bool{}
	var tools []string
	seenToolchains := map[string]bool{}
	var toolchains []string

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
		resolveImported := func(p string) (string, bool) {
			return resolveImportedToolPath(p, imports, hostPrefix)
		}
		// Toolchain tool (compiler / ar / nm) — highest priority, so a compiler
		// that ALSO happens to be a harvested bin/ export is routed to the
		// hermetic, config-correct cc_toolchain make-var rather than the
		// non-hermetic prebuilt lift.
		if repl, toolchain, ok := tc.toolchainMakeVar(tok); ok {
			b.WriteString(repl)
			if toolchain != "" && !seenToolchains[toolchain] {
				seenToolchains[toolchain] = true
				toolchains = append(toolchains, toolchain)
			}
			return
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
		// Manifest `tools` map: a host codegen tool with no native rule,
		// matched by absolute path or driver basename. Checked AFTER the
		// in-tree and link-path channels (an in-tree producer or an imported
		// library's IMPORTED_LOCATION wins), so the tools map is the explicit
		// fallback for the no-native-rule case (flatc, python3, perl, a
		// project's own absolute-path script).
		if label, ok := imports.LookupTool(key); ok {
			emitImported(label)
			return
		}
		// `VAR=<artifact-path>` form (VTK's `-DEXE_SQLITE3=bin/…`): the tool is
		// embedded after `=`, so the whole-token lookups above miss it.
		if prefix, name, label, ok := liftKeyedToolToken(tok, artifactToName, execArtifacts, imports, resolveImported); ok {
			b.WriteString(prefix)
			if name != "" {
				emitTool(name)
			} else {
				emitImported(label)
			}
			return
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
	return b.String(), tools, toolchains
}
