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
	// Driver is the tool basename (the manifest `tools` match key would use
	// this, or the absolute path).
	Driver string
	// Match is the suggested `tools` match — the basename for a PATH-resolved
	// tool, or the absolute path verbatim for an absolute-host-path driver.
	Match string
	// Absolute is true when the driver was referenced by an absolute host path
	// (definitely non-portable → Actionable); false for a PATH-resolved
	// basename (relies on the host PATH → Improvement).
	Absolute bool
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
// its driver is an un-hermeticized host codegen tool, returns the driver
// basename + whether it was an absolute host path. ok=false for a hermeticized
// driver ($(execpath)/$(location)), a benign cmake -E / shell-util driver, or a
// command whose first token isn't a clean program reference (a shell
// assignment / substitution / subshell — the scratch-dir workdir shapes).
func classifyHostCodegenTool(genruleCmd string) (driver string, absolute, ok bool) {
	c := strings.TrimSpace(genruleCmd)
	// Strip a leading `cd <dir> && ` (the recovered cwd anchor).
	if strings.HasPrefix(c, "cd ") {
		if i := strings.Index(c, " && "); i > 0 {
			c = strings.TrimSpace(c[i+4:])
		}
	}
	fields := strings.Fields(c)
	if len(fields) == 0 {
		return "", false, false
	}
	tok := fields[0]
	// Already hermeticized: the swap rewrote the driver to a Make-var ref.
	if strings.HasPrefix(tok, "$(") {
		return "", false, false
	}
	// Not a clean program token: a shell assignment (VAR=…), a command
	// substitution, or a subshell preamble (the workdir-buildout scratch-dir
	// shape). Don't try to classify these.
	if strings.ContainsAny(tok, "=(){}$`\"'") {
		return "", false, false
	}
	base := filepath.Base(tok)
	if base == "" || base == "." || base == "unknown" || benignGenruleDriver[base] {
		return "", false, false
	}
	return base, filepath.IsAbs(tok), true
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
	driver, absolute, ok := classifyHostCodegenTool(fallback.GenruleCmd)
	if !ok {
		return
	}
	match := driver
	if absolute {
		// An absolute host path: the verbatim path is the precise match key
		// (the basename would also work, but the path is what cmake recorded).
		if c := strings.TrimSpace(fallback.GenruleCmd); strings.HasPrefix(c, "cd ") {
			if i := strings.Index(c, " && "); i > 0 {
				c = strings.TrimSpace(c[i+4:])
			}
			if f := strings.Fields(c); len(f) > 0 {
				match = f[0]
			}
		} else if f := strings.Fields(c); len(f) > 0 {
			match = f[0]
		}
	}
	cc.HostCodegenTools = append(cc.HostCodegenTools, hostCodegenToolNote{
		Driver:   driver,
		Match:    match,
		Absolute: absolute,
		Genrule:  fallback.Name,
	})
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
	// Group by driver. The match key + absolute flag are stable per driver
	// across its genrules, so the first note's values represent the group.
	type group struct {
		match    string
		absolute bool
		genrules map[string]bool
	}
	byDriver := map[string]*group{}
	for _, n := range notes {
		g := byDriver[n.Driver]
		if g == nil {
			g = &group{match: n.Match, absolute: n.Absolute, genrules: map[string]bool{}}
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
		c.Add(todos.Todo{
			Kind:        "host-codegen-tool",
			Disposition: disp,
			GroupKey:    d,
			Anchors:     anchors,
			Evidence: map[string]any{
				"driver":   d,
				"match":    g.match,
				"absolute": g.absolute,
				"genrules": names,
			},
			SuggestedShape: hostCodegenToolStanza(g.match),
			Prompt: "genrule(s) drive the host codegen tool " + d + " by " +
				hostToolRefKind(g.absolute) + " — non-hermetic (it relies on the " +
				"host toolchain, and an absolute path won't exist on a clean Bazel " +
				"executor). Map it to a Bazel label by adding the suggested_shape " +
				"entry to the imports manifest (--imports-manifest): the converter's " +
				"tool-swap then rewrites the driver to $(execpath <label>) and stages " +
				"the hermetic tool. See docs/codegen-recognizers.md.",
		})
	}
}

// hostToolRefKind describes how the driver was referenced, for the prompt.
func hostToolRefKind(absolute bool) string {
	if absolute {
		return "absolute host path"
	}
	return "host PATH name"
}

// hostCodegenToolStanza renders the ready-to-paste imports-manifest `tools`
// entry for one host tool. The label is a placeholder the operator fills in
// with the real provider (a BCR module's tool, a wrapper rule, …).
func hostCodegenToolStanza(match string) string {
	var b strings.Builder
	b.WriteString("# imports manifest (--imports-manifest), under top-level \"tools\":\n")
	b.WriteString("{\n")
	b.WriteString("  \"match\": " + starlarkStr(match) + ",\n")
	b.WriteString("  \"label\": \"//path/to:" + sanitizeOutputName(filepath.Base(match)) + "\"  // the Bazel target that provides the tool\n")
	b.WriteString("}")
	return b.String()
}
