package lower

import (
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// ApplyPerConfigBakes folds per-build-type configure_file bodies into an
// already-lowered package — the consumer half of the per-config bake passes
// (convert-element-cmake's --per-config-bake).
//
// A multi-config cmake generator runs configure ONCE with no
// CMAKE_BUILD_TYPE, so a configure_file body a single-config-idiomatic
// project derives from CMAKE_BUILD_TYPE (LLVM's abi-breaking.h:
// LLVM_ENABLE_ABI_BREAKING_CHECKS follows assertions — on for Debug, off for
// Release) bakes ONE config's view for every //config:* arm. The per-config
// passes re-configure once per requested build type and hand the captured
// bodies here: bakes maps a recovered output's build-dir-relative path (the
// shape of ir.Target.WriteFileOut at this stage — pre-split) to each config
// name's bytes. Config names map onto //config:<name> select arm labels
// (configLabel); the fold itself is shared with the option axis via
// ApplyContentBakes.
func ApplyPerConfigBakes(pkg *ir.Package, bakes map[string]map[string][]byte, recordedSrcDir, recordedBuildDir, labelRoot string) (applied []string) {
	relabeled := make(map[string]map[string][]byte, len(bakes))
	for rel, perCfg := range bakes {
		m := make(map[string][]byte, len(perCfg))
		for cfg, body := range perCfg {
			m[configLabel(cfg)] = body
		}
		relabeled[rel] = m
	}
	return ApplyContentBakes(pkg, relabeled, recordedSrcDir, recordedBuildDir, labelRoot, "cmake-codegen-per-config-content")
}

// ApplyContentBakes folds captured alternate-cell configure_file bodies into
// an already-lowered package. It is the shared consumer half of both content
// bake axes: the per-config bake (ApplyPerConfigBakes; cells are build
// types) and the option lift's per-option bake (cells are option values —
// an option-derived `#cmakedefine` body differs across the flip configures
// exactly the way a CMAKE_BUILD_TYPE-derived one differs across build
// types).
//
// bakes maps a recovered output's build-dir-relative path (the shape of
// ir.Target.WriteFileOut at this stage — pre-split) to each SELECT ARM
// LABEL's bytes (`//config:<name>` or `//options:<name>_<value>` — callers
// pass final labels).
//
// For each KindWriteFile target whose bodies actually DIFFER from the
// primary view, the per-cell line lists land on WriteFileContentByConfig
// (WriteFileContent — the primary view — becomes the //conditions:default
// arm at emit) and the target is tagged `tag` for auditability. Arms merge
// into an existing WriteFileContentByConfig map (two lifted options can
// both shape one output; last writer wins per label, and labels are
// disjoint across options/configs by construction). Identical-to-primary
// bodies are skipped (the overwhelmingly common case), as is any cell body
// that fails the writeFileLines text gate — write_file can't carry it, and
// a mixed select (text arms + base64 genrule arm) isn't expressible on one
// rule, so the target keeps the single primary body rather than emitting a
// lying arm.
//
// Each captured body is re-anchored with the SAME policy its target's
// primary body got, so a path-spelling delta between a scratch dir
// (<buildDir>-cfg-<name> / <buildDir>-opt-<i>-<name>) and the primary dir
// can't fabricate a select(): file_generate-driven bakes go through
// reanchorResponseContent (exec-root form + @BSB_GENDIR@ markers),
// everything else through the configure_file strip policy (prefix removal,
// extended to the scratch dirs).
//
// Returns the names of the targets that gained per-cell content, for the
// caller's stderr surfacing.
func ApplyContentBakes(pkg *ir.Package, bakes map[string]map[string][]byte, recordedSrcDir, recordedBuildDir, labelRoot, tag string) (applied []string) {
	if pkg == nil || len(bakes) == 0 {
		return nil
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindWriteFile {
			continue
		}
		perCell := bakes[t.WriteFileOut]
		if len(perCell) == 0 {
			continue
		}
		fileGenDriven := stringSliceContains(t.Tags, "cmake-codegen-driver=file_generate")
		byLabel := make(map[string][]string, len(perCell))
		differs := false
		ok := true
		for label, body := range perCell {
			if fileGenDriven {
				body, _ = reanchorResponseContent(body, recordedSrcDir, recordedBuildDir, labelRoot)
			} else {
				body = []byte(stripConvertTimePathsCfg(string(body), recordedSrcDir, recordedBuildDir))
			}
			lines, textOK := writeFileLines(body)
			if !textOK {
				ok = false
				break
			}
			if !equalLines(lines, t.WriteFileContent) {
				differs = true
			}
			byLabel[label] = lines
		}
		if !ok || !differs {
			continue
		}
		if t.WriteFileContentByConfig == nil {
			t.WriteFileContentByConfig = byLabel
		} else {
			for label, lines := range byLabel {
				t.WriteFileContentByConfig[label] = lines
			}
		}
		if !stringSliceContains(t.Tags, tag) {
			t.Tags = append(t.Tags, tag)
		}
		applied = append(applied, t.Name)
	}
	return applied
}

// equalLines reports element-wise equality of two line lists.
func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
