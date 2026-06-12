// Package cmakecfg synthesizes a drop-in replacement for the cmake-config
// bundle that install(EXPORT) would have produced for the package. The bundle
// lets downstream CMake projects do `find_package(<Pkg> CONFIG REQUIRED)` and
// see the same imported targets they would have seen against an upstream
// `make install` of the project.
//
// Bundle layout (mirrors what cmake's cmExportCMakeConfigGenerator produces;
// see Source/cmExportCMakeConfigGenerator.cxx in the analysis doc):
//
//	<Pkg>Config.cmake          (find_package entry point)
//	<Pkg>Targets.cmake         (imported-target declarations)
//	<Pkg>Targets-release.cmake (per-config IMPORTED_LOCATION)
//
// Synthesis intentionally drops the multi-inclusion-protection block from
// upstream's emitter; consumers that re-include in the same scope can opt in
// later. The EXISTS-check loop at the bottom of <Pkg>Targets.cmake is
// preserved because docs/cmake_analysis.md documents that loop as the reason the
// shadow-prefix tree just needs to satisfy access(R_OK) — not actually carry
// content.
package cmakecfg

import (
	"bytes"
	"fmt"
	"path"
	"text/template"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// Bundle is the in-memory representation of one cmake-config directory.
// Files maps relative path -> contents.
type Bundle struct {
	Files map[string][]byte
}

// Options affect bundle synthesis. Defaults are sensible for hello-world-shape
// packages.
type Options struct {
	// Namespace is the prefix added to imported targets. Defaults to
	// "<PackageName>::". Set explicitly when the upstream uses a different
	// convention (e.g. "Foo::" when project is "foo").
	Namespace string

	// PackageName is the find_package(<name>) the bundle answers to —
	// it names the config files (<PackageName>Config.cmake etc.) and,
	// via the caller, the lib/cmake/<PackageName>/ directory. Defaults
	// to pkg.Name. Set it to the install(EXPORT ... NAMESPACE <ns>::)
	// stem (e.g. "ZLIB" for a project("zlib") that exports ZLIB::ZLIB)
	// so a consumer's find_package(ZLIB) resolves against this bundle
	// rather than the host's FindZLIB module. The cmake File API
	// codemodel drops the namespace, so the caller recovers it from
	// the trace (shadow.InstallExportCall).
	PackageName string

	// Configs lists the per-config bundle files to emit. Defaults to
	// ["Release"].
	Configs []string

	// Aliases lists consumer-facing add_library(<Name> ALIAS
	// <Underlying>) redirects to re-publish in the bundle. The cmake
	// File API codemodel omits ALIAS targets (they're configure-time
	// name redirects), so a consumer linking the alias name (e.g.
	// ZLIB::ZLIB aliasing the real target zlibstatic) would find no
	// such target in the synthesized config. Each alias renders as an
	// `add_library(<Name> ALIAS <Namespace><Underlying>)` line, so the
	// alias resolves to the same imported artifact. Underlyings not in
	// the importable set are dropped by Emit (the ALIAS would dangle).
	Aliases []Alias
}

// Alias is one add_library(<Name> ALIAS <Underlying>) redirect to
// re-publish in the synthesized bundle. Name is the verbatim
// consumer-facing name (already namespaced, e.g. "ZLIB::ZLIB");
// Underlying is the bare name of the importable target it points at.
type Alias struct {
	Name       string
	Underlying string
}

// Emit produces a Bundle for pkg. Kinds that map onto IMPORTED targets
// are exposed: libraries (STATIC/SHARED/MODULE/INTERFACE) and INSTALLED
// executables — real install(TARGETS <exe> EXPORT ...) sets ship IMPORTED
// executables (protobuf::protoc is the canonical consumer-facing tool),
// which downstreams drive via $<TARGET_FILE:Pkg::tool>. Non-installed
// executables and genrules are filtered: they never land in the prefix
// tree, so no consumer-visible path exists for them.
func Emit(pkg *ir.Package, opts Options) (*Bundle, error) {
	if pkg.Name == "" {
		return nil, fmt.Errorf("cmakecfg.Emit: package has empty Name")
	}
	if opts.PackageName == "" {
		opts.PackageName = pkg.Name
	}
	if opts.Namespace == "" {
		opts.Namespace = opts.PackageName + "::"
	}
	if len(opts.Configs) == 0 {
		opts.Configs = []string{"Release"}
	}

	libs := filterImportable(pkg.Targets)
	b := &Bundle{Files: map[string][]byte{}}
	if len(libs) == 0 {
		// Executable-only / no-library projects have nothing to
		// publish to cmake-side find_package consumers. Return
		// an empty bundle so the caller can still satisfy its
		// --out-bundle-dir contract (Bazel genrules require the
		// declared output dir to exist) without burning a
		// failure path. find_package(<Pkg> CONFIG) consumers of
		// such a bundle would see no imported targets, which
		// matches what cmake sees for a real project that only
		// installs an executable.
		return b, nil
	}

	cfg, err := renderConfig(opts.PackageName)
	if err != nil {
		return nil, err
	}
	b.Files[opts.PackageName+"Config.cmake"] = cfg

	// Keep only aliases whose underlying is an exported library; an
	// ALIAS to an unexported target would dangle at consumer
	// find_package time.
	importable := make(map[string]bool, len(libs))
	for _, l := range libs {
		importable[l.Name] = true
	}
	var aliases []Alias
	for _, a := range opts.Aliases {
		if importable[a.Underlying] {
			aliases = append(aliases, a)
		}
	}

	tgts, err := renderTargets(opts.PackageName, opts.Namespace, libs, aliases)
	if err != nil {
		return nil, err
	}
	b.Files[opts.PackageName+"Targets.cmake"] = tgts

	for _, conf := range opts.Configs {
		name := opts.PackageName + "Targets-" + lowercase(conf) + ".cmake"
		body, err := renderTargetsConfig(opts.Namespace, libs, conf)
		if err != nil {
			return nil, err
		}
		b.Files[name] = body
	}
	return b, nil
}

// ImportableTargets returns the library targets a find_package
// consumer can import from pkg — the same set Emit publishes as
// IMPORTED targets in the synthetic bundle. Exposed so the
// exports.json producer (convert-element-cmake) lists exactly the
// targets the bundle exports, keeping the two channels in lockstep.
func ImportableTargets(pkg *ir.Package) []ir.Target {
	return filterImportable(pkg.Targets)
}

func filterImportable(ts []ir.Target) []ir.Target {
	var out []ir.Target
	for _, t := range ts {
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCInterface, ir.KindCCImport:
			// Phase 6 install(EXPORT) derived cc_import
			// targets are a Bazel-side facade for the
			// producer's own cc_library — the IMPORTED entry
			// the bundle would emit for them is already
			// covered by the underlying cc_library's entry.
			// Skip to avoid duplicate "add_library(Pkg::foo
			// SHARED IMPORTED)" lines.
			if hasTag(t.Tags, "cmake-codegen-install-export-import") {
				continue
			}
			out = append(out, t)
		case ir.KindCCBinary, ir.KindCCTest:
			// INSTALLED executables only: the synth prefix
			// stages install destinations, so a non-installed
			// binary has no consumer-reachable path. cc_test
			// included for completeness (cmake installs test
			// drivers occasionally); same install gate.
			if t.InstallDest != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// isExecutable reports whether the imported entry renders as
// add_executable(... IMPORTED): cmake's executable form takes no
// STATIC/SHARED keyword and only IMPORTED_LOCATION per config (no
// link-interface languages).
func isExecutable(t ir.Target) bool {
	return t.Kind == ir.KindCCBinary || t.Kind == ir.KindCCTest
}

// importedKind picks the CMake STATIC|SHARED|INTERFACE keyword for a target.
func importedKind(t ir.Target) string {
	if t.Kind == ir.KindCCInterface {
		return "INTERFACE"
	}
	if t.Linkstatic {
		return "STATIC"
	}
	return "SHARED"
}

func lowercase(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func uppercase(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

const configTemplateSrc = `# Generated by convert-element-cmake. DO NOT EDIT.
include("${CMAKE_CURRENT_LIST_DIR}/{{.Pkg}}Targets.cmake")
`

const targetsTemplateSrc = `# Generated by convert-element-cmake. DO NOT EDIT.
# Computes _IMPORT_PREFIX as the parent of lib/cmake/<Pkg>/.
get_filename_component(_IMPORT_PREFIX "${CMAKE_CURRENT_LIST_FILE}" PATH)
get_filename_component(_IMPORT_PREFIX "${_IMPORT_PREFIX}" PATH)
get_filename_component(_IMPORT_PREFIX "${_IMPORT_PREFIX}" PATH)
get_filename_component(_IMPORT_PREFIX "${_IMPORT_PREFIX}" PATH)
if(_IMPORT_PREFIX STREQUAL "/")
  set(_IMPORT_PREFIX "")
endif()

{{range .Targets -}}
{{if isExecutable . -}}
add_executable({{$.NS}}{{.Name}} IMPORTED)
{{else -}}
add_library({{$.NS}}{{.Name}} {{importedKind .}} IMPORTED)
{{- if eq (importedKind .) "INTERFACE"}}
{{- else}}
{{end -}}
{{end -}}
{{- if .Includes}}
set_target_properties({{$.NS}}{{.Name}} PROPERTIES
  INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include"
)
{{end}}
{{end -}}
{{range .Aliases}}
# Consumer-facing alias; inherits the underlying imported target's
# properties. Executable underlyings alias via add_executable.
{{if .IsExecutable -}}
add_executable({{.Name}} ALIAS {{$.NS}}{{.Underlying}})
{{else -}}
add_library({{.Name}} ALIAS {{$.NS}}{{.Underlying}})
{{end -}}
{{end -}}

# Per-config target details.
file(GLOB _cmake_config_files "${CMAKE_CURRENT_LIST_DIR}/{{.Pkg}}Targets-*.cmake")
foreach(_cmake_config_file IN LISTS _cmake_config_files)
  include("${_cmake_config_file}")
endforeach()
unset(_cmake_config_file)
unset(_cmake_config_files)

set(_IMPORT_PREFIX)

# Verify referenced artifacts exist (access(R_OK) — content not read).
foreach(_cmake_target IN LISTS _cmake_import_check_targets)
  foreach(_cmake_file IN LISTS "_cmake_import_check_files_for_${_cmake_target}")
    if(NOT EXISTS "${_cmake_file}")
      message(FATAL_ERROR "Imported target \"${_cmake_target}\" references missing file \"${_cmake_file}\"")
    endif()
  endforeach()
  unset(_cmake_file)
  unset("_cmake_import_check_files_for_${_cmake_target}")
endforeach()
unset(_cmake_target)
unset(_cmake_import_check_targets)
`

const targetsConfigTemplateSrc = `# Generated by convert-element-cmake. DO NOT EDIT.
# Per-config import file for "{{.Config}}".

{{range .Targets -}}
{{- if .ArtifactName -}}
set_property(TARGET {{$.NS}}{{.Name}} APPEND PROPERTY IMPORTED_CONFIGURATIONS {{$.UpperConfig}})
{{if isExecutable . -}}
set_target_properties({{$.NS}}{{.Name}} PROPERTIES
  IMPORTED_LOCATION_{{$.UpperConfig}} "${_IMPORT_PREFIX}/{{installPath .}}"
)

{{else -}}
set_target_properties({{$.NS}}{{.Name}} PROPERTIES
  IMPORTED_LINK_INTERFACE_LANGUAGES_{{$.UpperConfig}} "{{.LinkLanguage}}"
  IMPORTED_LOCATION_{{$.UpperConfig}} "${_IMPORT_PREFIX}/{{installPath .}}"
)

{{end -}}
list(APPEND _cmake_import_check_targets {{$.NS}}{{.Name}})
list(APPEND _cmake_import_check_files_for_{{$.NS}}{{.Name}} "${_IMPORT_PREFIX}/{{installPath .}}")
{{end}}
{{end -}}`

func renderConfig(pkg string) ([]byte, error) {
	t := template.Must(template.New("config").Parse(configTemplateSrc))
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]string{"Pkg": pkg}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func renderTargets(pkg, ns string, targets []ir.Target, aliases []Alias) ([]byte, error) {
	funcs := template.FuncMap{
		"importedKind": importedKind,
		"isExecutable": isExecutable,
	}
	// Pre-resolve each alias's underlying kind: cmake aliases an
	// executable via add_executable(... ALIAS ...), a library via
	// add_library — the template can't see across the Targets slice.
	exeByName := map[string]bool{}
	for _, tgt := range targets {
		exeByName[tgt.Name] = isExecutable(tgt)
	}
	type aliasRender struct {
		Name, Underlying string
		IsExecutable     bool
	}
	ra := make([]aliasRender, 0, len(aliases))
	for _, a := range aliases {
		ra = append(ra, aliasRender{Name: a.Name, Underlying: a.Underlying, IsExecutable: exeByName[a.Underlying]})
	}
	t := template.Must(template.New("tgts").Funcs(funcs).Parse(targetsTemplateSrc))
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]any{
		"Pkg":     pkg,
		"NS":      ns,
		"Targets": targets,
		"Aliases": ra,
	}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func renderTargetsConfig(ns string, targets []ir.Target, conf string) ([]byte, error) {
	funcs := template.FuncMap{
		"installPath":  installPath,
		"isExecutable": isExecutable,
	}
	t := template.Must(template.New("tgtsCfg").Funcs(funcs).Parse(targetsConfigTemplateSrc))
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]any{
		"Config":      conf,
		"UpperConfig": uppercase(conf),
		"NS":          ns,
		"Targets":     targets,
	}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// installPath joins InstallDest and ArtifactName, defaulting destination to
// "lib" if unset (matches CMake's GNUInstallDirs default for libraries).
func installPath(t ir.Target) string {
	dest := t.InstallDest
	if dest == "" {
		dest = "lib"
	}
	return path.Join(dest, t.ArtifactName)
}
