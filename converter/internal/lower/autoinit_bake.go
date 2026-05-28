package lower

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// bakeAutoinitIncludeDefine closes the VTK AUTOINIT_INCLUDE define
// gap: cmake's VTK module machinery emits preprocessor defines
// whose VALUE is a quoted absolute path to a per-module auto-init
// header it generates at configure time:
//
//	vtkRenderingCore_AUTOINIT_INCLUDE="/abs/<build>/CMakeFiles/
//	vtkModuleAutoInit_<hash>.h"
//
// The C++ source then `#include`s the macro:
//
//	#include vtkRenderingCore_AUTOINIT_INCLUDE
//
// reanchorDefineValue currently drops these defines because the
// path lives under the cmake build dir (sandbox-invisible at Bazel
// action time). bakeAutoinitIncludeDefine closes the gap by:
//
//  1. Reading the file's bytes at convert time (the file was
//     already produced by cmake configure that drove ToIR).
//  2. Synthesizing a genrule that materializes the bytes via
//     base64-decode — one genrule per unique file, shared across
//     consumers (the same module's auto-init header is referenced
//     by every cc_library that includes its public API).
//  3. Adding the genrule's output (a package-relative basename
//     like `vtkModuleAutoInit_<hash>.h`) to irt.Hdrs so the
//     cc_library's compile action sees it in its input closure.
//  4. Rewriting the define to point at the basename:
//     `vtkRenderingCore_AUTOINIT_INCLUDE="vtkModuleAutoInit_<hash>.h"`
//     — Bazel's package-rooted include path (-iquote on the
//     package dir) resolves the basename.
//
// Returns (newDefine, true) on a clean bake; (def, false) when
// the shape doesn't match (no embedded path, not absolute, path
// outside buildDir, file unreadable) — caller falls back to the
// existing reanchorDefineValue path.
//
// Trade-off: outputs are convert-time-baked. If the upstream
// VTK module set changes (a new module gets registered between
// converts), Bazel won't notice without a re-convert. Same
// warning surface as cmake_script_bake: irt picks up the
// `cmake-codegen-autoinit-bake` tag so the
// warnConvertTimeBaking post-pass lists the targets.
func bakeAutoinitIncludeDefine(def string, buildDir string, cc *codegenContext, irt *ir.Target) (string, bool) {
	if buildDir == "" || cc == nil || irt == nil {
		return def, false
	}
	eq := strings.IndexByte(def, '=')
	if eq < 0 {
		return def, false
	}
	key, raw := def[:eq], def[eq+1:]
	stripped := strings.Trim(raw, `"`)
	if !filepath.IsAbs(stripped) {
		return def, false
	}
	rel, ok := relativeIfInside(buildDir, stripped)
	if !ok {
		return def, false
	}
	body, err := os.ReadFile(stripped)
	if err != nil {
		// File doesn't exist at convert time — cmake configure
		// didn't actually emit it. Let the caller fall back to
		// the drop path; the build would have failed at the cmake
		// generate step anyway.
		return def, false
	}

	// One genrule per unique baked file. Genrule output is the
	// basename (so the package-rooted include path resolves the
	// rewritten define value). genrule name encodes the rel path
	// so two files with the same basename under different
	// subdirs of buildDir don't collide.
	base := filepath.Base(stripped)
	name := "gen_autoinit_" + sanitizeForName(strings.TrimSuffix(rel, filepath.Ext(rel)))
	if existing, seen := cc.OutToGenrule[base]; seen {
		// Already baked by another target in this package. Reuse
		// the existing genrule's output; just attach the basename
		// to this target's hdrs.
		_ = existing
	} else {
		encoded := base64.StdEncoding.EncodeToString(body)
		gen := ir.Target{
			Name:        name,
			Kind:        ir.KindGenrule,
			GenruleCmd:  fmt.Sprintf(`echo %q | base64 -d > $@`, encoded),
			GenruleOuts: []string{base},
			Tags: []string{
				"cmake-codegen-autoinit-bake",
			},
			Visibility: []string{"//visibility:private"},
		}
		cc.Genrules = append(cc.Genrules, gen)
		if cc.OutToGenrule == nil {
			cc.OutToGenrule = map[string]string{}
		}
		cc.OutToGenrule[base] = name
	}
	if !stringSliceContains(irt.Hdrs, base) {
		irt.Hdrs = append(irt.Hdrs, base)
	}
	if !stringSliceContains(irt.Tags, "cmake-codegen-autoinit-bake") {
		irt.Tags = append(irt.Tags, "cmake-codegen-autoinit-bake")
	}
	return key + `="` + base + `"`, true
}
