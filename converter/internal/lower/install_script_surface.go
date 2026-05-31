package lower

import (
	"fmt"
	"io"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// surfaceInstallScriptInstallers emits a stderr warning summarising
// any install(SCRIPT) / install(CODE) directives in the cmake reply.
// These directory installers run cmake script code at install time
// — post-install patching of cmake config files, chmod adjustments,
// symlink creation. None of these have a clean Bazel translation:
// Bazel's install story is operator-side via rules_pkg or similar.
//
// The converter silently dropped these before; this surfacing makes
// the omission auditable. Each dropped directive is named with its
// cmake source site (resolved through the directory's BacktraceGraph)
// and, for install(SCRIPT), the script file — so operators can locate
// the declaration rather than just see a count.
//
// No-op when sink is nil (preserves the lower-as-pure-function
// shape every existing test depends on) or when no script/code
// installers are present.
func surfaceInstallScriptInstallers(r *fileapi.Reply, sink io.Writer) {
	if r == nil || sink == nil {
		return
	}
	type drop struct {
		scriptFile string // empty for install(CODE)
		site       string // "file:line", or "" when unresolved
	}
	var scripts, codes []drop
	for _, dir := range r.Directories {
		for _, inst := range dir.Installers {
			if inst.Type != "script" && inst.Type != "code" {
				continue
			}
			d := drop{scriptFile: inst.ScriptFile, site: installerSite(dir, inst.Backtrace)}
			if inst.Type == "script" {
				scripts = append(scripts, d)
			} else {
				codes = append(codes, d)
			}
		}
	}
	if len(scripts) == 0 && len(codes) == 0 {
		return
	}
	sortDrops := func(ds []drop) {
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].site != ds[j].site {
				return ds[i].site < ds[j].site
			}
			return ds[i].scriptFile < ds[j].scriptFile
		})
	}
	if len(scripts) > 0 {
		sortDrops(scripts)
		fmt.Fprintf(sink,
			"lower: %d install(SCRIPT) directive(s) dropped — no Bazel analogue for install-time cmake script execution; consider rules_pkg or operator-side install handling:\n",
			len(scripts))
		for _, d := range scripts {
			fmt.Fprintf(sink, "  install(SCRIPT%s)%s\n", scriptSuffix(d.scriptFile), siteSuffix(d.site))
		}
	}
	if len(codes) > 0 {
		sortDrops(codes)
		fmt.Fprintf(sink,
			"lower: %d install(CODE) directive(s) dropped — no Bazel analogue for install-time inline cmake code; consider rules_pkg or operator-side install handling:\n",
			len(codes))
		for _, d := range codes {
			fmt.Fprintf(sink, "  install(CODE)%s\n", siteSuffix(d.site))
		}
	}
}

// surfaceLauncherTargets emits a stderr warning naming any target
// launchers (CROSSCOMPILING_EMULATOR / TEST_LAUNCHER) the codemodel
// recorded. Bazel has no first-class per-target run-launcher — an
// emulator prefix for cross-built artifacts maps to toolchain /
// platform wiring the operator owns, and a test launcher to a
// test-runner wrapper — so these aren't routed automatically.
// Surfacing keeps them auditable instead of dropping them silently.
//
// No-op when sink is nil or no target carries a launcher (the common
// case — launchers only appear on cross builds, and are empty across
// the survey corpus).
func surfaceLauncherTargets(r *fileapi.Reply, sink io.Writer) {
	if r == nil || sink == nil {
		return
	}
	type hit struct{ target, typ, cmd string }
	var hits []hit
	for _, t := range r.Targets {
		for _, l := range t.Launchers {
			hits = append(hits, hit{target: t.Name, typ: l.Type, cmd: l.Command})
		}
	}
	if len(hits) == 0 {
		return
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].target != hits[j].target {
			return hits[i].target < hits[j].target
		}
		return hits[i].typ < hits[j].typ
	})
	fmt.Fprintf(sink,
		"lower: %d target launcher(s) recorded (CROSSCOMPILING_EMULATOR / TEST_LAUNCHER) — Bazel has no per-target run-launcher; not routed:\n",
		len(hits))
	for _, h := range hits {
		fmt.Fprintf(sink, "  %s: %s launcher %q\n", h.target, h.typ, h.cmd)
	}
}

// installerSite resolves a DirectoryInstaller.Backtrace index to a
// "file:line" string via the directory's BacktraceGraph. Returns ""
// when the reply carries no usable backtrace (older cmake, or a graph
// the index doesn't address) so callers can omit the site cleanly.
func installerSite(dir fileapi.Directory, backtrace int) string {
	if backtrace <= 0 || backtrace >= len(dir.BacktraceGraph.Nodes) {
		return ""
	}
	file, line, _ := outermostUserFrame(dir.BacktraceGraph, backtrace)
	if file == "" {
		return ""
	}
	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
}

func scriptSuffix(scriptFile string) string {
	if scriptFile == "" {
		return ""
	}
	return " " + scriptFile
}

func siteSuffix(site string) string {
	if site == "" {
		return ""
	}
	return " at " + site
}
