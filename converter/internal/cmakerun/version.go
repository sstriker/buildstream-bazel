package cmakerun

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
)

// MinCMakeMajor and MinCMakeMinor are the codemodel-v2 floor: the minimum
// cmake version that emits the File API objects we consume. Older cmakes
// silently produce malformed or missing replies. Bumping this floor is a
// breaking change for any orchestration that runs against host cmake.
//
// No upper bound is enforced here. The fileapi loader validates per-object
// schema majors (codemodel/cache/toolchains/cmakeFiles) and surfaces a clear
// error when a future cmake bumps any of them; that's the more precise
// compatibility tripwire. Tested across CMake 3.20 through 4.x.
const (
	MinCMakeMajor = 3
	MinCMakeMinor = 20
)

// AssertVersion runs `cmake --version` and returns an error if the parsed
// version is below the codemodel-v2 floor. Callers that need the version
// for diagnostics also receive (major, minor, patch).
func AssertVersion(ctx context.Context) (major, minor, patch int, err error) {
	cmakeBin, err := exec.LookPath("cmake")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("cmakerun: cmake not on PATH: %w", err)
	}
	out, err := exec.CommandContext(ctx, cmakeBin, "--version").Output()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("cmakerun: %s --version failed: %w", cmakeBin, err)
	}
	major, minor, patch, ok := parseCMakeVersion(string(out))
	if !ok {
		return 0, 0, 0, fmt.Errorf("cmakerun: could not parse cmake version: %q", out)
	}
	if (major < MinCMakeMajor) ||
		(major == MinCMakeMajor && minor < MinCMakeMinor) {
		return major, minor, patch,
			fmt.Errorf("cmake %d.%d.%d is below the codemodel-v2 floor (%d.%d); pass --allow-cmake-version-mismatch to override",
				major, minor, patch, MinCMakeMajor, MinCMakeMinor)
	}
	return major, minor, patch, nil
}

var cmakeVersionRe = regexp.MustCompile(`cmake version (\d+)\.(\d+)\.(\d+)`)

func parseCMakeVersion(s string) (major, minor, patch int, ok bool) {
	m := cmakeVersionRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return major, minor, patch, true
}
