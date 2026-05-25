package cmakerun

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// InstallOptions drives a post-Configure cmake build + install
// sequence. Phase 6 of the generator-parity uplift (ROADMAP.md)
// uses this to materialize install(EXPORT) bundles at convert
// time when the exportshape classifier says the bundle is
// declarative (safe to pre-resolve).
//
// The shape mirrors cmakerun.Options enough that callers can
// pass build-dir / prefix-dir / stdout / stderr without
// duplicating env setup. BuildDir must point at the same
// directory Configure populated.
type InstallOptions struct {
	// BuildDir is the cmake build directory Configure was run
	// against. Required.
	BuildDir string

	// InstallPrefix is the target prefix passed to
	// `cmake --install --prefix <here>`. Required. The directory
	// is created if it doesn't exist; existing contents are NOT
	// cleared (callers stage a fresh scratch dir if they need
	// isolation).
	InstallPrefix string

	// Component, when non-empty, restricts the install to the
	// named cmake install component (--component <name>). Empty
	// installs all default components.
	Component string

	// BuildType selects the configuration to install (cmake
	// `--config <type>`). Defaults to "Release". For multi-config
	// builds (BuildTypes passed to Configure), pass one of the
	// configured types here.
	BuildType string

	// Stdout, Stderr capture ninja's and cmake --install's output.
	// Nil discards.
	Stdout, Stderr io.Writer
}

// BuildAndInstall runs `cmake --build <buildDir>` followed by
// `cmake --install <buildDir> --prefix <installPrefix>`. Returns
// nil on success or a typed error describing which step failed.
//
// The build step always runs before install — cmake refuses to
// install a project that hasn't been built (the install rules
// reference build outputs by path). Callers that want install-only
// from an already-built tree can skip this helper and shell out
// directly.
//
// Phase 6 acceptance: the function gates on the exportshape
// classifier's verdict at the caller (convert-element-cmake's
// main); imperative bundles take the round-2 fallback path
// instead of going through here.
func BuildAndInstall(ctx context.Context, opts InstallOptions) error {
	if opts.BuildDir == "" {
		return fmt.Errorf("cmakerun: InstallOptions.BuildDir required")
	}
	if opts.InstallPrefix == "" {
		return fmt.Errorf("cmakerun: InstallOptions.InstallPrefix required")
	}
	if opts.BuildType == "" {
		opts.BuildType = "Release"
	}
	if err := os.MkdirAll(opts.InstallPrefix, 0o755); err != nil {
		return fmt.Errorf("cmakerun: prepare install prefix: %w", err)
	}

	buildArgv := []string{"--build", opts.BuildDir, "--config", opts.BuildType}
	build := exec.CommandContext(ctx, "cmake", buildArgv...)
	build.Stdout = opts.Stdout
	build.Stderr = opts.Stderr
	build.Env = installEnv()
	if err := build.Run(); err != nil {
		return fmt.Errorf("cmakerun: cmake --build %s: %w", opts.BuildDir, err)
	}

	installArgv := []string{
		"--install", opts.BuildDir,
		"--prefix", opts.InstallPrefix,
		"--config", opts.BuildType,
	}
	if opts.Component != "" {
		installArgv = append(installArgv, "--component", opts.Component)
	}
	install := exec.CommandContext(ctx, "cmake", installArgv...)
	install.Stdout = opts.Stdout
	install.Stderr = opts.Stderr
	install.Env = installEnv()
	if err := install.Run(); err != nil {
		return fmt.Errorf("cmakerun: cmake --install %s --prefix %s: %w",
			opts.BuildDir, opts.InstallPrefix, err)
	}
	return nil
}

// installEnv produces the env cmake --build / --install run
// under. Mirrors configureEnv's reproducibility knobs (fixed
// locale + SOURCE_DATE_EPOCH) so install-time outputs are
// byte-stable across machines.
func installEnv() []string {
	env := []string{
		"LANG=C",
		"LC_ALL=C",
		"SOURCE_DATE_EPOCH=" + SourceDateEpoch,
	}
	// Pass through PATH so cmake can find ninja / make / the
	// compiler driver. Empty PATH would break the install.
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	return env
}

// WalkInstallPrefix returns every file under prefix in
// deterministic (sorted, slash-form) order. Used by the
// install(EXPORT) consumer to enumerate the materialized bundle's
// contents for cc_import / pkg_files emission.
//
// Symlinks are followed for size and mode metadata but recorded
// as their original relative path under prefix. Directories are
// not returned (the file list alone is enough for the consumer's
// shape).
func WalkInstallPrefix(prefix string) ([]string, error) {
	var out []string
	err := filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(prefix, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cmakerun: walk install prefix %s: %w", prefix, err)
	}
	// filepath.Walk visits in lexical order already; an explicit
	// sort would be a no-op. Documented here so a future change
	// to the walker preserves the contract.
	return out, nil
}
