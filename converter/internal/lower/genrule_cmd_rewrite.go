package lower

import (
	"path/filepath"
	"strings"
)

// rewriteGenruleCmd strips convert-time-only constructs from a
// recovered ninja CUSTOM_COMMAND cmd before it lands as the
// genrule's `cmd` attribute. Without this normalisation the
// rendered genrule leaks the convert-host filesystem layout
// (`/tmp/<build>/...` paths, `cd <abs-subdir> &&` preambles)
// into the BUILD.bazel, where Bazel's hermetic sandbox can't
// resolve any of them at action time.
//
// Rewrites applied:
//
//   - `cd <abs-under-buildDir> && ` prefix: cmake's Ninja
//     generator prepends this so the command runs in a per-target
//     build subdirectory at convert time. Bazel runs the genrule
//     in $(GENDIR) which doesn't exist at convert time, and the
//     cd path doesn't exist at action time. Drop the prefix
//     entirely when the target dir resolves under buildDir or
//     cmakeSrc.
//   - Verbatim `<buildDir>/`-rooted path references in the cmd
//     body: re-anchor to genrule-output-relative by stripping the
//     convert-time prefix. For files the genrule itself produces
//     this is correct (Bazel puts genrule outputs under $(GENDIR)
//     at the matching relative path).
//   - Verbatim `<cmakeSrc>/`-rooted path references: re-anchor
//     to workspace-relative.
//
// All rewrites are conservative — paths only re-anchor when they
// start with the canonical anchor prefix + "/", so partial-match
// hazards (e.g. `<buildDir>` vs `<buildDir>_other`) are avoided.
//
// Tokens without an embedded anchor pass through unchanged.
func rewriteGenruleCmd(cmd, cmakeSrc, buildDir string) string {
	if cmd == "" {
		return cmd
	}
	// Strip `cd <abs> && ` prefix when <abs> is under buildDir or
	// cmakeSrc. Bazel runs the genrule in its sandbox-rooted
	// $(GENDIR); the cd is cmake-internal.
	if strings.HasPrefix(cmd, "cd ") {
		if end := strings.Index(cmd, " && "); end > 0 {
			target := strings.TrimSpace(strings.TrimPrefix(cmd[:end], "cd "))
			if filepath.IsAbs(target) {
				drop := false
				if buildDir != "" {
					if _, ok := relativeIfInside(buildDir, target); ok {
						drop = true
					}
				}
				if !drop && cmakeSrc != "" {
					if _, ok := relativeIfInside(cmakeSrc, target); ok {
						drop = true
					}
				}
				if drop {
					cmd = cmd[end+4:]
				}
			}
		}
	}
	// Strip cmakeSrc and buildDir prefixes from the cmd body. The
	// trailing slash on the anchor ensures partial-match safety
	// against e.g. `<buildDir>_other`.
	for _, anchor := range []string{cmakeSrc, buildDir} {
		if anchor == "" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, anchor+"/", "")
	}
	return cmd
}
