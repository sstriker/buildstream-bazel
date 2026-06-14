package lower

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// liftCMakeETarCreate lifts `cmake -E tar <create-mode> <archive> [files…]`
// to a pkg_tar rule (rules_pkg) — the idiomatic Bazel packaging shape,
// emitted generically via KindNativeRule (the codegen-recognizer
// registry substrate; the emit auto-loads @rules_pkg//pkg:tar.bzl and
// write-a already carries the rules_pkg bazel_dep for cmake elements).
// args == argv[3:] == [mode, archive, files…]. The compression flag in
// the mode (z/j/J) maps to pkg_tar's `extension`. Every input must
// resolve to a Bazel label (a source-tree file, or a build-dir file a
// recovered producer emits) — an unresolvable input refuses the lift so
// the archive never silently drops a member.
func liftCMakeETarCreate(args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if len(args) < 2 {
		return nil, "cmake -E tar create: need a mode and an archive", false
	}
	mode, archive, files := args[0], args[1], args[2:]
	if len(files) == 0 {
		return nil, "cmake -E tar create: no input files to package", false
	}
	archiveRel, ok := executeProcessAnchorOutput(archive, anc)
	if !ok {
		return nil, fmt.Sprintf("cmake -E tar create: archive %q is not under the build dir", archive), false
	}
	if cc.outputClaimed(archiveRel) {
		return []string{archiveRel}, "", true
	}

	srcLabels := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, f := range files {
		label, ok := tarInputLabel(f, anc, cc)
		if !ok {
			return nil, fmt.Sprintf("cmake -E tar create: input %q is neither a source-tree file nor a recovered build output", f), false
		}
		if !seen[label] {
			seen[label] = true
			srcLabels = append(srcLabels, label)
		}
	}
	sort.Strings(srcLabels)

	name := "tar_" + sanitizePathToNameStem(archiveRel)
	spec := &ir.NativeRuleSpec{
		Kind:       "pkg_tar",
		LoadFrom:   "@rules_pkg//pkg:tar.bzl",
		LoadSymbol: "pkg_tar",
		Attrs: []ir.NativeAttr{
			{Name: "srcs", List: srcLabels},
			{Name: "out", Str: archiveRel},
		},
	}
	if ext := tarExtensionForMode(mode); ext != "tar" {
		spec.Attrs = append(spec.Attrs, ir.NativeAttr{Name: "extension", Str: ext})
	}
	spec.Attrs = append(spec.Attrs, ir.NativeAttr{Name: "visibility", List: []string{"//visibility:private"}})
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:       name,
		Kind:       ir.KindNativeRule,
		NativeRule: spec,
		Tags:       cmakeETags("tar"),
	})
	cc.OutToGenrule[archiveRel] = name
	return []string{archiveRel}, "", true
}

// tarInputLabel resolves one tar input to a Bazel label: a source-tree
// file becomes its package-relative label (umbrella-aware), a build-dir
// file a recovered producer emits becomes that producer's :label.
func tarInputLabel(f string, anc execAnchors, cc *codegenContext) (string, bool) {
	if rel, ok := executeProcessAnchorSource(f, anc); ok && rel != "" {
		return rel, true
	}
	if rel, inside := relativeIfInside(anc.recordedBuildDir, f); inside {
		if name, produced := cc.OutToGenrule[rel]; produced {
			return ":" + name, true
		}
	}
	return "", false
}

// tarExtensionForMode maps the cmake tar mode's compression flag to the
// pkg_tar `extension` value. cmake: z=gzip, j=bzip2, J=xz (uppercase).
func tarExtensionForMode(mode string) string {
	switch {
	case strings.Contains(mode, "z"):
		return "tar.gz"
	case strings.Contains(mode, "j"):
		return "tar.bz2"
	case strings.Contains(mode, "J"):
		return "tar.xz"
	default:
		return "tar"
	}
}
