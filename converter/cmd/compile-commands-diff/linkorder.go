package main

// Link-line ORDER fidelity (Q2). For static archives the first library to
// satisfy an undefined symbol wins, so the ORDER of libraries on the link line
// is semantically load-bearing. This compares, per linked executable, the
// relative order of libraries on cmake's link line (codemodel
// link.commandFragments, role=libraries — the authoritative ordered form; the
// Ninja generator emits no link.txt) against Bazel's (`aquery
// 'mnemonic("CppLink",//...)'` arguments).
//
// SCOPE: SYSTEM libraries (stdc++, m, pthread, dl, rt, … — they name
// identically on both sides) AND PROJECT ARCHIVES. A project archive maps to its
// cmake Target.Name on both sides via three forms: cmake's link fragment (a
// library path like lib/libz.so) → Target.Name through the NameOnDisk map;
// Bazel's DYNAMIC solib ref (-lelements_Szlib_Slibzlib) → Target.Name through
// demangleBazelSolib; and Bazel's STATIC archive path
// (bazel-out/.../bin/<pkg>/lib<target>.a, under --dynamic_mode=off) →
// Target.Name through targetFromBazelArchive. So the same cmake Name is the
// common key whichever link mode Bazel used.
//
// CAVEAT surfaced in the report: under DEFAULT dynamic_mode Bazel links
// cc_library deps DYNAMICALLY, where symbol resolution is order-INDEPENDENT,
// while cmake here links static — so a project-archive order divergence only
// has teeth when Bazel ALSO links static (the derive-build-lens-link-mode work,
// --dynamic_mode=off for all-static codemodels). The system-lib tail is
// comparable regardless. find_package/external libs (mapping via the imports
// manifest's BazelLabel) are the remaining sub-layer, not yet matched.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// systemLibs is the allowlist of libraries whose identity is stable across
// cmake and Bazel link lines, so their relative order is directly comparable.
var systemLibs = map[string]bool{
	"stdc++": true, "c++": true, "c++abi": true, "m": true, "pthread": true,
	"dl": true, "rt": true, "c": true, "gcc": true, "gcc_s": true, "util": true,
	"nsl": true, "resolv": true, "anl": true, "crypt": true, "atomic": true,
	"execinfo": true, "socket": true,
}

// linkOrderReport is the per-binary link-order comparison outcome.
type linkOrderReport struct {
	Matched    int                 `json:"matched"`     // binaries present in both
	Inversions map[string][]string `json:"inversions"`  // binary → human-readable inverted system-lib pairs
	CmakeOrder map[string][]string `json:"cmake_order"` // binary → cmake system-lib order
	BazelOrder map[string][]string `json:"bazel_order"` // binary → bazel system-lib order
}

// libIdentity extracts a comparable system-lib identity from a single link
// token (cmake fragment or bazel argv entry), or "" if the token isn't a
// recognized system lib. Handles `-l<name>`, `-l <name>`-joined, and absolute
// paths like /usr/lib/x86_64-linux-gnu/libpthread.so.0.
func libIdentity(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return ""
	}
	var name string
	switch {
	case strings.HasPrefix(tok, "-l"):
		name = strings.TrimPrefix(tok, "-l")
	case strings.Contains(tok, "/lib") && (strings.Contains(tok, ".so") || strings.HasSuffix(tok, ".a")):
		base := filepath.Base(tok)
		base = strings.TrimPrefix(base, "lib")
		// strip extension(s): libpthread.so.0 -> pthread
		if i := strings.Index(base, ".so"); i >= 0 {
			base = base[:i]
		} else {
			base = strings.TrimSuffix(base, ".a")
		}
		name = base
	default:
		return ""
	}
	if systemLibs[name] {
		return name
	}
	return ""
}

// sonameBase normalizes a library file basename to its unversioned form so a
// cmake link fragment (libz.so.1.3.1) and a target's NameOnDisk (libz.so.1) map
// to the same key (libz.so). Static archives keep their .a name.
func sonameBase(base string) string {
	if i := strings.Index(base, ".so"); i >= 0 {
		return base[:i+len(".so")]
	}
	return base
}

// demangleBazelSolib reverses Bazel's solib name escaping in a `-l<mangled>`
// token (`_S`→`/`, `_U`→`_`, `_D`→`-`, `_C`→`:`) and returns the cmake target
// name it corresponds to: the path's basename with a leading `lib` stripped.
// `-lelements_Szlib_Slibzlib` → `elements/zlib/libzlib` → `zlib`. Returns "" if
// the token isn't a `-l` solib reference.
func demangleBazelSolib(tok string) string {
	if !strings.HasPrefix(tok, "-l") {
		return ""
	}
	m := strings.TrimPrefix(tok, "-l")
	if !strings.Contains(m, "_S") { // not a mangled package path
		return ""
	}
	r := strings.NewReplacer("_S", "/", "_U", "_", "_D", "-", "_C", ":")
	p := r.Replace(m)
	base := filepath.Base(p)
	return strings.TrimPrefix(base, "lib")
}

// orderedLibIdentities returns the libraries in argv/fragment order — system
// libs ("sys:<name>") AND project archives ("tgt:<cmake-target>") — deduped to
// first occurrence (order semantics care about first appearance). nameToTarget
// maps a cmake artifact basename (sonameBase form) → cmake target name, used to
// resolve cmake-side in-tree path fragments; pass nil on the Bazel side, where
// project libs resolve via solib demangling instead.
func orderedLibIdentities(tokens []string, nameToTarget map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, t := range tokens {
		if sys := libIdentity(t); sys != "" {
			add("sys:" + sys)
			continue
		}
		// Bazel solib reference -> cmake target (project archive).
		if tgt := demangleBazelSolib(t); tgt != "" {
			add("tgt:" + tgt)
			continue
		}
		// A .a/.so library PATH. On the cmake side (nameToTarget != nil) resolve
		// it via the NameOnDisk map. On the Bazel side (nil map) it's the STATIC
		// link form — `bazel-out/.../bin/<pkg>/lib<target>.a` — so the target is
		// the basename with lib/.a stripped (the rule name == cmake Target.Name);
		// this is what makes project-archive order comparable under
		// --dynamic_mode=off (dynamic project deps arrive as -l<mangled> solibs,
		// handled above).
		if strings.Contains(t, ".so") || strings.HasSuffix(t, ".a") {
			if nameToTarget != nil {
				key := sonameBase(filepath.Base(strings.TrimSpace(t)))
				if tgt, ok := nameToTarget[key]; ok {
					add("tgt:" + tgt)
				}
			} else if tgt := targetFromBazelArchive(t); tgt != "" {
				add("tgt:" + tgt)
			}
		}
	}
	return out
}

// targetFromBazelArchive maps a Bazel STATIC archive path (the --dynamic_mode=off
// link form, `bazel-out/.../bin/<pkg>/lib<target>.a`) to its cmake target name:
// the basename with the `lib` prefix and `.a` suffix stripped. The converter
// names a cc_library after the cmake Target.Name, so its artifact is
// lib<Name>.a — landing on the same key the cmake side resolves via NameOnDisk.
// "" when the token isn't a lib*.a path.
func targetFromBazelArchive(tok string) string {
	tok = strings.TrimSpace(tok)
	if !strings.HasSuffix(tok, ".a") {
		return ""
	}
	base := filepath.Base(tok)
	if !strings.HasPrefix(base, "lib") {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(base, "lib"), ".a")
}

// aqueryLinkDoc is the slice of a CppLink aquery jsonproto needed to recover
// each link action's argv and resolve its output binary's basename.
type aqueryLinkDoc struct {
	Actions []struct {
		Mnemonic        string   `json:"mnemonic"`
		Arguments       []string `json:"arguments"`
		PrimaryOutputId int      `json:"primaryOutputId"`
		OutputIds       []int    `json:"outputIds"`
	} `json:"actions"`
	Artifacts []struct {
		Id             int `json:"id"`
		PathFragmentId int `json:"pathFragmentId"`
	} `json:"artifacts"`
	PathFragments []struct {
		Id       int    `json:"id"`
		Label    string `json:"label"`
		ParentId int    `json:"parentId"`
	} `json:"pathFragments"`
}

// resolveOutputBase walks the pathFragment tree to build an artifact's path and
// returns its basename (the linked binary name, e.g. "example").
func (d *aqueryLinkDoc) resolveOutputBase(artifactId int) string {
	fragID := 0
	for _, a := range d.Artifacts {
		if a.Id == artifactId {
			fragID = a.PathFragmentId
			break
		}
	}
	if fragID == 0 {
		return ""
	}
	labels := map[int]string{}
	parent := map[int]int{}
	for _, p := range d.PathFragments {
		labels[p.Id] = p.Label
		parent[p.Id] = p.ParentId
	}
	// The basename is the leaf fragment's label.
	return labels[fragID]
}

// linkOrderDiff loads cmake's codemodel reply + a CppLink aquery and compares,
// per matched executable, the relative order of system libs. Returns nil (no
// report) when either source is unavailable.
func linkOrderDiff(replyDir, aqueryLinkPath string) (*linkOrderReport, error) {
	reply, err := fileapi.Load(replyDir)
	if err != nil {
		return nil, fmt.Errorf("cmake codemodel: %w", err)
	}
	b, err := os.ReadFile(aqueryLinkPath)
	if err != nil {
		return nil, fmt.Errorf("aquery-link: %w", err)
	}
	var doc aqueryLinkDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("aquery-link parse: %w", err)
	}
	return compareLinkOrder(reply, &doc), nil
}

// compareLinkOrder is the pure comparison half of linkOrderDiff: given a loaded
// cmake codemodel reply and a parsed CppLink aquery, it builds each matched
// executable's ordered lib identities on both sides and reports the order
// inversions. Split out so the wiring (cmake Link.CommandFragments → ordered
// identities, aquery args → ordered identities, binary matching by basename,
// inversion detection) is unit-testable without an on-disk reply dir.
func compareLinkOrder(reply *fileapi.Reply, doc *aqueryLinkDoc) *linkOrderReport {
	// Map every in-tree library target's artifact basename (sonameBase form) to
	// its cmake target name, so cmake-side link fragments (which name libraries
	// by output path, e.g. lib/libcurl.a) resolve to the same identity Bazel's
	// demangled solib refs do.
	nameToTarget := map[string]string{}
	for _, t := range reply.Targets {
		switch t.Type {
		case "STATIC_LIBRARY", "SHARED_LIBRARY", "MODULE_LIBRARY":
			if t.NameOnDisk != "" {
				nameToTarget[sonameBase(t.NameOnDisk)] = t.Name
			}
		}
	}

	// cmake: binary basename -> ordered lib identities (EXECUTABLE targets).
	cmakeOrd := map[string][]string{}
	for _, t := range reply.Targets {
		if t.Type != "EXECUTABLE" || t.Link == nil {
			continue
		}
		var toks []string
		for _, f := range t.Link.CommandFragments {
			if f.Role == "libraries" || f.Role == "flags" {
				toks = append(toks, strings.Fields(f.Fragment)...)
			}
		}
		base := t.NameOnDisk
		if base == "" {
			base = t.Name
		}
		if ord := orderedLibIdentities(toks, nameToTarget); len(ord) > 0 {
			cmakeOrd[base] = ord
		}
	}

	// bazel: binary basename -> ordered lib identities (CppLink actions).
	bazelOrd := map[string][]string{}
	for _, a := range doc.Actions {
		if a.Mnemonic != "CppLink" {
			continue
		}
		base := doc.resolveOutputBase(a.PrimaryOutputId)
		if base == "" {
			continue
		}
		if ord := orderedLibIdentities(a.Arguments, nil); len(ord) > 0 {
			bazelOrd[base] = ord
		}
	}

	rep := &linkOrderReport{
		Inversions: map[string][]string{},
		CmakeOrder: map[string][]string{},
		BazelOrder: map[string][]string{},
	}
	for base, cord := range cmakeOrd {
		bord, ok := bazelOrd[base]
		if !ok {
			continue
		}
		rep.Matched++
		if inv := orderInversions(cord, bord); len(inv) > 0 {
			rep.Inversions[base] = inv
			rep.CmakeOrder[base] = cord
			rep.BazelOrder[base] = bord
		}
	}
	return rep
}

// orderInversions returns the pairs (x before y) whose relative order differs
// between the two sequences, considering only libs present in BOTH.
func orderInversions(a, b []string) []string {
	posB := map[string]int{}
	for i, x := range b {
		posB[x] = i
	}
	// keep only common libs, in a's order
	var common []string
	for _, x := range a {
		if _, ok := posB[x]; ok {
			common = append(common, x)
		}
	}
	var inv []string
	for i := 0; i < len(common); i++ {
		for j := i + 1; j < len(common); j++ {
			// common[i] is before common[j] in a (by construction); flag if
			// it's AFTER in b.
			if posB[common[i]] > posB[common[j]] {
				inv = append(inv, fmt.Sprintf("%s<->%s", common[i], common[j]))
			}
		}
	}
	sort.Strings(inv)
	return inv
}

func (r *linkOrderReport) print(w *os.File) {
	fmt.Fprintf(w, "\nlink-order fidelity (system + project libs): %d binaries matched\n", r.Matched)
	if len(r.Inversions) == 0 {
		fmt.Fprintln(w, "  no link-order inversions on common libs")
		return
	}
	keys := make([]string, 0, len(r.Inversions))
	for k := range r.Inversions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  %s: inverted pairs %s\n", k, strings.Join(r.Inversions[k], " "))
		fmt.Fprintf(w, "      cmake: %s\n", strings.Join(r.CmakeOrder[k], " "))
		fmt.Fprintf(w, "      bazel: %s\n", strings.Join(r.BazelOrder[k], " "))
	}
	fmt.Fprintln(w, "  (note: Bazel may link cc_library deps dynamically — project-archive")
	fmt.Fprintln(w, "   order is order-independent there; system-lib order shown above is comparable.)")
}
