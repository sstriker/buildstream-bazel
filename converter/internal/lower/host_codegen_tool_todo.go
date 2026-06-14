package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// A recovered codegen genrule should drive a HERMETIC Bazel tool. The
// converter already hermeticizes two channels automatically — an in-tree
// built generator lifts to $(location :name), and an imported library /
// manifest-mapped tool lifts to $(execpath <label>) (see
// rewriteToolFromTarget + the imports-manifest `tools` map). What it CANNOT
// do is invent a label for a host generator with no native rule and no
// manifest entry: a project's own python/perl script, a `flatc`/`thrift`
// the operator hasn't mapped, an absolute host-install binary. Those land in
// the genrule fallback driving the raw host tool — non-hermetic, and broken
// on a clean executor for an absolute path.
//
// So instead of silently emitting the non-hermetic genrule, the converter
// DETECTS the un-hermeticized host-tool driver and surfaces a structured
// `host-codegen-tool` conversion-todo: it names the tool and hands the
// operator the exact imports-manifest `tools` entry to author. This is the
// honest, producer-side half of host-codegen-tool hermeticization — the
// label can't be auto-derived, but the work-item can be auto-detected.

// hostCodegenToolNote is one recovered genrule whose driver is an
// un-hermeticized host codegen tool.
type hostCodegenToolNote struct {
	// Driver is the tool basename — the GroupKey AND the suggested `tools`
	// match. A basename is deterministic (an absolute synth-prefix path is
	// per-run-ephemeral, so it must never become the suggested key or land in
	// the report) and resolves through LookupTool's basename branch.
	Driver string
	// Path is the driver's absolute path when it was referenced absolutely,
	// ANCHORED to ManifestPrefixAnchor (/opt/prefix/…) when it resolved from
	// the synth-prefix — so it's deterministic across converts. "" for a
	// PATH-resolved basename driver.
	Path string
	// Absolute is true when the driver was an absolute path (prefix or host) —
	// definitely non-portable → Actionable; false for a PATH basename (relies
	// on the host PATH → Improvement).
	Absolute bool
	// Prefix is true when the absolute driver resolved from the synth-prefix
	// (cc.HostPrefixDir): a CROSS-ELEMENT tool whose label is the producing
	// element's manifest Export (the orchestrator-auto-derivable case), not a
	// fresh host `tools` entry.
	Prefix bool
	// Genrule is the recovered genrule's name (the todo anchor's construct).
	Genrule string
}

// benignGenruleDriver is the set of genrule drivers that are NOT host codegen
// tools needing hermeticization: cmake's own machinery (recovered or handled
// by other producers), and pure file/shell utilities that don't generate code
// from project inputs. A `sh`/`bash -c "<real tool> …"` wrapper hides its
// real tool too deep to classify cleanly, so it's treated as benign here
// rather than mis-attributed.
var benignGenruleDriver = map[string]bool{
	"cmake": true, "ctest": true, "cpack": true,
	"cp": true, "mv": true, "rm": true, "mkdir": true, "rmdir": true,
	"touch": true, "ln": true, "chmod": true, "chown": true,
	"true": true, "false": true, "test": true, "[": true, ":": true,
	"env": true, "sh": true, "bash": true,
}

// classifyHostCodegenTool inspects a recovered genrule's final command and, if
// its driver is an un-hermeticized host codegen tool, returns the raw driver
// token, its basename, and whether it was an absolute path. ok=false for a
// hermeticized driver ($(execpath)/$(location)), a benign cmake -E / shell-util
// driver, or a command whose first token isn't a clean program reference (a
// shell assignment / substitution / subshell — the scratch-dir workdir shapes).
func classifyHostCodegenTool(genruleCmd string) (rawTok, driver string, absolute, ok bool) {
	c := strings.TrimSpace(genruleCmd)
	// Strip a leading `cd <dir> && ` (the recovered cwd anchor).
	if strings.HasPrefix(c, "cd ") {
		if i := strings.Index(c, " && "); i > 0 {
			c = strings.TrimSpace(c[i+4:])
		}
	}
	fields := strings.Fields(c)
	if len(fields) == 0 {
		return "", "", false, false
	}
	tok := fields[0]
	// Already hermeticized: the swap rewrote the driver to a Make-var ref.
	if strings.HasPrefix(tok, "$(") {
		return "", "", false, false
	}
	// Not a clean program token: a shell assignment (VAR=…), a command
	// substitution, or a subshell preamble (the workdir-buildout scratch-dir
	// shape). Don't try to classify these.
	if strings.ContainsAny(tok, "=(){}$`\"'") {
		return "", "", false, false
	}
	base := filepath.Base(tok)
	if base == "" || base == "." || base == "unknown" || benignGenruleDriver[base] {
		return "", "", false, false
	}
	return tok, base, filepath.IsAbs(tok), true
}

// noteHostCodegenTool records a recovered genrule fallback whose driver is an
// un-hermeticized host codegen tool, for the end-of-lower todo. No-op when the
// sink isn't allocated (the report wasn't requested) or the driver is
// hermeticized / benign. Called from the single recognizer-aware chokepoint
// (recognizeOrGenrule), so it covers BOTH the standalone custom-command path
// and the per-target ninja path — and fires regardless of --recognize-codegen
// (the genrule fallback is what carries the non-hermetic driver either way).
func noteHostCodegenTool(cc *codegenContext, fallback ir.Target) {
	if cc == nil {
		return
	}
	rawTok, driver, absolute, ok := classifyHostCodegenTool(fallback.GenruleCmd)
	if !ok {
		return
	}
	note := hostCodegenToolNote{Driver: driver, Absolute: absolute, Genrule: fallback.Name}
	if absolute {
		note.Path = rawTok
		// Synth-prefix-resident: a CROSS-ELEMENT tool. The raw path is
		// per-run-ephemeral, so anchor it to the deterministic /opt/prefix/
		// form (the same key the manifest uses) — never let the ephemeral path
		// reach the report (it would break the byte-identical contract).
		if p := strings.TrimRight(cc.HostPrefixDir, "/"); p != "" &&
			(rawTok == p || strings.HasPrefix(rawTok, p+"/")) {
			note.Prefix = true
			note.Path = ManifestPrefixAnchor + strings.TrimPrefix(rawTok, p+"/")
		}
	}
	cc.HostCodegenTools = append(cc.HostCodegenTools, note)
}

// emitHostCodegenToolTodos folds the recorded un-hermeticized host-tool
// genrules into one `host-codegen-tool` todo PER DRIVER (N genrules driven by
// the same tool → one todo with N anchors), with a ready-to-paste manifest
// `tools` entry in SuggestedShape. An absolute-host-path driver is Actionable
// (it cannot resolve on a clean executor); a PATH-resolved basename is
// Improvement (it builds where the tool is installed, but isn't hermetic).
func emitHostCodegenToolTodos(c *todos.Collector, notes []hostCodegenToolNote) {
	if c == nil || len(notes) == 0 {
		return
	}
	// Group by driver. The path / absolute / prefix facets are stable per
	// driver across its genrules, so the first note's values represent the
	// group.
	type group struct {
		path     string
		absolute bool
		prefix   bool
		genrules map[string]bool
	}
	byDriver := map[string]*group{}
	for _, n := range notes {
		g := byDriver[n.Driver]
		if g == nil {
			g = &group{path: n.Path, absolute: n.Absolute, prefix: n.Prefix, genrules: map[string]bool{}}
			byDriver[n.Driver] = g
		}
		g.genrules[n.Genrule] = true
	}
	drivers := make([]string, 0, len(byDriver))
	for d := range byDriver {
		drivers = append(drivers, d)
	}
	sort.Strings(drivers)
	for _, d := range drivers {
		g := byDriver[d]
		names := make([]string, 0, len(g.genrules))
		for n := range g.genrules {
			names = append(names, n)
		}
		sort.Strings(names)
		anchors := make([]todos.Anchor, 0, len(names))
		for _, n := range names {
			anchors = append(anchors, todos.Anchor{Construct: "genrule " + n + " (driver " + d + ")"})
		}
		disp := todos.Improvement
		if g.absolute {
			disp = todos.Actionable
		}
		origin := "host"
		if g.prefix {
			origin = "prefix"
		}
		// The suggested `tools` match is the BASENAME — deterministic (the
		// absolute path is informational evidence only) and resolves through
		// LookupTool's basename branch regardless of where the tool sat.
		ev := map[string]any{
			"driver":   d,
			"match":    d,
			"origin":   origin,
			"absolute": g.absolute,
			"genrules": names,
		}
		if g.path != "" {
			ev["path"] = g.path
		}
		c.Add(todos.Todo{
			Kind:           "host-codegen-tool",
			Disposition:    disp,
			GroupKey:       d,
			Anchors:        anchors,
			Evidence:       ev,
			SuggestedShape: hostCodegenToolStanza(d),
			Prompt:         hostCodegenToolPrompt(d, g.absolute, g.prefix),
		})
	}
}

// hostCodegenToolPrompt tailors the remedy to the tool's origin. A prefix tool
// is cross-element — its label is the PRODUCING element's manifest Export (the
// orchestrator can auto-derive it), with a basename `tools` entry as a stopgap.
// A host tool (PATH or host-install absolute) wants a `tools` entry mapping it
// to the providing label (a BCR module, a wrapper rule).
func hostCodegenToolPrompt(driver string, absolute, prefix bool) string {
	base := "genrule(s) drive the codegen tool " + driver + " by " + hostToolRefKind(absolute, prefix) +
		" — non-hermetic (it relies on the host toolchain, and an absolute path won't " +
		"exist on a clean Bazel executor). "
	if prefix {
		return base + "It resolved from the synth-prefix, so it's a CROSS-ELEMENT tool: " +
			"wire it through the PRODUCING element's imports-manifest Export (the orchestrator " +
			"can auto-derive that label). As a stopgap, the basename `tools` entry in " +
			"suggested_shape also works. The converter's tool-swap then rewrites the driver " +
			"to $(execpath <label>) and stages the hermetic tool. See docs/codegen-recognizers.md."
	}
	return base + "Map it to a Bazel label by adding the suggested_shape entry to the imports " +
		"manifest (--imports-manifest): the tool-swap then rewrites the driver to " +
		"$(execpath <label>) and stages the hermetic tool. See docs/codegen-recognizers.md."
}

// hostToolRefKind describes how the driver was referenced, for the prompt.
func hostToolRefKind(absolute, prefix bool) string {
	switch {
	case prefix:
		return "an absolute synth-prefix path"
	case absolute:
		return "an absolute host path"
	default:
		return "a host PATH name"
	}
}

// hostCodegenToolStanza renders the ready-to-paste imports-manifest `tools`
// entry for one host tool, keyed by the (deterministic) basename. The label is
// a placeholder the operator fills in with the real provider (a BCR module's
// tool, a wrapper rule, the producing element's export).
func hostCodegenToolStanza(match string) string {
	var b strings.Builder
	b.WriteString("# imports manifest (--imports-manifest), under top-level \"tools\":\n")
	b.WriteString("{\n")
	b.WriteString("  \"match\": " + starlarkStr(match) + ",\n")
	b.WriteString("  \"label\": \"//path/to:" + sanitizeOutputName(filepath.Base(match)) + "\"  // the Bazel target that provides the tool\n")
	b.WriteString("}")
	return b.String()
}
