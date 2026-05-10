// Drives `meson setup` against a source tree. Sister of
// converter/internal/cmakerun for the meson side.
//
// Hermeticity is the caller's responsibility — the package only
// scrubs the obvious env that would steer meson off a deterministic
// configure (LC_*/LANG/SOURCE_DATE_EPOCH/HOME).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// sourceDateEpoch is the project-wide fixed timestamp for
// deterministic configure-time outputs. Same value
// cmakerun.SourceDateEpoch uses; keeps cross-converter logs
// comparable. Unexported because mesonrun is the only consumer
// today; promote + unify with cmakerun's constant once a second
// caller appears.
const sourceDateEpoch = "1577836800"

type mesonOptions struct {
	SourceRoot string
	BuildDir   string

	// ExtraArgs are operator-supplied -Dopt=value entries (the
	// FDSDK `meson-local` slot). Passed verbatim after `meson
	// setup`. Empty for fixture runs.
	ExtraArgs []string

	Stdout io.Writer
	Stderr io.Writer
}

// runMesonSetup invokes `meson setup <build> <src>` with the
// stable env. Returns the absolute path of the meson-info dir
// the configure pass populated.
func runMesonSetup(ctx context.Context, opts mesonOptions) (string, error) {
	if opts.SourceRoot == "" || opts.BuildDir == "" {
		return "", fmt.Errorf("mesonrun: SourceRoot and BuildDir required")
	}
	if err := os.MkdirAll(opts.BuildDir, 0o755); err != nil {
		return "", fmt.Errorf("mesonrun: mkdir build dir: %w", err)
	}

	homeDir, err := os.MkdirTemp("", "mesonrun-home-*")
	if err != nil {
		return "", fmt.Errorf("mesonrun: stage home: %w", err)
	}
	defer os.RemoveAll(homeDir)

	argv := []string{"setup", opts.BuildDir, opts.SourceRoot}
	argv = append(argv, opts.ExtraArgs...)

	cmd := exec.CommandContext(ctx, "meson", argv...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	cmd.Env = mesonEnv(homeDir)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mesonrun: meson setup failed: %w", err)
	}
	infoDir := filepath.Join(opts.BuildDir, "meson-info")
	if _, err := os.Stat(infoDir); err != nil {
		return "", fmt.Errorf("mesonrun: meson-info missing after setup: %w", err)
	}
	return infoDir, nil
}

func mesonEnv(homeDir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homeDir,
		"LC_ALL=C",
		"LANG=C",
		"SOURCE_DATE_EPOCH=" + sourceDateEpoch,
	}
}
