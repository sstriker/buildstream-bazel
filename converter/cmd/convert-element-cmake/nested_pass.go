package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// runNestedCMakePass is the nested-cmake warm second pass (see
// lower/nested_cmake.go for the full design): stage File API queries
// into each nested build dir pass 1 detected, re-run the WARM outer
// configure (execute_process re-runs the nested cmake, which now writes
// a codemodel reply), and harvest each nested reply + ninja graph for
// the merge re-lower. Failures degrade to nil — the not-lifted warning
// + structured todo stay loud in the final pass.
func runNestedCMakePass(ctx context.Context, a cli.Args, hostBuildDir string, sink map[string]string) []lower.NestedBuildInput {
	rels := make([]string, 0, len(sink))
	for rel := range sink {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	staged := 0
	for _, rel := range rels {
		nb := filepath.Join(hostBuildDir, filepath.FromSlash(rel))
		if st, err := os.Stat(nb); err != nil || !st.IsDir() {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested cmake build dir %s not on disk; skipping its lift.\n", rel)
			continue
		}
		if err := cmakerun.StageFileAPIQueries(nb); err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: staging File API queries into nested build %s failed (%v); skipping its lift.\n", rel, err)
			continue
		}
		staged++
	}
	if staged == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "convert-element-cmake: %d nested cmake build(s) detected; running warm second configure pass to capture their File API replies.\n", staged)
	// Warm outer reconfigure with NO extra hooks (same discipline as the
	// stamp second pass): the nested execute_process re-runs, and the
	// nested cmake sees the staged queries.
	if _, cfgErr := cmakerun.Configure(ctx, cmakerun.Options{
		SourceRoot:         a.SourceRoot,
		BuildDir:           hostBuildDir,
		PrefixDir:          a.PrefixDir,
		ToolchainCMakeFile: a.ToolchainCMakeFile,
		BuildType:          a.BuildType,
		BuildTypes:         a.BuildTypes,
		Stdout:             os.Stderr,
		Stderr:             os.Stderr,
	}); cfgErr != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: warm second configure (nested cmake) failed (%v); nested builds stay unlifted.\n", cfgErr)
		return nil
	}
	var out []lower.NestedBuildInput
	for _, rel := range rels {
		nb := filepath.Join(hostBuildDir, filepath.FromSlash(rel))
		replyDir := filepath.Join(nb, ".cmake", "api", "v1", "reply")
		r, err := fileapi.Load(replyDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: nested build %s produced no loadable File API reply (%v); staying unlifted.\n", rel, err)
			continue
		}
		// The nested ninja graph is the exclusion set for the
		// generated-header bake; nil (non-ninja nested generator,
		// parse failure) degrades to baking more than necessary
		// rather than failing the lift.
		g, _ := ninja.ParseFile(filepath.Join(nb, "build.ninja"))
		out = append(out, lower.NestedBuildInput{
			BuildRel:     rel,
			SrcDir:       sink[rel],
			Reply:        r,
			Graph:        g,
			HostBuildDir: nb,
		})
	}
	return out
}
