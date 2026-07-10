package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// detectCyclicStaticArchives finds static archives cmake emits MORE THAN
// ONCE on a target's codemodel link line. cmake breaks a cyclic
// static-archive SCC — mutual symbol references the DECLARED dep graph
// (`.pc` / INTERFACE_LINK_LIBRARIES) doesn't encode — not with
// `-Wl,--start-group`/`--end-group` but by REPETITION: it lists the archive
// again so a single-pass linker satisfies the back-reference on a later
// occurrence. Bazel links each dep once, at one position, so such an SCC
// would be left with undefined symbols. The repetition is visible only here
// (the codemodel link line during lowering); the harvester sees only the
// acyclic declared graph.
//
// The Bazel-native equivalent of cmake's repetition is whole-archive
// (`alwayslink = True`): every object is linked regardless of position, so a
// back-reference always resolves. For an IN-CODEBASE archive (an emitted
// cc_library) the converter applies it directly. For a PREBUILT archive (a
// cc_import the harvester's wrappergen owns, upstream of the converter) it
// can't reach the wrapper — it emits an actionable todo naming the SCC + the
// remedy. Over-detection is safe: alwayslink only ever links more objects.
func detectCyclicStaticArchives(r *fileapi.Reply, pkg *ir.Package, imports *manifest.Resolver, hostPrefix string, tc *todos.Collector) {
	if r == nil || pkg == nil {
		return
	}
	// In-codebase library artifact basename -> cmake target name.
	nameToTarget := map[string]string{}
	for _, t := range r.Targets {
		switch t.Type {
		case "STATIC_LIBRARY", "SHARED_LIBRARY", "MODULE_LIBRARY":
			if t.NameOnDisk != "" {
				nameToTarget[t.NameOnDisk] = t.Name
			}
		}
	}
	byName := make(map[string]*ir.Target, len(pkg.Targets))
	for i := range pkg.Targets {
		byName[pkg.Targets[i].Name] = &pkg.Targets[i]
	}

	inCodebase := map[string]bool{} // cmake target name -> alwayslink applied
	prebuilt := map[string]string{} // export cmake target -> archive basename
	for _, t := range r.Targets {
		if t.Link == nil {
			continue
		}
		for archive := range repeatedStaticArchives(t.Link.CommandFragments) {
			base := filepath.Base(archive)
			// IN-CODEBASE: apply alwayslink directly on the emitted
			// cc_library (the Bazel-native whole-archive).
			if name, ok := nameToTarget[base]; ok {
				if irt := byName[name]; irt != nil && irt.Kind == ir.KindCCLibrary {
					irt.Alwayslink = true
					inCodebase[name] = true
				}
				continue
			}
			// PREBUILT: the archive is a manifest export whose cc_import
			// wrapper the converter can't touch (wrappergen owns it).
			anchored := archive
			if hostPrefix != "" && strings.HasPrefix(archive, hostPrefix+string(filepath.Separator)) {
				anchored = manifestPrefixAnchor + archive[len(hostPrefix)+1:]
			}
			if ex := imports.LookupLinkPath(anchored); ex != nil && !ex.AlwaysLink {
				// Skip when the harvester already flagged the export
				// AlwaysLink (its wrapper's cc_import is already
				// whole-archive) — only surface a prebuilt the produce-time
				// evidence missed, visible only in this consumer's codemodel.
				prebuilt[ex.CMakeTarget] = base
			}
		}
	}
	emitCyclicArchiveTodos(tc, inCodebase, prebuilt)
}

// repeatedStaticArchives returns the set of static-archive (`.a`) link
// fragments that appear more than once — cmake's cyclic-SCC repetition.
// Only role="libraries" fragments count; only `.a` carries the single-pass
// ordering problem (a shared lib's symbols resolve regardless of position).
func repeatedStaticArchives(frags []fileapi.CommandFragment) map[string]bool {
	counts := map[string]int{}
	for _, f := range frags {
		if f.Role != "libraries" {
			continue
		}
		p := strings.TrimSpace(f.Fragment)
		if strings.HasSuffix(p, ".a") {
			counts[p]++
		}
	}
	repeated := map[string]bool{}
	for p, n := range counts {
		if n >= 2 {
			repeated[p] = true
		}
	}
	return repeated
}

func emitCyclicArchiveTodos(tc *todos.Collector, inCodebase map[string]bool, prebuilt map[string]string) {
	if tc == nil {
		return
	}
	// In-codebase: auto-fixed (alwayslink applied). Informational, so the
	// applied whole-archive is traceable rather than a silent behavior change.
	for _, name := range cyclicSortedKeys(inCodebase) {
		tc.Add(todos.Todo{
			Kind:        "cyclic-static-archive",
			Disposition: todos.Informational,
			GroupKey:    name,
			Anchors:     []todos.Anchor{{Construct: "cyclic static-archive SCC: " + name}},
			Evidence:    map[string]any{"target": name, "remedy": "alwayslink", "applied": true},
			Prompt: "cmake links " + name + " more than once to break a cyclic static-archive SCC (mutual symbol " +
				"references the declared dep graph doesn't encode). Bazel links each dep once, so the converter set " +
				"alwayslink = True on this in-codebase cc_library — the Bazel-native whole-archive equivalent of " +
				"cmake's repetition. No action needed.",
		})
	}
	// Prebuilt: the converter can't touch the cc_import wrapper (wrappergen
	// owns it, upstream), so the operator must whole-archive it.
	for _, name := range cyclicSortedKeys(prebuilt) {
		tc.Add(todos.Todo{
			Kind:        "cyclic-static-archive",
			Disposition: todos.Actionable,
			GroupKey:    name,
			Anchors:     []todos.Anchor{{Construct: "cyclic static-archive SCC: " + name + " (" + prebuilt[name] + ")"}},
			Evidence:    map[string]any{"cmake_target": name, "archive": prebuilt[name], "remedy": "alwayslink"},
			Prompt: "cmake links the prebuilt archive " + prebuilt[name] + " more than once to break a cyclic " +
				"static-archive SCC. Bazel links each dep once, so the back-references would be unresolved (undefined " +
				"symbols at the consumer's link). Set alwayslink = True on this import's cc_import wrapper (" + name +
				") so every object is linked regardless of position — the Bazel-native equivalent of cmake's repetition.",
		})
	}
}

func cyclicSortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
