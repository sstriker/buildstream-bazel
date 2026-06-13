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
	File    string
	Line    int
}

// ExtractFileWriterCalls walks an EXPANDED trace and returns the file()
// writer calls from the project's own tree (cmake's modules write
// plenty of bookkeeping; sourceRoot gating mirrors the sibling
// extractors). Unmodeled variants decline per call (a declined writer
// just leaves the on-disk bake as that path's recovery):
//   - file(COPY … FILES_MATCHING/PATTERN/REGEX …) filters contents;
//   - file(TOUCH_NOCREATE) creates nothing;
//   - file(DOWNLOAD <url>) with no destination file.
func ExtractFileWriterCalls(traceRaw []byte, sourceRoot string) []FileWriterCall {
	if sourceRoot != "" {
		sourceRoot = filepath.Clean(sourceRoot)
	}
	var out []FileWriterCall
	for _, ev := range ParseTrace(traceRaw) {
		if !strings.EqualFold(ev.Cmd, "file") || len(ev.Args) < 2 {
			continue
		}
		if sourceRoot != "" && !inSourceTree(ev.File, sourceRoot) {
			continue
		}
		if call, ok := classifyFileWriter(ev); ok {
			out = append(out, call)
		}
	}
	return out
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
		// Filtering options change WHAT lands; decline those rather
		// than model them. DESTINATION outputs are <dir>/<basename(src)>
		// per source (directory sources copy recursively — the lower
		// side discriminates on disk and declines dirs).
		var srcs []string
		dest := ""
		for i := 1; i < len(ev.Args); i++ {
			a := ev.Args[i]
			switch strings.ToUpper(a) {
			case "DESTINATION":
				if i+1 < len(ev.Args) {
					dest = ev.Args[i+1]
					i++
				}
			case "FILES_MATCHING", "PATTERN", "REGEX", "EXCLUDE", "PERMISSIONS":
				return FileWriterCall{}, false
			case "FILE_PERMISSIONS", "DIRECTORY_PERMISSIONS", "NO_SOURCE_PERMISSIONS", "USE_SOURCE_PERMISSIONS", "FOLLOW_SYMLINK_CHAIN":
				// Permission-only options don't change content; the
				// FILE_/DIRECTORY_PERMISSIONS forms consume their mode
				// tokens, which are plain (non-path) operands the source
				// scan below ignores anyway.
			default:
				if !strings.HasPrefix(a, "-") {
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
		return call, true
	}
	return FileWriterCall{}, false
}
