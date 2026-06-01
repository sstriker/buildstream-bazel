package ninja

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Parser controls how Parse resolves include / subninja directives and how
// strict it is about unknown top-level constructs. Zero value is fine for
// CMake-generated files in the common case.
type Parser struct {
	// FileResolver is called for each `include` / `subninja` directive
	// with the path as written in the directive (relative to the
	// including file's dir). Returning (nil, nil) skips the include.
	// Default: resolve relative to the parent file's directory and open
	// from disk.
	FileResolver func(parentDir, path string) (io.ReadCloser, error)
}

// Parse reads ninja text from r and returns the parsed Graph. The dir
// parameter is the logical directory of r (used to resolve relative
// include/subninja paths); use "" if r is an in-memory snippet.
func Parse(r io.Reader, dir string, p *Parser) (*Graph, error) {
	g := newGraph()
	if p == nil {
		p = &Parser{}
	}
	if p.FileResolver == nil {
		p.FileResolver = defaultFileResolver
	}
	if err := parseInto(g, r, dir, p); err != nil {
		return nil, err
	}
	g.Index()
	return g, nil
}

// ParseFile is a convenience for parsing a build.ninja from disk.
func ParseFile(path string) (*Graph, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, filepath.Dir(path), nil)
}

func defaultFileResolver(parentDir, path string) (io.ReadCloser, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(parentDir, path)
	}
	return os.Open(path)
}

func parseInto(g *Graph, r io.Reader, dir string, p *Parser) error {
	s := newLineScanner(r)
	for {
		ll, ok, err := s.next()
		if err != nil {
			return fmt.Errorf("ninja: read line %d: %w", s.line, err)
		}
		if !ok {
			break
		}
		if ll.indent > 0 {
			// Stray indented line at top level: ninja treats this as an
			// error. Be strict to surface generator bugs early.
			return fmt.Errorf("ninja: line %d: unexpected indented line %q", ll.lineNo, ll.text)
		}
		if ll.text == "" {
			continue
		}
		head, rest := splitFirstWord(ll.text)
		switch head {
		case "rule":
			if err := parseRuleStmt(g, s, ll, rest); err != nil {
				return err
			}
		case "build":
			if err := parseBuildStmt(g, s, ll, rest); err != nil {
				return err
			}
		case "default":
			g.Defaults = append(g.Defaults, splitTokens(rest)...)
		case "pool":
			if err := parsePoolStmt(g, s, ll, rest); err != nil {
				return err
			}
		case "include", "subninja":
			path := strings.TrimSpace(rest)
			if path == "" {
				return fmt.Errorf("ninja: line %d: %s without path", ll.lineNo, head)
			}
			if err := includeFile(g, p, dir, path); err != nil {
				return fmt.Errorf("ninja: line %d: %s %q: %w", ll.lineNo, head, path, err)
			}
		default:
			// Top-level variable assignment: `name = value`.
			name, value, ok := splitVarAssign(ll.text)
			if !ok {
				return fmt.Errorf("ninja: line %d: unexpected statement %q", ll.lineNo, ll.text)
			}
			if _, exists := g.Vars[name]; !exists {
				g.VarOrder = append(g.VarOrder, name)
			}
			g.Vars[name] = value
		}
	}
	return nil
}

func includeFile(g *Graph, p *Parser, parentDir, path string) error {
	rc, err := p.FileResolver(parentDir, path)
	if err != nil {
		return err
	}
	if rc == nil {
		return nil // resolver elected to skip
	}
	defer rc.Close()
	// ninja resolves include / subninja paths relative to the
	// top-level build directory (the working dir ninja was
	// invoked in), NOT relative to the including file's
	// directory. Cmake's Ninja Multi-Config generator relies on
	// this: `build.ninja` does `include CMakeFiles/impl-<cfg>.ninja`,
	// and impl-<cfg>.ninja in turn does `include CMakeFiles/common.ninja`
	// — the latter must resolve to `<build>/CMakeFiles/common.ninja`,
	// not to `<build>/CMakeFiles/CMakeFiles/common.ninja`. Pass the
	// same parentDir through recursive parses so nested includes
	// all anchor at the build root.
	return parseInto(g, rc, parentDir, p)
}

func parseRuleStmt(g *Graph, s *lineScanner, head logicalLine, rest string) error {
	name := strings.TrimSpace(rest)
	if name == "" {
		return fmt.Errorf("ninja: line %d: rule without name", head.lineNo)
	}
	r := &Rule{Name: name, Bindings: map[string]string{}}
	if err := readBindings(s, r.Bindings, &r.BindingOrder); err != nil {
		return fmt.Errorf("ninja: rule %q at line %d: %w", name, head.lineNo, err)
	}
	g.Rules[name] = r
	return nil
}

func parsePoolStmt(g *Graph, s *lineScanner, head logicalLine, rest string) error {
	name := strings.TrimSpace(rest)
	if name == "" {
		return fmt.Errorf("ninja: line %d: pool without name", head.lineNo)
	}
	pool := &Pool{Name: name, Bindings: map[string]string{}}
	var order []string
	if err := readBindings(s, pool.Bindings, &order); err != nil {
		return fmt.Errorf("ninja: pool %q at line %d: %w", name, head.lineNo, err)
	}
	g.Pools[name] = pool
	return nil
}

// parseBuildStmt: `build out1 out2 | implicit_out: rule in1 in2 | implicit_in
// || order_only |@ validation`
func parseBuildStmt(g *Graph, s *lineScanner, head logicalLine, rest string) error {
	b := &Build{Bindings: map[string]string{}, Line: head.lineNo}

	colon := indexOfTopLevelColon(rest)
	if colon < 0 {
		return fmt.Errorf("ninja: line %d: build statement without colon", head.lineNo)
	}
	outsPart := rest[:colon]
	tail := strings.TrimSpace(rest[colon+1:])

	outs, implicitOuts, _ := splitOutputs(outsPart)
	b.Outputs = outs
	b.ImplicitOuts = implicitOuts

	ruleName, inputsPart := splitFirstWord(tail)
	if ruleName == "" {
		return fmt.Errorf("ninja: line %d: build without rule name", head.lineNo)
	}
	b.Rule = ruleName

	ins, impIns, orderOnly, validations := splitInputs(inputsPart)
	b.Inputs = ins
	b.ImplicitInputs = impIns
	b.OrderOnly = orderOnly
	b.Validations = validations

	if err := readBindings(s, b.Bindings, &b.BindingOrder); err != nil {
		return fmt.Errorf("ninja: build at line %d: %w", head.lineNo, err)
	}
	g.Builds = append(g.Builds, b)
	return nil
}

// readBindings consumes the indented `name = value` lines that follow a
// rule/build/pool header. Stops at the first non-indented (or empty) logical
// line and pushes that line back for the caller to re-scan.
func readBindings(s *lineScanner, dst map[string]string, order *[]string) error {
	for {
		ll, ok, err := s.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if ll.indent == 0 {
			s.unread(ll)
			return nil
		}
		if ll.text == "" {
			continue
		}
		name, value, ok := splitVarAssign(ll.text)
		if !ok {
			return fmt.Errorf("line %d: expected `name = value`, got %q", ll.lineNo, ll.text)
		}
		if _, exists := dst[name]; !exists {
			*order = append(*order, name)
		}
		dst[name] = value
	}
}

// ---- token / line helpers ---------------------------------------------------

// splitFirstWord splits s into (first whitespace-delimited word, rest with
// leading whitespace trimmed). Treats `$<space>` as a non-separator (escaped
// space).
func splitFirstWord(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) && s[i+1] == ' ' {
			i++ // skip the escape pair
			continue
		}
		if c == ' ' || c == '\t' {
			return s[:i], strings.TrimLeft(s[i:], " \t")
		}
	}
	return s, ""
}

// splitTokens splits a whitespace-delimited list, honoring `$ ` escapes.
func splitTokens(s string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) && s[i+1] == ' ' {
			cur.WriteByte(' ')
			i++
			continue
		}
		if c == ' ' || c == '\t' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitVarAssign parses `name = value` with optional surrounding whitespace.
// Returns (name, value, true) or ("", "", false) if no = is present.
func splitVarAssign(s string) (string, string, bool) {
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return "", "", false
	}
	name := strings.TrimSpace(s[:eq])
	if name == "" {
		return "", "", false
	}
	for _, r := range name {
		if !(r == '_' || r == '.' || r == '-' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return "", "", false
		}
	}
	return name, strings.TrimLeft(s[eq+1:], " \t"), true
}

// indexOfTopLevelColon finds the colon that separates outputs from rule, in a
// build statement. Honors `$:` escapes.
func indexOfTopLevelColon(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) {
			i++
			continue
		}
		if c == ':' {
			return i
		}
	}
	return -1
}

// barKind classifies a segment by the separator that preceded it.
type barKind int

const (
	barNone        barKind = iota // first segment, no preceding bar
	barSingle                     // `|` (implicit deps / implicit outs)
	barDouble                     // `||` (order-only)
	barValidations                // `|@` (validations)
)

type barSegment struct {
	kind barKind
	text string
}

// splitOutputs returns (explicit outs, implicit outs, validations).
func splitOutputs(s string) (outs, implicit, validations []string) {
	for _, seg := range splitOnBar(s) {
		toks := splitTokens(seg.text)
		switch seg.kind {
		case barNone:
			outs = toks
		case barSingle:
			implicit = toks
		case barValidations:
			validations = toks
		}
	}
	return outs, implicit, validations
}

// splitInputs returns (explicit, implicit, order-only, validations) from the
// rule-and-after side of a build statement.
func splitInputs(s string) (explicit, implicit, orderOnly, validations []string) {
	for _, seg := range splitOnBar(s) {
		toks := splitTokens(seg.text)
		switch seg.kind {
		case barNone:
			explicit = toks
		case barSingle:
			implicit = toks
		case barDouble:
			orderOnly = toks
		case barValidations:
			validations = toks
		}
	}
	return explicit, implicit, orderOnly, validations
}

// splitOnBar splits s on `|`, `||`, and `|@`, returning typed segments.
// Honors `$|` and `$:` escapes (next-byte-after-$ is preserved literally).
func splitOnBar(s string) []barSegment {
	var segs []barSegment
	var cur strings.Builder
	curKind := barNone
	flush := func() {
		segs = append(segs, barSegment{kind: curKind, text: strings.TrimSpace(cur.String())})
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) {
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
			continue
		}
		if c != '|' {
			cur.WriteByte(c)
			continue
		}
		flush()
		switch {
		case i+1 < len(s) && s[i+1] == '|':
			curKind = barDouble
			i++
		case i+1 < len(s) && s[i+1] == '@':
			curKind = barValidations
			i++
		default:
			curKind = barSingle
		}
	}
	flush()
	return segs
}

// ---- line scanner ----------------------------------------------------------

type logicalLine struct {
	text   string // joined text with $-newline continuations applied; comments stripped
	indent int    // count of leading space/tab characters in the first physical line
	lineNo int    // 1-based line number of the first physical line
}

type lineScanner struct {
	r       *bufio.Reader
	line    int
	pending *logicalLine
}

func newLineScanner(r io.Reader) *lineScanner {
	return &lineScanner{r: bufio.NewReader(r)}
}

// next returns the next non-comment logical line, or (zero, false, nil) at
// EOF. Errors are sticky.
func (s *lineScanner) next() (logicalLine, bool, error) {
	if s.pending != nil {
		ll := *s.pending
		s.pending = nil
		return ll, true, nil
	}
	for {
		raw, err := s.readPhysicalLine()
		if errors.Is(err, io.EOF) && raw == "" {
			return logicalLine{}, false, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return logicalLine{}, false, err
		}
		s.line++
		startLine := s.line

		// Compute indent on the FIRST physical line as-read (pre-strip).
		indent := 0
		for indent < len(raw) && (raw[indent] == ' ' || raw[indent] == '\t') {
			indent++
		}

		// Apply $-newline continuations.
		for strings.HasSuffix(raw, "$") {
			next, nerr := s.readPhysicalLine()
			if nerr != nil && !errors.Is(nerr, io.EOF) {
				return logicalLine{}, false, nerr
			}
			s.line++
			raw = raw[:len(raw)-1] + strings.TrimLeft(next, " \t")
			if errors.Is(nerr, io.EOF) {
				break
			}
		}

		text := raw[indent:]
		// Strip trailing comment (# ...). A '$#' is a literal #; ninja
		// only treats `#` as a comment at the start of a token. Cheap
		// approximation: only treat `#` as comment if it's at column 0 of
		// the trimmed text.
		if strings.HasPrefix(text, "#") {
			// pure comment line
			if errors.Is(err, io.EOF) {
				return logicalLine{}, false, nil
			}
			continue
		}
		text = strings.TrimRight(text, " \t")

		ll := logicalLine{
			text:   text,
			indent: indent,
			lineNo: startLine,
		}
		if errors.Is(err, io.EOF) && text == "" {
			return logicalLine{}, false, nil
		}
		return ll, true, nil
	}
}

// readPhysicalLine returns one physical line (no trailing \n). May return
// (line, io.EOF) for the last line. Returns ("", io.EOF) at clean EOF.
func (s *lineScanner) readPhysicalLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line, err
}

// unread pushes a logical line back onto the scanner so the next next() call
// returns it. Single-slot pushback; calling twice in a row panics.
func (s *lineScanner) unread(ll logicalLine) {
	if s.pending != nil {
		panic("ninja: lineScanner unread overflow")
	}
	s.pending = &ll
}
