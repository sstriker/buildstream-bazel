package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// runPerConfigBakes is the orchestration half of the per-build-type
// configure_file bake (--per-config-bake; the fold half is
// lower.ApplyPerConfigBakes, the render half emit's write_file content
// select()).
//
// Trigger: multi-config mode (≥2 --build-types), a live build dir
// (source-root mode), ≥1 recovered write_file bake, and — under the default
// "auto" — the pass-1 trace showing the project's OWN files consulting
// CMAKE_BUILD_TYPE (shadow.ReadsBuildType); "on" skips the detection, "off"
// disables. The passes are COLD (one fresh single-config configure per build
// type into a sibling scratch dir): the multi-config build dir can't be
// reused warm because cmake refuses a generator switch in an existing build
// dir, and CMAKE_BUILD_TYPE-derived logic only runs under a single-config
// generator at all.
//
// Every failure path degrades to the pass-1 result (the multi-config body
// for all arms) with a stderr warning — exactly the output the convert
// produces without the feature.
func runPerConfigBakes(ctx context.Context, a cli.Args, hostBuildDir string, traceRaw []byte, pkg *ir.Package) {
	if pkg == nil || hostBuildDir == "" || len(a.BuildTypes) < 2 {
		return
	}
	// Trace event paths are absolute; --source-root may be given relative
	// (the doc'd convention is absolute, but nothing normalizes it), so
	// absolutize before the in-source-tree filter or detection never fires.
	srcRoot := a.SourceRoot
	if abs, absErr := filepath.Abs(srcRoot); absErr == nil {
		srcRoot = abs
	}
	switch strings.ToLower(a.PerConfigBake) {
	case "off", "false", "0", "no":
		return
	case "on", "force":
	default: // auto
		if len(traceRaw) == 0 || !shadow.ReadsBuildType(traceRaw, srcRoot) {
			return
		}
	}
	var outs []string
	for i := range pkg.Targets {
		if pkg.Targets[i].Kind == ir.KindWriteFile && pkg.Targets[i].WriteFileOut != "" {
			outs = append(outs, pkg.Targets[i].WriteFileOut)
		}
	}
	if len(outs) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "convert-element-cmake: multi-config + project consults CMAKE_BUILD_TYPE; running %d per-config configure pass(es) to capture build-type-dependent configure_file bodies.\n", len(a.BuildTypes))
	scratch := func(cfg string) string { return hostBuildDir + "-cfg-" + sanitizeConfigName(cfg) }
	defer func() {
		for _, cfg := range a.BuildTypes {
			os.RemoveAll(scratch(cfg))
		}
	}()
	// The per-config configures are independent cmake processes in disjoint
	// scratch dirs — run them CONCURRENTLY (the conversion-latency profile
	// showed converter wall time is dominated by serial subprocess waits;
	// these are the largest single block on CMAKE_BUILD_TYPE-reading
	// projects, a full cold configure each). Wall cost drops from sum to
	// max. Any failure degrades the WHOLE feature to the pass-1 body, same
	// contract as the serial form.
	// Each configure's output is buffered per config (concurrent streaming
	// to stderr would interleave into garbage) and dumped ONLY on failure —
	// success stays silent, failure keeps cmake's own diagnostic alongside
	// the Go-level error.
	type cfgResult struct {
		cfg string
		err error
		out bytes.Buffer
	}
	results := make([]cfgResult, len(a.BuildTypes))
	var wg sync.WaitGroup
	for i, cfg := range a.BuildTypes {
		results[i].cfg = cfg
		wg.Add(1)
		go func(i int, cfg string) {
			defer wg.Done()
			_, cfgErr := cmakerun.Configure(ctx, cmakerun.Options{
				SourceRoot:         a.SourceRoot,
				BuildDir:           scratch(cfg),
				PrefixDir:          a.PrefixDir,
				ToolchainCMakeFile: a.ToolchainCMakeFile,
				BuildType:          cfg,
				ExtraCacheVars:     cmakeDefinesToMap(a.CmakeDefines),
				Stdout:             &results[i].out,
				Stderr:             &results[i].out,
			})
			results[i].err = cfgErr
		}(i, cfg)
	}
	wg.Wait()
	for i := range results {
		if results[i].err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: per-config configure (%s) failed (%v); keeping the multi-config body for all arms. cmake output:\n", results[i].cfg, results[i].err)
			_, _ = os.Stderr.Write(results[i].out.Bytes())
			return
		}
	}
	bakes := map[string]map[string][]byte{}
	for _, cfg := range a.BuildTypes {
		cfgDir := scratch(cfg)
		for _, rel := range outs {
			body, readErr := os.ReadFile(filepath.Join(cfgDir, filepath.FromSlash(rel)))
			if readErr != nil {
				// Not produced under this config (config-gated
				// configure_file); per-output coverage check below drops it.
				continue
			}
			if bakes[rel] == nil {
				bakes[rel] = map[string][]byte{}
			}
			bakes[rel][cfg] = body
		}
	}
	// An output missing under SOME config can't form an honest select — keep
	// its single multi-config body instead.
	for rel, m := range bakes {
		if len(m) != len(a.BuildTypes) {
			delete(bakes, rel)
		}
	}
	if applied := lower.ApplyPerConfigBakes(pkg, bakes); len(applied) > 0 {
		sort.Strings(applied)
		fmt.Fprintf(os.Stderr, "convert-element-cmake: per-config bake: %d write_file body(ies) differ across build types; emitted content select() arms: %s\n", len(applied), strings.Join(applied, ", "))
	}
}

// sanitizeConfigName maps a cmake config name onto a filesystem-safe
// scratch-dir suffix.
func sanitizeConfigName(cfg string) string {
	var b strings.Builder
	for _, r := range cfg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
