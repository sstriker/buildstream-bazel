package lower

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// knownCcEmbedEncoders is the set of cmake -P encoder script basenames
// whose `-D` argument contract matches vtkEncodeString's
// (source_file / output_name / binary / nul_terminate / export_symbol /
// export_header). Other encoders sharing that contract can be added here.
var knownCcEmbedEncoders = map[string]bool{
	"vtkEncodeString.cmake": true,
}

// recognizeCcEmbed detects a custom command running a known file-embedding
// cmake -P encoder (VTK's vtkEncodeString) and lowers it to the native
// cc_embed rule (//tools:cc-embed) — so the converted project needs no
// cmake at build time. This is the Bazel-native end-state for the
// "embed a file as a C array" codegen idiom; it's faithful (the symbol
// name and the embedded bytes are preserved) and deterministic (no
// convert-time execution).
//
// Returns (name, true) on success — the caller (recoverGenrule) returns
// its own per-source relOut, so a consumer referencing the .cxx maps to
// the .cxx and one referencing the .h maps to the .h (both outputs are
// registered in cc.OutToGenrule; the second is served by recoverGenrule's
// SeenBuilds reuse). Returns ("", false) to fall through to the
// runner/bake/refuse path when the flag is off, the script isn't a known
// encoder, the args don't parse into a complete embed spec, or the spec
// would violate the cc_embed rule's own constraints.
func recognizeCcEmbed(cc *codegenContext, b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir string) (name string, ok bool) {
	if !cc.LiftCCEmbed {
		return "", false
	}
	if !knownCcEmbedEncoders[filepath.Base(scriptArg)] {
		return "", false
	}
	d := parseCmakeDashDMap(cmd)
	srcAbs, symbol := d["source_file"], d["output_name"]
	if srcAbs == "" || symbol == "" {
		return "", false
	}
	// Decline rather than emit a cc_embed that deterministically fails the
	// rule's own fail() checks: export_symbol/header must be set together,
	// nul_terminate only with binary, and out_header/out_source must share
	// a directory (the rule self-includes the header by basename).
	binary, nulTerminate := cmakeTruthy(d["binary"]), cmakeTruthy(d["nul_terminate"])
	if (d["export_symbol"] == "") != (d["export_header"] == "") {
		return "", false
	}
	if nulTerminate && !binary {
		return "", false
	}
	src, inSrc := relativeIfInside(cmakeSrc, srcAbs)
	if !inSrc {
		// A source_file outside the source tree (generated, or an absolute
		// path that won't survive the sandbox) — leave it to the fallback.
		return "", false
	}
	header, source := pickHeaderSource(genruleOuts(b, buildDir))
	if header == "" || source == "" {
		return "", false
	}
	if path.Dir(header) != path.Dir(source) {
		return "", false
	}

	name = genruleNameFor(b, buildDir)
	t := ir.Target{
		Name: name,
		Kind: ir.KindCCEmbed,
		CCEmbed: &ir.CCEmbedSpec{
			Src:          src,
			Symbol:       symbol,
			OutHeader:    header,
			OutSource:    source,
			Binary:       binary,
			NulTerminate: nulTerminate,
			ExportSymbol: d["export_symbol"],
			ExportHeader: d["export_header"],
		},
		Tags:       []string{"cmake-codegen-cc-embed"},
		Visibility: []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, t)
	cc.SeenBuilds[b] = name
	for _, o := range genruleOuts(b, buildDir) {
		cc.OutToGenrule[o] = name
	}
	return name, true
}

// parseCmakeDashDMap parses the `-D <var>=<val>` / `-D<var>=<val>`
// arguments of a cmake -P invocation into a map (last value wins). Built
// on extractCmakePDashArgs so it shares the cd-strip + tokenization.
func parseCmakeDashDMap(cmd string) map[string]string {
	m := map[string]string{}
	args := extractCmakePDashArgs(cmd)
	for i := 0; i < len(args); i++ {
		kv := args[i]
		if kv == "-D" && i+1 < len(args) {
			kv = args[i+1]
			i++
		} else if strings.HasPrefix(kv, "-D") {
			kv = strings.TrimPrefix(kv, "-D")
		} else {
			continue
		}
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			m[kv[:eq]] = kv[eq+1:]
		}
	}
	return m
}

// pickHeaderSource classifies a custom command's outputs into the header
// (.h/.hpp/.hxx) and the source (.cxx/.cpp/.cc/.c) cc_embed emits — the
// two files vtkEncodeString writes. First-of-each wins; non-matching
// outputs (e.g. ninja's ${cmake_ninja_workdir} shadows) are ignored.
func pickHeaderSource(outs []string) (header, source string) {
	for _, o := range outs {
		switch path.Ext(o) {
		case ".h", ".hpp", ".hxx":
			if header == "" {
				header = o
			}
		case ".cxx", ".cpp", ".cc", ".c":
			if source == "" {
				source = o
			}
		}
	}
	return header, source
}
