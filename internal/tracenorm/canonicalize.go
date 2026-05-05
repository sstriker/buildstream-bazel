// Package tracenorm carries the byte-stable shaping the
// build-tracer + trace-publish + trace-lookup pipeline shares.
//
// Three concerns live here:
//
//   - Canonicalize: rewrite a raw strace-format trace into the form
//     downstream consumers (convert-element-autotools, the action
//     cache) hash. Strips pids, replaces gcc-driver mkstemps random
//     suffixes with stable counters, applies caller-supplied
//     prefix substitutions for sandbox mktemp paths.
//   - FilterMakeDB: drop the `make -np` lines whose bytes vary
//     across runs of an otherwise-identical build (mtime
//     diagnostics, file-count summaries, the print-data-base
//     timestamps). Same filter the autotools install genrule
//     applies inline via sed; lifted here so trace-publish can
//     do it in-process for cross-node determinism.
//   - SyntheticActionDigest: derive the AC rendezvous key from a
//     srckey. Both publisher (cmd/trace-publish) and consumer
//     (cmd/trace-lookup, the _trace_repo Bazel rule) compute the
//     same key from the same srckey, so a round-1 build's
//     publish lands at the key the round-2 lookup queries.
package tracenorm

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PrefixSub is one (from → to) substitution; Canonicalize replaces
// every occurrence of From in each trace line with To. Used for
// sandbox-path neutralization (INSTALL_ROOT, BUILD_ROOT, DEP_PREFIX).
type PrefixSub struct {
	From, To string
}

// Canonicalize rewrites a raw strace-format trace file at rawPath
// into a byte-stable form at outPath. See package doc for the
// transforms applied. prefixSubs is sorted longest-first internally
// so a substitution like `/sandbox/install_root` applies before a
// shorter `/sandbox` that's also in the list.
func Canonicalize(rawPath, outPath string, prefixSubs []PrefixSub) error {
	in, err := os.Open(rawPath)
	if err != nil {
		return fmt.Errorf("open raw trace: %w", err)
	}
	defer in.Close()

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create canonical trace: %w", err)
	}
	defer out.Close()

	w := bufio.NewWriter(out)
	defer w.Flush()

	c := newCanonicalizer(prefixSubs)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if _, err := io.WriteString(w, c.line(scanner.Text())+"\n"); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// CanonicalizeBytes applies the same transforms as Canonicalize but
// against an in-memory byte slice. Used by trace-publish, which
// already has the trace bytes in hand and re-canonicalizes
// defensively before publishing (so a publisher whose upstream
// build-tracer is older / produces non-canonical bytes still lands
// a stable digest).
func CanonicalizeBytes(raw []byte, prefixSubs []PrefixSub) []byte {
	c := newCanonicalizer(prefixSubs)
	var b strings.Builder
	b.Grow(len(raw))
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		b.WriteString(c.line(scanner.Text()))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

type canonicalizer struct {
	tempPaths   map[string]string
	tempCounter int
	prefixSubs  []PrefixSub
}

func newCanonicalizer(prefixSubs []PrefixSub) *canonicalizer {
	subs := append([]PrefixSub(nil), prefixSubs...)
	sort.Slice(subs, func(i, j int) bool {
		return len(subs[i].From) > len(subs[j].From)
	})
	return &canonicalizer{tempPaths: map[string]string{}, prefixSubs: subs}
}

// pidPrefixRE matches a leading `<pid>  ` (decimal pid, two
// spaces — strace's exact separator). Trailing 2-space matters
// because some argv strings contain bare numbers we don't want
// to strip.
var pidPrefixRE = regexp.MustCompile(`^\d+  `)

// gccTempPathRE matches the gcc-driver mkstemps shape: `/tmp/`
// + `cc` + 6+ alphanumerics + `.` + short alphabetic extension.
var gccTempPathRE = regexp.MustCompile(`/tmp/cc[A-Za-z0-9]{6,}\.[a-zA-Z]+`)

func (c *canonicalizer) line(s string) string {
	s = pidPrefixRE.ReplaceAllString(s, "")
	s = gccTempPathRE.ReplaceAllStringFunc(s, c.replaceTemp)
	for _, sub := range c.prefixSubs {
		s = strings.ReplaceAll(s, sub.From, sub.To)
	}
	return s
}

func (c *canonicalizer) replaceTemp(match string) string {
	if existing, ok := c.tempPaths[match]; ok {
		return existing
	}
	c.tempCounter++
	ext := filepath.Ext(match)
	canonical := "/tmp/cc" + strconv.Itoa(c.tempCounter) + ext
	c.tempPaths[match] = canonical
	return canonical
}
