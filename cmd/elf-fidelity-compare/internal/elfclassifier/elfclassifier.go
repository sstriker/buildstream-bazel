// Package elfclassifier compares the DYNAMIC-section / ABI surface of two ELF
// artifacts — a cmake-built one vs the converted-then-Bazel-built one — and
// classifies each delta benign / impactful, mirroring the symbol-set classifier
// (cmd/fidelity-compare) one tier deeper. It handles BOTH artifact kinds the
// converter produces: shared libraries (.so) and executables (PIE, which readelf
// reports as ET_DYN, and classic ET_EXEC) — the dynamic section is read the same
// way for each.
//
// The symbol-fidelity lens compares EXPORTED-SYMBOL SETS (nm) — it deliberately
// abstracts away binary structure, the right call for static archives. It
// can't, however, see the dynamic/ABI facts a symbol-NAME set doesn't express:
// the SONAME, the DT_NEEDED runtime-dependency list, symbol VERSIONING
// (.gnu.version_d nodes — the same symbol names under different version tags is
// an ABI break the nm-set compare passes clean), and DT_RPATH/DT_RUNPATH. This
// classifier extracts those via `readelf` and buckets the diff.
//
// Artifact-kind handling: DT_NEEDED and DT_RPATH/DT_RUNPATH carry a converter
// signal for BOTH libraries and executables (lost/extra runtime deps; host-leak
// hermeticity). SONAME and .gnu.version_d are library-specific — an executable
// carries neither, so those checks are graceful no-ops on an exe (empty soname
// on both sides is skipped; zero version-def nodes compare clean). Deliberate
// non-goals: an executable's .gnu.version_r (version REQUIREMENTS — which
// versioned glibc symbols it imports) and PIE-vs-ET_EXEC type are toolchain-
// determined, not converter signal, so they're not compared.
//
// Pure: it shells out to `readelf` only; the CLI wrapper
// (cmd/elf-fidelity-compare/main.go) owns file I/O and exit codes.
package elfclassifier

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// ElfInfo is the dynamic-section snapshot of one shared object.
type ElfInfo struct {
	Soname      string          // DT_SONAME ("" when absent)
	Needed      map[string]bool // DT_NEEDED entries
	Rpath       []string        // DT_RPATH path list (legacy)
	Runpath     []string        // DT_RUNPATH path list
	VersionDefs map[string]bool // .gnu.version_d node names, excluding the BASE node
}

// Report is the structured result of comparing two shared objects.
type Report struct {
	CMakeArtifact string `json:"cmake_artifact"`
	BazelArtifact string `json:"bazel_artifact"`

	// SonameMatch is the SONAME when both sides agree (the headline
	// "the ABI handle is identical" fact); "" when they differ or are absent.
	SonameMatch string `json:"soname_match,omitempty"`

	// NeededBoth is the count of DT_NEEDED entries present on both sides.
	NeededBoth int `json:"needed_both"`

	// VersionDefsBoth is the count of version-definition nodes on both sides.
	VersionDefsBoth int `json:"version_defs_both"`

	// BenignDeltas catalogs explained differences (distro-default NEEDED,
	// soname version-suffix, RUNPATH-vs-RPATH form, allowlist-suppressed).
	// Informational; doesn't gate the exit code.
	BenignDeltas []Delta `json:"benign_deltas,omitempty"`

	// ImpactfulDeltas catalogs differences that warrant investigation
	// (dropped/extra project NEEDED, soname mismatch, lost/extra version
	// node, host-leak RPATH/RUNPATH in the Bazel artifact). Exit 1 when set.
	ImpactfulDeltas []Delta `json:"impactful_deltas,omitempty"`
}

// Delta is one classified difference.
type Delta struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// HasImpactful reports whether any impactful delta remains.
func (r *Report) HasImpactful() bool { return len(r.ImpactfulDeltas) > 0 }

// FormatForOperator renders a human-readable summary for stderr.
func (r *Report) FormatForOperator() string {
	var b strings.Builder
	fmt.Fprintf(&b, "elf-fidelity-compare: %s vs %s\n", r.CMakeArtifact, r.BazelArtifact)
	if r.SonameMatch != "" {
		fmt.Fprintf(&b, "  soname (both): %s\n", r.SonameMatch)
	}
	fmt.Fprintf(&b, "  DT_NEEDED in both: %d\n", r.NeededBoth)
	fmt.Fprintf(&b, "  version-def nodes in both: %d\n", r.VersionDefsBoth)
	fmt.Fprintf(&b, "  benign deltas: %d\n", len(r.BenignDeltas))
	fmt.Fprintf(&b, "  impactful deltas: %d\n", len(r.ImpactfulDeltas))
	if len(r.ImpactfulDeltas) > 0 {
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "  IMPACTFUL DELTAS (these are bugs or new benign categories to allowlist):")
		for _, d := range r.ImpactfulDeltas {
			fmt.Fprintf(&b, "    - %s: %s\n", d.Kind, d.Detail)
		}
	}
	return b.String()
}

// Compare extracts the dynamic-section snapshots and classifies the deltas.
func Compare(cmakePath, bazelPath string, allowed Allowlist) (*Report, error) {
	if _, err := os.Stat(cmakePath); err != nil {
		return nil, fmt.Errorf("stat cmake artifact: %w", err)
	}
	if _, err := os.Stat(bazelPath); err != nil {
		return nil, fmt.Errorf("stat bazel artifact: %w", err)
	}
	ci, err := extract(cmakePath)
	if err != nil {
		return nil, fmt.Errorf("readelf cmake artifact: %w", err)
	}
	bi, err := extract(bazelPath)
	if err != nil {
		return nil, fmt.Errorf("readelf bazel artifact: %w", err)
	}

	rep := &Report{CMakeArtifact: cmakePath, BazelArtifact: bazelPath}
	rep.NeededBoth = countCommon(ci.Needed, bi.Needed)
	rep.VersionDefsBoth = countCommon(ci.VersionDefs, bi.VersionDefs)
	classifySoname(rep, ci.Soname, bi.Soname, allowed)
	classifyNeeded(rep, ci.Needed, bi.Needed, allowed)
	classifyRunpath(rep, ci, bi)
	classifyVersionDefs(rep, ci.VersionDefs, bi.VersionDefs, allowed)
	sortDeltas(rep.BenignDeltas)
	sortDeltas(rep.ImpactfulDeltas)
	return rep, nil
}

// extract reads a shared object's dynamic section + version definitions.
func extract(path string) (*ElfInfo, error) {
	info := &ElfInfo{Needed: map[string]bool{}, VersionDefs: map[string]bool{}}
	dyn, err := runReadelf("-d", path)
	if err != nil {
		return nil, err
	}
	parseDynamic(dyn, info)
	// Version definitions are optional (most libraries carry none); a readelf
	// failure here is treated as "no version info" rather than fatal, so a
	// stripped or version-less .so still compares.
	if v, vErr := runReadelf("--version-info", path); vErr == nil {
		parseVersionDefs(v, info)
	}
	return info, nil
}

func runReadelf(flag, path string) ([]byte, error) {
	cmd := exec.Command("readelf", "-W", flag, path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// parseDynamic scans `readelf -d` output for SONAME / NEEDED / RPATH / RUNPATH.
// Each entry's payload is bracketed: `(SONAME) Library soname: [libfoo.so.1]`.
func parseDynamic(buf []byte, info *ElfInfo) {
	s := bufio.NewScanner(bytes.NewReader(buf))
	for s.Scan() {
		line := s.Text()
		switch {
		case strings.Contains(line, "(SONAME)"):
			if v := bracketed(line); v != "" {
				info.Soname = v
			}
		case strings.Contains(line, "(NEEDED)"):
			if v := bracketed(line); v != "" {
				info.Needed[v] = true
			}
		case strings.Contains(line, "(RPATH)"):
			info.Rpath = append(info.Rpath, splitPathList(bracketed(line))...)
		case strings.Contains(line, "(RUNPATH)"):
			info.Runpath = append(info.Runpath, splitPathList(bracketed(line))...)
		}
	}
}

// parseVersionDefs scans `readelf --version-info` for the .gnu.version_d
// section's defined version nodes, dropping the BASE node (which restates the
// soname). Only lines inside the version_d section are considered, so version
// REQUIREMENTS (.gnu.version_r, the imported versions) aren't mistaken for
// exported ABI version nodes.
func parseVersionDefs(buf []byte, info *ElfInfo) {
	s := bufio.NewScanner(bytes.NewReader(buf))
	inDefs := false
	for s.Scan() {
		line := s.Text()
		trimmed := strings.TrimSpace(line)
		// Section headers read e.g. "Version definition section '.gnu.version_d'"
		// / "Version symbols section '.gnu.version'" / "Version needs section
		// '.gnu.version_r'". We're inside the DEFINITIONS block only for the
		// version_d header — version REQUIREMENTS (version_r) are imports, not
		// this library's exported ABI nodes.
		if strings.HasPrefix(trimmed, "Version ") && strings.Contains(trimmed, "section") {
			inDefs = strings.Contains(trimmed, ".gnu.version_d")
			continue
		}
		if !inDefs {
			continue
		}
		if strings.Contains(line, "Flags: BASE") {
			continue // the BASE node is the soname, not an ABI version tag
		}
		if i := strings.Index(line, "Name: "); i >= 0 {
			name := strings.TrimSpace(line[i+len("Name: "):])
			// A def entry can carry a trailing "  Name: parent" on a Parent
			// continuation line; take the first whitespace-delimited token.
			if f := strings.Fields(name); len(f) > 0 {
				info.VersionDefs[f[0]] = true
			}
		}
	}
}

// bracketed returns the text inside the LAST [...] on a line, or "".
func bracketed(line string) string {
	open := strings.LastIndexByte(line, '[')
	close := strings.LastIndexByte(line, ']')
	if open < 0 || close <= open {
		return ""
	}
	return strings.TrimSpace(line[open+1 : close])
}

// splitPathList splits a colon-separated RPATH/RUNPATH value into entries.
func splitPathList(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ":") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// classifySoname compares the two SONAMEs. Equal → SonameMatch. A pure
// version-suffix difference (libfoo.so.1 vs libfoo.so.1.2.3) is benign; a base
// mismatch, or a missing soname on the Bazel side, is impactful (a consumer
// links against the soname, so the wrong handle breaks the runtime link).
func classifySoname(rep *Report, cmake, bazel string, allowed Allowlist) {
	switch {
	case cmake == "" && bazel == "":
		return
	case cmake == bazel:
		rep.SonameMatch = cmake
	case allowed.Match(bazel) || allowed.Match(cmake):
		rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "soname-allowlist-suppressed", Detail: cmake + " -> " + bazel})
	case bazel == "":
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "soname-missing-in-bazel", Detail: cmake})
	case cmake == "":
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "soname-only-in-bazel", Detail: bazel})
	case sonameMajor(cmake) == sonameMajor(bazel):
		rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "soname-version-suffix", Detail: cmake + " vs " + bazel})
	default:
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "soname-mismatch", Detail: cmake + " vs " + bazel})
	}
}

// sonameBase strips the trailing version components after `.so` so
// libfoo.so.1.2.3 and libfoo.so.1 share the base "libfoo.so". Used for
// distro-runtime FAMILY matching (libc.so.6 -> libc.so).
//
// The qualifying `.so` is the SUFFIX one — followed by end-of-string or
// `.<digit>` (the version) — scanned right-to-left so a `.so` MID-name
// (libfoo.software.so.1) doesn't mis-base on the embedded occurrence.
func sonameBase(s string) string {
	best := -1
	for i := 0; ; {
		j := strings.Index(s[i:], ".so")
		if j < 0 {
			break
		}
		after := i + j + len(".so")
		switch {
		case after == len(s): // ends in ".so" (no version)
			best = after
		case s[after] == '.' && after+1 < len(s) && s[after+1] >= '0' && s[after+1] <= '9':
			best = after // ".so" followed by ".<digit>"
		}
		i = i + j + len(".so")
	}
	if best < 0 {
		return s
	}
	return s[:best]
}

// sonameMajor keeps the FIRST version component after `.so` — the ABI-major.
// libfoo.so.1 and libfoo.so.1.2.3 → "libfoo.so.1" (benign minor/patch suffix);
// libfoo.so.1 vs libfoo.so.2 → different majors (an ABI break, impactful).
func sonameMajor(s string) string {
	base := sonameBase(s)
	if base == s {
		return s // no ".so" or no suffix
	}
	rest := s[len(base):] // ".1.2.3" or ".1"
	if !strings.HasPrefix(rest, ".") {
		return base
	}
	comp := rest[1:]
	if dot := strings.IndexByte(comp, '.'); dot >= 0 {
		comp = comp[:dot]
	}
	return base + "." + comp
}

// classifyNeeded buckets the DT_NEEDED diff. A distro-runtime library
// (libc/libm/libstdc++/libgcc_s/…) appearing on only one side is toolchain
// noise (the hermetic Bazel toolchain and the host distro toolchain link the
// runtime differently). A PROJECT library dropped from the Bazel side is a lost
// runtime dependency (impactful); an extra one is over-linking (impactful).
func classifyNeeded(rep *Report, cmake, bazel map[string]bool, allowed Allowlist) {
	for n := range setSub(cmake, bazel) {
		switch {
		case isDistroRuntime(n):
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "needed-distro-runtime-only-in-cmake", Detail: n})
		case allowed.Match(n):
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "needed-allowlist-suppressed", Detail: n})
		default:
			rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "needed-only-in-cmake", Detail: n})
		}
	}
	for n := range setSub(bazel, cmake) {
		switch {
		case isDistroRuntime(n):
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "needed-distro-runtime-only-in-bazel", Detail: n})
		case allowed.Match(n):
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "needed-allowlist-suppressed", Detail: n})
		default:
			rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "needed-only-in-bazel", Detail: n})
		}
	}
}

// isDistroRuntime reports whether a DT_NEEDED soname is a C/C++ runtime /
// libc-family library the toolchain — not the project — pulls in. These differ
// freely between the host distro link and the hermetic Bazel link without being
// a converter signal.
func isDistroRuntime(soname string) bool {
	base := sonameBase(soname)
	switch base {
	case "libc.so", "libm.so", "libdl.so", "librt.so", "libpthread.so",
		"libgcc_s.so", "libstdc++.so", "libatomic.so", "libresolv.so",
		"ld-linux-x86-64.so", "ld-linux.so", "ld-linux-aarch64.so":
		return true
	}
	// ld-linux* sonames carry an arch infix that .so-base stripping leaves;
	// match the family by prefix as a backstop.
	return strings.HasPrefix(soname, "ld-linux")
}

// classifyRunpath flags a host-leak RPATH/RUNPATH baked into the Bazel
// artifact (a hermeticity break — the converter must not embed the build dir or
// source tree). A pure RUNPATH-vs-RPATH FORM difference over the same path set
// is benign (the new-style tag vs the legacy one). Any other path-set
// difference is informational/benign.
func classifyRunpath(rep *Report, cmake, bazel *ElfInfo) {
	for _, p := range append(append([]string(nil), bazel.Rpath...), bazel.Runpath...) {
		if isHostLeakPath(p) {
			rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "rpath-host-leak-in-bazel", Detail: p})
		}
	}
	cmakeSet := pathSet(cmake.Rpath, cmake.Runpath)
	bazelSet := pathSet(bazel.Rpath, bazel.Runpath)
	if sameStringSet(cmakeSet, bazelSet) {
		// Same paths; if the TAG differs (one RPATH, one RUNPATH) note it benign.
		if (len(cmake.Rpath) > 0) != (len(bazel.Rpath) > 0) ||
			(len(cmake.Runpath) > 0) != (len(bazel.Runpath) > 0) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "rpath-vs-runpath-form", Detail: strings.Join(sortedKeys(bazelSet), ":")})
		}
		return
	}
	for p := range bazelSet {
		if !cmakeSet[p] && !isHostLeakPath(p) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "rpath-entry-only-in-bazel", Detail: p})
		}
	}
	for p := range cmakeSet {
		if !bazelSet[p] {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "rpath-entry-only-in-cmake", Detail: p})
		}
	}
}

// isHostLeakPath reports whether an RPATH/RUNPATH entry points into a build /
// home / scratch tree — a non-hermetic absolute path the converter shouldn't
// bake in. `$ORIGIN`-relative entries and distro system dirs are fine.
func isHostLeakPath(p string) bool {
	for _, pre := range []string{"/tmp/", "/home/", "/root/", "/var/", "/build/", "/workspace/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// classifyVersionDefs buckets the symbol-versioning diff. A version node
// present on only one side is an ABI-versioning mismatch — the SAME symbol
// names under a different version tag link against a different versioned symbol,
// which the nm-set compare can't see. Impactful unless allowlisted.
func classifyVersionDefs(rep *Report, cmake, bazel map[string]bool, allowed Allowlist) {
	for n := range setSub(cmake, bazel) {
		if allowed.Match(n) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "version-node-allowlist-suppressed", Detail: n})
			continue
		}
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "version-node-only-in-cmake", Detail: n})
	}
	for n := range setSub(bazel, cmake) {
		if allowed.Match(n) {
			rep.BenignDeltas = append(rep.BenignDeltas, Delta{Kind: "version-node-allowlist-suppressed", Detail: n})
			continue
		}
		rep.ImpactfulDeltas = append(rep.ImpactfulDeltas, Delta{Kind: "version-node-only-in-bazel", Detail: n})
	}
}

func pathSet(a, b []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range a {
		out[p] = true
	}
	for _, p := range b {
		out[p] = true
	}
	return out
}

func sameStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countCommon(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

func setSub(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func sortDeltas(s []Delta) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Kind != s[j].Kind {
			return s[i].Kind < s[j].Kind
		}
		return s[i].Detail < s[j].Detail
	})
}
