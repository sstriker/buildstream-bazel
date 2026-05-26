package shadow

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/cmakeparse"
)

// ExtractPlatformConditionalSourcesTier2 closes the cross-
// platform half of #217's source-partition story (Tier 2,
// see ROADMAP.md → Later).
//
// Tier 1 (ExtractPlatformConditionalSources, above) walks the
// trace and attributes the sources cmake actually executed on
// this configure (the local platform's arm of any platform-
// conditional `if()` block). Tier 2 reopens the
// CMakeLists.txt at every recognized platform-predicate
// `if()` event the trace recorded and parses the OTHER arms —
// the bodies cmake didn't enter — to recover the sources
// those arms would have attached on a different platform.
// When bazel later reconfigures for that platform, the
// partition routes the recovered sources via
// `@platforms//os:*` so the produced BUILD.bazel is cross-
// platform sound.
//
// Returns the same record shape as Tier 1
// (PlatformConditionalSource) so the lower path can append
// the two slices and consume them uniformly.
//
// The scanner is deliberately conservative:
//
//   - Only `target_sources(<target> [PUBLIC|PRIVATE|INTERFACE]
//     <src>...)`, `add_library(<target> <src>...)`, and
//     `add_executable(<target> <src>...)` are recognized inside
//     skipped arms. Anything else in the arm body is ignored
//     silently.
//   - Recognized predicates match Tier 1's set
//     (selectKeyFromIfArgs). Unrecognized predicates leave the
//     arm intact for a later tier to revisit.
//   - Variable references / generator expressions in a source-
//     path argument cause the source to be SKIPPED with no
//     record emitted — we don't have a cmake variable
//     namespace at parse time. The intentional gap is
//     documented per source skip via the returned errors slice
//     (callers may surface it; the converter currently logs +
//     drops).
//   - cross-file `include()` directives inside a skipped arm
//     are NOT followed: parsing would need to interpret cmake's
//     include path-resolution rules. Skipping them is the
//     conservative choice.
//   - Function/macro definitions and foreach/while loops in a
//     skipped arm are not entered. Function bodies inside a
//     skipped arm don't contribute Tier-2 records.
//
// traceRaw is the cmake --trace-expand JSON stream (same as
// Tier 1's input). traceSourceRoot scopes which files we trust
// to parse — cmake-internal modules under /usr/share/cmake-*
// don't belong here. It's the source path recorded in the
// trace (cmake's `r.Codemodel.Paths.Source`), the same value
// Tier 1's sourceRoot argument carries.
//
// hostSourceRoot is the on-disk location of the same tree.
// When the trace was recorded on a different host (offline
// replay), `ev.File` in the trace points at the recording-host
// path; we need the local path to actually read the
// CMakeLists.txt. Pass "" to use the trace-recorded paths
// verbatim (live-cmake mode where the two coincide).
//
// knownTargets gates which `target_sources` / `add_library`
// / `add_executable` calls we trust to be addressing in-codebase
// targets. Same gating Tier 1 uses for the same reason.
//
// existing is the set of records Tier 1 already produced from
// the same trace. Tier 2 dedups against this so a source that
// Tier 1 attributed under one constraint doesn't get re-
// attributed by Tier 2 under another arm of the same if-block
// (the trace-entered arm and the parsed skipped arm don't
// overlap by construction, but defensive dedup keeps the
// invariant tight against future refactors).
func ExtractPlatformConditionalSourcesTier2(
	traceRaw []byte,
	traceSourceRoot string,
	hostSourceRoot string,
	knownTargets map[string]bool,
	existing []PlatformConditionalSource,
) []PlatformConditionalSource {
	out, _ := extractPlatformConditionalSourcesTier2(
		traceRaw, traceSourceRoot, hostSourceRoot, knownTargets, existing, defaultFS{},
	)
	return out
}

// fsReader abstracts file reading so tests can feed synthetic
// CMakeLists.txt bytes without touching disk. The production
// path uses os.ReadFile via defaultFS.
type fsReader interface {
	ReadFile(path string) ([]byte, error)
}

type defaultFS struct{}

func (defaultFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// UnsupportedTier2Reason describes why one Tier-2 candidate
// source was skipped without an emitted record. Surfaced for
// diagnostics; the converter currently logs these and drops.
//
// The shape mirrors converter/internal/failure's typed errors
// without the dependency — internal/shadow stays leaf in the
// dep graph so callers can wrap as they please.
type UnsupportedTier2Reason struct {
	File   string
	Line   int
	Source string
	Reason string
}

func (u UnsupportedTier2Reason) Error() string {
	return fmt.Sprintf("%s:%d: %s (source=%q)", u.File, u.Line, u.Reason, u.Source)
}

// ErrTier2UnknownVarRef is returned (wrapped in a
// UnsupportedTier2Reason) when a source path contains a
// `${...}` reference the Tier-2 driver doesn't know how to
// expand. Documented intentional gap.
var ErrTier2UnknownVarRef = errors.New("source path contains unsupported variable reference")

// ErrTier2GenexInSource is returned when a source path
// contains a `$<...>` generator expression. The Tier 3 path
// (not implemented in this PR) would evaluate these; the
// conservative refusal here keeps the partition honest.
var ErrTier2GenexInSource = errors.New("source path contains unsupported generator expression")

func extractPlatformConditionalSourcesTier2(
	traceRaw []byte,
	traceSourceRoot string,
	hostSourceRoot string,
	knownTargets map[string]bool,
	existing []PlatformConditionalSource,
	fs fsReader,
) ([]PlatformConditionalSource, []UnsupportedTier2Reason) {
	events := ParseTrace(traceRaw)
	// Locate the (file, line) of every platform-recognized
	// `if()` event in the trace whose file is inside the
	// source tree. cmake's --trace-expand records the `if`
	// event for both the entered and skipped arms — for the
	// entered arm we then see the body commands too, but the
	// `if` event itself records the predicate args
	// pre-evaluation in either case. Tier 2 uses the if-event
	// as the entry-point coordinate into the file.
	//
	// We need to parse each candidate CMakeLists exactly once;
	// stash already-parsed trees in a per-file cache.
	type fileEntry struct {
		nodes []cmakeparse.Node
		err   error
	}
	fileCache := map[string]fileEntry{}
	parseFile := func(tracePath string) ([]cmakeparse.Node, error) {
		hostPath := remapHostPath(tracePath, traceSourceRoot, hostSourceRoot)
		if e, ok := fileCache[hostPath]; ok {
			return e.nodes, e.err
		}
		raw, err := fs.ReadFile(hostPath)
		if err != nil {
			fileCache[hostPath] = fileEntry{err: err}
			return nil, err
		}
		nodes, err := cmakeparse.Parse(string(raw))
		fileCache[hostPath] = fileEntry{nodes: nodes, err: err}
		return nodes, err
	}

	// existingSrcs is the dedup key set: (target, source) the
	// Tier-1 pass already populated. Tier 2 won't re-emit the
	// same (target, source) under a different constraint.
	existingSrcs := map[string]map[string]bool{}
	for _, e := range existing {
		if existingSrcs[e.Target] == nil {
			existingSrcs[e.Target] = map[string]bool{}
		}
		existingSrcs[e.Target][e.Source] = true
	}

	// dedupTier2 tracks (target, source) pairs Tier 2 has
	// already emitted, so multiple skipped arms inside the
	// same if-block (e.g. `if(LINUX)…elseif(WIN32)…endif()` on
	// a Linux configure: both arms are skipped from Tier 2's
	// perspective if neither matches the configured platform —
	// in practice exactly one arm will have been entered, but
	// the dedup keeps the invariant honest under nested if-
	// blocks).
	dedup := map[string]map[string]bool{}
	addedTier2 := func(target, src string) bool {
		if dedup[target] == nil {
			dedup[target] = map[string]bool{}
		}
		if dedup[target][src] {
			return false
		}
		dedup[target][src] = true
		return true
	}

	// Track which (file, line) `if` events we've already
	// processed so a deeply-nested elseif/else chain doesn't
	// drive us to re-walk the same if-block. cmake emits the
	// `if` event once per evaluation; multiple events at the
	// same (file, line) only occur under loop bodies, which
	// we don't follow.
	processedIfs := map[string]bool{}

	var out []PlatformConditionalSource
	var unsupp []UnsupportedTier2Reason

	for _, ev := range events {
		if !strings.EqualFold(ev.Cmd, "if") {
			continue
		}
		if !inSourceTree(ev.File, traceSourceRoot) {
			continue
		}
		// Only descend on recognized predicates — the trace
		// records every if() event but we only emit Tier-2
		// records for the arms of platform-predicate-shaped
		// blocks. Unrecognized shapes fall through unchanged.
		if selectKeyFromIfArgs(ev.Args) == "" {
			continue
		}
		evKey := fmt.Sprintf("%s:%d", ev.File, ev.Line)
		if processedIfs[evKey] {
			continue
		}
		processedIfs[evKey] = true

		nodes, err := parseFile(ev.File)
		if err != nil {
			// Parse error / read error: log + drop. We can't
			// reasonably recover; the file remains Tier-1 only.
			continue
		}
		// Find the if-block in the parsed tree whose StartLine
		// matches the trace event. If the parser saw a
		// different shape (e.g. the file changed between
		// configure and us reading), bail.
		block := findIfBlockAt(nodes, ev.Line)
		if block == nil {
			continue
		}
		// For each arm of the block: if the predicate maps to
		// a recognized constraint, walk its body and emit
		// records for every source-attaching call.
		for _, arm := range block.Arms {
			if arm.Kind == "else" {
				// `else()`'s predicate is "not any of the
				// above" — not single-positive-constraint
				// expressible. Skip; matches Tier 1's policy.
				continue
			}
			key := selectKeyFromIfArgs(unquoteAll(arm.PredicateArgs))
			if key == "" {
				continue
			}
			// Skip the arm if the trace shows cmake entered it
			// — Tier 1 already attributed those sources, and
			// re-attributing would be at best a no-op (the
			// constraint is the same) and at worst a dedup
			// surprise. The arm range is [arm.StartLine,
			// arm.EndLine].
			if traceEnteredArm(events, ev.File, arm.StartLine, arm.EndLine) {
				continue
			}
			collectFromArm(arm.Body, ev.File, traceSourceRoot, knownTargets, key, addedTier2, existingSrcs, &out, &unsupp)
		}
	}
	return out, unsupp
}

// remapHostPath translates a trace-recorded absolute path
// rooted at traceSourceRoot to the equivalent path under
// hostSourceRoot. Returns the input unchanged when:
//
//   - hostSourceRoot is empty (caller doesn't need remap).
//   - tracePath isn't under traceSourceRoot (would escape the
//     remap; fall back to the literal path which lets the
//     reader fail loudly).
//
// Used so an offline-replay test fixture (where the recorded
// trace's `file` paths are e.g. `/recording-host/src/...` but
// the actual CMakeLists.txt lives at
// `/local-fixture/src/...`) still finds the right file.
func remapHostPath(tracePath, traceSourceRoot, hostSourceRoot string) string {
	if hostSourceRoot == "" || traceSourceRoot == "" {
		return tracePath
	}
	if traceSourceRoot == hostSourceRoot {
		return tracePath
	}
	if !strings.HasPrefix(tracePath, traceSourceRoot) {
		return tracePath
	}
	tail := tracePath[len(traceSourceRoot):]
	return hostSourceRoot + tail
}

// findIfBlockAt locates the IfBlock in a flat parse-tree slice
// whose StartLine equals startLine. Searches one level deep
// into nested IfBlock arms — handles the common case of a
// platform conditional inside a `if(BUILD_TESTING)` outer
// guard. Deeper nesting requires recursion; we cap at one
// level here because Tier 2's coverage doesn't need more, and
// deeper trees just mean we miss those records (Tier 1 still
// covers the entered arm; the skipped-arm recovery is best-
// effort).
//
// Returns nil when no block matches — defensive against the
// CMakeLists having been edited between the recorded configure
// and the parse, or against parser shape mismatches the spec
// doesn't cover.
func findIfBlockAt(nodes []cmakeparse.Node, startLine int) *cmakeparse.IfBlock {
	for i := range nodes {
		if blk := nodes[i].If; blk != nil {
			if blk.StartLine == startLine {
				return blk
			}
			// Recurse into each arm's body.
			for j := range blk.Arms {
				if found := findIfBlockAt(blk.Arms[j].Body, startLine); found != nil {
					return found
				}
			}
		}
	}
	return nil
}

// traceEnteredArm reports whether the trace shows cmake
// executed at least one command (any cmd) inside the line
// range [start+1, end-1] of the given file. The start and end
// lines themselves are the arm-delimiter lines (if / elseif /
// else / endif); the body is strictly between them.
//
// Used to distinguish "this is the arm cmake entered, Tier 1
// owns it" from "this is a skipped arm, Tier 2 should walk".
func traceEnteredArm(events []TraceEvent, file string, start, end int) bool {
	for _, ev := range events {
		if ev.File != file {
			continue
		}
		if ev.Line > start && ev.Line < end {
			return true
		}
	}
	return false
}

// collectFromArm walks an arm's body, emitting Tier-2 records
// for every recognized source-attaching call. Nested if-blocks
// inside a skipped arm aren't walked: we don't have a clean
// way to predict which inner arms cmake would have entered
// without simulating the platform's variable namespace.
// Conservative: inner records are deferred (acknowledged Tier
// 2 gap; Tier 3 could revisit).
//
// Variable references / generator expressions in source paths
// surface as UnsupportedTier2Reason entries rather than
// silently emitting wrong sources. Pure relative paths are
// resolved against the trace event's file directory the same
// way Tier 1 resolves them.
func collectFromArm(
	body []cmakeparse.Node,
	file, sourceRoot string,
	knownTargets map[string]bool,
	key string,
	addedTier2 func(target, src string) bool,
	existingSrcs map[string]map[string]bool,
	out *[]PlatformConditionalSource,
	unsupp *[]UnsupportedTier2Reason,
) {
	for _, n := range body {
		if n.Command == nil {
			continue
		}
		cmd := n.Command
		// Synthesize a TraceEvent-shaped arg vector to reuse
		// Tier 1's sourcesFromAddOrTargetCall helper. The
		// helper expects Args as the post-trace-decode strings
		// (quoted args already unquoted). Use Unquote to match.
		args := unquoteAll(cmd.Args)
		ev := TraceEvent{File: file, Line: cmd.Line, Cmd: cmd.Name, Args: args}
		target, srcs, ok := sourcesFromAddOrTargetCall(ev)
		if !ok {
			continue
		}
		if !knownTargets[target] {
			continue
		}
		for _, src := range srcs {
			// Refuse `${...}` and `$<...>` shapes — the parser
			// preserves them as opaque tokens in cmd.Args, so
			// they survive Unquote intact.
			if strings.Contains(src, "$<") {
				*unsupp = append(*unsupp, UnsupportedTier2Reason{
					File:   file,
					Line:   cmd.Line,
					Source: src,
					Reason: ErrTier2GenexInSource.Error(),
				})
				continue
			}
			if strings.Contains(src, "${") {
				*unsupp = append(*unsupp, UnsupportedTier2Reason{
					File:   file,
					Line:   cmd.Line,
					Source: src,
					Reason: ErrTier2UnknownVarRef.Error(),
				})
				continue
			}
			rel := resolveSourceRelative(src, file, sourceRoot)
			if rel == "" {
				continue
			}
			if existingSrcs[target] != nil && existingSrcs[target][rel] {
				continue
			}
			if !addedTier2(target, rel) {
				continue
			}
			*out = append(*out, PlatformConditionalSource{
				Target:    target,
				Source:    rel,
				SelectKey: key,
			})
		}
	}
}

// unquoteAll strips a single layer of double-quotes from each
// element of args, applying cmakeparse.Unquote's escape rules.
// Tier-1 receives the post-unquote view (the trace decoder
// already unquoted before serializing); Tier-2 parser output
// preserves the quotes, so this aligns the two so they can
// share sourcesFromAddOrTargetCall + selectKeyFromIfArgs.
func unquoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = cmakeparse.Unquote(a)
	}
	return out
}
