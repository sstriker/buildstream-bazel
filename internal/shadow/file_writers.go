package shadow

import (
	"path/filepath"
	"strings"
)

// FileWriterCall is one trace-recorded file() WRITER — the commands that
// CREATE build-dir files at configure time outside every modeled
// recovery channel (configure_file, file(GENERATE), execute_process):
// file(WRITE/APPEND/TOUCH/COPY/COPY_FILE/DOWNLOAD). The expanded trace
// carries everything needed to tie a codemodel-referenced build-dir
// path back to its producer: WRITE/APPEND events carry the FULLY
// EXPANDED content, COPY/COPY_FILE the source path, DOWNLOAD the URL.
// The lower-side writer index (build_dir_writer_lift.go) consults these
// before falling back to the on-disk byte-bake.
type FileWriterCall struct {
	Op      string   // "write", "append", "touch", "copy", "copy_file", "download"
	Outputs []string // absolute output paths as the trace recorded them
	Sources []string // copy/copy_file source paths (absolute)
	Content string   // write/append payload (expanded; args concatenated, cmake semantics)
	URL     string   // download source URL
	Hash    string   // download EXPECTED_HASH as "<algo>=<value>" (e.g. "SHA256=abc…"); "" when absent
	File    string
	Line    int
}

// ExtractFileWriterCalls walks an EXPANDED trace and returns the file()
// writer calls that are part of the project's intent. Two buckets, mirroring
// the projectIO-rescue split execute_process gained in PR #752
// (converter/internal/lower/out_of_tree_execute_process.go:
// partitionOutOfTreeExec / outOfTreeExecTouchesProjectIO) and the
// build-tree-aware inProjectScope gate the sibling output-bearing extractors
// (classifyConfigureFile / classifyFileGenerate / classifyFileRename) use:
//
//	[13] IN-PROJECT (source tree OR build tree): a writer ISSUED from the
//	     project — a CMakeLists, or a generated+include()d recipe `.cmake`
//	     under the build dir (excluding cmake's try_compile scratch under
//	     <build>/CMakeFiles). A build-tree-issued file(COPY) recipe is real
//	     project intent; gating it on the strict source-tree check alone (the
//	     prior behavior) dropped it to a frozen on-disk bake instead of a
//	     regenerating cp-genrule — the same build-tree gap inProjectScope
//	     closed for configure_file.
//	[14] OUT-OF-TREE + projectIO RESCUE: a writer ISSUED from a cmake module
//	     on CMAKE_MODULE_PATH / a find_package prefix tree (outside both the
//	     source and build trees) but that TOUCHES the project's own I/O — it
//	     writes a build-dir OUTPUT or copies an in-source-tree SOURCE. Whose
//	     DATA the writer processes is project intent even when the helper that
//	     ISSUED it lives out of tree, exactly like an out-of-tree
//	     execute_process driving a tool on the project's own files. Such a
//	     writer is rescued (so an out-of-tree-module file(WRITE) of a
//	     project header lifts) rather than silently dropped. A purely
//	     out-of-tree writer with no project I/O (a module's own bookkeeping)
//	     stays dropped — the historical behavior for everything that reached
//	     the strict gate.
//
// buildRoot == "" falls back to the source-tree-only gate (no build-tree or
// projectIO rescue), preserving the prior behavior for callers — the
// non-expanded warm pass and script re-traces — that don't supply one.
//
// Unmodeled variants decline per call (a declined writer just leaves the
// on-disk bake as that path's recovery):
//   - file(COPY … FILES_MATCHING/PATTERN/REGEX …) filters contents;
//   - file(TOUCH_NOCREATE) creates nothing;
//   - file(DOWNLOAD <url>) with no destination file.
//
// NOTE: this is a standalone ParseTrace sweep (the JSON decode is
// memoized; the per-event walk is linear and re-runs per lower pass).
// Folding it into DecodeTrace's single dispatched walk is the known
// next step if trace-walk count ever shows up in profiles.
func ExtractFileWriterCalls(traceRaw []byte, sourceRoot, buildRoot string) []FileWriterCall {
	if sourceRoot != "" {
		sourceRoot = filepath.Clean(sourceRoot)
	}
	if buildRoot != "" {
		buildRoot = filepath.Clean(buildRoot)
	}
	var out []FileWriterCall
	for _, ev := range ParseTrace(traceRaw) {
		if !strings.EqualFold(ev.Cmd, "file") || len(ev.Args) < 2 {
			continue
		}
		call, ok := classifyFileWriter(ev)
		if !ok {
			continue
		}
		if sourceRoot != "" && !keepFileWriterCall(ev.File, call, sourceRoot, buildRoot) {
			continue
		}
		out = append(out, call)
	}
	return out
}

// keepFileWriterCall decides whether one classified writer is project intent.
// [13] in-project (source OR build tree, less try_compile scratch) — the same
// inProjectScope gate the sibling output-bearing extractors use; OR [14] an
// out-of-tree writer that touches the project's own I/O (projectIO rescue,
// mirroring outOfTreeExecTouchesProjectIO). See ExtractFileWriterCalls.
func keepFileWriterCall(file string, call FileWriterCall, sourceRoot, buildRoot string) bool {
	if inProjectScope(file, sourceRoot, buildRoot) {
		return true
	}
	// The out-of-tree projectIO rescue ([14]) is active only when a build root
	// is supplied — the main converter path. buildRoot == "" keeps the strict
	// source-tree-only gate for callers (the non-expanded warm pass, script
	// re-traces) that intentionally don't pass one.
	if buildRoot == "" {
		return false
	}
	return writerTouchesProjectIO(call, sourceRoot, buildRoot)
}

// writerTouchesProjectIO reports whether a writer's outputs or copy sources
// land under the project's source tree or build dir — the location-independent
// signal that an out-of-tree-issued writer is the PROJECT's own I/O (it writes
// a build-dir output, or copies an in-source-tree file). Mirrors lower's
// outOfTreeExecTouchesProjectIO: the issuing site says WHERE the call was
// written; the I/O says WHOSE data it processes. The downstream writer index
// (build_dir_writer_lift.go) only keeps build-dir outputs and verifies the
// composed bytes against the on-disk file before emitting, so a rescued
// out-of-tree writer that doesn't actually back a consumed build-dir path is
// inert — the rescue only ever UPGRADES a path that would otherwise bake.
//
// A path under <build>/CMakeFiles is cmake's own try_compile / compiler-id
// scratch (never a project build input), so it does NOT count as project I/O —
// the same exclusion inProjectScope applies to a scratch-ISSUED call (and
// hasScratchSegment applies to out-of-tree execute_process operands).
func writerTouchesProjectIO(call FileWriterCall, sourceRoot, buildRoot string) bool {
	for _, p := range call.Outputs {
		if pathIsProjectIO(p, sourceRoot, buildRoot) {
			return true
		}
	}
	for _, p := range call.Sources {
		if pathIsProjectIO(p, sourceRoot, buildRoot) {
			return true
		}
	}
	return false
}

// pathIsProjectIO reports whether the absolute path p lies inside the source
// tree or build dir but NOT in cmake's <build>/CMakeFiles scratch. The
// whole-component prefix match reuses inSourceTree (the same check applied to
// a call's issuing file); an empty root never matches.
func pathIsProjectIO(p, sourceRoot, buildRoot string) bool {
	if inSourceTree(p, sourceRoot) {
		return true
	}
	if inSourceTree(p, buildRoot) {
		return !strings.Contains(p[len(buildRoot):], "/CMakeFiles/")
	}
	return false
}

// classifyFileWriter parses one file() event into a writer call.
func classifyFileWriter(ev TraceEvent) (FileWriterCall, bool) {
	op := strings.ToUpper(ev.Args[0])
	call := FileWriterCall{File: ev.File, Line: ev.Line}
	switch op {
	case "WRITE", "APPEND":
		call.Op = strings.ToLower(op)
		call.Outputs = []string{ev.Args[1]}
		// cmake concatenates the content arguments directly.
		call.Content = strings.Join(ev.Args[2:], "")
		return call, true
	case "TOUCH":
		call.Op = "touch"
		call.Outputs = append(call.Outputs, ev.Args[1:]...)
		return call, len(call.Outputs) > 0
	case "COPY":
		// file(COPY <srcs…> DESTINATION <dir> [permission/filter opts]).
		// cmake's grammar puts the source files strictly BEFORE
		// DESTINATION and every option AFTER the dest dir, so collect
		// sources only from the pre-DESTINATION segment. That excludes
		// the FILE_PERMISSIONS / DIRECTORY_PERMISSIONS mode tokens
		// (OWNER_READ, GROUP_READ, …) categorically — they're plain
		// non-flag operands that would otherwise be mistaken for source
		// files and produce bogus `<dest>/OWNER_READ` outputs. Filtering
		// options (which change WHAT lands) still decline the lift.
		// DESTINATION outputs are <dir>/<basename(src)> per source
		// (directory sources copy recursively — the lower side
		// discriminates on disk and declines dirs).
		var srcs []string
		dest := ""
		sawDest := false
		for i := 1; i < len(ev.Args); i++ {
			a := ev.Args[i]
			switch strings.ToUpper(a) {
			case "DESTINATION":
				if i+1 < len(ev.Args) {
					dest = ev.Args[i+1]
					i++
				}
				sawDest = true
			case "FILES_MATCHING", "PATTERN", "REGEX", "EXCLUDE", "PERMISSIONS":
				return FileWriterCall{}, false
			default:
				// Sources precede DESTINATION; everything after it is an
				// option operand (permission mode tokens, flags) and is
				// not a source.
				if !sawDest && !strings.HasPrefix(a, "-") {
					srcs = append(srcs, a)
				}
			}
		}
		if dest == "" || len(srcs) == 0 {
			return FileWriterCall{}, false
		}
		call.Op = "copy"
		call.Sources = srcs
		for _, s := range srcs {
			call.Outputs = append(call.Outputs, filepath.Join(dest, filepath.Base(s)))
		}
		return call, true
	case "COPY_FILE":
		if len(ev.Args) < 3 {
			return FileWriterCall{}, false
		}
		call.Op = "copy_file"
		call.Sources = []string{ev.Args[1]}
		call.Outputs = []string{ev.Args[2]}
		return call, true
	case "DOWNLOAD":
		if len(ev.Args) < 3 || strings.Contains(ev.Args[2], "=") {
			// Destination-less form (status-only probe) or an option
			// token where the dst should be.
			return FileWriterCall{}, false
		}
		call.Op = "download"
		call.URL = ev.Args[1]
		call.Outputs = []string{ev.Args[2]}
		call.Hash = downloadExpectedHash(ev.Args[3:])
		return call, true
	}
	return FileWriterCall{}, false
}

// downloadExpectedHash scans file(DOWNLOAD) options for the integrity
// keyword and returns it as "<algo>=<value>" (the http_file lift maps
// SHA* to integrity/sha256). EXPECTED_HASH takes "<algo>=<value>";
// EXPECTED_MD5 takes a bare value. "" when neither is present.
func downloadExpectedHash(opts []string) string {
	for i := 0; i < len(opts); i++ {
		switch strings.ToUpper(opts[i]) {
		case "EXPECTED_HASH":
			if i+1 < len(opts) && strings.Contains(opts[i+1], "=") {
				return opts[i+1]
			}
		case "EXPECTED_MD5":
			if i+1 < len(opts) {
				return "MD5=" + opts[i+1]
			}
		}
	}
	return ""
}
