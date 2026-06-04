package lower

import (
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// knownCcHashScripts is the set of cmake -P hashing script basenames whose
// `-D` argument contract matches vtkHashSource's (input_file / output_file /
// output_name / algorithm). Other scripts sharing that contract can be added.
var knownCcHashScripts = map[string]bool{
	"vtkHashSource.cmake": true,
}

// knownCcHashAlgorithms is the digest set the cc_hash rule + cc-hash tool
// support (cmake's file(<ALGO> …) names). Recognition declines for anything
// outside it so the lift never emits a cc_hash the rule would fail() on.
var knownCcHashAlgorithms = map[string]bool{
	"MD5": true, "SHA1": true, "SHA224": true,
	"SHA256": true, "SHA384": true, "SHA512": true,
}

// recognizeCcHash detects a custom command running a known file-hashing cmake
// -P script (VTK's vtkHashSource) and lowers it to the native cc_hash rule
// (//tools:cc-hash) — so the converted project needs no cmake at build time.
// This is the Bazel-native end-state for the "hash a file into a generated
// header" codegen idiom; it's faithful (the #define name + digest are
// preserved) and — unlike --cmake-script-bake — recomputes the digest at
// build time, so it auto-refreshes when the input changes.
//
// Returns (name, true) on success. Returns ("", false) to fall through to the
// runner/bake/refuse path when the flag is off, the script isn't a known
// hasher, the args don't parse into a complete hash spec, the algorithm isn't
// one the rule supports, or the input file isn't under the source tree.
func recognizeCcHash(cc *codegenContext, b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir string) (name string, ok bool) {
	if !cc.LiftCCHash {
		return "", false
	}
	if !knownCcHashScripts[filepath.Base(scriptArg)] {
		return "", false
	}
	d := parseCmakeDashDMap(cmd)
	srcAbs, defineName := d["input_file"], d["output_name"]
	if srcAbs == "" || defineName == "" {
		return "", false
	}
	// cmake's vtk_hash_source defaults ALGORITHM to MD5 and always passes
	// -Dalgorithm=, so the trace normally carries it; default here too for
	// robustness. Decline for any algorithm the rule/tool can't honor —
	// emitting a cc_hash the rule would fail() on is worse than the fallback.
	algorithm := d["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}
	if !knownCcHashAlgorithms[algorithm] {
		return "", false
	}
	src, inSrc := relativeIfInside(cmakeSrc, srcAbs)
	if !inSrc {
		// An input_file outside the source tree (generated, or an absolute
		// path that won't survive the sandbox) — leave it to the fallback.
		return "", false
	}
	header, _ := pickHeaderSource(genruleOuts(b, buildDir))
	if header == "" {
		return "", false
	}

	name = genruleNameFor(b, buildDir)
	t := ir.Target{
		Name: name,
		Kind: ir.KindCCHash,
		CCHash: &ir.CCHashSpec{
			Src:       src,
			Name:      defineName,
			Algorithm: algorithm,
			OutHeader: header,
		},
		Tags:       []string{"cmake-codegen-cc-hash"},
		Visibility: []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, t)
	cc.SeenBuilds[b] = name
	for _, o := range genruleOuts(b, buildDir) {
		cc.OutToGenrule[o] = name
	}
	return name, true
}
