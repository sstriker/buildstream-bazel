package harvest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
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
				// The harvest-computed prefix seed WINS over the file's
				// build-time `prefix=` line (pkg-config's --define-prefix
				// semantics): a RELOCATED tree — a bst artifact checkout,
				// the headline use case — carries the original build
				// prefix in its .pc files, and letting it clobber the
				// seed would expand ${libdir}/${includedir} outside the
				// harvested tree, silently dropping every derived path.
				if key != "prefix" {
					vars[key] = val
				}
			} else {
				fields[key] = val
			}
		}
	}
	r := &row{cmakeTarget: "pkgconfig::" + name, origin: "pkgconfig " + name + ".pc"}
	h.applyPCLibs(r, fields["Libs"]+" "+fields["Libs.private"])
	for _, tok := range strings.Fields(fields["Cflags"]) {
		if d, ok := strings.CutPrefix(tok, "-I"); ok {
			if anchored, ok := h.anchoredFromImportPrefix(d); ok {
				r.includes = appendUnique(r.includes, strings.TrimPrefix(anchored, manifest.PrefixAnchor))
			}
		}
	}
	for _, req := range splitPCRequires(fields["Requires"] + "," + fields["Requires.private"]) {
		r.depRefs = append(r.depRefs, "pkgconfig::"+req)
	}
	// Same library already harvested (artifact identity OR matching
	// consumer-facing name with no contradicting artifact): MERGE the
	// pc channel's keys into the claimant instead of dropping them —
	// the -l name keeps the LookupLinkLibrary redirect alive and the
	// Requires deps keep resolving (the pc name registers as an alias).
	if claimant := h.sameLibraryClaimant(r); claimant != nil {
		h.warnf("pkgconfig %s: same library as %s (%s); channels merged", name, claimant.cmakeTarget, claimant.origin)
		h.mergeInto(claimant, r)
		return
	}
	h.addRow(r)
}

// applyPCLibs walks the Libs/Libs.private token stream: -L dirs feed
// the probe's search path, each -l<name> records the link name AND
// resolves the artifact in the prefix — the path key the tool/fragment
// lifts, wrapper-gen, and the same-library dedup need. The probe
// covers lib64 and versioned sonames (libfoo.so.1.2.3 with no plain
// .so): a miss here is what lets a bundle+pc pair slip past path
// identity and collide at generation time.
func (h *harvester) applyPCLibs(r *row, libsField string) {
	var searchDirs []string
	for _, tok := range strings.Fields(libsField) {
		switch {
		case strings.HasPrefix(tok, "-L"):
			searchDirs = append(searchDirs, strings.TrimPrefix(tok, "-L"))
		case strings.HasPrefix(tok, "-l"):
			lib := strings.TrimPrefix(tok, "-l")
			r.linkLibs = appendUnique(r.linkLibs, lib)
			dirs := append(append([]string{}, searchDirs...),
				filepath.Join(h.prefix, "lib"), filepath.Join(h.prefix, "lib64"))
			for _, d := range dirs {
				h.probeArtifact(r, d, lib)
			}
		}
	}
}

// probeArtifact records the anchored path of lib<name>.{a,so} (or the
// first versioned soname) under dir, when present.
func (h *harvester) probeArtifact(r *row, dir, lib string) {
	for _, ext := range []string{".a", ".so"} {
		cand := filepath.Join(dir, "lib"+lib+ext)
		if _, err := os.Stat(cand); err == nil {
			if anchored, ok := h.anchoredFromImportPrefix(cand); ok {
				r.linkPaths = appendUnique(r.linkPaths, anchored)
			}
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "lib"+lib+".so.*")); len(matches) > 0 {
		sort.Strings(matches)
		if anchored, ok := h.anchoredFromImportPrefix(matches[0]); ok {
			r.linkPaths = appendUnique(r.linkPaths, anchored)
		}
	}
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
		// operator/version continuation is consumed with it. The
		// no-space constraint shape ("name>=1.2") splits at the
		// operator — dropping it silently would lose a real dep edge.
		name := fields[0]
		if i := strings.IndexAny(name, "<>="); i >= 0 {
			name = name[:i]
		}
		if name == "" {
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
