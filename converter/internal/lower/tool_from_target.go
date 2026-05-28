package lower

import (
	"path"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// buildArtifactToLabelMap projects already-lowered ir.Targets into
// a (artifact-basename → ":<targetName>") lookup so the
// standalone-custom-command lift can rewrite verbatim
// cmake-emitted tool invocations like `bin/vtkWrapHierarchy-9.3`
// (the cmake build dir's relative artifact path with the cmake-side
// OUTPUT_NAME/VERSION rename baked in) into `$(location :WrapHierarchy)`
// references that Bazel resolves at action time.
//
// Only KindCCBinary targets contribute — those are the only IR
// shape whose artifact is a runnable tool. Static libraries can
// match too in pathological cases (a generator that links in an
// archive driver) but the survey hasn't surfaced one, and the
// safer minimum is "executable targets only" — wider matching
// can come later if a real case surfaces.
//
// Returns nil when no targets contribute; callers handle nil
// identically to an empty map.
func buildArtifactToLabelMap(existing []ir.Target) map[string]string {
	var out map[string]string
	add := func(key, label string) {
		if key == "" {
			return
		}
		if out == nil {
			out = map[string]string{}
		}
		out[key] = label
	}
	for i := range existing {
		t := &existing[i]
		if t.Kind != ir.KindCCBinary {
			continue
		}
		label := ":" + t.Name
		// cmake's Artifact.Path is build-dir-relative — typically
		// `bin/<artifact>` for EXECUTABLE targets. Register both
		// the full key (`bin/<artifact>`) and the basename
		// (`<artifact>`) so cmd tokens that reference either form
		// hit the rewrite: cmake-Ninja-recorded `bin/<artifact>`
		// AND post-rewriteGenruleCmd bare-basename forms (e.g.
		// `ln -sf llvm-ar` after the cmake -E create_symlink
		// rewrite + buildDir-prefix strip).
		if t.ArtifactName != "" {
			add(t.ArtifactName, label)
			if base := t.ArtifactName[strings.LastIndex(t.ArtifactName, "/")+1:]; base != t.ArtifactName {
				add(base, label)
			}
		}
		// Always register the cmake target name as a key too —
		// some custom-command cmds reference the source-level
		// target name rather than the artifact path.
		add(t.Name, label)
	}
	return out
}

// rewriteToolFromTargetTokens scans cmd for tokens that name a
// known in-tree artifact (via artifactToLabel) and rewrites them
// to `$(location :<targetName>)` form. The returned tools slice
// lists each matched label (deduped) so the caller populates
// the genrule's tools attribute — Bazel needs the explicit edge
// so the binary lands in the action's input closure.
//
// Matching: each whitespace-separated token is checked verbatim,
// then with the leading directory prefix stripped (so cmake's
// canonical `bin/<artifact>` shape matches the same `<artifact>`
// key the artifactToLabel map carries). The first hit wins; the
// rewrite is conservative — a token that doesn't match passes
// through untouched. A nil / empty artifactToLabel is a no-op.
//
// Limitation: the function tokenises by ASCII whitespace only,
// matching `strings.Fields`. Shell quoting (e.g. `"tool" arg1`)
// is preserved verbatim because cmake's standalone-custom-command
// emit doesn't quote tool names. Should a future cmake codegen
// shape emit `"bin/foo"` (with embedded quotes), this helper
// will conservatively leave the token unchanged — a real fidelity
// loss not a corruption.
func rewriteToolFromTargetTokens(cmd string, artifactToLabel map[string]string) (string, []string) {
	if len(artifactToLabel) == 0 || cmd == "" {
		return cmd, nil
	}
	var tools []string
	seenTool := map[string]bool{}
	tokens := strings.Fields(cmd)
	for i, tok := range tokens {
		base := path.Base(tok)
		label, ok := artifactToLabel[base]
		if !ok {
			// Also try the verbatim token (no basename strip)
			// for the not-rooted case.
			label, ok = artifactToLabel[tok]
			if !ok {
				continue
			}
		}
		tokens[i] = "$(location " + label + ")"
		if !seenTool[label] {
			tools = append(tools, label)
			seenTool[label] = true
		}
	}
	if len(tools) == 0 {
		return cmd, nil
	}
	return strings.Join(tokens, " "), tools
}
