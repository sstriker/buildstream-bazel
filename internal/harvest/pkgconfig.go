package harvest

import (
	"os"
	"path/filepath"
	"strings"
)

// parsePkgConfig folds lib/pkgconfig/*.pc + share/pkgconfig/*.pc into
// rows — the fallback channel for libraries that ship no cmake bundle.
// A pc package whose -L/-l resolution lands on an artifact a bundle
// row already claims is skipped entirely (the bundle is richer).
// Requires/Requires.private are the DIRECT dep edges, recorded as pc
// names and resolved against other pc rows in resolveDeps.
func (h *harvester) parsePkgConfig() error {
	var files []string
	for _, dir := range []string{"lib/pkgconfig", "share/pkgconfig"} {
		files = append(files, walkFiles(filepath.Join(h.prefix, filepath.FromSlash(dir)), func(p string) bool {
			return strings.HasSuffix(p, ".pc")
		})...)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		h.applyPC(strings.TrimSuffix(filepath.Base(f), ".pc"), string(body))
	}
	return nil
}

func (h *harvester) applyPC(name, body string) {
	vars := map[string]string{"prefix": strings.TrimSuffix(h.prefix, "/")}
	fields := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexAny(line, ":="); i > 0 {
			key, val := strings.TrimSpace(line[:i]), expandPCVars(strings.TrimSpace(line[i+1:]), vars)
			if line[i] == '=' {
				vars[key] = val
			} else {
				fields[key] = val
			}
		}
	}
	r := &row{cmakeTarget: "pkgconfig::" + name}
	var searchDirs []string
	for _, tok := range strings.Fields(fields["Libs"] + " " + fields["Libs.private"]) {
		switch {
		case strings.HasPrefix(tok, "-L"):
			searchDirs = append(searchDirs, strings.TrimPrefix(tok, "-L"))
		case strings.HasPrefix(tok, "-l"):
			lib := strings.TrimPrefix(tok, "-l")
			r.linkLibs = appendUnique(r.linkLibs, lib)
			// Resolve the archive in the prefix so the row carries the
			// path key the tool/fragment lifts and wrapper-gen need.
			for _, d := range append(searchDirs, filepath.Join(h.prefix, "lib")) {
				for _, ext := range []string{".a", ".so"} {
					cand := filepath.Join(d, "lib"+lib+ext)
					if _, err := os.Stat(cand); err == nil {
						if anchored, ok := h.anchoredFromImportPrefix(cand); ok {
							r.linkPaths = appendUnique(r.linkPaths, anchored)
						}
					}
				}
			}
		}
	}
	for _, tok := range strings.Fields(fields["Cflags"]) {
		if d, ok := strings.CutPrefix(tok, "-I"); ok {
			if anchored, ok := h.anchoredFromImportPrefix(d); ok {
				r.includes = appendUnique(r.includes, strings.TrimPrefix(anchored, "/opt/prefix/"))
			}
		}
	}
	for _, req := range splitPCRequires(fields["Requires"] + "," + fields["Requires.private"]) {
		r.depRefs = append(r.depRefs, "pkgconfig::"+req)
	}
	// Bundle-claimed artifact → the bundle row wins; drop the pc row.
	for _, lp := range r.linkPaths {
		if _, claimed := h.byPath[lp]; claimed {
			h.warnf("pkgconfig %s: artifact %s already harvested from a cmake bundle; pc row skipped", name, lp)
			return
		}
	}
	h.addRow(r)
}

// splitPCRequires splits a Requires list — comma- or space-separated
// package names with optional version constraints (`foo >= 1.2`).
func splitPCRequires(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' }) {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		// "name", "name >= 1.2": the name is the first field; a bare
		// operator/version continuation is consumed with it.
		name := fields[0]
		if name == "" || strings.ContainsAny(name, "<>=") {
			continue
		}
		out = append(out, name)
		// Space-separated multi-requires without versions: keep the rest.
		for _, f := range fields[1:] {
			if strings.ContainsAny(f, "<>=") || isVersionLike(f) {
				break
			}
			out = append(out, f)
		}
	}
	return out
}

func isVersionLike(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return s != ""
}

// expandPCVars substitutes ${var} occurrences from the accumulated
// variable table (unknown vars expand empty, matching pkg-config).
func expandPCVars(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end > 0 {
				b.WriteString(vars[s[i+2:i+end]])
				i += end + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
