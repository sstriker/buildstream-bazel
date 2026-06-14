package lower

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// No silent drops of anything in the codemodel: a build-dir source or
// header the codemodel references is either RECOVERED or accounted
// LOUDLY. The recovery ladder for configure-time-created build-dir files
// whose producer the trace classifiers don't model (`file(WRITE)` /
// `file(APPEND)` / `file(COPY)` / `file(TOUCH)` / `file(DOWNLOAD)`, or
// any custom macro writing through them) is the on-disk bake: the
// configure already ran, so the bytes exist in the live build dir (and
// in recorded reply mirrors, which stash configure outputs at their
// build-relative paths), and bakeFileTarget materializes them exactly
// like the configure_file bake tier — same convert-time-bake trade
// (re-run convert to refresh), riding the bake-warning + bake-todo
// channels via the cmake-codegen-build-dir-bake facet. Only when the
// bytes are NOT on disk (offline replay without a mirror) does the elide
// remain — now recorded per (target, path) and surfaced as one stderr
// aggregate plus structured `source-elided` conversion-todos instead of
// a tag nobody is forced to read.

// elidedSourceRecord is one accounted drop: the consuming target, the
// path as the codemodel recorded it, and the drop class
// ("build-dir-source", "non-compile-group-source", …).
type elidedSourceRecord struct {
	Target string
	Path   string
	Class  string
}

// bakedBuildDirName names the bake rule for one build-relative path.
func bakedBuildDirName(rel string) string {
	return "baked_" + sanitizeOutputName(rel)
}

// buildDirBakeTags is the audit facet set for the on-disk bake; the
// -build-dir-bake entry in convertTimeBakedShapes routes these through
// the convert-time-bake warning + todo channels.
func buildDirBakeTags() []string {
	return []string{"cmake-codegen", "cmake-codegen-build-dir-bake"}
}

// bakeBuildDirFile bakes one build-relative file from the on-disk bytes
// under lc.hostBuild, registering the producer. Returns the rule name
// and false when the bytes aren't available (offline replay without a
// recorded mirror).
func bakeBuildDirFile(rel string, lc targetLowerCtx) (string, bool) {
	// Already wired to a producer? Don't byte-bake it. The motivating case is a
	// recognized native rule (e.g. an execute_process `protoc --cpp_out` lowered
	// to cc_proto_library, which compiles foo.pb.{cc,h} itself) —
	// rewriteNativeRuleConsumers strips it from the consumer's srcs and wires the
	// deps edge; recoverExecuteProcess populates the claim before target lowering
	// runs. The generic outputClaimed predicate also covers a genrule producer.
	if lc.cc.outputClaimed(rel) {
		return "", false
	}
	// Producer first (the bake-audit's tie-to-the-generation-command
	// close-out): a traced file() writer chain recovers the file from
	// its COMMAND — a true cp lift for COPY/COPY_FILE, trace-content
	// write_file for WRITE/APPEND/TOUCH (works OFFLINE too) — before
	// any on-disk byte falls back. See build_dir_writer_lift.go.
	if name, ok := liftBuildDirFileFromWriter(rel, lc); ok {
		return name, true
	}
	// --lift-download (opt-in): a file(DOWNLOAD) output is sourced from an
	// http_file repo at build time (declared via the download-repos.json
	// lockfile + the staged module extension) rather than byte-baked. The
	// fetch genrule produces the SAME <rel> output, so every consumer wires
	// through OutToGenrule unchanged — only the producer differs (cp from
	// @<repo>//file vs. embedded bytes). Trace-derived, so it runs BEFORE
	// the on-disk gate below: the hermetic fetch happens at repo-rule time,
	// no convert-time bytes required.
	if lc.cc.LiftDownload {
		if dl, isDL := downloadWriterFor(rel, lc); isDL {
			name := bakedBuildDirName(rel)
			lc.cc.Genrules = append(lc.cc.Genrules, downloadFetchTarget(name, rel, downloadRepoName(lc.bazelPackagePath, rel)))
			lc.cc.OutToGenrule[rel] = name
			recordDownloadLift(lc.cc, rel, dl.url, dl.hash)
			return name, true
		}
	}
	if lc.hostBuild == "" {
		return "", false
	}
	body, err := os.ReadFile(filepath.Join(lc.hostBuild, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	name := bakedBuildDirName(rel)
	// A file(DOWNLOAD) output stays a byte-bake by policy (no network
	// at build time) but cites its producer: the download facet rides
	// the bake-warning/todo channels and the provenance carries the
	// URL for a hand-lift to http_file (ROADMAP). Resolve the chain
	// and pick the tags BEFORE constructing the target once — the
	// body encode isn't free for large downloads.
	tags := buildDirBakeTags()
	dl, isDL := downloadWriterFor(rel, lc)
	if isDL {
		tags = downloadBakeTags()
	}
	t := bakeFileTarget(name, rel, body, tags)
	if isDL {
		t.Provenance = writerProvenance(dl, lc)
		t.Provenance.Command = "file(DOWNLOAD " + dl.url + ")"
		// Surface the http_file hand-off (operator opt-in; the bake is
		// the hermetic default). See download_lift.go.
		recordDownloadLift(lc.cc, rel, dl.url, dl.hash)
	}
	lc.cc.Genrules = append(lc.cc.Genrules, t)
	lc.cc.OutToGenrule[rel] = name
	return name, true
}

// bakeConsumedBuildDirHeaders closes the header half of the class: a
// target whose includes cover the build dir (targetBuildIncs) consumes
// whatever headers the configure wrote there, but the consumer
// attribution loops iterate RECOVERED outputs only — an untraced-writer
// header (`file(WRITE ${CMAKE_BINARY_DIR}/gen.h …)`) is invisible and
// the consumer fails at compile time with no converter signal. Walk each
// consumed include dir once (cached on cc), bake every on-disk
// header-shaped file with no producer (ninja outs and already-recovered
// outputs excluded; cmake bookkeeping skipped), and attach the baked
// rels to this consumer's hdrs with the same package-root genfiles
// handling the configure_file attribution applies.
func bakeConsumedBuildDirHeaders(irt *ir.Target, lc targetLowerCtx, targetBuildIncs map[string]bool) {
	if lc.hostBuild == "" || len(targetBuildIncs) == 0 {
		return
	}
	cc := lc.cc
	for inc := range targetBuildIncs {
		if !cc.BuildDirHdrWalked[inc] {
			cc.BuildDirHdrWalked[inc] = true
			walkBuildDirHeaders(inc, lc)
		}
	}
	// Attach every baked header covered by this consumer's include set.
	var rels []string
	for rel := range cc.BuildDirBakedHdrs {
		for inc := range targetBuildIncs {
			if isPathPrefix(inc, rel) {
				rels = append(rels, rel)
				break
			}
		}
	}
	if len(rels) == 0 {
		return
	}
	sort.Strings(rels)
	seen := map[string]bool{}
	for _, h := range irt.Hdrs {
		seen[h] = true
	}
	needsPkgRoot := false
	attached := false
	for _, rel := range rels {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		irt.Hdrs = append(irt.Hdrs, rel)
		attached = true
		for inc := range targetBuildIncs {
			if isPathPrefix(inc, rel) && !pkgPathIsRoot(lc.bazelPackagePath) && needsPkgRootInclude(inc, rel) {
				needsPkgRoot = true
			}
		}
	}
	if attached {
		if !stringSliceContains(irt.Tags, "has-cmake-codegen") {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
		if needsPkgRoot && !stringSliceContains(irt.Includes, ".") {
			irt.Includes = append(irt.Includes, ".")
		}
	}
}

// walkBuildDirHeaders bakes the unproduced on-disk headers under one
// consumed build-dir include (build-relative; "" is the build root).
// Bookkeeping trees are skipped; ninja-built and already-recovered
// outputs are excluded — only configure-written orphans bake.
func walkBuildDirHeaders(inc string, lc targetLowerCtx) {
	cc := lc.cc
	root := filepath.Join(lc.hostBuild, filepath.FromSlash(inc))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "CMakeFiles", ".cmake":
				return filepath.SkipDir
			}
			return nil
		}
		relHost, rerr := filepath.Rel(lc.hostBuild, p)
		if rerr != nil {
			return nil
		}
		rel := filepath.ToSlash(relHost)
		if !nestedBakeableHeader(rel) || cc.NinjaOuts[rel] {
			return nil
		}
		if _, produced := cc.OutToGenrule[rel]; produced {
			return nil
		}
		// Delegate to bakeBuildDirFile: it runs the writer-index lift
		// first (cp / stamp / write_file from the trace) and the
		// download branch (hermetic bake + the http_file hand-off todo)
		// before the on-disk byte-bake — so a downloaded or
		// writer-traced header gets the same recovery as a build-dir
		// source, not a bare on-disk bake.
		if name, ok := bakeBuildDirFile(rel, lc); ok {
			cc.BuildDirBakedHdrs[rel] = name
		}
		return nil
	})
}

// recordElidedSource accounts one codemodel-referenced drop for the
// end-of-lower aggregate warning + structured todos.
func recordElidedSource(cc *codegenContext, target, path, class string) {
	cc.ElidedSources = append(cc.ElidedSources, elidedSourceRecord{Target: target, Path: path, Class: class})
}

// warnElidedSources is the loudness backstop: every codemodel-referenced
// source the lowering DROPPED (rather than recovered) surfaces as one
// aggregated stderr warning plus one structured `source-elided` todo per
// (target, class) — the per-target tags remain for BUILD-side audits,
// but a tag alone is not accounting.
func warnElidedSources(opts Options, cc *codegenContext) {
	if len(cc.ElidedSources) == 0 {
		return
	}
	recs := append([]elidedSourceRecord(nil), cc.ElidedSources...)
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Target != recs[j].Target {
			return recs[i].Target < recs[j].Target
		}
		return recs[i].Path < recs[j].Path
	})
	if opts.Warnings != nil {
		fmt.Fprintf(opts.Warnings,
			"lower: %d codemodel source(s) DROPPED without recovery — the emitted rules are missing inputs cmake compiled (re-run with a live build dir to enable the on-disk bake, or wire the producer):\n",
			len(recs))
		for _, r := range recs {
			fmt.Fprintf(opts.Warnings, "  - %s: %s (%s)\n", r.Target, normalizeReportPath(r.Path, opts.HostSourceRoot, opts.BuildDir), r.Class)
		}
	}
	emitElidedSourceTodos(opts.Todos, recs, opts.HostSourceRoot, opts.BuildDir)
}

// emitElidedSourceTodos mirrors the drops into structured todos: one per
// (target, class), anchors carrying the normalized paths.
func emitElidedSourceTodos(c *todos.Collector, recs []elidedSourceRecord, sourceRoot, buildDir string) {
	if c == nil || len(recs) == 0 {
		return
	}
	type key struct{ target, class string }
	groups := map[key][]string{}
	var order []key
	for _, r := range recs {
		k := key{r.Target, r.Class}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], normalizeReportPath(r.Path, sourceRoot, buildDir))
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].target != order[j].target {
			return order[i].target < order[j].target
		}
		return order[i].class < order[j].class
	})
	for _, k := range order {
		paths := groups[k]
		sort.Strings(paths)
		anchors := make([]todos.Anchor, 0, len(paths))
		seenA := map[string]bool{}
		for _, p := range paths {
			if seenA[p] {
				continue
			}
			seenA[p] = true
			anchors = append(anchors, todos.Anchor{Construct: p})
		}
		c.Add(todos.Todo{
			Kind:        "source-elided",
			Disposition: todos.Actionable,
			GroupKey:    k.target + "|" + k.class,
			Anchors:     anchors,
			Evidence: map[string]any{
				"target": k.target,
				"class":  k.class,
				"paths":  paths,
			},
			SuggestedShape: "re-run the converter with a live build dir (the on-disk bake recovers configure-written files automatically); or wire the file's producer (genrule / write_file) and reference its output",
			Prompt: "Target " + k.target + " references " + plural(len(anchors), "codemodel source") +
				" the lowering dropped (" + k.class + ") — the emitted rule is missing inputs cmake compiled. Recover each (bake or producer wiring) or confirm the file is genuinely obsolete.",
		})
	}
}

// elidedKindsRefuseWhenEmpty reports whether the all-sources-elided
// refusal applies to this rule kind: binaries/tests (a srcs-less
// executable is Bazel-invalid) AND libraries with no headers either — an
// empty cc_library links nothing, so every consumer fails at final link
// with no signal pointing back here.
func elidedKindsRefuseWhenEmpty(irt *ir.Target) bool {
	switch irt.Kind {
	case ir.KindCCBinary, ir.KindCCTest:
		return len(irt.Srcs) == 0
	case ir.KindCCLibrary:
		return len(irt.Srcs) == 0 && len(irt.Hdrs) == 0 && len(irt.TextualHdrs) == 0
	}
	return false
}
