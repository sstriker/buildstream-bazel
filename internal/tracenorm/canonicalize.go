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
//
// Openat handling: when build-tracer is invoked with
// --source-root=PATH, it captures openat events alongside the
// existing execve set so the canonical trace doubles as a
// configure-time read oracle for trace-driven kinds (parallel
// to cmake's RERUN_CMAKE-derived oracle in
// converter/internal/ninja). Canonicalize filters those events
// against Options.SourceRoot: paths inside the root pass
// through (rewritten as source-relative slash form, with the
// volatile fd return value stripped); paths outside drop. With
// no SourceRoot in Options, openat lines are dropped entirely
// — preserves the legacy AC byte schema for elements not opted
// into the oracle.
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

// Options carries the knobs Canonicalize / CanonicalizeBytes
// apply to a raw trace. PrefixSubs is the longest-first
// substitution list (sandbox-path neutralization). SourceRoot,
// when non-empty, opts the trace into the openat read-oracle:
// openat lines for paths under SourceRoot pass through with the
// path rewritten source-relative and the volatile `= <fd>`
// return value stripped; openat lines outside SourceRoot are
// dropped. Without SourceRoot, openat lines are dropped
// entirely (legacy AC byte schema).
type Options struct {
	PrefixSubs []PrefixSub
	SourceRoot string
}

// Canonicalize rewrites a raw strace-format trace file at rawPath
// into a byte-stable form at outPath. See package doc for the
// transforms applied. prefixSubs is sorted longest-first internally
// so a substitution like `/sandbox/install_root` applies before a
// shorter `/sandbox` that's also in the list.
func Canonicalize(rawPath, outPath string, prefixSubs []PrefixSub) error {
	return CanonicalizeWith(rawPath, outPath, Options{PrefixSubs: prefixSubs})
}

// CanonicalizeWith is Canonicalize with the full Options surface.
// Threads SourceRoot through so the openat read-oracle filter
// applies (see package doc).
func CanonicalizeWith(rawPath, outPath string, opts Options) error {
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

	c := newCanonicalizer(opts)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		out, keep := c.line(scanner.Text())
		if !keep {
			continue
		}
		if _, err := io.WriteString(w, out+"\n"); err != nil {
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
	return CanonicalizeBytesWith(raw, Options{PrefixSubs: prefixSubs})
}

// CanonicalizeBytesWith is CanonicalizeBytes with the full
// Options surface; mirrors CanonicalizeWith for in-memory
// callers (trace-publish).
func CanonicalizeBytesWith(raw []byte, opts Options) []byte {
	c := newCanonicalizer(opts)
	var b strings.Builder
	b.Grow(len(raw))
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		out, keep := c.line(scanner.Text())
		if !keep {
			continue
		}
		b.WriteString(out)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

type canonicalizer struct {
	tempPaths   map[string]string
	tempCounter int
	prefixSubs  []PrefixSub
	sourceRoot  string // absolute, with trailing slash stripped
}

func newCanonicalizer(opts Options) *canonicalizer {
	subs := append([]PrefixSub(nil), opts.PrefixSubs...)
	sort.Slice(subs, func(i, j int) bool {
		return len(subs[i].From) > len(subs[j].From)
	})
	root := opts.SourceRoot
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		// Strip a single trailing slash for the typical
		// "/path/to/src/" → "/path/to/src" normalization, but
		// don't TrimRight the universe of '/' characters: a
		// SourceRoot of "/" (the filesystem root) is a legal
		// — if pathological — input that TrimRight would
		// collapse to "" and silently disable the openat
		// filter. filepath.Clean is the safe normalizer.
		root = filepath.Clean(root)
	}
	return &canonicalizer{tempPaths: map[string]string{}, prefixSubs: subs, sourceRoot: root}
}

// pidPrefixRE matches a leading `<pid> ` (decimal pid + one or
// more spaces — the native backend writes two spaces, strace
// writes one). Anchored at line start, so argv-embedded
// numbers don't trip the match.
var pidPrefixRE = regexp.MustCompile(`^\d+ +`)

// gccTempPathRE matches the gcc-driver mkstemps shape: `/tmp/`
// + `cc` + 6+ alphanumerics + `.` + short alphabetic extension.
var gccTempPathRE = regexp.MustCompile(`/tmp/cc[A-Za-z0-9]{6,}\.[a-zA-Z]+`)

// openatLineRE matches strace-format openat lines. Capture
// groups:
//
//  1. `openat(AT_FDCWD, "` — fixed prefix.
//  2. The pathname (between the surrounding double-quotes;
//     embedded `\"` escapes preserved).
//  3. The rest of the call: closing `"`, optional `, <flags>`,
//     and the closing `)`. Inspected by openatLine to filter
//     write-mode opens out of the read oracle.
//  4. Optional ` = <retval>` suffix (stripped at canonicalize
//     time because fd numbers are run-volatile).
//
// The leading `<pid>  ` prefix is already stripped by pidPrefixRE
// before this fires.
//
// Format we accept:
//
//	openat(AT_FDCWD, "/path", O_RDONLY|O_CLOEXEC) = 3
//	openat(AT_FDCWD, "/path", O_RDONLY|O_CLOEXEC, 0666) = 3
//
// The capture group for the path uses non-greedy matching so
// embedded `\"` escapes inside the path string don't terminate
// the match early.
var openatLineRE = regexp.MustCompile(`^(openat\(AT_FDCWD, ")((?:[^"\\]|\\.)*)("(?:, [^)]*)?\))(\s*=\s*-?\d+)?$`)

func (c *canonicalizer) line(s string) (string, bool) {
	s = pidPrefixRE.ReplaceAllString(s, "")
	if strings.HasPrefix(s, "openat(") {
		return c.openatLine(s)
	}
	s = gccTempPathRE.ReplaceAllStringFunc(s, c.replaceTemp)
	for _, sub := range c.prefixSubs {
		s = strings.ReplaceAll(s, sub.From, sub.To)
	}
	return s, true
}

// openatLine canonicalizes (or drops) a single openat trace
// line. Without a SourceRoot configured, the line is dropped
// (legacy AC byte schema preserved for non-oracle elements).
// With a SourceRoot configured:
//
//   - Path inside the root AND access mode is O_RDONLY: line
//     passes through with the path rewritten source-relative
//     (slash form) and the `= <fd>` suffix replaced with `= ?`
//     (the literal fd return is run-volatile and must not enter
//     the AC digest).
//   - Path outside the root: dropped.
//   - Write-mode open (O_WRONLY / O_RDWR): dropped — the read
//     oracle is purely about reads. Build-tracer's native
//     backend filters these at capture time; the strace
//     fallback doesn't (strace's -e trace=openat captures every
//     access mode), so the parity filter lives here.
//   - Malformed line that doesn't match openatLineRE: dropped
//     (defensive — strace can emit unfinished/resumed split
//     forms we don't model yet).
func (c *canonicalizer) openatLine(s string) (string, bool) {
	if c.sourceRoot == "" {
		return "", false
	}
	m := openatLineRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	prefix := m[1] // `openat(AT_FDCWD, "`
	pathQuoted := m[2]
	suffix := m[3] // closing `"` + flags + `)`

	// Read-only filter (parity with native backend's capture-
	// time filter). Strace renders each access mode as a literal
	// O_* token; checking the suffix for the write-mode tokens
	// is sufficient because they only appear in the flags arg
	// (not in path strings, which were already split off into m[2]).
	if strings.Contains(suffix, "O_WRONLY") || strings.Contains(suffix, "O_RDWR") {
		return "", false
	}

	path := unquoteStrace(pathQuoted)
	if !filepath.IsAbs(path) {
		// Relative paths in openat are resolved against AT_FDCWD
		// (cwd). We don't have per-call cwd context here, so
		// drop. The configure-time read oracle cares about the
		// source-relative set; relative paths that reach into
		// the source tree via cwd land as absolute in practice
		// (cmake/make always pass absolute, autoconf usually
		// resolves before opening).
		return "", false
	}
	rel, err := filepath.Rel(c.sourceRoot, path)
	if err != nil || !insideSourceRoot(rel) {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	return prefix + quoteStrace(rel) + suffix + " = ?", true
}

// insideSourceRoot reports whether rel (a filepath.Rel result)
// names a path inside its base — i.e., it isn't `""`, `"."`,
// `".."`, and doesn't start with `"../"`. Plain
// `strings.HasPrefix(rel, "..")` would also reject legitimate
// in-tree paths whose first component literally starts with
// the bytes `..` (e.g. `..foo/bar`). filepath.Rel never produces
// an internal `..` component, so this check only needs to consider
// the leading position.
func insideSourceRoot(rel string) bool {
	if rel == "" || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// unquoteStrace inverts straceQuote (cmd/build-tracer): undoes
// the \\, \", \n, \t, \r, \xNN escapes. Best-effort; embedded
// invalid escapes pass through as-is.
func unquoteStrace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'x':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// quoteStrace escapes the body of a strace-quoted string —
// applies the same backslash-escapes (`\\`, `\"`, `\n`, `\t`,
// `\r`, `\xNN`) build-tracer's straceQuote uses, BUT does NOT
// emit the surrounding double-quotes. openatLine reuses the
// caller-supplied quotes from the original line (m[1] / m[3]
// in openatLineRE) and only needs the body re-escaped here.
// Naming kept "quote" rather than "escape" for symmetry with
// the build-tracer helper, even though the responsibility is
// narrower; the (short) helper is duplicated rather than
// imported to avoid a layering cycle (build-tracer imports
// tracenorm, not the other way).
func quoteStrace(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
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
