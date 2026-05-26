package main

// Multi-platform manifest loader for the per-platform fold of
// round-2 trace-driven kinds (and, in follow-ups, kind:cmake
// Phase B and kind:meson Phase B).
//
// The manifest carries an optional reapi_properties field per
// platform — the REAPI Platform.properties wire shape, a list of
// {name, value} pairs. write-a maps each pair onto a Bazel
// exec_properties dict entry and emits a platform() rule per
// declared platform into project A's //platforms package
// (renderPlatformsBuild). The legacy orchestrator consumed
// reapi_properties to fan out per-platform REAPI Actions directly;
// the write-a + Bazel path instead lets Bazel route the per-element
// converter genrules — each already carries exec_compatible_with =
// <constraints> — to the matching execution platform, where the
// action inherits exec_properties and so selects the right Buildbarn
// worker pool — the routing the (now-deleted) orchestrator's
// reapi_properties handled in-process.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/elementfold"
)

// tracePlatform is the per-platform record write-a's multi-
// platform mode threads through to the trace-driven handlers.
// Each entry drives one converter genrule + one trace_repo
// instance in project A — the project-A render fans out per
// platform and a fold-element genrule composes the per-platform
// ir.json outputs — and one install genrule + one
// install_tree.tar select() arm in project B for kinds that
// went through the per-platform install fan-out. The fan-out
// covers pipelineHandler kinds (kind:make / manual / script /
// makemaker / modulebuild), kind:autotools (RenderB routes
// through renderPipelineRound2B), and kind:cmake Phase B
// fallback (renderCmakeRound2B), so every cc-emitting trace-
// driven kind participates today. kind:meson Phase B is the
// last queued bullet under ROADMAP Next ("Per-platform fold
// for round-2 trace-driven kinds") — pending the Phase B
// landing itself.
type tracePlatform struct {
	// Name is the platform identifier. Used as the URL-safe
	// suffix on derived names — "trace_<elem>__<name>" for the
	// per-platform _trace_repo, "<elem>/<name>/ir.json" for the
	// per-platform converter output, "<name>" as the
	// project-B install genrule's NameSuffix /
	// OutputPrefix — and nothing else. The select() arm label
	// for this platform is SelectKey, derived from Constraints
	// + SelectLabel by resolvePlatformSelectKeys at
	// loadPlatformsManifest time.
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

	// ExecProperties is the platform's reapi_properties mapped onto
	// a Bazel exec_properties dict — each {name, value} pair becomes
	// one entry. write-a emits it on the //platforms:<Name>
	// platform() rule in project A; when the per-element converter
	// genrules run on a Buildbarn cluster via --remote_executor,
	// Bazel routes each genrule (via its exec_compatible_with
	// constraint set) to the matching execution platform and the
	// action inherits these properties — selecting the worker pool.
	// Nil/empty when the manifest entry declared no reapi_properties.
	ExecProperties map[string]string
}

// reapiProperty is one entry of a platform's reapi_properties list —
// the REAPI Platform.properties wire shape.
type reapiProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// platformsManifestEntry mirrors the on-disk JSON shape.
type platformsManifestEntry struct {
	Name            string          `json:"name"`
	Constraints     []string        `json:"constraints"`
	SelectLabel     string          `json:"select_label,omitempty"`
	REAPIProperties []reapiProperty `json:"reapi_properties,omitempty"`
}

// loadPlatformsManifest parses the JSON manifest into a slice
// of tracePlatform. Empty path → nil slice (caller's signal
// to use the single-platform legacy render path; multi-platform
// mode requires opt-in).
//
// Validation: platform names are URL-safe and unique, each
// platform has at least one constraint, constraints don't embed
// the ',' / '|' delimiters that fold-element's --cell argv parser
// uses, and each platform's reapi_properties map cleanly onto an
// exec_properties dict (no empty or repeated property name).
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
		execProps, err := reapiToExecProperties(e.REAPIProperties)
		if err != nil {
			return nil, fmt.Errorf("write-a: platform %q in %s: %w", e.Name, path, err)
		}
		out[i] = tracePlatform{
			Name:           e.Name,
			Constraints:    normalised,
			SelectLabel:    selectLabel,
			ExecProperties: execProps,
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

// reapiToExecProperties maps a platform's reapi_properties list onto
// a Bazel exec_properties dict. The REAPI Platform.properties wire
// shape is a list of {name, value} pairs; Bazel exec_properties is a
// string->string map, so each pair becomes one entry. REAPI tolerates
// a repeated property name (multi-valued properties); exec_properties
// can't, so a repeated — or empty — name is rejected here with a
// clear diagnostic rather than silently last-write-winning. Empty
// list → nil map (the platform() rule emits no exec_properties).
func reapiToExecProperties(props []reapiProperty) (map[string]string, error) {
	if len(props) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(props))
	for i, p := range props {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return nil, fmt.Errorf("reapi_properties[%d] has an empty name", i)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("reapi_properties lists name %q twice; Bazel exec_properties is a map and can't carry a repeated key", name)
		}
		out[name] = p.Value
	}
	return out, nil
}

// renderPlatformsBuild renders project A's //platforms/BUILD.bazel:
// one platform() rule per declared platform, carrying the platform's
// constraint_values and — when reapi_properties was declared — the
// exec_properties dict that selects its Buildbarn worker pool.
//
// The per-element converter genrules already carry
// exec_compatible_with = <constraints>; an operator who registers
// these platforms as execution platforms
// (--extra_execution_platforms) gets each genrule routed to the
// matching pool, the action inheriting that platform's
// exec_properties. Returns "" for an empty matrix — the
// single-platform render emits no //platforms package.
func renderPlatformsBuild(platforms []tracePlatform) string {
	if len(platforms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Generated by write-a. Execution platforms for the\n")
	b.WriteString("# multi-platform converter-genrule fan-out. Register them\n")
	b.WriteString("# with --extra_execution_platforms so Bazel routes each\n")
	b.WriteString("# per-platform genrule (exec_compatible_with = <constraints>)\n")
	b.WriteString("# to the matching worker pool via exec_properties.\n\n")
	for i, p := range platforms {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("platform(\n")
		fmt.Fprintf(&b, "    name = %q,\n", p.Name)
		cs := append([]string(nil), p.Constraints...)
		sort.Strings(cs)
		b.WriteString("    constraint_values = [\n")
		for _, c := range cs {
			fmt.Fprintf(&b, "        %q,\n", c)
		}
		b.WriteString("    ],\n")
		if len(p.ExecProperties) > 0 {
			keys := make([]string, 0, len(p.ExecProperties))
			for k := range p.ExecProperties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString("    exec_properties = {\n")
			for _, k := range keys {
				fmt.Fprintf(&b, "        %q: %q,\n", k, p.ExecProperties[k])
			}
			b.WriteString("    },\n")
		}
		b.WriteString("    visibility = [\"//visibility:public\"],\n")
		b.WriteString(")\n")
	}
	return b.String()
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
