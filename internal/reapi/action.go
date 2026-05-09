// Package reapi builds REAPI Action / Command / ActionResult shapes
// for one convert-element conversion. Local execution stays in M5; this
// package gives the cache layer (and M3b's eventual remote execution)
// the wire-stable shapes they consume.
//
// Canonical input-root layout (paths are RELATIVE to the action's
// working directory):
//
//	source/                ← shadow tree (the converter's --source-root)
//	prefix/                ← synth-prefix (--prefix-dir, optional)
//	imports.json           ← imports manifest (--imports-manifest, optional)
//	toolchain.cmake        ← derive-toolchain output (--toolchain-cmake-file, optional)
//	bin/convert-element    ← the converter binary, +x
//
// Canonical output paths (declared in Command.output_paths):
//
//	BUILD.bazel
//	cmake-config           (directory; the bundle)
//	failure.json           (only on Tier-1 failure)
//	read_paths.json
//	timings.json           (per-phase wall-clock timings)
//
// Outputs sit at the top level of the action's working directory, NOT
// nested under the input layout — they don't exist before the run
// (which is exactly what input/output separation requires) and the
// orchestrator's existing per-element layout already uses these paths.
//
// The argv inside the action references those paths; the host paths
// the orchestrator actually uses are NOT part of the Action — that's
// what lets two orchestrators on different machines compute identical
// Action digests.
package reapi

import (
	"fmt"
	"os"
	"sort"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
)

// PlatformProperty is one (name, value) pair in the Action's platform.
type PlatformProperty struct {
	Name  string
	Value string
}

// Inputs bundles every host path that contributes to the input root,
// plus the platform properties to encode in the Command.
type Inputs struct {
	// ShadowDir is the local path to the converter's source-root
	// (the shadow tree). Required.
	ShadowDir string

	// ImportsManifest is the local path to the imports.json file, or
	// "" if this element has no cross-element deps.
	ImportsManifest string

	// PrefixDir is the local path to the synth-prefix tree, or "" if
	// this element has no cross-element deps.
	PrefixDir string

	// ToolchainCMakeFile is the optional path to a CMake toolchain
	// file (typically derive-toolchain's toolchain.cmake) that
	// pre-populates compiler-detection cache so cmake skips its
	// probe. When set, the file lands at toolchain.cmake in the
	// input root and the converter's argv passes
	// --toolchain-cmake-file=toolchain.cmake.
	ToolchainCMakeFile string

	// ConverterBin is the local path to the convert-element binary.
	// Required.
	ConverterBin string

	// Platform encodes Linux + arch + tool versions. Two converters
	// with different cmake/ninja/bwrap pins produce different Action
	// digests by design.
	Platform []PlatformProperty

	// Timeout, when non-zero, is encoded into Action.timeout so
	// remote workers enforce a hard cap on this conversion. Zero
	// leaves Action.timeout unset (worker-default applies).
	Timeout time.Duration

	// EnvVars are encoded into Command.EnvironmentVariables. They
	// land in the worker's environment when the Action runs. Used
	// by the orchestrator to plumb per-element identity (e.g.
	// ORCHESTRATOR_ELEMENT_NAME) into the converter without
	// changing argv. Names are sorted into the proto for stable
	// digests across hosts.
	EnvVars map[string]string

	// CollectToolchainSignal, when true, requests that
	// convert-element copy its cmake File API reply directory into
	// the action's output tree at <pathOutToolchainSignal>. The
	// orchestrator uses this when it intends to feed
	// unify-toolchains' --element-signal flag downstream — every
	// element's reply contributes any builtin-include / sysroot
	// fact the dedicated toolchain probe missed.
	CollectToolchainSignal bool
}

// BuiltAction is the result of Build: a complete REAPI Action plus
// every byte of the input root, ready for upload to CAS and ActionCache
// lookup. Output paths are exposed so step 6 can synthesize an
// ActionResult after a local execution.
type BuiltAction struct {
	Action        *repb.Action
	ActionDigest  *cas.Digest
	ActionBlob    []byte // serialized Action proto, ready for CAS upload
	Command       *repb.Command
	CommandDigest *cas.Digest
	CommandBlob   []byte

	// InputRoot bundles every Directory proto and file blob that
	// participates in the action's input.
	InputRoot *InputRoot

	// OutputPaths mirrors Command.output_paths (and the converter's
	// argv) for callers that need to walk the local output tree.
	OutputPaths []string
}

// InputRoot is the flattened representation of every blob that the
// action's input depends on. It carries enough information for a caller
// to upload every byte to CAS without re-walking the source.
type InputRoot struct {
	Root        *repb.Directory
	RootDigest  *cas.Digest
	Directories map[string]*repb.Directory // keyed by digest hash
	Files       map[string]string          // file digest hash -> local path
}

// canonical input-root paths.
const (
	pathSource         = "source"
	pathPrefix         = "prefix"
	pathImports        = "imports.json"
	pathToolchainCMake = "toolchain.cmake"
	pathBinDir         = "bin"
	pathConverter      = "convert-element"
	pathOutBuild       = "BUILD.bazel"
	pathOutBundle      = "cmake-config"
	pathOutFailure     = "failure.json"
	pathOutReads       = "read_paths.json"
	pathOutTimings     = "timings.json"

	// pathOutToolchainSignal is the action's output directory for
	// per-element fileapi reply contents (Stage 6). Activated by
	// Inputs.CollectToolchainSignal. The unifier (Stage 5) consumes
	// the resulting trees via --element-signal.
	pathOutToolchainSignal = "toolchain-signal"
)

// Build constructs the Action / Command / InputRoot for one conversion.
// The returned BuiltAction is byte-stable across hosts as long as the
// input contents are byte-stable.
func Build(in Inputs) (*BuiltAction, error) {
	if in.ShadowDir == "" {
		return nil, fmt.Errorf("reapi.Build: ShadowDir required")
	}
	if in.ConverterBin == "" {
		return nil, fmt.Errorf("reapi.Build: ConverterBin required")
	}

	ir, err := buildInputRoot(in)
	if err != nil {
		return nil, err
	}

	cmd := buildCommand(in)
	cmdDigest, cmdBlob, err := cas.DigestProto(cmd)
	if err != nil {
		return nil, fmt.Errorf("reapi.Build: digest command: %w", err)
	}

	action := &repb.Action{
		CommandDigest:   cmdDigest,
		InputRootDigest: ir.RootDigest,
		DoNotCache:      false,
		Platform:        cmd.Platform,
	}
	if in.Timeout > 0 {
		action.Timeout = durationpb.New(in.Timeout)
	}
	actionDigest, actionBlob, err := cas.DigestProto(action)
	if err != nil {
		return nil, fmt.Errorf("reapi.Build: digest action: %w", err)
	}

	return &BuiltAction{
		Action:        action,
		ActionDigest:  actionDigest,
		ActionBlob:    actionBlob,
		Command:       cmd,
		CommandDigest: cmdDigest,
		CommandBlob:   cmdBlob,
		InputRoot:     ir,
		OutputPaths:   append([]string(nil), cmd.OutputPaths...),
	}, nil
}

func buildCommand(in Inputs) *repb.Command {
	args := []string{
		pathBinDir + "/" + pathConverter,
		"--source-root", pathSource,
		"--out-build", pathOutBuild,
		"--out-bundle-dir", pathOutBundle,
		"--out-failure", pathOutFailure,
		"--out-read-paths", pathOutReads,
		"--out-timings", pathOutTimings,
	}
	if in.ImportsManifest != "" {
		args = append(args, "--imports-manifest", pathImports)
	}
	if in.PrefixDir != "" {
		args = append(args, "--prefix-dir", pathPrefix)
	}
	if in.ToolchainCMakeFile != "" {
		args = append(args, "--toolchain-cmake-file", pathToolchainCMake)
	}
	if in.CollectToolchainSignal {
		args = append(args, "--out-toolchain-signal-dir", pathOutToolchainSignal)
	}

	platform := &repb.Platform{}
	props := append([]PlatformProperty(nil), in.Platform...)
	sort.Slice(props, func(i, j int) bool { return props[i].Name < props[j].Name })
	for _, p := range props {
		platform.Properties = append(platform.Properties, &repb.Platform_Property{
			Name:  p.Name,
			Value: p.Value,
		})
	}

	envKeys := make([]string, 0, len(in.EnvVars))
	for k := range in.EnvVars {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	envVars := make([]*repb.Command_EnvironmentVariable, 0, len(envKeys))
	for _, k := range envKeys {
		envVars = append(envVars, &repb.Command_EnvironmentVariable{
			Name:  k,
			Value: in.EnvVars[k],
		})
	}

	outputs := []string{
		pathOutBuild,
		pathOutBundle,
		pathOutFailure,
		pathOutReads,
		pathOutTimings,
	}
	if in.CollectToolchainSignal {
		outputs = append(outputs, pathOutToolchainSignal)
	}

	return &repb.Command{
		Arguments:            args,
		EnvironmentVariables: envVars,
		OutputPaths:          outputs,
		WorkingDirectory:     "",
		Platform:             platform,
	}
}

// buildInputRoot composes the canonical input-root Directory by
// grafting subtrees (shadow, prefix) and adding loose files (imports,
// converter binary) under their canonical names.
func buildInputRoot(in Inputs) (*InputRoot, error) {
	ir := &InputRoot{
		Directories: make(map[string]*repb.Directory),
		Files:       make(map[string]string),
	}
	root := &repb.Directory{}

	// source/ subtree
	if err := graftSubtree(ir, root, pathSource, in.ShadowDir); err != nil {
		return nil, err
	}

	// prefix/ subtree (optional)
	if in.PrefixDir != "" {
		if err := graftSubtree(ir, root, pathPrefix, in.PrefixDir); err != nil {
			return nil, err
		}
	}

	// imports.json (optional, top-level file)
	if in.ImportsManifest != "" {
		d, err := cas.DigestFile(in.ImportsManifest)
		if err != nil {
			return nil, fmt.Errorf("reapi: digest imports manifest: %w", err)
		}
		ir.Files[d.Hash] = in.ImportsManifest
		root.Files = append(root.Files, &repb.FileNode{
			Name:   pathImports,
			Digest: d,
		})
	}

	// toolchain.cmake (optional, top-level file). Lands in the
	// input root so remote workers see it; argv references the
	// in-action path. The action's input-root digest changes when
	// the toolchain file does, so a host bumping its toolchain
	// invalidates every cache entry derived from the prior one —
	// exactly the desired cache-coherence property.
	if in.ToolchainCMakeFile != "" {
		d, err := cas.DigestFile(in.ToolchainCMakeFile)
		if err != nil {
			return nil, fmt.Errorf("reapi: digest toolchain cmake file: %w", err)
		}
		ir.Files[d.Hash] = in.ToolchainCMakeFile
		root.Files = append(root.Files, &repb.FileNode{
			Name:   pathToolchainCMake,
			Digest: d,
		})
	}

	// bin/convert-element
	binDir, binDigest, err := buildBinDir(ir, in.ConverterBin)
	if err != nil {
		return nil, err
	}
	_ = binDir
	root.Directories = append(root.Directories, &repb.DirectoryNode{
		Name:   pathBinDir,
		Digest: binDigest,
	})

	cas.SortDirectory(root)
	rootDigest, _, err := cas.DigestProto(root)
	if err != nil {
		return nil, fmt.Errorf("reapi: digest input root: %w", err)
	}
	ir.Directories[rootDigest.Hash] = root
	ir.Root = root
	ir.RootDigest = rootDigest
	return ir, nil
}

// graftSubtree packs the local directory at hostPath, merges all of its
// blobs and Directory protos into ir, and adds a DirectoryNode entry on
// parent referencing the subtree's root by digest.
func graftSubtree(ir *InputRoot, parent *repb.Directory, name, hostPath string) error {
	if _, err := os.Stat(hostPath); err != nil {
		return fmt.Errorf("reapi: stat %s subtree %s: %w", name, hostPath, err)
	}
	tree, err := cas.PackDir(hostPath)
	if err != nil {
		return fmt.Errorf("reapi: pack %s subtree: %w", name, err)
	}
	for h, d := range tree.Directories {
		ir.Directories[h] = d
	}
	for h, p := range tree.Files {
		ir.Files[h] = p
	}
	parent.Directories = append(parent.Directories, &repb.DirectoryNode{
		Name:   name,
		Digest: tree.RootDigest,
	})
	return nil
}

// buildBinDir creates the bin/ Directory holding only convert-element
// (executable). Returns the directory proto and its digest.
func buildBinDir(ir *InputRoot, converterBin string) (*repb.Directory, *cas.Digest, error) {
	d, err := cas.DigestFile(converterBin)
	if err != nil {
		return nil, nil, fmt.Errorf("reapi: digest converter bin: %w", err)
	}
	ir.Files[d.Hash] = converterBin

	binDir := &repb.Directory{
		Files: []*repb.FileNode{
			{Name: pathConverter, Digest: d, IsExecutable: true},
		},
	}
	cas.SortDirectory(binDir)
	binDigest, _, err := cas.DigestProto(binDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reapi: digest bin dir: %w", err)
	}
	ir.Directories[binDigest.Hash] = binDir
	return binDir, binDigest, nil
}
