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
		probe := GenexProbe{Name: name, Interface: map[string]string{}}
		tgtDir := filepath.Join(root, name)
		propEntries, err := os.ReadDir(tgtDir)
		if err != nil {
			return nil, fmt.Errorf("cmakerun: read genex probe %s: %w", name, err)
		}
		for _, pe := range propEntries {
			if pe.IsDir() {
				// Unexpected — flat per-target schema only — but
				// don't fail the whole probe read for one stray
				// entry. Skip.
				continue
			}
			val, err := os.ReadFile(filepath.Join(tgtDir, pe.Name()))
			if err != nil {
				return nil, fmt.Errorf("cmakerun: read genex probe %s/%s: %w", name, pe.Name(), err)
			}
			s := string(val)
			switch pe.Name() {
			case "type.txt":
				probe.Type = s
			case "file.txt":
				probe.File = s
			case "file_dir.txt":
				probe.FileDir = s
			case "file_name.txt":
				probe.FileName = s
			case "objects.txt":
				probe.Objects = s
			default:
				const prefix = "interface_"
				const suffix = ".txt"
				if strings.HasPrefix(pe.Name(), prefix) && strings.HasSuffix(pe.Name(), suffix) {
					key := pe.Name()[len(prefix) : len(pe.Name())-len(suffix)]
					probe.Interface[key] = s
				}
				// Unknown filename: silently ignored. Forward-compat
				// with future probe additions that older readers
				// haven't been taught about.
			}
		}
		out = append(out, probe)
	}
	return out, nil
}
