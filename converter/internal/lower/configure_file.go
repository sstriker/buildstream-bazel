package lower

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
	"github.com/sstriker/cmake-to-bazel/internal/shadow"
)

// configureFileOut is one recovered configure_file emission: the
// recorded absolute path of the output (used to associate the
// output with consuming targets via build-dir include matching)
// and the build-dir-relative path the genrule writes the file at
// (used as the package-relative path consumers reference in
// hdrs/srcs).
type configureFileOut struct {
	AbsOutput string // recording-machine absolute path: ${cmakeBuild}/<rel>
	RelOutput string // <rel>: build-dir-relative path; package-relative in the BUILD file
}

// recoverConfigureFiles walks the trace's configure_file events
// and emits one Bazel genrule per call. The genrule's cmd
// base64-encodes the rendered output bytes (configure-time
// substitution already done by cmake) and decodes them at Bazel
// build time — sidesteps the need to re-run cmake or implement
// @VAR@ expansion. Returns the list of recovered outputs so
// lowerTarget can attach them to consuming targets.
//
// hostBuildDir is the host-real path of the cmake build dir
// (where configured outputs live on this machine);
// recordedBuildDir is the path cmake itself recorded in the
// trace (= r.Codemodel.Paths.Build). They differ in offline
// tests where the recording machine wrote the trace and this
// machine doesn't have that path. We strip recordedBuildDir
// from the trace's output path to get a relative path, then
// re-anchor to hostBuildDir for the actual byte read.
//
// Returns an empty slice with no error when traceRaw is empty
// or no configure_file events are recorded — preserves the
// pre-trace behavior for offline runs without a stashed
// fixture.
func recoverConfigureFiles(traceRaw []byte, hostBuildDir, recordedSrcDir, recordedBuildDir string, cc *codegenContext) ([]configureFileOut, error) {
	if len(traceRaw) == 0 || hostBuildDir == "" {
		return nil, nil
	}
	return recoverConfigureFilesFromCalls(shadow.ExtractConfigureFiles(traceRaw, recordedSrcDir), hostBuildDir, recordedBuildDir, cc)
}

// recoverConfigureFilesFromCalls is the same logic as
// recoverConfigureFiles but takes pre-decoded ConfigureFileCall
// records. Used by Lower's single-pass trace dispatch so the
// trace is parsed once total across all extractors (including
// the configure_file recovery), instead of one pass per
// extractor.
func recoverConfigureFilesFromCalls(calls []shadow.ConfigureFileCall, hostBuildDir, recordedBuildDir string, cc *codegenContext) ([]configureFileOut, error) {
	if len(calls) == 0 || hostBuildDir == "" {
		return nil, nil
	}

	var out []configureFileOut
	seenRel := map[string]bool{}
	for _, call := range calls {
		// configure_file output is sometimes a relative path
		// (cmake resolves against the current binary dir at
		// expand time). Trace records the resolved string so
		// most calls have absolute paths. Skip relative —
		// can't anchor without per-call binary-dir context.
		if !filepath.IsAbs(call.Output) {
			continue
		}
		rel, ok := relativeIfInsideRelaxed(recordedBuildDir, call.Output)
		if !ok {
			// Output landed outside the build dir — unusual
			// (configure_file with absolute non-build dest).
			// Drop silently; not a recovery target.
			continue
		}
		if seenRel[rel] {
			// Trace can record duplicate calls when cmake
			// re-evaluates the same configure_file across
			// multiple frames. Dedupe by output path.
			continue
		}
		seenRel[rel] = true

		body, err := os.ReadFile(filepath.Join(hostBuildDir, rel))
		if err != nil {
			// Configured output not on disk — for offline
			// fixtures the stash may not include every
			// output, and for production the live build dir
			// always has them. Skip with no error so
			// missing fixtures degrade gracefully to the
			// pre-trace shape.
			continue
		}

		name := configureFileGenruleName(rel)
		cmd := configureFileCmd(rel, body)
		gen := ir.Target{
			Name:        name,
			Kind:        ir.KindGenrule,
			GenruleCmd:  cmd,
			GenruleOuts: []string{rel},
			Tags:        configureFileTags(),
			Visibility:  []string{"//visibility:private"},
		}
		cc.Genrules = append(cc.Genrules, gen)
		cc.OutToGenrule[rel] = name

		out = append(out, configureFileOut{
			AbsOutput: call.Output,
			RelOutput: rel,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelOutput < out[j].RelOutput })
	return out, nil
}

// configureFileGenruleName turns a build-dir-relative output
// path into a Bazel-rule-name-safe identifier mirroring
// genruleNameFor: "config.h" -> "gen_config_h".
func configureFileGenruleName(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	var sb strings.Builder
	sb.WriteString("gen_")
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		switch {
		case (c >= 'a' && c <= 'z'),
			(c >= 'A' && c <= 'Z'),
			(c >= '0' && c <= '9'),
			c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// configureFileCmd builds a shell command that writes the
// rendered bytes to $@. Encodes via base64 so any byte content
// (including embedded newlines, single-quotes, $, etc.) round-
// trips losslessly without shell-escaping concerns. The
// `mkdir -p $$(dirname $@)` prefix is harmless for top-level
// outputs and necessary for nested ones (e.g. subdir/version.h).
func configureFileCmd(rel string, body []byte) string {
	encoded := base64.StdEncoding.EncodeToString(body)
	return fmt.Sprintf("mkdir -p $$(dirname $@) && echo %s | base64 -d > $@", encoded)
}

// configureFileTags returns the cmake-codegen tag set for a
// configure_file emission. Distinguishes from
// CUSTOM_COMMAND-recovered genrules via cmake-codegen-driver=
// =configure_file, so audit queries can split the two cleanly.
func configureFileTags() []string {
	return []string{
		"cmake-codegen",
		"cmake-codegen-configure-file",
		"cmake-codegen-driver=configure_file",
	}
}
