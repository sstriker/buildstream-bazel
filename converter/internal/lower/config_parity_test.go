package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/emit/configsettings"
)

// TestConfigLabel_MatchesConfigSettingsEmit is the cross-package guard that
// the two halves of the multi-config select() contract agree on naming:
// lower's configLabel mints the //config:<name> keys the fold puts on
// select() arms, and configsettings.Emit mints the config_setting targets
// that back them. If either side's lowercasing/spelling drifts, the
// emitted select() arms reference labels that don't exist and the
// converted BUILD stops loading — so pin that every configLabel key has a
// matching config_setting name in Emit's output.
func TestConfigLabel_MatchesConfigSettingsEmit(t *testing.T) {
	configs := []string{"Debug", "Release", "RelWithDebInfo", "MinSizeRel"}

	emitted := string(configsettings.Emit(configs, configs[0]))

	for _, c := range configs {
		// The select() arm key the fold emits for this config.
		key := configLabel(c) // "//config:<lowercased>"
		name := strings.TrimPrefix(key, "//config:")
		if name == key {
			t.Fatalf("configLabel(%q) = %q, expected a //config: prefix", c, key)
		}
		// configsettings.Emit must define a config_setting with exactly
		// that name (so the //config:<name> label resolves).
		want := "name = \"" + name + "\","
		if !strings.Contains(emitted, want) {
			t.Errorf("config %q: select arm key %q has no backing config_setting (%q) in configsettings.Emit output:\n%s",
				c, key, want, emitted)
		}
	}
}
