package main

// Multi-platform manifest loader for the per-platform fold of
// round-2 trace-driven kinds (and, in follow-ups, kind:cmake
// Phase B and kind:meson Phase B).
//
// Format mirrors orchestrator/internal/orchestrator/multiplatform.go's
// platformsManifestEntry, minus the reapi_properties field: write-a
// runs at render time and emits Bazel BUILD files, so it doesn't
// route REAPI Actions per platform — the platforms manifest's
// REAPIProperties slice only matters to the orchestrator's
// per-platform convert-element Action fan-out (kind:cmake Phase A).
// A single platforms.json on disk serves both consumers; each
// reads the fields it cares about and ignores the rest.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/elementfold"
)

// tracePlatform is the per-platform record write-a's multi-
// platform mode threads through to the trace-driven handlers.
// Each entry drives one converter genrule + one trace_repo
// instance in project A — the project-A render fans out per
// platform and a fold-element genrule composes the per-platform
// ir.json outputs. Project B's install-genrule fan-out is a
// queued follow-up; today every entry shares the single install
// genrule project B already emits, so multi-platform mode is
// render-shape complete on the A side but at runtime publishes
// only one platform's trace.
type tracePlatform struct {
	// Name is the platform identifier. Used as the URL-safe
	// suffix on derived names: "trace_<elem>__<name>" for the
	// per-platform _trace_repo, "<elem>/<name>/ir.json" for the
	// per-platform converter output, "<name>" as the SelectKey
	// constraint axis for fold-element when SelectLabel is
	// empty.
	Name string

	// Constraints are the Bazel constraint_value labels (e.g.
	// "@platforms//os:linux") declared on the platform() rule
	// in the operator's //platforms package. Used to derive
	// the SelectKey via elementfold.PickSelectKeys.
	Constraints []string

	// SelectLabel, when non-empty, is the operator-declared
	// config_setting label that overrides PickSelectKeys'
	// auto-detection — the escalation path for matrices where
	// no single constraint axis uniquely identifies each
	// platform (e.g. {linux_x86_64, linux_aarch64, darwin_arm64}).
	SelectLabel string

	// SelectKey is the resolved select() arm label this platform
	// uses in rendered Bazel select() blocks (project A's fold,
	// project B's install_tree.tar filegroup). Populated by
	// loadPlatformsManifest via elementfold.PickSelectKeys after
	// the matrix is loaded, so consumers can read it directly
	// without re-validating. The same algorithm runs at fold-
	// element invocation time on the project A side, so both
	// consumers pick matching labels for the same matrix.
	SelectKey string
}

// platformsManifestEntry mirrors the on-disk JSON shape.
// reapi_properties is read but discarded — orchestrator
// consumes it, write-a doesn't. A field-mismatch would
// fail unmarshalling; absent the field is fine.
type platformsManifestEntry struct {
	Name        string   `json:"name"`
	Constraints []string `json:"constraints"`
	SelectLabel string   `json:"select_label,omitempty"`
	// REAPIProperties intentionally untyped: write-a doesn't
	// route REAPI Actions, so json.RawMessage absorbs whatever
	// shape the operator declared without us having to model it.
	REAPIProperties json.RawMessage `json:"reapi_properties,omitempty"`
}

// loadPlatformsManifest parses the JSON manifest into a slice
// of tracePlatform. Empty path → nil slice (caller's signal
// to use the single-platform legacy render path; multi-platform
// mode requires opt-in).
//
// Validation mirrors the orchestrator's loader's checks (same
// manifest serves both consumers, so misuses should fail with
// the same diagnostics either way): platform names are URL-
// safe and unique, each platform has at least one constraint,
// constraints don't embed the ',' / '|' delimiters that fold-
// element's --cell argv parser uses. We don't validate
// REAPIProperties — that's the orchestrator's concern.
func loadPlatformsManifest(path string) ([]tracePlatform, error) {
	if path == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("write-a: read platforms manifest %s: %w", path, err)
	}
	var raw []platformsManifestEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("write-a: parse platforms manifest %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("write-a: platforms manifest %s contains no platforms", path)
	}
	seen := map[string]bool{}
	out := make([]tracePlatform, len(raw))
	for i, e := range raw {
		if err := safePlatformName(e.Name); err != nil {
			return nil, fmt.Errorf("write-a: platforms[%d] in %s: invalid name %q: %w (names become path components and embedded in derived Bazel labels like trace_<elem>__<name>; pick a short URL-safe identifier like 'linux_x86_64')", i, path, e.Name, err)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("write-a: platform %q appears twice in %s", e.Name, path)
		}
		seen[e.Name] = true
		if len(e.Constraints) == 0 {
			return nil, fmt.Errorf("write-a: platform %q in %s has no constraints", e.Name, path)
		}
		normalised := make([]string, 0, len(e.Constraints))
		seenC := map[string]bool{}
		for j, rawC := range e.Constraints {
			c := strings.TrimSpace(rawC)
			if c == "" {
				return nil, fmt.Errorf("write-a: platform %q in %s constraints[%d] is empty/whitespace", e.Name, path, j)
			}
			if strings.ContainsAny(c, ",|") {
				return nil, fmt.Errorf("write-a: platform %q in %s constraints[%d] %q contains delimiter (',' or '|') — these would break --cell argv parsing in fold-element", e.Name, path, j, c)
			}
			if seenC[c] {
				return nil, fmt.Errorf("write-a: platform %q in %s constraints lists %q twice; each constraint label must appear at most once per platform", e.Name, path, c)
			}
			seenC[c] = true
			normalised = append(normalised, c)
		}
		selectLabel := strings.TrimSpace(e.SelectLabel)
		if selectLabel != "" && strings.ContainsAny(selectLabel, ",|") {
			return nil, fmt.Errorf("write-a: platform %q in %s select_label %q contains delimiter (',' or '|') — these would break --cell argv parsing in fold-element", e.Name, path, selectLabel)
		}
		out[i] = tracePlatform{
			Name:        e.Name,
			Constraints: normalised,
			SelectLabel: selectLabel,
		}
	}
	// Pre-validate the matrix via the same select-key
	// derivation fold-element runs at invocation time, and
	// cache the resolved keys on each tracePlatform. Catches
	// ambiguous matrices ({linux_x86_64, linux_aarch64,
	// darwin_arm64} without operator-supplied select_labels)
	// + duplicate override labels at write-a startup, with the
	// clear "no constraint that uniquely identifies it"
	// diagnostic — rather than deep in render where a nil keys
	// map would surface as a degenerate select() block with
	// empty arm labels.
	if err := resolvePlatformSelectKeys(out); err != nil {
		return nil, fmt.Errorf("write-a: platforms manifest %s: %w", path, err)
	}
	return out, nil
}

// resolvePlatformSelectKeys runs elementfold.PickSelectKeys over
// a tracePlatform slice and writes the resolved label back to
// each entry's SelectKey field. Factored out of
// loadPlatformsManifest so tests that construct tracePlatform
// values directly (without going through manifest loading) can
// share the same resolution pass — the invariant "every
// tracePlatform consumed by write-a has SelectKey populated"
// holds for both production and test paths.
//
// Mutates the slice in place. Returns the error from
// PickSelectKeys unchanged (callers wrap with context).
func resolvePlatformSelectKeys(platforms []tracePlatform) error {
	if len(platforms) == 0 {
		return nil
	}
	in := make([]elementfold.Platform, len(platforms))
	for i, p := range platforms {
		in[i] = elementfold.Platform{
			Name:        p.Name,
			Constraints: p.Constraints,
			SelectKey:   p.SelectLabel,
		}
	}
	keys, err := elementfold.PickSelectKeys(in)
	if err != nil {
		return err
	}
	for i := range platforms {
		platforms[i].SelectKey = keys[platforms[i].Name]
	}
	return nil
}

// safePlatformName rejects names with characters that would
// break derived label / path forms. Same conservative rule the
// orchestrator's loader applies: letters, digits, underscore,
// hyphen, period. Non-empty. Path-traversal forms (".", "..",
// or any name containing "..") are rejected explicitly — platform
// names land as path components (e.g. "<platform>/ir.json") and
// embedded in derived Bazel repo / target names (e.g.
// "trace_<elem>__<platform>"), so a `..` substring could escape
// the per-element output dir or produce labels that confuse
// Bazel's parser.
func safePlatformName(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	if s == "." || s == ".." {
		return fmt.Errorf("reserved path component %q", s)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("contains path-traversal substring %q", "..")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return fmt.Errorf("contains disallowed character %q", r)
		}
	}
	return nil
}
