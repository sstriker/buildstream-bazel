package lower

import (
	"strings"
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
// Empty / nil map → no rewriting, cmd returned unchanged.
//
// Conservative tokenisation: splits on shell metacharacter
// boundaries (whitespace, `&`, `|`, `;`, `(`, `)`, backticks,
// dollar). A token matches only when it's an exact key in
// artifactToName — partial-string matches (e.g. `prefix/bin/X`
// containing `bin/X` as a suffix) don't rewrite. Conservative
// because the alternative — substring rewrite — would corrupt
// args like `--toolchain=bin/foo/include`.
func rewriteToolFromTarget(cmd string, artifactToName map[string]string) (string, []string) {
	if cmd == "" || len(artifactToName) == 0 {
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
		key := strings.TrimPrefix(tok, "./")
		if name, ok := artifactToName[key]; ok {
			b.WriteString("$(location :")
			b.WriteString(name)
			b.WriteByte(')')
			if !seenTools[name] {
				seenTools[name] = true
				tools = append(tools, ":"+name)
			}
		} else {
			b.WriteString(tok)
		}
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
