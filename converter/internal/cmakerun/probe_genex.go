package cmakerun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GenexProbe is the per-target snapshot the probe-genex hook
// (Options.ProbeGenex) writes at cmake generation time. Each field
// holds the resolved bytes of a specific genex shape — captured by
// cmake's own evaluator and read back here without cmake-side help
// at Bazel time.
//
// Empty fields mean either:
//   - The target's TYPE excluded that probe (e.g. TARGET_FILE is
//     skipped for INTERFACE_LIBRARY; TARGET_OBJECTS is emitted only
//     for OBJECT_LIBRARY).
//   - The genex resolved to an empty string (e.g. a target with no
//     INTERFACE_LINK_LIBRARIES has an empty interface_link_libraries
//     file — both "absent" and "empty list" look the same here).
//
// Probe values are project-source-root relative or absolute as cmake
// recorded them; the consumer is responsible for projecting them
// onto the converter's output layout.
//
// Multi-config note: probe-genex.cmake encodes `$<CONFIG>` in the
// OUTPUT filenames (e.g. file.Release.txt, file.ASan.txt) so the
// hook composes with the Ninja Multi-Config generator — without
// per-config OUTPUT, cmake would refuse the file(GENERATE) with
// "Evaluation file to be written multiple times with different
// content." ReadGenexProbe collapses the per-config files back to
// the single-string fields below when every config resolved to the
// same value (the common case for TARGET_FILE_NAME and most
// INTERFACE_* aggregates); when values diverge across configs
// (the common case for TARGET_FILE / TARGET_FILE_DIR under Ninja
// Multi-Config, which puts each config's artifacts in a per-config
// subdir like `/build/Release/...` vs `/build/ASan/...`) the
// reader drops the field silently — equivalent to "probe didn't
// run for this field" — so downstream genexeval surfaces its
// existing UnsupportedError on missing data and the lifter falls
// back to (b) / legacy. Bazel has no select() shape for per-
// artifact path that lifter consumers honor today, so dropping is
// the v1 safe choice; the PerConfigMismatchError type below is
// retained for callers that want to inspect the divergence.
type GenexProbe struct {
	// Name is the cmake target name the probe corresponds to.
	Name string
	// Type is "$<TARGET_PROPERTY:t,TYPE>" — STATIC_LIBRARY,
	// SHARED_LIBRARY, EXECUTABLE, OBJECT_LIBRARY, INTERFACE_LIBRARY,
	// MODULE_LIBRARY, UTILITY, etc.
	Type string
	// File is "$<TARGET_FILE:t>" — full path to the on-disk
	// artifact. Empty for INTERFACE_LIBRARY targets.
	File string
	// FileDir is "$<TARGET_FILE_DIR:t>".
	FileDir string
	// FileName is "$<TARGET_FILE_NAME:t>" — the bare filename
	// without directory.
	FileName string
	// Objects is "$<TARGET_OBJECTS:t>" — semicolon-separated list
	// of .o paths. Populated only for OBJECT_LIBRARY targets;
	// empty everywhere else.
	Objects string
	// Interface holds the post-genex-eval value of cmake's
	// INTERFACE_* properties. Keys are the property suffix without
	// the leading "INTERFACE_" (INCLUDE_DIRECTORIES,
	// COMPILE_DEFINITIONS, COMPILE_OPTIONS, LINK_LIBRARIES,
	// LINK_OPTIONS). Values are semicolon-separated lists.
	Interface map[string]string

	// Properties holds the post-genex-eval value of cmake target
	// properties beyond the INTERFACE_* set — non-aggregating
	// properties Bazel cc rules can honor: BUILD_RPATH /
	// INSTALL_RPATH (linkopts -Wl,-rpath,…), POSITION_INDEPENDENT_CODE
	// (features=["pic"]), CXX_VISIBILITY_PRESET / C_VISIBILITY_PRESET
	// (copts -fvisibility=…). Keys match the cmake property name
	// verbatim; values are the cmake-recorded string (may be
	// empty when the property is unset).
	Properties map[string]string
}

// PerConfigMismatchError signals that probe-genex captured
// per-config values for one probe basename that disagreed across
// the project's configurations. In single-config builds (one
// `$<CONFIG>` value) divergence is impossible, so this error is a
// pure multi-config concern.
//
// The typical cause is a TARGET_FILE / TARGET_FILE_DIR expansion
// that genuinely differs per configuration. Under Ninja Multi-
// Config, the cmake generator places per-config artifacts in
// per-config subdirs of CMAKE_BINARY_DIR, so file/file_dir for any
// target naturally diverges. Other divergence sources include
// per-config OUTPUT_NAME, CMAKE_<CONFIG>_POSTFIX, or per-config
// $<CONFIG>-bearing genexes in INTERFACE_* aggregates.
//
// ReadGenexProbe does NOT bubble this out as a fatal error — it
// drops the diverging field on the per-target probe (equivalent
// to "probe didn't run for this field"). The PerConfigMismatchError
// type remains exported so future callers / diagnostic surfaces
// can re-inspect the divergence shape if they need to (e.g. a
// `--probe-genex-strict` mode or a debug log) without
// re-implementing the collapse logic.
type PerConfigMismatchError struct {
	Target   string
	Basename string
	// Values is keyed by config name (e.g. "Release", "ASan") with
	// the per-config string the probe captured. Stable iteration
	// for the error message is the caller's job.
	Values map[string]string
}

func (e *PerConfigMismatchError) Error() string {
	configs := make([]string, 0, len(e.Values))
	for c := range e.Values {
		configs = append(configs, c)
	}
	sort.Strings(configs)
	return fmt.Sprintf("cmakerun: probe-genex target %q file %q diverged across configs %v",
		e.Target, e.Basename, configs)
}

// ReadGenexProbe walks <buildDir>/cmake-to-bazel.genex/ — the
// per-target probe-genex output directory cmake's file(GENERATE)
// declarations populate at generation time — and returns one
// GenexProbe per target subdirectory. Returns ("", nil) when the
// directory does not exist (the hook wasn't staged, cmake < 3.24,
// or the project's configure failed before reaching generation).
//
// Target enumeration is deterministic (sorted by directory name) so
// consumers can use the slice index as a stable key in goldens.
//
// Multi-config layout: per-config files (file.Release.txt,
// file.ASan.txt, …) collapse to a single GenexProbe field when the
// per-config values match. When values diverge across configs
// (the common case for TARGET_FILE / TARGET_FILE_DIR under Ninja
// Multi-Config) the diverging field is dropped from the per-target
// probe — equivalent to "probe didn't capture this field" — so
// downstream genexeval surfaces its existing UnsupportedError on
// missing data and the lifter falls back to (b) / legacy. type.txt
// is captured config-invariantly (single emit; cmake's
// TARGET_PROPERTY:TYPE doesn't honor $<CONFIG>) and uses the
// historical no-config-suffix layout.
func ReadGenexProbe(buildDir string) ([]GenexProbe, error) {
	root := filepath.Join(buildDir, ProbeGenexDirname)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cmakerun: read genex probe dir: %w", err)
	}

	// Stable order regardless of filesystem iteration: cmake's
	// directory walk landed entries in whatever order the operating
	// system handed them back, which on ext4 is hash-table order.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]GenexProbe, 0, len(names))
	for _, name := range names {
		probe, err := readOneGenexProbe(filepath.Join(root, name), name)
		if err != nil {
			return nil, err
		}
		out = append(out, probe)
	}
	return out, nil
}

// readOneGenexProbe handles a single <target>/ subdir under
// cmake-to-bazel.genex. Split out from ReadGenexProbe so the
// per-config collapse logic stays at one indentation level.
func readOneGenexProbe(tgtDir, name string) (GenexProbe, error) {
	probe := GenexProbe{
		Name:       name,
		Interface:  map[string]string{},
		Properties: map[string]string{},
	}
	propEntries, err := os.ReadDir(tgtDir)
	if err != nil {
		return probe, fmt.Errorf("cmakerun: read genex probe %s: %w", name, err)
	}
	// Aggregate per-basename per-config values. The on-disk layout
	// is <basename>.<config>.txt for everything but type.txt; map
	// basename → (config → value) and collapse at the end.
	perBasename := map[string]map[string]string{}
	for _, pe := range propEntries {
		if pe.IsDir() {
			// Unexpected — flat per-target schema only — but
			// don't fail the whole probe read for one stray
			// entry. Skip.
			continue
		}
		fname := pe.Name()
		val, err := os.ReadFile(filepath.Join(tgtDir, fname))
		if err != nil {
			return probe, fmt.Errorf("cmakerun: read genex probe %s/%s: %w", name, fname, err)
		}
		s := string(val)
		if fname == "type.txt" {
			// TYPE is the one probe that doesn't carry $<CONFIG>
			// in OUTPUT — keep the historical single-file layout
			// so older readers + the affirmative-type gate stay
			// unchanged.
			probe.Type = s
			continue
		}
		basename, config, ok := splitProbeConfigFilename(fname)
		if !ok {
			// Unknown filename shape: silently ignored. Forward-
			// compat with future probe additions that older
			// readers haven't been taught about.
			continue
		}
		bucket, ok := perBasename[basename]
		if !ok {
			bucket = map[string]string{}
			perBasename[basename] = bucket
		}
		bucket[config] = s
	}
	// Collapse per-config values per basename. Same string across
	// every config → single value (the common case for
	// TARGET_FILE_NAME and most INTERFACE_* aggregates). Divergence
	// → drop the basename from the probe entirely (leave the
	// matching GenexProbe field empty), letting downstream
	// genexeval surface its existing UnsupportedError on missing
	// data so the lift falls back cleanly. TARGET_FILE /
	// TARGET_FILE_DIR routinely diverge under Ninja Multi-Config
	// (each config has its own subdir of CMAKE_BINARY_DIR), so
	// hard-erroring on this would defeat the whole point of
	// per-config OUTPUT.
	for basename, perConfig := range perBasename {
		unified, ok := collapseConfigValues(perConfig)
		if !ok {
			continue
		}
		assignProbeField(&probe, basename, unified)
	}
	return probe, nil
}

// splitProbeConfigFilename parses a probe filename like
// "file.Release.txt" into ("file", "Release", true). Returns
// (_, _, false) for filenames that don't match the per-config
// pattern (no recognized suffix). The grammar splits on the first
// `.` after the leading basename: basenames are
// underscore-joined cmake property identifiers (no `.`), so the
// first `.` reliably marks the basename/config boundary even if
// the config name itself contains a `.` (e.g. "Release.1").
func splitProbeConfigFilename(fname string) (basename, config string, ok bool) {
	const suffix = ".txt"
	if !strings.HasSuffix(fname, suffix) {
		return "", "", false
	}
	stem := fname[:len(fname)-len(suffix)]
	// Split into <basename>.<config>. The basename can't contain
	// "." (cmake property names are [A-Z_]+ with optional digits),
	// so the first "." is the separator.
	dot := strings.Index(stem, ".")
	if dot <= 0 || dot == len(stem)-1 {
		return "", "", false
	}
	return stem[:dot], stem[dot+1:], true
}

// collapseConfigValues returns (value, true) when all per-config
// values for one basename agree, or ("", false) when they diverge.
// Single-config builds have one entry so divergence is impossible;
// multi-config builds where every config resolved to the same
// string also collapse cleanly. Divergence is reported as a
// "not ok" return rather than a typed error because the reader's
// policy is to drop the diverging field silently — bubbling an
// error here would fail the whole probe read, which kills
// multi-config compose. Callers that want to introspect the
// divergence values can construct PerConfigMismatchError from
// the same input map.
func collapseConfigValues(perConfig map[string]string) (string, bool) {
	if len(perConfig) == 0 {
		return "", true
	}
	var first string
	have := false
	for _, v := range perConfig {
		if !have {
			first = v
			have = true
			continue
		}
		if v != first {
			return "", false
		}
	}
	return first, true
}

// assignProbeField routes a unified per-target value into the
// GenexProbe field matching its basename. Mirrors the original
// switch table in ReadGenexProbe, kept local so the per-config
// collapse loop reads linearly. Unknown basenames are dropped
// (forward-compat with future hook additions older readers
// haven't been taught about).
func assignProbeField(p *GenexProbe, basename, value string) {
	switch basename {
	case "file":
		p.File = value
	case "file_dir":
		p.FileDir = value
	case "file_name":
		p.FileName = value
	case "objects":
		p.Objects = value
	default:
		const ifacePrefix = "interface_"
		const propPrefix = "property_"
		switch {
		case strings.HasPrefix(basename, ifacePrefix):
			p.Interface[basename[len(ifacePrefix):]] = value
		case strings.HasPrefix(basename, propPrefix):
			p.Properties[basename[len(propPrefix):]] = value
		}
	}
}
