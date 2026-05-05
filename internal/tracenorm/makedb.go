package tracenorm

import (
	"bufio"
	"bytes"
	"regexp"
)

// FilterMakeDB drops the lines from a `make -np` dump that vary
// across runs of an otherwise-identical build. Mirrors the sed
// filter the kind:autotools install genrule applies inline (see
// cmd/write-a/handler_autotools_native.go's autotoolsConverterStep).
//
// Dropped categories:
//
//   - `#  Last modified <timestamp>` — file mtime drift even when
//     content is unchanged.
//   - `# (device X, inode Y): N files, ...` and
//     `# N files, M impossibilities in D directories` — vary with
//     filesystem state (.deps files etc.).
//   - `# Make data base, printed on <timestamp>` and
//     `# Finished Make data base on <timestamp>` — dry-run start /
//     end timestamps.
//
// Lifted into Go from the shell-side filter so trace-publish can
// re-apply it defensively. The pipeline genrule keeps its sed-side
// filter for in-action determinism; this is a second layer on the
// publisher path so a publisher running against an older /
// non-filtered make-db still lands a byte-stable AC entry.
func FilterMakeDB(body []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(body))
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if shouldDropMakeDBLine(line) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

var (
	mdLastModifiedRE      = regexp.MustCompile(`^#[ \t]+Last modified `)
	mdDeviceInodeRE       = regexp.MustCompile(`\(device [0-9]+, inode [0-9]+\): [0-9]+ files,`)
	mdFilesImpossibleRE   = regexp.MustCompile(`^# [0-9]+ files,.*impossibilities in `)
	mdMakeDataBasePrintRE = regexp.MustCompile(`^# Make data base, printed on `)
	mdFinishedMakeDataRE  = regexp.MustCompile(`^# Finished Make data base on `)
)

func shouldDropMakeDBLine(line []byte) bool {
	switch {
	case mdLastModifiedRE.Match(line):
		return true
	case mdDeviceInodeRE.Match(line):
		return true
	case mdFilesImpossibleRE.Match(line):
		return true
	case mdMakeDataBasePrintRE.Match(line):
		return true
	case mdFinishedMakeDataRE.Match(line):
		return true
	}
	return false
}
