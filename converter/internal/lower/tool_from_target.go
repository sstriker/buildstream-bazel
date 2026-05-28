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
	for i := range existing {
		t := &existing[i]
		if t.Kind != ir.KindCCBinary {
			continue
		}
		name := t.ArtifactName
		if name == "" {
			name = t.Name
		}
		if name == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = ":" + t.Name
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

// applyToolFromTargetToGenrules walks every genrule in pkg.Targets
// and applies the artifactToLabel rewrite: cmd tokens that match
// an artifact basename become `$(location :<targetName>)`, the
// matched labels join the genrule's tools attribute, and the
// corresponding srcs entry (the verbatim cmake-recorded artifact
// path that the ninja graph captured as an input) is removed so
// it doesn't double as a phantom src-shaped file Bazel would
// fail to find under the package.
//
// Runs as a post-pass over pkg.Targets after every cc_binary /
// cc_library is in place so the artifact lookup sees the complete
// target set — the per-target recoverGenrule path runs before all
// targets are lowered, so its cmds couldn't be rewritten in-place.
//
// Conservative: a nil / empty artifactToLabel is a no-op; targets
// other than KindGenrule are untouched; the rewrite only fires
// when at least one token matches.
func applyToolFromTargetToGenrules(pkg *ir.Package, artifactToLabel map[string]string) {
	if pkg == nil || len(artifactToLabel) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindGenrule {
			continue
		}
		newCmd, newTools := rewriteToolFromTargetTokens(t.GenruleCmd, artifactToLabel)
		if len(newTools) == 0 {
			continue
		}
		t.GenruleCmd = newCmd
		for _, lbl := range newTools {
			if !stringSliceContains(t.GenruleTools, lbl) {
				t.GenruleTools = append(t.GenruleTools, lbl)
			}
		}
		// Drop srcs entries whose basename matches an artifact —
		// those moved into tools and shouldn't double as phantom
		// src-shaped files Bazel can't locate under the package.
		kept := t.Srcs[:0]
		for _, s := range t.Srcs {
			if _, isTool := artifactToLabel[path.Base(s)]; isTool {
				continue
			}
			if _, isTool := artifactToLabel[s]; isTool {
				continue
			}
			kept = append(kept, s)
		}
		t.Srcs = kept
	}
}
