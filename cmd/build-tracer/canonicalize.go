package main

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

// canonicalize rewrites a raw strace-format trace into a form
// that's byte-stable across runs of the same build:
//
//   - Strips the leading pid prefix from each line. The pid is
//     a kernel-assigned process identifier that varies between
//     runs even when the build is otherwise identical.
//   - Replaces gcc-driver temp paths (`/tmp/cc<random>.<ext>`,
//     produced by mkstemps inside `cc1`/`as`/`collect2`) with
//     stable counter-based placeholders (`/tmp/cc<N>.<ext>`).
//     Same path within a single trace gets the same placeholder
//     so cross-event correlation (compile output → archive
//     input) still resolves.
//   - Applies caller-supplied prefix substitutions
//     (`prefixSubs`). Used to neutralize sandbox-private
//     mktemp paths (INSTALL_ROOT, BUILD_ROOT, DEP_PREFIX) so a
//     fresh `bazel build` of the same action produces the same
//     trace bytes — a prerequisite for the registry-based
//     skip-the-build path. Substitutions apply per-line; longer
//     prefixes match before shorter ones to avoid partial-prefix
//     overlap.
//
// Both transforms apply uniformly to every line — argv strings,
// path strings, signal/exit lines. The canonicalized trace is
// what Bazel hashes for the action cache key, so the same
// build's trace is byte-identical across runs and across
// machines (modulo identical toolchain).
//
// Lines that don't match the strace `<pid>  <event>` shape are
// passed through unchanged (defensive — strace occasionally
// emits warnings or truncated lines we don't want to swallow).
func canonicalize(rawPath, outPath string, prefixSubs []prefixSub) error {
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
	// strace argv strings can be long; raise the scan buffer to
	// match build-tracer's strace fallback `-s 4096` plus headroom
	// for the surrounding line shape.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if _, err := io.WriteString(w, c.line(scanner.Text())+"\n"); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// prefixSub is one (from → to) substitution; canonicalize
// replaces every occurrence of `from` in each trace line with
// `to`. Used for sandbox-path neutralization.
type prefixSub struct {
	From, To string
}

// canonicalizer carries the per-trace state for stable
// placeholder assignment: temp-path → "/tmp/cc<N>.<ext>".
// Single-pass; first appearance order determines the counter.
type canonicalizer struct {
	tempPaths   map[string]string
	tempCounter int
	prefixSubs  []prefixSub
}

func newCanonicalizer(prefixSubs []prefixSub) *canonicalizer {
	// Sort longest-first so a substitution like
	// `/sandbox/install_root` is applied before a shorter
	// `/sandbox` that's also in the list — without this,
	// the shorter prefix would match first and leave a
	// suffix that still varies.
	subs := append([]prefixSub(nil), prefixSubs...)
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
// + `cc` (the prefix gcc uses) + 6+ alphanumerics + `.` +
// short alphabetic extension. Captures cover the random part
// + the extension so the replacement can keep the extension.
//
// Extensions seen empirically:
//   - .s   (cc1 → as: assembly)
//   - .o   (as → ld: object)
//   - .res (lto-wrapper resolution file)
//   - .d   (preprocessor deps)
//   - .i   (preprocessor output)
//   - .ld  (linker script)
//   - .le  (lto wrapper input list)
//
// Pattern `[a-zA-Z]+` for the extension is permissive enough to
// catch extensions we haven't seen yet without false-matching
// non-extension trailing chars.
var gccTempPathRE = regexp.MustCompile(`/tmp/cc[A-Za-z0-9]{6,}\.[a-zA-Z]+`)

func (c *canonicalizer) line(s string) string {
	s = pidPrefixRE.ReplaceAllString(s, "")
	s = gccTempPathRE.ReplaceAllStringFunc(s, c.replaceTemp)
	for _, sub := range c.prefixSubs {
		s = strings.ReplaceAll(s, sub.From, sub.To)
	}
	return s
}

// replaceTemp assigns each unique temp path a stable
// `<N>` counter on first sight; subsequent sightings of the
// same path return the same placeholder.
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
