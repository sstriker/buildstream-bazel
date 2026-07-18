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
// Requires/Requires.private are the DIRECT dep edges, recorded as pc
// names and resolved against other pc rows in resolveDeps.
//
// Three phases, because a Libs line mixes a package's OWN library with
// libraries it merely links against (`Libs: -lfoo -labc` where abc has
// its own abc.pc), and treating every -l as owned fuses unrelated
// packages — whichever side parses first claims the artifact and the
// other merges into it as a duplicate, losing its label:
//
//  1. parse every .pc, classifying each -l as SELF (name affinity
//     with the pc package, see pcSelfLib) or FOREIGN;
//  2. place each row — same-library identity and byPath claims use
//     SELF artifacts only;
//  3. resolve FOREIGN references against the now-complete ownership
//     map: an artifact another row owns becomes a dep edge on the
//     owner (order-independent); an unowned one stays here with the
//     keys, as the sole describer.
func (h *harvester) parsePkgConfig() error {
	var files []string
	for _, dir := range []string{"lib/pkgconfig", "share/pkgconfig"} {
		files = append(files, walkFiles(filepath.Join(h.prefix, filepath.FromSlash(dir)), func(p string) bool {
			return strings.HasSuffix(p, ".pc")
		})...)
	}
	type pending struct {
		r       *row
		foreign []pcForeign
	}
	var pendings []pending
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		r, foreign := h.parsePC(strings.TrimSuffix(filepath.Base(f), ".pc"), filepath.Dir(f), string(body))
		pendings = append(pendings, pending{r, foreign})
	}
	// Placement: same library already harvested (SELF-artifact identity
	// OR matching consumer-facing name with no contradicting artifact):
	// MERGE the pc channel's keys into the claimant instead of dropping
	// them — the -l name keeps the LookupLinkLibrary redirect alive and
	// the Requires deps keep resolving (the pc name registers as an
	// alias). The surviving row carries the pc's foreign references.
	surviving := make([]*row, len(pendings))
	for i, p := range pendings {
		if claimant := h.sameLibraryClaimant(p.r); claimant != nil {
			name := strings.TrimPrefix(p.r.cmakeTarget, "pkgconfig::")
			h.warnf("pkgconfig %s: same library as %s (%s); channels merged", name, claimant.cmakeTarget, claimant.origin)
			h.mergeInto(claimant, p.r)
			surviving[i] = claimant
			continue
		}
		h.addRow(p.r)
		surviving[i] = p.r
	}
	for i, p := range pendings {
		h.resolvePCForeign(surviving[i], p.foreign)
	}
	return nil
}

// pcForeign is one Libs -l token that pcSelfLib did NOT tie to the pc
// package itself, with the artifact candidates the probe resolved for
// it — ownership is decided late, in resolvePCForeign.
type pcForeign struct {
	lib   string
	paths []string // anchored
}

// resolvePCForeign settles a row's foreign -l references once every
// channel's SELF claims are registered. An artifact another row owns
// turns into a DIRECT dep edge on the owner — the .pc spelled a
// dependency as a raw -l instead of a Requires entry, and first-parse
// order must not decide which package keeps its label. The -l name
// joins the OWNER's link_libraries (it names the owner's artifact, so
// the LookupLinkLibrary redirect lands there). Unowned references keep
// today's shape: this row carries the name + any resolved paths and
// claims them, as the only description the prefix has.
func (h *harvester) resolvePCForeign(r *row, foreign []pcForeign) {
	for _, fr := range foreign {
		var owner *row
		for _, p := range fr.paths {
			if prev := h.claimantOf(h.byPath[h.canonicalKey(p)]); prev != nil {
				owner = prev
				break
			}
		}
		if owner != nil && owner != r {
			owner.linkLibs = appendUnique(owner.linkLibs, fr.lib)
			r.depRefs = append(r.depRefs, owner.cmakeTarget)
			h.warnf("%s: Libs -l%s resolves to an artifact %s owns; recorded as a dep edge, not claimed", r.cmakeTarget, fr.lib, owner.cmakeTarget)
			continue
		}
		r.linkLibs = appendUnique(r.linkLibs, fr.lib)
		for _, p := range fr.paths {
			r.linkPaths = appendUnique(r.linkPaths, p)
			if k := h.canonicalKey(p); h.byPath[k] == nil {
				h.byPath[k] = r
			}
		}
	}
}

func (h *harvester) parsePC(name, pcdir, body string) (*row, []pcForeign) {
	// Seed pkg-config's two built-in path vars alongside prefix.
	// ${pcfiledir} is the directory holding THIS .pc file — the
	// increasingly-common fully-relocatable idiom derives every path from
	// it (libdir=${pcfiledir}/../lib, Cflags: -I${pcfiledir}/../include)
	// rather than from prefix=; without the seed those expand empty and
	// the -L/-I silently vanish. ${pc_sysrootdir} defaults empty (the
	// PKG_CONFIG_SYSROOT_DIR-unset case), matching pkg-config.
	vars := map[string]string{
		"prefix":        strings.TrimSuffix(h.prefix, "/"),
		"pcfiledir":     strings.TrimSuffix(pcdir, "/"),
		"pc_sysrootdir": "",
	}
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
	foreign := h.applyPCLibs(r, name, fields["Libs"]+" "+fields["Libs.private"])
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
	return r, foreign
}

// applyPCLibs walks the Libs/Libs.private token stream: -L dirs feed
// the probe's search path, each -l<name> resolves its artifact in the
// prefix — the path key the tool/fragment lifts, wrapper-gen, and the
// same-library dedup need. The probe covers lib64 and versioned
// sonames (libfoo.so.1.2.3 with no plain .so): a miss here is what
// lets a bundle+pc pair slip past path identity and collide at
// generation time.
//
// Only SELF tokens — name affinity with the pc package per pcSelfLib,
// or the package's sole prefix-resolved (or sole overall) -l — land on
// the row here; everything else returns as foreign for
// resolvePCForeign, so a -l that names ANOTHER package's library can
// neither claim its artifact nor drag this row into a bogus
// same-library merge.
func (h *harvester) applyPCLibs(r *row, name, libsField string) []pcForeign {
	var searchDirs []string
	var toks []pcForeign
	// A `-l` repeated on the Libs line is pkg-config's way of breaking a
	// cyclic static-archive SCC (a single-pass linker rescans the archive on
	// the later occurrence). Count occurrences so the SELF archive can be
	// flagged whole-archive (alwayslink) below — the Bazel-native equivalent.
	libCount := map[string]int{}
	// The prefix multilib dirs are invariant per invocation; compute them once
	// (each call globs/stats the tree) rather than per -l token.
	libDirs := h.probeLibDirs()
	for _, tok := range strings.Fields(libsField) {
		switch {
		case strings.HasPrefix(tok, "-L"):
			searchDirs = append(searchDirs, strings.TrimPrefix(tok, "-L"))
		case strings.HasPrefix(tok, "-l"):
			lib := strings.TrimPrefix(tok, "-l")
			libCount[lib]++
			dirs := append(append([]string{}, searchDirs...), libDirs...)
			var paths []string
			for _, d := range dirs {
				paths = h.appendProbedArtifacts(paths, d, lib)
			}
			toks = append(toks, pcForeign{lib, paths})
		}
	}
	self := make([]bool, len(toks))
	anySelf := false
	for i, t := range toks {
		if pcSelfLib(name, t.lib) {
			self[i], anySelf = true, true
		}
	}
	// No affinity match at all: name matching is only an ARBITER, so a
	// package whose one real library carries a divergent name still
	// owns it. Self is then the SOLE -l that resolves to an artifact in
	// the prefix (zlib's `Libs: -lz -lm`: -lm probes to nothing — a
	// system lib — leaving -lz the only resolved candidate), or the
	// sole -l outright when nothing resolves (partial trees). A line
	// with SEVERAL resolved candidates and no name match stays
	// all-foreign — guessing an owner among them (an umbrella .pc
	// aggregating other packages' libs) is exactly the over-claiming
	// this split exists to stop.
	if !anySelf {
		resolved := -1
		for i, t := range toks {
			if len(t.paths) == 0 {
				continue
			}
			if resolved >= 0 {
				resolved = -1
				break
			}
			resolved = i
		}
		switch {
		case resolved >= 0:
			self[resolved] = true
		case len(toks) == 1:
			self[0] = true
		}
	}
	var foreign []pcForeign
	for i, t := range toks {
		if self[i] {
			r.linkLibs = appendUnique(r.linkLibs, t.lib)
			for _, p := range t.paths {
				r.linkPaths = appendUnique(r.linkPaths, p)
			}
			// This package's OWN `-l` repeated on the Libs line → its
			// archive is a cyclic static-archive SCC member. Flag it
			// whole-archive so wrappergen emits alwayslink on the cc_import.
			if libCount[t.lib] >= 2 {
				r.alwayslink = true
			}
			continue
		}
		foreign = append(foreign, t)
	}
	return foreign
}

// pcSelfLib reports whether a Libs `-l<lib>` plausibly names the pc
// package's OWN library: the pc name and the lib stem match exactly or
// modulo a `lib` prefix on either side, case-insensitively (libpng16.pc
// → -lpng16, SDL2.pc → -lsdl2).
func pcSelfLib(pcName, lib string) bool {
	n, l := strings.ToLower(pcName), strings.ToLower(lib)
	return n == l || n == "lib"+l || l == "lib"+n
}

// appendProbedArtifacts appends the anchored path of lib<name>.{a,so}
// (or the first versioned soname) under dir, when present.
func (h *harvester) appendProbedArtifacts(paths []string, dir, lib string) []string {
	for _, ext := range []string{".a", ".so"} {
		cand := filepath.Join(dir, "lib"+lib+ext)
		if _, err := os.Stat(cand); err == nil {
			if anchored, ok := h.anchoredFromImportPrefix(cand); ok {
				paths = appendUnique(paths, anchored)
			}
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "lib"+lib+".so.*")); len(matches) > 0 {
		sort.Strings(matches)
		if anchored, ok := h.anchoredFromImportPrefix(matches[0]); ok {
			paths = appendUnique(paths, anchored)
		}
	}
	return paths
}

// probeLibDirs returns the prefix-relative library directories the archive probe
// scans, covering the common multilib layouts so an archive that lives in a
// variant dir still resolves to a link_path:
//
//   - lib, lib64, lib32, libx32 — the standard {default,64,32,x32-ABI} split;
//   - lib/<triplet> — Debian/Ubuntu multiarch (lib/x86_64-linux-gnu,
//     lib/aarch64-linux-gnu, …). The triplet varies by arch, so lib/'s subdirs
//     are ENUMERATED (os.ReadDir) and filtered by triplet shape rather than
//     guessed; appendProbedArtifacts stats each candidate, so a non-lib dir here
//     is harmless (it just won't contain lib<name>.{a,so}).
//
// The result is invariant per harvester instance and MEMOIZED, so the ReadDir
// runs once regardless of how many archive fragments probe. The caller may
// prepend channel-specific search dirs (a .pc's own -L dirs).
func (h *harvester) probeLibDirs() []string {
	if h.libDirsMemo != nil {
		return h.libDirsMemo
	}
	var dirs []string
	for _, d := range []string{"lib", "lib64", "lib32", "libx32"} {
		dirs = append(dirs, filepath.Join(h.prefix, d))
	}
	// Debian/Ubuntu multiarch: lib/<triplet>/ (arch-os-abi, e.g.
	// x86_64-linux-gnu). Enumerate lib/'s subdirs rather than glob, so a prefix
	// path containing glob metacharacters can't mis-match or swallow an error;
	// a triplet has at least two hyphens, which excludes ordinary subdirs.
	libDir := filepath.Join(h.prefix, "lib")
	if ents, err := os.ReadDir(libDir); err == nil {
		var triplets []string
		for _, e := range ents {
			if e.IsDir() && strings.Count(e.Name(), "-") >= 2 {
				triplets = append(triplets, filepath.Join(libDir, e.Name()))
			}
		}
		sort.Strings(triplets)
		dirs = append(dirs, triplets...)
	}
	h.libDirsMemo = dirs
	return dirs
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
