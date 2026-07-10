package main

// Link-graph SET fidelity (Q3). Where link-ORDER (linkorder.go) compares the
// relative ORDER of libraries on the link line, this compares the SET of link
// edges per binary: an edge present on cmake's link line but ABSENT from
// Bazel's is a candidate silently-dropped dependency — the exact symptom the
// harvest→wrappergen→lowering link-fidelity work targets. It reuses the same
// identity space and inputs as the order lens (cmake codemodel reply + CppLink
// aquery), so a dropped project archive or system lib surfaces as an
// only-in-cmake edge, and it is report-only (like the compile-db and link-order
// lenses; the symbol/ELF lens is the one that gates).
//
// SCOPE / CAVEAT (same as link-order): a project-archive edge only has teeth
// when Bazel ALSO links static — under default dynamic_mode a cc_library dep
// arrives as a `-l<mangled>` solib (still matched by demangling) but a genuinely
// absent one reads the same as a drop, so pass Bazel `--dynamic_mode=off` (a
// Bazel link-mode flag, not a cmake setting) when generating the CppLink aquery
// for a clean project-archive comparison. The
// system-lib layer is comparable regardless. find_package/external labels
// (imports-manifest BazelLabel) remain the sub-layer not yet matched on the
// Bazel side — see linkorder.go's caveat; they are neither in "sys:" nor "tgt:"
// identity, so an unmatched external is not misreported as a project drop.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// linkGraphReport is the per-binary link-edge SET comparison outcome.
type linkGraphReport struct {
	Matched   int                 `json:"matched"`    // binaries present in both link lines
	Dropped   int                 `json:"dropped"`    // total only-in-cmake edges (headline: candidate silent drops)
	OnlyCmake map[string][]string `json:"only_cmake"` // binary → edges on cmake's line, missing from Bazel's
	OnlyBazel map[string][]string `json:"only_bazel"` // binary → edges on Bazel's line, absent from cmake's
}

// compareLinkGraph is the pure comparison half of the link-graph lens: for each
// binary present on BOTH link lines it reports the set-difference of link-edge
// identities in each direction. It takes already-loaded inputs (main shares the
// parse with the link-order lens via loadLinkInputs) and is unit-testable
// without an on-disk reply dir.
func compareLinkGraph(reply *fileapi.Reply, doc *aqueryLinkDoc) *linkGraphReport {
	cmakeIDs, bazelIDs := perBinaryLibIdentities(reply, doc)
	rep := &linkGraphReport{
		OnlyCmake: map[string][]string{},
		OnlyBazel: map[string][]string{},
	}
	for base, cids := range cmakeIDs {
		bids, ok := bazelIDs[base]
		if !ok {
			continue
		}
		rep.Matched++
		if onlyC := setMinus(cids, bids); len(onlyC) > 0 {
			rep.OnlyCmake[base] = onlyC
			rep.Dropped += len(onlyC)
		}
		if onlyB := setMinus(bids, cids); len(onlyB) > 0 {
			rep.OnlyBazel[base] = onlyB
		}
	}
	return rep
}

// setMinus returns the elements of a not present in b, sorted for stable output.
func setMinus(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, x := range b {
		inB[x] = true
	}
	var out []string
	for _, x := range a {
		if !inB[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func (r *linkGraphReport) print(w *os.File) {
	fmt.Fprintf(w, "\nlink-graph fidelity (edge set, system + project libs): %d binaries matched\n", r.Matched)
	if r.Dropped == 0 {
		fmt.Fprintln(w, "  no cmake link edges missing from Bazel (0 is the healthy state)")
	} else {
		fmt.Fprintf(w, "  %d cmake link edge(s) MISSING from Bazel (candidate silent drops):\n", r.Dropped)
		for _, k := range sortedStrKeys(r.OnlyCmake) {
			fmt.Fprintf(w, "    %s: %s\n", k, strings.Join(r.OnlyCmake[k], " "))
		}
	}
	if len(r.OnlyBazel) > 0 {
		fmt.Fprintln(w, "  edges only on Bazel's line (over-link / static-vs-dynamic; informational):")
		for _, k := range sortedStrKeys(r.OnlyBazel) {
			fmt.Fprintf(w, "    %s: %s\n", k, strings.Join(r.OnlyBazel[k], " "))
		}
	}
	fmt.Fprintln(w, "  (note: project-archive edges are only comparable when Bazel links static —")
	fmt.Fprintln(w, "   --dynamic_mode=off; external/find_package labels are not yet matched, see link-order.)")
}

// writeJSON writes the report for the survey harness to consume. No trailing
// newline, matching the compile-db report.writeJSON in this tool.
func (r *linkGraphReport) writeJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
