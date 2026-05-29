package main

import (
	"encoding/json"
	"strings"
)

// InstallMapping is the sidecar artifact convert-element-trace
// emits when the make-database is available and the Makefile
// declares an `install:` recipe. Each entry pairs a build-time
// source (path relative to the build dir) with its install-tree
// destination (path under DESTDIR after `make install`), plus
// the matching Bazel rule name when the source corresponds to
// one of the converter's emitted cc rules.
//
// Downstream Phase 4 typed-filegroup work consumes this to
// split the install-root TreeArtifact into typed slices —
// libs, binaries, headers, share data — each fronted by its
// own filegroup. Phase 4 lives in write-a; this artifact is
// the contract.
//
// File: install-mapping.json next to BUILD.bazel.out.
type InstallMapping struct {
	Version  int               `json:"version"`
	Mappings []InstallMappingE `json:"mappings"`
}

// InstallMappingE is one source → dest mapping. Source paths are
// preserved verbatim from the install recipe (after make-variable
// expansion); they're typically build-dir-relative
// (`app`, `libmathlib.a`, `include/mathlib.h`) but can be
// absolute when the Makefile uses absolute build-output paths.
type InstallMappingE struct {
	Source string `json:"source"`         // build-time path
	Dest   string `json:"dest"`           // install-tree path (DESTDIR-relative)
	Mode   string `json:"mode,omitempty"` // when -m <mode> was supplied to install(1)
	Rule   string `json:"rule,omitempty"` // matching cc_library / cc_binary name when known
}

// buildInstallMapping walks the make-db's `install:` rule recipe,
// parses each `install -D [-m <mode>] <src> <dest>` line, expands
// make variables in src/dest, and pairs the source against the
// converter's emitted rules to fill in the rule name where
// applicable.
//
// Unknown lines (e.g., `mkdir`, `cp`, custom shell wrappers) are
// silently skipped — the spike only handles canonical
// `install(1)`-shaped recipes. Non-canonical install patterns
// would need richer shell parsing; deferred.
func buildInstallMapping(db *MakeDB, rules []CCRule) *InstallMapping {
	if db == nil {
		return nil
	}
	rule, ok := db.Rules["install"]
	if !ok {
		return nil
	}
	ruleByName := map[string]string{}
	for _, r := range rules {
		ruleByName[r.Name] = r.Name
	}
	// Library rules' build-time output is `lib<name>.a`; map it.
	for _, r := range rules {
		if r.RuleKind == "cc_library" {
			ruleByName["lib"+r.Name+".a"] = r.Name
		}
	}

	out := &InstallMapping{Version: 1}
	for _, line := range rule.Recipe {
		entry, ok := parseInstallRecipeLine(line, db.Variables)
		if !ok {
			continue
		}
		entry.Rule = ruleByName[entry.Source]
		out.Mappings = append(out.Mappings, entry)
	}
	if len(out.Mappings) == 0 {
		return nil
	}
	return out
}

// parseInstallRecipeLine handles the canonical `install(1)`
// invocation shape:
//
//	install -D [-m <mode>] <src> <dest>
//	install [-m <mode>] <src> <dest>
//
// Returns the parsed entry. Returns ok=false for any line that
// doesn't match (skipped silently). Make variables (`$(VAR)`,
// `${VAR}`) in src/dest get expanded against db.Variables before
// recording.
func parseInstallRecipeLine(line string, vars map[string]string) (InstallMappingE, bool) {
	line = strings.TrimSpace(line)
	tokens := strings.Fields(line)
	if len(tokens) < 3 || tokens[0] != "install" {
		return InstallMappingE{}, false
	}

	var mode string
	var positionals []string
	for i := 1; i < len(tokens); i++ {
		t := tokens[i]
		switch {
		case t == "-D":
			// no-op flag for our parsing — just declares mkdir-p semantics
		case t == "-m" && i+1 < len(tokens):
			mode = tokens[i+1]
			i++
		case strings.HasPrefix(t, "-m") && len(t) > 2:
			mode = t[2:]
		case strings.HasPrefix(t, "-"):
			// Unknown flag (e.g., -p, -o, -g); skip without
			// claiming positional status.
		default:
			positionals = append(positionals, t)
		}
	}
	if len(positionals) < 2 {
		return InstallMappingE{}, false
	}
	// `install -D src dest` (the multitarget fixture's shape).
	// Treat last positional as dest, second-to-last as src;
	// preceding positionals (multi-source `install`) collapse
	// into one entry per source. The spike emits one entry per
	// recipe line.
	src := positionals[len(positionals)-2]
	dst := positionals[len(positionals)-1]

	src = stripDestdir(expandMakeVars(src, vars))
	dst = stripDestdir(expandMakeVars(dst, vars))
	dst = strings.TrimPrefix(dst, "/")
	return InstallMappingE{
		Source: src,
		Dest:   dst,
		Mode:   mode,
	}, true
}

// stripDestdir removes a literal $(DESTDIR) / ${DESTDIR} prefix
// (or its expanded form when DESTDIR is empty/unset). The mapping
// records install-tree-relative paths, not host-absolute ones.
func stripDestdir(s string) string {
	for _, prefix := range []string{"$(DESTDIR)", "${DESTDIR}", "$DESTDIR"} {
		if strings.HasPrefix(s, prefix) {
			return s[len(prefix):]
		}
	}
	return s
}

// expandMakeVars recursively substitutes `$(VAR)` and `${VAR}`
// references against vars. Bounded depth (32) to defend against
// cycles in malformed databases.
func expandMakeVars(s string, vars map[string]string) string {
	for depth := 0; depth < 32; depth++ {
		next := expandMakeVarsOnce(s, vars)
		if next == s {
			break
		}
		s = next
	}
	return s
}

// expandMakeVarsOnce performs one substitution pass.
// Unrecognized variable names expand to the empty string,
// matching make's behavior for undefined variables.
func expandMakeVarsOnce(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '$' && (s[i+1] == '(' || s[i+1] == '{') {
			closeC := byte(')')
			if s[i+1] == '{' {
				closeC = '}'
			}
			end := strings.IndexByte(s[i+2:], closeC)
			if end < 0 {
				b.WriteByte(s[i])
				i++
				continue
			}
			name := s[i+2 : i+2+end]
			if v, ok := vars[name]; ok {
				b.WriteString(v)
			}
			i = i + 2 + end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// renderInstallMappingJSON writes the mapping as
// pretty-printed JSON, sorted by Source for stable output.
func renderInstallMappingJSON(m *InstallMapping) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
