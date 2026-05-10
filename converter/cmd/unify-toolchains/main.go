// unify-toolchains reads probe.json artifacts produced by project A's
// per-cell genrules and writes the standard multi-platform Bazel
// toolchain layout into the operator's repo:
//
//	platforms/BUILD.bazel
//	toolchains/BUILD.bazel
//	toolchains/cc_toolchain_config.bzl
//	.bazelrc       (operator overrides via try-import user.bazelrc)
//
// MODULE.bazel is intentionally NOT touched. On first run, if it
// lacks `register_toolchains("//toolchains:all")`, the tool prints
// a one-time setup banner. After that, all toolchain churn happens
// inside //toolchains/BUILD.bazel — MODULE.bazel never needs
// re-editing as platforms / kits / sanitizers evolve.
//
// Inputs:
//
//	--probe-cells <dir>     Directory of probe.json artifacts. Names
//	                        match render-project-a's cell labels:
//	                        `<platform>.<variant>.probe.json`.
//	--platforms-json <path> Same shape as render-project-a consumes
//	                        (so the platform identity is consistent
//	                        across the matrix and the unifier).
//	--repo-root <dir>       Operator's repo root. Tool-owned files
//	                        land at the four known paths above.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/emit/bazeltoolchain"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain/probejson"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "unify-toolchains:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("unify-toolchains", flag.ContinueOnError)
	var (
		probeCells    = fs.String("probe-cells", "", "directory of <platform>.<variant>.probe.json artifacts")
		platformsJSON = fs.String("platforms-json", "", "JSON manifest pairing platform names with constraint_value labels")
		repoRoot      = fs.String("repo-root", "", "operator's repo root; tool writes platforms/, toolchains/, .bazelrc")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *probeCells == "" {
		return fmt.Errorf("--probe-cells is required")
	}
	if *platformsJSON == "" {
		return fmt.Errorf("--platforms-json is required")
	}
	if *repoRoot == "" {
		return fmt.Errorf("--repo-root is required")
	}

	plats, err := loadPlatforms(*platformsJSON)
	if err != nil {
		return fmt.Errorf("load platforms: %w", err)
	}

	cellsByPlatform, err := groupProbeCells(*probeCells, plats)
	if err != nil {
		return fmt.Errorf("group probe cells: %w", err)
	}

	// Per platform, fold the variant cells through Observe to get
	// a ResolvedToolchain. Drop platforms that produced zero cells
	// (with a diagnostic) — they'd render an empty cc_toolchain that
	// wouldn't compile anything.
	var ptcs []bazeltoolchain.PlatformToolchain
	for _, p := range plats {
		results := cellsByPlatform[p.Name]
		if len(results) == 0 {
			fmt.Fprintf(os.Stderr, "unify-toolchains: warning: no probe cells found for platform %q; skipping\n", p.Name)
			continue
		}
		rt := toolchain.Observe(results)
		if rt == nil {
			fmt.Fprintf(os.Stderr, "unify-toolchains: warning: Observe returned nil for platform %q\n", p.Name)
			continue
		}
		ptcs = append(ptcs, bazeltoolchain.PlatformToolchain{
			Name:        p.Name,
			Constraints: p.Constraints,
			Resolved:    rt,
		})
	}
	if len(ptcs) == 0 {
		return fmt.Errorf("no platforms produced probe cells; aborting")
	}

	bundle, err := bazeltoolchain.EmitUnified(ptcs, bazeltoolchain.UnifiedConfig{})
	if err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	for relPath, body := range bundle.Files {
		full := filepath.Join(*repoRoot, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}
	}

	if err := maybePrintSetupBanner(*repoRoot); err != nil {
		// Best-effort; don't fail the whole run.
		fmt.Fprintf(os.Stderr, "unify-toolchains: setup-banner check: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "unify-toolchains: wrote %d files for %d platforms\n", len(bundle.Files), len(ptcs))
	return nil
}

type platformSpec struct {
	Name        string   `json:"name"`
	Constraints []string `json:"constraints"`
}

func loadPlatforms(path string) ([]platformSpec, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []platformSpec
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no platforms in %s", path)
	}
	// Validate each platform spec up front. The probe-cell filename
	// convention (<platform>.<variant>.probe.json) and the per-
	// platform fold both assume non-empty names from the
	// Bazel-target-safe charset, with non-empty constraints; an
	// invalid spec here would surface later as misleading "no
	// probe cells found" warnings or as Bazel parse errors on the
	// generated platforms/ + toolchains/ files.
	seen := map[string]bool{}
	for i, p := range out {
		if p.Name == "" {
			return nil, fmt.Errorf("platforms[%d] in %s has empty name", i, path)
		}
		if err := checkBazelTargetSafeName(p.Name); err != nil {
			return nil, fmt.Errorf("platforms[%d].name %q in %s: %w", i, p.Name, path, err)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("platforms[].name %q appears twice in %s", p.Name, path)
		}
		seen[p.Name] = true
		if len(p.Constraints) == 0 {
			return nil, fmt.Errorf("platforms[%d] (%s) in %s has no constraints", i, p.Name, path)
		}
	}
	return out, nil
}

// checkBazelTargetSafeName mirrors projecta's name-charset check
// (the imported package keeps it private to avoid widening the
// surface; small enough to duplicate cheaply here). Platform
// names from --platforms-json become Bazel target names
// (platform() rules, cc_toolchain target prefixes), .bazelrc
// --config aliases, and per-cell probe artifact filename halves;
// anything outside [a-zA-Z0-9_-] would produce broken BUILD
// files or ambiguous artifacts. '.' rejected explicitly because
// groupProbeCells uses it as the <platform>.<variant> filename
// separator.
func checkBazelTargetSafeName(name string) error {
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("contains %q; allowed: [a-zA-Z0-9_-]", r)
		}
	}
	return nil
}

// groupProbeCells walks <probeCellsDir> and groups the
// <platform>.<variant>.probe.json files by platform. Decoded
// ProbeJSON documents become toolchain.ProbeResult values for
// Observe consumption.
func groupProbeCells(dir string, plats []platformSpec) (map[string][]toolchain.ProbeResult, error) {
	platSet := make(map[string]bool, len(plats))
	for _, p := range plats {
		platSet[p.Name] = true
	}

	out := map[string][]toolchain.ProbeResult{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".probe.json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".probe.json")
		dot := strings.IndexByte(stem, '.')
		if dot <= 0 {
			return nil, fmt.Errorf("probe cell %q does not match <platform>.<variant>.probe.json", name)
		}
		platName := stem[:dot]
		if !platSet[platName] {
			fmt.Fprintf(os.Stderr, "unify-toolchains: warning: probe cell %q references platform %q not in --platforms-json; skipping\n", name, platName)
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		variant, reply, err := probejson.Unmarshal(body)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		m, err := toolchain.FromReply(reply)
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", name, err)
		}
		out[platName] = append(out[platName], toolchain.ProbeResult{
			Variant: variant,
			Model:   m,
			Reply:   reply,
		})
	}

	// Stable per-platform variant order: by Variant.Name.
	for _, results := range out {
		sort.Slice(results, func(i, j int) bool {
			return results[i].Variant.Name < results[j].Variant.Name
		})
	}
	return out, nil
}

// maybePrintSetupBanner reads the operator's MODULE.bazel and, if
// it lacks `register_toolchains("//toolchains:all")`, emits a one-
// time instruction. Doesn't modify MODULE.bazel itself.
func maybePrintSetupBanner(repoRoot string) error {
	path := filepath.Join(repoRoot, "MODULE.bazel")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "  unify-toolchains: MODULE.bazel not found at "+path)
			fmt.Fprintln(os.Stderr, "  Add `register_toolchains(\"//toolchains:all\")` to your MODULE.bazel")
			fmt.Fprintln(os.Stderr, "  to activate the generated toolchains.")
			fmt.Fprintln(os.Stderr, "")
			return nil
		}
		return err
	}
	if !registerToolchainsCallPresent(body) {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  unify-toolchains: ONE-TIME SETUP")
		fmt.Fprintln(os.Stderr, "  Your MODULE.bazel does not yet register the generated toolchains.")
		fmt.Fprintln(os.Stderr, "  Add this line:")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "      register_toolchains(\"//toolchains:all\")")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  Subsequent runs of unify-toolchains regenerate platforms/, toolchains/,")
		fmt.Fprintln(os.Stderr, "  and .bazelrc — MODULE.bazel never needs editing again.")
		fmt.Fprintln(os.Stderr, "")
	}
	return nil
}

// registerToolchainsRE matches a `register_toolchains` call
// referencing //toolchains:all. The (?m) anchor pins the call to
// the start of a (possibly indented) line so an inline comment
// like `module()  # register_toolchains("//toolchains:all")` does
// NOT match — only a register_toolchains identifier that begins
// its own logical line counts. Inside the parens we allow
// whitespace, newlines, and additional args before/after the
// label.
var registerToolchainsRE = regexp.MustCompile(`(?m)^[ \t]*register_toolchains\s*\([^)]*['"]//toolchains:all['"]`)

// commentedLineRE matches a Starlark comment line — optional
// leading whitespace, then `#`, then the rest of the line. Used
// by registerToolchainsCallPresent to strip fully-commented
// lines so a `# register_toolchains("//toolchains:all")` snippet
// in MODULE.bazel doesn't suppress the setup banner.
var commentedLineRE = regexp.MustCompile(`(?m)^[ \t]*#.*$`)

// registerToolchainsCallPresent reports whether body contains a
// register_toolchains call that references //toolchains:all. The
// match is tolerant of whitespace, newlines, and quote-style so
// the setup banner doesn't re-fire on a reformatted MODULE.bazel.
// Fully-commented lines are stripped before the regexp runs so
// `# register_toolchains("//toolchains:all")` snippets in the
// file don't suppress the banner. Inline comments after a real
// call (`register_toolchains("//toolchains:all") # ok`) still
// match because the call itself precedes the `#`.
func registerToolchainsCallPresent(body []byte) bool {
	stripped := commentedLineRE.ReplaceAll(body, nil)
	return registerToolchainsRE.Match(stripped)
}
