package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/reapi"
)

// resolveFoldElementBinary picks the fold-element binary to spawn.
// Resolution order: (1) bin as absolute or relative path that
// exists; (2) bin's basename next to convAbs (installs typically
// drop both binaries in the same dir); (3) exec.LookPath. Empty
// bin defaults to "fold-element". Returns the absolute path or
// an error with the locations tried.
func resolveFoldElementBinary(bin, convAbs string) (string, error) {
	if bin == "" {
		bin = "fold-element"
	}
	if strings.ContainsAny(bin, string(filepath.Separator)) {
		if _, err := os.Stat(bin); err == nil {
			return filepath.Abs(bin)
		}
		return "", fmt.Errorf("orchestrator: fold-element binary %q not found", bin)
	}
	if convAbs != "" {
		side := filepath.Join(filepath.Dir(convAbs), bin)
		if _, err := os.Stat(side); err == nil {
			return side, nil
		}
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("orchestrator: fold-element binary %q not found next to converter (%s) or on PATH; set --converter to point at an installation directory containing fold-element, or place fold-element on PATH", bin, filepath.Dir(convAbs))
	}
	return path, nil
}

// safePlatformName rejects names that would be unsafe as path
// components (`/`, `\`, `..`, control chars) or that would
// collide with the fold-element --cell flag's pipe / comma
// separators. Manifest names flow straight into
// filepath.Join(elemOut, name) and into a piped-and-comma'd
// argv string, so guard both surfaces here rather than downstream.
func safePlatformName(name string) error {
	if name == "" {
		return fmt.Errorf("empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("reserved name %q", name)
	}
	for _, r := range name {
		switch r {
		case '/', '\\', '|', ',', ':':
			return fmt.Errorf("contains separator %q", r)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("contains control character")
		}
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("contains %q", "..")
	}
	return nil
}

// convertPlatform is the orchestrator-side per-platform spec
// the multi-platform fan-out consumes. Carries the constraint
// labels (passed through to fold-element so the rendered
// BUILD.bazel's select() arms reference them), the REAPI
// Action.Platform properties (used to route per-platform
// Actions to the matching worker pool), and an optional
// operator-declared SelectLabel that overrides
// elementfold.PickSelectKeys' auto-detection — needed for
// matrices where no single constraint axis uniquely
// identifies each platform (the classic
// {linux_x86_64, linux_aarch64, darwin_arm64} shape, where
// `@platforms//os:linux` and `@platforms//cpu:arm64` each
// appear twice).
type convertPlatform struct {
	Name            string
	Constraints     []string
	SelectLabel     string
	REAPIProperties []reapi.PlatformProperty
}

// platformsManifestEntry mirrors the JSON shape on disk.
// SelectLabel ("select_label") is optional; when set, the
// operator commits to declaring a `config_setting` with that
// label in their //platforms package, and the rendered
// select() in the per-element BUILD.bazel keys arms on it
// instead of on a raw constraint_value. When unset across all
// platforms the auto-detection path (single varying axis) is
// the only contract; mixing some-set / some-unset is fine —
// the ones with select_label use it, the ones without
// auto-detect from constraints.
type platformsManifestEntry struct {
	Name            string                   `json:"name"`
	Constraints     []string                 `json:"constraints"`
	SelectLabel     string                   `json:"select_label,omitempty"`
	REAPIProperties []reapi.PlatformProperty `json:"reapi_properties"`
}

// loadPlatformsManifest parses the JSON manifest into
// convertPlatform values. Empty path → nil matrix (caller's
// signal to use the single-platform path).
func loadPlatformsManifest(path string) ([]convertPlatform, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: read platforms manifest %s: %w", path, err)
	}
	var raw []platformsManifestEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("orchestrator: parse platforms manifest %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("orchestrator: platforms manifest %s contains no platforms", path)
	}
	seen := map[string]bool{}
	out := make([]convertPlatform, len(raw))
	for i, e := range raw {
		if err := safePlatformName(e.Name); err != nil {
			return nil, fmt.Errorf("orchestrator: platforms[%d] in %s: invalid name %q: %w (names become path components and are embedded in --cell argv where %q is a delimiter; pick a name like 'linux_x86_64')", i, path, e.Name, err, "|")
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("orchestrator: platform %q appears twice in %s", e.Name, path)
		}
		seen[e.Name] = true
		if len(e.Constraints) == 0 {
			return nil, fmt.Errorf("orchestrator: platform %q in %s has no constraints", e.Name, path)
		}
		// Normalise + dedupe constraints so a manifest with
		// trailing whitespace or an accidentally-repeated label
		// fails fast with a targeted message rather than at
		// PickSelectKeys' ambiguity check downstream. Constraints
		// flow into the fold-element --cell argv (comma-joined)
		// and into PickSelectKeys' uniqueness count, so both
		// surfaces benefit from clean input.
		normalised := make([]string, 0, len(e.Constraints))
		seenC := map[string]bool{}
		for j, raw := range e.Constraints {
			c := strings.TrimSpace(raw)
			if c == "" {
				return nil, fmt.Errorf("orchestrator: platform %q in %s constraints[%d] is empty/whitespace", e.Name, path, j)
			}
			if strings.ContainsAny(c, ",|") {
				return nil, fmt.Errorf("orchestrator: platform %q in %s constraints[%d] %q contains delimiter (',' or '|') — these would break --cell argv parsing", e.Name, path, j, c)
			}
			if seenC[c] {
				return nil, fmt.Errorf("orchestrator: platform %q in %s constraints lists %q twice; each constraint label must appear at most once per platform", e.Name, path, c)
			}
			seenC[c] = true
			normalised = append(normalised, c)
		}
		if len(e.REAPIProperties) == 0 {
			return nil, fmt.Errorf("orchestrator: platform %q in %s has no reapi_properties; declare them explicitly so per-platform Actions route to the matching worker pool", e.Name, path)
		}
		selectLabel := strings.TrimSpace(e.SelectLabel)
		if selectLabel != "" {
			if strings.ContainsAny(selectLabel, ",|") {
				return nil, fmt.Errorf("orchestrator: platform %q in %s select_label %q contains delimiter (',' or '|') — these would break --cell argv parsing", e.Name, path, selectLabel)
			}
		}
		out[i] = convertPlatform{
			Name:            e.Name,
			Constraints:     normalised,
			SelectLabel:     selectLabel,
			REAPIProperties: e.REAPIProperties,
		}
	}
	return out, nil
}

// runConvertActions dispatches to either the multi-platform
// fan-out or the single-platform reapi.Build / AC / exec flow,
// depending on whether r.platformsMatrix is set. Both paths
// land their canonical outputs at elemOut/{BUILD.bazel,
// cmake-config, read_paths.json} so the shared depRecord-
// stitching tail in processElement consumes them uniformly.
//
// Returns (true, nil) when a Tier-1 failure was appended (the
// caller's processElement returns nil and downstream
// dependents see it via firstFailedDep). Returns a non-nil err
// for Tier-2/3 conditions.
func (r *runner) runConvertActions(
	ctx context.Context,
	name, realSrcRoot, elemOut, shadowSrc, importsPath, prefixPath string,
) (failed bool, err error) {
	if len(r.platformsMatrix) > 0 {
		if err := r.processElementMultiPlatform(ctx, name, realSrcRoot, elemOut, shadowSrc, importsPath, prefixPath); err != nil {
			return false, err
		}
		// Tier-1 failures during the fan-out / fold are
		// recorded as FailureRecords on r.res; surface
		// "failed" to the caller so it skips the depRecord
		// tail.
		r.mu.Lock()
		for _, f := range r.res.Failed {
			if f.Element == name {
				r.mu.Unlock()
				return true, nil
			}
		}
		r.mu.Unlock()
		return false, nil
	}

	// Single-platform path: original reapi.Build / AC / exec
	// flow, byte-identical to today's behaviour.
	built, err := reapi.Build(reapi.Inputs{
		ShadowDir:              shadowSrc,
		ImportsManifest:        importsPath,
		PrefixDir:              prefixPath,
		ToolchainCMakeFile:     r.opts.ToolchainCMakeFile,
		ConverterBin:           r.convAbs,
		Platform:               r.platform,
		Timeout:                r.timeout,
		CollectToolchainSignal: r.opts.CollectToolchainSignal,
		EnvVars: map[string]string{
			"ORCHESTRATOR_ELEMENT_NAME": name,
		},
	})
	if err != nil {
		return false, fmt.Errorf("element %s: build action: %w", name, err)
	}
	hit, fr, err := tryActionCacheHit(ctx, r.store, built, elemOut)
	if err != nil {
		return false, fmt.Errorf("element %s: action cache lookup: %w", name, err)
	}
	if hit {
		r.logger.Info("cache hit", "name", name, "action_digest", built.ActionDigest.Hash)
		r.appendCacheHit(name)
	} else {
		if err := os.RemoveAll(elemOut); err != nil {
			return false, fmt.Errorf("element %s: clear elemOut: %w", name, err)
		}
		fr, err = remoteExecute(ctx, r.store, r.executor, built, elemOut, name, logOf(r.opts))
		if err != nil {
			return false, err
		}
		if fr == nil {
			r.appendCacheMiss(name)
		}
	}
	if fr != nil {
		r.appendFailure(*fr)
		return true, nil
	}
	return false, nil
}

// processElementMultiPlatform fans out one (element, platform)
// REAPI Action per platform in the matrix, submits all,
// collects per-platform IR JSONs, then spawns the fold-element
// binary to compose them into one unified BUILD.bazel via
// elementfold + bazel.Emit.
//
// Per-platform outputs land at <elemOut>/<platform>/. The
// fold-element binary reads each platform's ir.json and writes
// the unified BUILD.bazel directly to elemOut. After the fold,
// canonical bundle / read_paths come from the FIRST platform's
// Action output via stageCanonicalArtifacts — bundle contents
// (synth-prefix's <Pkg>Config.cmake) are platform-agnostic for
// the things they carry.
//
// On per-platform failure: the first failed cell's
// FailureRecord is recorded and the function returns nil (the
// orchestrator's mainline treats Tier-1 as non-fatal).
func (r *runner) processElementMultiPlatform(
	ctx context.Context,
	name, realSrcRoot, elemOut, shadowSrc, importsPath, prefixPath string,
) error {
	cells := make([]foldCell, 0, len(r.platformsMatrix))
	var firstSuccessElemOut string
	hits := 0
	for _, p := range r.platformsMatrix {
		platOut := filepath.Join(elemOut, p.Name)
		built, err := reapi.Build(reapi.Inputs{
			ShadowDir:              shadowSrc,
			ImportsManifest:        importsPath,
			PrefixDir:              prefixPath,
			ToolchainCMakeFile:     r.opts.ToolchainCMakeFile,
			ConverterBin:           r.convAbs,
			Platform:               p.REAPIProperties,
			Timeout:                r.timeout,
			CollectToolchainSignal: r.opts.CollectToolchainSignal,
			EmitIRJSON:             true,
			EnvVars: map[string]string{
				"ORCHESTRATOR_ELEMENT_NAME":    name,
				"ORCHESTRATOR_TARGET_PLATFORM": p.Name,
			},
		})
		if err != nil {
			return fmt.Errorf("element %s platform %s: build action: %w", name, p.Name, err)
		}
		hit, fr, err := tryActionCacheHit(ctx, r.store, built, platOut)
		if err != nil {
			return fmt.Errorf("element %s platform %s: action cache lookup: %w", name, p.Name, err)
		}
		if hit {
			r.logger.Info("cache hit", "name", name, "platform", p.Name, "action_digest", built.ActionDigest.Hash)
			hits++
		} else {
			if err := os.RemoveAll(platOut); err != nil {
				return fmt.Errorf("element %s platform %s: clear platOut: %w", name, p.Name, err)
			}
			fr, err = remoteExecute(ctx, r.store, r.executor, built, platOut, name+"["+p.Name+"]", logOf(r.opts))
			if err != nil {
				return err
			}
		}
		if fr != nil {
			r.appendFailure(*fr)
			return nil
		}
		cells = append(cells, foldCell{
			platform:   p,
			irJSONPath: filepath.Join(platOut, "ir.json"),
		})
		if firstSuccessElemOut == "" {
			firstSuccessElemOut = platOut
		}
	}

	if len(cells) == 0 {
		return nil
	}

	// Spawn fold-element to read per-platform IRs and emit the
	// unified BUILD.bazel. The fold logic lives inside the
	// converter's internal packages (elementfold, ir,
	// emit/bazel); the orchestrator can't import them
	// directly, so we shell out — same pattern the orchestrator
	// uses for convert-element itself.
	if err := r.runFoldElement(ctx, name, elemOut, cells); err != nil {
		// Fold-time errors (target shape diverges in a way
		// select() can't express; an ambiguous matrix; etc.)
		// surface as Tier-1: the operator can address them by
		// reducing the matrix, declaring config_setting rules,
		// or making the divergent target's shape uniform.
		r.appendFailure(FailureRecord{
			Element: name,
			Tier:    1,
			Code:    "elementfold-failed",
			Message: err.Error(),
		})
		return nil
	}

	// Stage canonical bundle / read_paths from the first
	// successful platform so the existing depRecord stitching
	// code (which reads elemOut/cmake-config and elemOut/
	// read_paths.json) keeps working without per-platform
	// awareness.
	if err := stageCanonicalArtifacts(elemOut, firstSuccessElemOut); err != nil {
		return fmt.Errorf("element %s: stage canonical artifacts: %w", name, err)
	}

	// Cache accounting at the element level: a multi-platform
	// element is "hit" iff every per-platform Action hit (so no
	// remote execution and no fold-side work was strictly
	// avoidable); otherwise at least one platform exercised the
	// executor, so record a miss. Per-platform hit counts land in
	// the structured log above for finer-grained dashboards.
	if hits == len(cells) {
		r.appendCacheHit(name)
	} else {
		r.appendCacheMiss(name)
	}
	return nil
}

// foldCell is one (platform, IR-path) pair the fold-element
// binary consumes via its --cell flag.
type foldCell struct {
	platform   convertPlatform
	irJSONPath string
}

// runFoldElement invokes the fold-element binary with one
// --cell flag per platform. The binary reads each ir.json,
// folds via elementfold, emits via bazel.Emit, and writes the
// unified BUILD.bazel to elemOut/BUILD.bazel.
func (r *runner) runFoldElement(ctx context.Context, name, elemOut string, cells []foldCell) error {
	args := []string{"--out-build", filepath.Join(elemOut, "BUILD.bazel")}
	for _, c := range cells {
		// --cell <name>|<constraint1,constraint2,...>|<irJSONPath>[|<select_label>]
		// Pipe separator: Bazel constraint labels embed colons
		// (@platforms//os:linux), so a colon-separated layout
		// would collide. The optional 4th field is the operator-
		// declared config_setting label that overrides the
		// auto-detected SelectKey when set.
		constraintsCSV := strings.Join(c.platform.Constraints, ",")
		raw := c.platform.Name + "|" + constraintsCSV + "|" + c.irJSONPath
		if c.platform.SelectLabel != "" {
			raw += "|" + c.platform.SelectLabel
		}
		args = append(args, "--cell", raw)
	}
	bin := r.foldElementAbs
	if bin == "" {
		// Defensive: runConvertActions only reaches the
		// multi-platform path when platformsMatrix is non-empty,
		// and Run() resolves foldElementAbs in that case. Surface
		// a clear error rather than fall back to a bare-name
		// PATH lookup that may pick up something unexpected.
		return fmt.Errorf("element %s: fold-element binary not resolved; this is an internal invariant", name)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = logOf(r.opts)
	cmd.Stderr = logOf(r.opts)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("element %s: fold-element: %w", name, err)
	}
	return nil
}

// stageCanonicalArtifacts wires the multi-platform aggregate
// elemOut to the first successful platform's outputs so the
// existing depRecord stitching code (which reads elemOut/
// cmake-config and elemOut/read_paths.json) keeps working
// without per-platform awareness.
func stageCanonicalArtifacts(elemOut, firstPlatOut string) error {
	for _, name := range []string{"cmake-config", "read_paths.json"} {
		dst := filepath.Join(elemOut, name)
		src := filepath.Join(firstPlatOut, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		rel, err := filepath.Rel(elemOut, src)
		if err != nil {
			return err
		}
		if err := os.Symlink(rel, dst); err != nil {
			return err
		}
	}
	return nil
}
