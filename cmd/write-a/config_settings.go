package main

import (
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/converter/emit/configsettings"
)

// writeConfigSettingsPackage renders project B's //config package — a
// string_flag build_type plus one config_setting per cmake configuration —
// backing the //config:<name> select() arms the multi-config fold lands in
// the staged BUILD.bazel.out files. Reuses converter/emit/configsettings so
// the standalone convert-element-cmake --out-config-settings path and the
// write-a pipeline path emit byte-identical packages (the
// TestConfigLabel_MatchesConfigSettingsEmit parity guard pins the naming).
//
// The first build type is the string_flag default, so an unset flag
// reproduces lower's flattened baseline view. (Sanitizer-shaped configs
// route through cc_toolchain --features rather than select() arms, but
// write-a doesn't thread --out-sanitizer-features today; when it does, the
// filtering convert-element-cmake already applies will move here too — a
// stray config_setting for an unreferenced config is harmless until then.)
//
// No-op (writes nothing) when --build-types is unset or resolves to fewer
// than two distinct configs — there's no multi-config select to back,
// keeping the single-config render byte-stable.
func writeConfigSettingsPackage(outDir string) error {
	if len(cmakeConfig.buildTypes) == 0 {
		return nil
	}
	body := configsettings.Emit(cmakeConfig.buildTypes, cmakeConfig.buildTypes[0])
	if body == nil {
		return nil
	}
	return writeFile(filepath.Join(outDir, "config", "BUILD.bazel"), string(body))
}
