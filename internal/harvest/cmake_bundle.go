package harvest

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// parseBundles walks lib/cmake/*/ *.cmake files and folds every
// IMPORTED-target declaration into rows. The grammar is cmake's export
// format (the same one emit/cmakecfg synthesizes and cmake's
// cmExportCMakeConfigGenerator writes): add_library/add_executable
// (... IMPORTED), add_library(<alias> ALIAS <t>), set_target_properties /
// set_property(TARGET ... PROPERTY ...).
func (h *harvester) parseBundles() error {
	files := walkFiles(filepath.Join(h.prefix, "lib", "cmake"), func(p string) bool {
		return strings.HasSuffix(p, ".cmake")
	})
	// Two passes so properties can reference targets declared in a
	// sibling file (<Pkg>Targets-<config>.cmake sets locations for
	// targets <Pkg>Targets.cmake declared).
	var allCalls []cmakeCall
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		allCalls = append(allCalls, parseCMakeCalls(string(body))...)
	}
	for _, c := range allCalls {
		h.applyDeclaration(c)
	}
	for _, c := range allCalls {
		h.applyProperties(c)
	}
	return nil
}

func (h *harvester) applyDeclaration(c cmakeCall) {
	switch c.name {
	case "add_library":
		if len(c.args) >= 3 && strings.EqualFold(c.args[1], "ALIAS") {
			h.addRow(&row{cmakeTarget: c.args[0], aliasOf: c.args[2], origin: "bundle"})
			return
		}
		if hasArg(c.args, "IMPORTED") {
			h.addRow(&row{cmakeTarget: c.args[0], origin: "bundle"})
		}
	case "add_executable":
		if len(c.args) >= 3 && strings.EqualFold(c.args[1], "ALIAS") {
			h.addRow(&row{cmakeTarget: c.args[0], aliasOf: c.args[2], origin: "bundle"})
			return
		}
		if hasArg(c.args, "IMPORTED") {
			h.addRow(&row{cmakeTarget: c.args[0], origin: "bundle", kind: manifest.KindExecutable})
		}
	}
}

func (h *harvester) applyProperties(c cmakeCall) {
	var target string
	var kv []string
	switch c.name {
	case "set_target_properties":
		// Export bundles write one target per call; the multi-target
		// form (set_target_properties(t1 t2 ... PROPERTIES ...)) never
		// appears in generator output, so only args[0] is applied.
		i := indexArg(c.args, "PROPERTIES")
		if i < 1 {
			return
		}
		target, kv = c.args[0], c.args[i+1:]
	case "set_property":
		// set_property(TARGET <t> [APPEND] PROPERTY <K> <v...>)
		if len(c.args) < 4 || !strings.EqualFold(c.args[0], "TARGET") {
			return
		}
		target = c.args[1]
		pi := indexArg(c.args, "PROPERTY")
		if pi < 0 || pi+1 >= len(c.args) {
			return
		}
		kv = []string{c.args[pi+1], strings.Join(c.args[pi+2:], ";")}
	default:
		return
	}
	r, ok := h.byName[target]
	if !ok {
		return
	}
	for i := 0; i+1 < len(kv); i += 2 {
		h.applyProperty(r, kv[i], kv[i+1])
	}
}

func (h *harvester) applyProperty(r *row, key, value string) {
	switch {
	case key == "INTERFACE_INCLUDE_DIRECTORIES":
		for _, v := range strings.Split(value, ";") {
			if anchored, ok := h.anchoredFromImportPrefix(v); ok {
				rel := strings.TrimPrefix(anchored, manifest.PrefixAnchor)
				r.includes = appendUnique(r.includes, rel)
			}
		}
	case key == "INTERFACE_LINK_LIBRARIES":
		for _, v := range strings.Split(value, ";") {
			h.applyLinkEntry(r, v)
		}
	case strings.HasPrefix(key, "IMPORTED_LOCATION"):
		if anchored, ok := h.anchoredFromImportPrefix(value); ok {
			r.linkPaths = appendUnique(r.linkPaths, anchored)
			if k := h.canonicalKey(anchored); h.byPath[k] == nil {
				h.byPath[k] = r
			}
		}
	}
}

// applyLinkEntry classifies one INTERFACE_LINK_LIBRARIES entry — the
// DIRECT dependency vocabulary of cmake's export format. Genex handling
// is deliberately conservative but must not silently drop real link
// edges:
//
//   - $<LINK_ONLY:x> unwraps — it IS a link dep; privacy is a
//     consumer-side concern.
//   - $<INSTALL_INTERFACE:x> unwraps — this manifest describes an
//     installed prefix, so the install half is the consumer-visible one.
//   - $<BUILD_INTERFACE:x> is skipped silently — it is empty for an
//     installed consumer, so dropping it is correct, not a lost edge.
//   - $<$<CONFIG:...>:x>, $<$<PLATFORM_ID:...>:x> and other
//     nested-condition genexes unwrap and wire the guarded dep
//     UNCONDITIONALLY. A superset link edge is sound for a link
//     manifest — over-linking never breaks a link, under-linking does —
//     and it keeps config-conditional sibling deps from vanishing.
//   - anything else generator-expression-shaped still warns and drops.
func (h *harvester) applyLinkEntry(r *row, v string) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
	case strings.HasPrefix(v, "$<LINK_ONLY:") && strings.HasSuffix(v, ">"):
		h.applyGenexContent(r, v[len("$<LINK_ONLY:"):len(v)-1])
	case strings.HasPrefix(v, "$<INSTALL_INTERFACE:") && strings.HasSuffix(v, ">"):
		h.applyGenexContent(r, v[len("$<INSTALL_INTERFACE:"):len(v)-1])
	case strings.HasPrefix(v, "$<BUILD_INTERFACE:"):
		// Build-tree-only; empty for an installed consumer. Skip silently.
	case strings.HasPrefix(v, "$<$<"):
		if content, ok := conditionalGenexContent(v); ok {
			h.applyGenexContent(r, content)
		} else {
			h.warnf("%s: generator expression in INTERFACE_LINK_LIBRARIES skipped: %s", r.cmakeTarget, v)
		}
	case strings.HasPrefix(v, "$<"):
		h.warnf("%s: generator expression in INTERFACE_LINK_LIBRARIES skipped: %s", r.cmakeTarget, v)
	case strings.Contains(v, "::"):
		r.depRefs = append(r.depRefs, v)
	case strings.HasPrefix(v, "-l"):
		r.linkLibs = appendUnique(r.linkLibs, strings.TrimPrefix(v, "-l"))
	case strings.HasPrefix(v, "-"):
		h.warnf("%s: link flag %q skipped (no manifest channel)", r.cmakeTarget, v)
	default:
		if anchored, ok := h.anchoredFromImportPrefix(v); ok {
			r.linkPaths = appendUnique(r.linkPaths, anchored)
			return
		}
		// Bare library name (pthread, m, dl): the -l vocabulary.
		r.linkLibs = appendUnique(r.linkLibs, v)
	}
}

// applyGenexContent re-classifies the guarded content of an unwrapped
// generator expression. The content can be a ';'-separated list, and
// each element can itself be a genex, so re-split and recurse.
func (h *harvester) applyGenexContent(r *row, content string) {
	for _, item := range strings.Split(content, ";") {
		h.applyLinkEntry(r, item)
	}
}

// conditionalGenexContent unwraps a condition-guarded generator
// expression whose head is itself a genex — $<$<CONFIG:Release>:App::x>,
// $<$<PLATFORM_ID:Linux>:m>. It returns everything after the outer
// genex's first depth-0 ':' (the guarded content); ok is false when v is
// not shaped like a nested-condition genex. The condition itself is
// dropped by design: the guarded dep is wired unconditionally, a sound
// over-approximation for a link manifest.
func conditionalGenexContent(v string) (string, bool) {
	if !strings.HasPrefix(v, "$<$<") || !strings.HasSuffix(v, ">") {
		return "", false
	}
	inner := v[len("$<") : len(v)-1] // strip the outer $< >
	depth := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '<':
			depth++
		case '>':
			depth--
		case ':':
			if depth == 0 {
				return inner[i+1:], true
			}
		}
	}
	return "", false
}

// cmakeCall is one parsed `name(args...)` invocation.
type cmakeCall struct {
	name string
	args []string
}

// parseCMakeCalls tokenizes the subset of cmake syntax export bundles
// use: command calls with quoted/unquoted arguments, `#` comments.
// Block constructs (foreach/if) pass through as calls whose bodies'
// inner calls are ALSO surfaced — fine for harvesting, since the
// guarded bodies in export bundles (the EXISTS check loop) reference
// no IMPORTED declarations.
func parseCMakeCalls(src string) []cmakeCall {
	var calls []cmakeCall
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			start := i
			for i < n && isIdentRune(src[i]) {
				i++
			}
			name := src[start:i]
			for i < n && (src[i] == ' ' || src[i] == '\t') {
				i++
			}
			if name == "" || i >= n || src[i] != '(' {
				// Not a call; skip to be safe.
				for i < n && src[i] != '\n' {
					i++
				}
				continue
			}
			i++ // consume '('
			args, next := parseCallArgs(src, i)
			i = next
			calls = append(calls, cmakeCall{name: strings.ToLower(name), args: args})
		}
	}
	return calls
}

// parseCallArgs reads arguments until the matching ')', honoring
// double quotes (cmake's export bundles never nest parens outside
// quotes except in genexes, which carry no spaces and parse as single
// unquoted tokens — balanced-genex tracking keeps `$<...>` intact).
func parseCallArgs(src string, i int) ([]string, int) {
	var args []string
	var cur strings.Builder
	n := len(src)
	depth := 0 // genex `$<...>` nesting
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for i < n {
		c := src[i]
		switch {
		case c == '"':
			// Quoted argument: read to the closing quote verbatim.
			j := i + 1
			for j < n && src[j] != '"' {
				if src[j] == '\\' && j+1 < n {
					j++
				}
				j++
			}
			args = append(args, src[i+1:j])
			i = j + 1
		case c == '$' && i+1 < n && src[i+1] == '<':
			depth++
			cur.WriteString("$<")
			i += 2
		case c == '>' && depth > 0:
			depth--
			cur.WriteByte('>')
			i++
		case c == ')' && depth == 0:
			flush()
			return args, i + 1
		case (c == ' ' || c == '\t' || c == '\n' || c == '\r') && depth == 0:
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return args, i
}

func isIdentRune(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

func indexArg(args []string, want string) int {
	for i, a := range args {
		if strings.EqualFold(a, want) {
			return i
		}
	}
	return -1
}
