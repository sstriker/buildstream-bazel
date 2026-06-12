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
// name's bytes.
//
// For each KindWriteFile target whose bodies actually DIFFER across configs,
// the per-config line lists land on WriteFileContentByConfig (keyed by
// //config:<name> label, matching the multi-config fold's select arms;
// WriteFileContent — the primary multi-config view — becomes the
// //conditions:default arm at emit) and the target is tagged
// cmake-codegen-per-config-content for auditability. Identical-across-config
// bodies are skipped (the overwhelmingly common case), as is any config body
// that fails the writeFileLines text gate — write_file can't carry it, and a
// mixed select (text arms + base64 genrule arm) isn't expressible on one
// rule, so the target keeps the single primary body rather than emitting a
// lying arm.
//
// Each captured body is re-anchored with the SAME policy its target's
// primary body got, so a path-spelling delta between a per-config
// scratch dir (<buildDir>-cfg-<name>) and the multi-config dir can't
// fabricate a select(): file_generate-driven bakes go through
// reanchorResponseContent (exec-root form + @BSB_GENDIR@ markers),
// everything else through the configure_file strip policy (prefix
// removal, extended here to the -cfg-<name> scratch dirs).
//
// Returns the names of the targets that gained per-config content, for the
// caller's stderr surfacing.
func ApplyPerConfigBakes(pkg *ir.Package, bakes map[string]map[string][]byte, recordedSrcDir, recordedBuildDir, labelRoot string) (applied []string) {
	if pkg == nil || len(bakes) == 0 {
		return nil
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindWriteFile {
			continue
		}
		perCfg := bakes[t.WriteFileOut]
		if len(perCfg) == 0 {
			continue
		}
		fileGenDriven := stringSliceContains(t.Tags, "cmake-codegen-driver=file_generate")
		byLabel := make(map[string][]string, len(perCfg))
		differs := false
		ok := true
		for cfg, body := range perCfg {
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
			byLabel[configLabel(cfg)] = lines
		}
		if !ok || !differs {
			continue
		}
		t.WriteFileContentByConfig = byLabel
		if !stringSliceContains(t.Tags, "cmake-codegen-per-config-content") {
			t.Tags = append(t.Tags, "cmake-codegen-per-config-content")
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
