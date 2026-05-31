package main

import (
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/converter/emit/configsettings"
)

// standardConfigurationTypes is cmake's standard multi-config set. write-a
// emits the //config package over this set when --build-types=auto: write-a
// can't (and must not) run cmake at render time to detect a project's actual
// CMAKE_CONFIGURATION_TYPES — that detection happens in project A's
// conversion genrule at build time (convert-element-cmake --build-types=auto
// lets the project's own config types stand). A project's detected configs
// are a subset of this standard set for virtually all projects, so the
// //config:<name> select() arm labels resolve; a project declaring a
// non-standard config (a custom `set(CMAKE_CONFIGURATION_TYPES …)`) needs an
// explicit --build-types list so write-a emits that config_setting too.
var standardConfigurationTypes = []string{"Debug", "Release", "RelWithDebInfo", "MinSizeRel"}

// writeConfigSettingsPackage renders project B's //config package — a
// string_flag build_type plus one config_setting per cmake configuration —
// backing the //config:<name> select() arms the multi-config fold lands in
// the staged BUILD.bazel.out files. Reuses converter/emit/configsettings.Emit
// so the standalone convert-element-cmake --out-config-settings path and the
// write-a pipeline path emit byte-identical packages (the
// TestConfigLabel_MatchesConfigSettingsEmit parity guard pins the naming).
//
// Config set: the explicit --build-types list when given, else cmake's
// standard set for --build-types=auto (see standardConfigurationTypes — the
// per-project detection happens in A's conversion genrule, not here). The
// first config is the string_flag default, so an unset flag reproduces
// lower's flattened baseline view.
//
// No-op (writes nothing) when neither --build-types nor auto is set, or the
// config set resolves to fewer than two distinct configs — there's no
// multi-config select to back, keeping the single-config render byte-stable.
func writeConfigSettingsPackage(outDir string) error {
	configs := cmakeConfig.buildTypes
	if cmakeConfig.autoBuildTypes {
		configs = standardConfigurationTypes
	}
	if len(configs) == 0 {
		return nil
	}
	body := configsettings.Emit(configs, configs[0])
	if body == nil {
		return nil
	}
	return writeFile(filepath.Join(outDir, "config", "BUILD.bazel"), string(body))
}
