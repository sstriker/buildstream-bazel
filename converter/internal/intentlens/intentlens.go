// Package intentlens implements the deterministic halves of the
// intent-capture survey lens: an agent-as-oracle "what did we miss?" pass over
// a converted project (see the lens item in ROADMAP.md).
//
// The lens itself has three steps; only the first and third are deterministic
// and live here. The middle step — the actual LLM judgment — is a PLUGGABLE
// command the operator wires (e.g. `claude -p`), so this package never calls a
// model and the survey/gate can stub it:
//
//  1. AssemblePrompt — given the converted bundle (rendered BUILD.bazel +
//     MODULE.bazel + the original cmake sources) and the reports the converter
//     already produced (conversion-todos.json + rejections.json), build the
//     grounded prompt: standing context, the one question, the file manifest to
//     read, the ALREADY-FLAGGED set (so the judge reports only NET-new misses),
//     and the grounding rule (cite a cmake source ref per finding).
//  2. <pluggable judge> — reads the prompt on stdin, emits JudgeOutput JSON.
//  3. Triage — classify each finding net-new vs already-flagged (deduped
//     against the todos/rejections), bucket by severity, write the Report.
//
// The non-determinism is quarantined to step 2, so the lens is a triage queue,
// not a pass/fail gate (unlike the deterministic lenses' counts). A real miss
// it surfaces that is NOT already a todo/rejection is a producer/lowering gap —
// that's the point: this is the inverse of the conversion-todos producer.
package intentlens

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

// SchemaVersion fences future readers of intent-capture.json.
const SchemaVersion = 1

// Finding statuses.
const (
	StatusNetNew       = "net-new"
	StatusDupTodo      = "dup-todo"
	StatusDupRejection = "dup-rejection"
)

// Finding is one claimed intent-miss the judge reports.
type Finding struct {
	// Category is the kind of intent dropped: test | install | option |
	// visibility | codegen | target | flag | other. Free-form (the judge may
	// use another label); used only for grouping.
	Category string `json:"category"`
	// Severity is the judge's importance estimate: high | medium | low.
	Severity string `json:"severity"`
	// Summary is the one-line description of what's missing.
	Summary string `json:"summary"`
	// Evidence is the judge's cmake-side-vs-Bazel-side justification.
	Evidence string `json:"evidence,omitempty"`
	// CMakeRef grounds the finding in the cmake sources: "file" or
	// "file:line". REQUIRED for a finding to be checkable; an empty ref can't
	// be deduped, so it's reported net-new with a caveat.
	CMakeRef string `json:"cmake_ref,omitempty"`
}

// JudgeOutput is the raw JSON the pluggable judge emits on stdout.
type JudgeOutput struct {
	Findings []Finding `json:"findings"`
}

// TriagedFinding is a Finding plus its dedup verdict.
type TriagedFinding struct {
	Finding
	// Status is net-new | dup-todo | dup-rejection.
	Status string `json:"status"`
	// MatchedID is the todo id (or rejection code) a dup matched, for audit.
	MatchedID string `json:"matched_id,omitempty"`
}

// Summary is the headline aggregate.
type Summary struct {
	Total          int            `json:"total"`
	NetNew         int            `json:"net_new"`
	AlreadyFlagged int            `json:"already_flagged"`
	BySeverity     map[string]int `json:"by_severity"`
}

// Report is the on-disk intent-capture.json.
type Report struct {
	Version     int              `json:"version"`
	ToolVersion string           `json:"tool_version,omitempty"`
	Element     string           `json:"element,omitempty"`
	Summary     Summary          `json:"summary"`
	Findings    []TriagedFinding `json:"findings"`
}

// DefaultContext is the standing context handed to the judge: what the project
// is and what it's authored against, so a "miss" is judged against the right
// target idioms rather than rediscovered. Mirrors the conversion-todos
// preamble's environment block so both halves of the agent flow agree.
const DefaultContext = "This is a Bazel project mechanically converted from a CMake project. " +
	"It targets Bazel 9 and is authored against these rule providers, NOT native rules " +
	"(Bazel 9 removed the native sh rules and deprecated native cc): C/C++ via " +
	"`@rules_cc//cc:defs.bzl`; shell tests/binaries via `@rules_shell//shell:*.bzl`; " +
	"file-comparison tests via `@bazel_skylib//rules:diff_test.bzl`; install/packaging " +
	"via `@rules_pkg//pkg:mappings.bzl`. The converted BUILD files are gazelle-cc-maintained. " +
	"Some constructs have no faithful mechanical translation and were deliberately left for " +
	"a post-pass (see conversion-todos.json) or refused (see rejections.json) — those are " +
	"ALREADY KNOWN, not misses."

// PromptInputs are the bundle + reports AssemblePrompt grounds the question in.
type PromptInputs struct {
	// Element is the converted element's name (for the prompt header).
	Element string
	// ConvertedDir holds the rendered BUILD.bazel + MODULE.bazel.
	ConvertedDir string
	// CMakeSrcDir is the original cmake source tree (anchors point here).
	CMakeSrcDir string
	// Todos is the parsed conversion-todos.json (already-flagged set).
	Todos todos.Report
	// Rejections is the parsed rejections.json (already-refused set).
	Rejections []rejection.Rejection
	// Context overrides DefaultContext when non-empty.
	Context string
}

// manifest returns the sorted, dir-relative files under dir whose base name
// satisfies keep — used to enumerate the cmake sources and the rendered Bazel
// files the judge should read, layout-agnostically (flat fixture or packaged
// workspace).
func manifest(dir string, keep func(base string) bool) []string {
	var out []string
	if dir == "" {
		return out
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if keep(d.Name()) {
			if rel, rerr := filepath.Rel(dir, p); rerr == nil {
				out = append(out, rel)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func cmakeSourceManifest(dir string) []string {
	return manifest(dir, func(base string) bool {
		return base == "CMakeLists.txt" || strings.HasSuffix(base, ".cmake")
	})
}

// bazelFileManifest enumerates the rendered Bazel files (BUILD/MODULE/.bzl) the
// judge audits, so the prompt works whether they're flat in ConvertedDir (the
// gate fixture) or packaged under it (a survey workspace: MODULE.bazel at top,
// BUILD.bazel in the element package).
func bazelFileManifest(dir string) []string {
	return manifest(dir, func(base string) bool {
		return base == "BUILD.bazel" || base == "BUILD" ||
			base == "MODULE.bazel" || strings.HasSuffix(base, ".bzl")
	})
}

// AssemblePrompt builds the grounded prompt text. Deterministic: same inputs →
// same bytes.
func AssemblePrompt(in PromptInputs) string {
	ctx := in.Context
	if ctx == "" {
		ctx = DefaultContext
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Intent-capture review")
	if in.Element != "" {
		fmt.Fprintf(&b, " — %s", in.Element)
	}
	b.WriteString("\n\n")
	b.WriteString(ctx)
	b.WriteString("\n\n## Your task\n")
	b.WriteString("Did the converted Bazel project capture ALL the intent of the cmake project? " +
		"What did it MISS? Hunt the SILENT losses — a dropped test target, an install layout, " +
		"an option default, a visibility constraint, a build-time codegen step — that compile " +
		"fine and are flagged NOWHERE. Do NOT re-report anything already in the " +
		"already-flagged set below; report only NET-NEW misses.\n")

	b.WriteString("\n## Read these (the converted bundle)\n")
	b.WriteString("- Rendered Bazel (the mechanical output you are auditing):\n")
	bzl := bazelFileManifest(in.ConvertedDir)
	if len(bzl) == 0 {
		b.WriteString("  - " + filepath.Join(in.ConvertedDir, "BUILD.bazel") + "\n")
	}
	for _, f := range bzl {
		b.WriteString("  - " + filepath.Join(in.ConvertedDir, f) + "\n")
	}
	b.WriteString("- Original cmake sources (the intent to compare against):\n")
	for _, f := range cmakeSourceManifest(in.CMakeSrcDir) {
		b.WriteString("  - " + filepath.Join(in.CMakeSrcDir, f) + "\n")
	}

	b.WriteString("\n## Already flagged — do NOT re-report these\n")
	if len(in.Todos.Todos) == 0 && len(in.Rejections) == 0 {
		b.WriteString("(none)\n")
	}
	for _, t := range in.Todos.Todos {
		fmt.Fprintf(&b, "- todo `%s` [%s/%s] %s\n", t.ID, t.Kind, t.Disposition, t.GroupKey)
	}
	for _, r := range in.Rejections {
		ref := r.Source
		if r.Target != "" {
			ref = r.Target + " @ " + ref
		}
		fmt.Fprintf(&b, "- rejection `%s` %s\n", r.Code, ref)
	}

	b.WriteString("\n## Output contract\n")
	b.WriteString("Emit STRICT JSON only (no prose), shape:\n")
	b.WriteString("`{\"findings\":[{\"category\":\"test|install|option|visibility|codegen|target|flag|other\"," +
		"\"severity\":\"high|medium|low\",\"summary\":\"…\",\"evidence\":\"cmake side … vs Bazel side …\"," +
		"\"cmake_ref\":\"<file>:<line>\"}]}`\n")
	b.WriteString("GROUNDING: every finding MUST set `cmake_ref` to the cmake source file " +
		"(and line if known) that proves the intent exists — an ungrounded claim can't be " +
		"verified. If you find nothing net-new, emit `{\"findings\":[]}`.\n")
	return b.String()
}

// refFile splits a "file:line" (or "file") CMakeRef into its file part.
func refFile(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	// Split off a trailing :line if present (line is all-digits).
	if i := strings.LastIndex(ref, ":"); i > 0 {
		if _, err := strconv.Atoi(ref[i+1:]); err == nil {
			return ref[:i]
		}
	}
	return ref
}

// sameFile reports whether two cmake source refs name the same file, tolerant
// of dir-relative vs absolute forms (compares by basename + path suffix).
func sameFile(a, b string) bool {
	a, b = refFile(a), refFile(b)
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.ToSlash(a), filepath.ToSlash(b)
	if a == b {
		return true
	}
	if filepath.Base(a) != filepath.Base(b) {
		return false
	}
	// Same basename: treat as a match if one path is a suffix of the other
	// (handles "sub/foo.cmake" vs "/abs/sub/foo.cmake").
	return strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

// refBaseForMatch returns the basename of a CMakeRef's file part, but only when
// it's specific enough to dedup on. A bare "CMakeLists.txt" appears in nearly
// every construct/group_key, so substring-matching on it would over-dedup —
// return "" for it (such refs still match via sameFile path equality).
func refBaseForMatch(ref string) string {
	base := filepath.Base(refFile(ref))
	if base == "." || base == "/" || base == "CMakeLists.txt" {
		return ""
	}
	return base
}

// todoMatchesRef reports whether a finding's cmake ref names the same construct
// a todo already covers. The producers populate structured Anchor.File only on
// the rejection-mirror path; most carry the unit's identity in GroupKey (a
// runner path / unit id) or the Construct text, so we match on all three:
// reliable sameFile against any anchor File, plus a specific-basename substring
// of GroupKey / Construct.
func todoMatchesRef(t todos.Todo, ref string) bool {
	for _, a := range t.Anchors {
		if sameFile(ref, a.File) {
			return true
		}
	}
	base := refBaseForMatch(ref)
	if base == "" {
		return false
	}
	if strings.Contains(t.GroupKey, base) {
		return true
	}
	for _, a := range t.Anchors {
		if strings.Contains(a.Construct, base) {
			return true
		}
	}
	return false
}

// rejectionMatchesRef reports whether a finding's cmake ref names a construct a
// rejection already covers (sameFile against Source, or a specific-basename
// substring of Source / Target / Message).
func rejectionMatchesRef(r rejection.Rejection, ref string) bool {
	if sameFile(ref, r.Source) {
		return true
	}
	base := refBaseForMatch(ref)
	if base == "" {
		return false
	}
	return strings.Contains(r.Source, base) ||
		strings.Contains(r.Target, base) ||
		strings.Contains(r.Message, base)
}

// Triage classifies each judge finding net-new vs already-flagged by grounding
// its cmake_ref against the todos' anchors and the rejections' sources, then
// assembles the deterministic Report (findings sorted by severity then summary).
func Triage(j JudgeOutput, rep todos.Report, rejections []rejection.Rejection, element, toolVersion string) Report {
	out := Report{
		Version:     SchemaVersion,
		ToolVersion: toolVersion,
		Element:     element,
		Summary:     Summary{BySeverity: map[string]int{}},
		Findings:    []TriagedFinding{},
	}
	for _, f := range j.Findings {
		tf := TriagedFinding{Finding: f, Status: StatusNetNew}
		if ref := f.CMakeRef; ref != "" {
			// Match against todos first (the no-mechanical-form set), then
			// rejections (the refused set).
			for _, t := range rep.Todos {
				if todoMatchesRef(t, ref) {
					tf.Status, tf.MatchedID = StatusDupTodo, t.ID
					break
				}
			}
			if tf.Status == StatusNetNew {
				for _, r := range rejections {
					if rejectionMatchesRef(r, ref) {
						tf.Status, tf.MatchedID = StatusDupRejection, string(r.Code)
						break
					}
				}
			}
		}
		out.Findings = append(out.Findings, tf)
	}
	sort.SliceStable(out.Findings, func(i, k int) bool {
		ri, rk := severityRank(out.Findings[i].Severity), severityRank(out.Findings[k].Severity)
		if ri != rk {
			return ri < rk
		}
		if out.Findings[i].Category != out.Findings[k].Category {
			return out.Findings[i].Category < out.Findings[k].Category
		}
		return out.Findings[i].Summary < out.Findings[k].Summary
	})
	for _, tf := range out.Findings {
		out.Summary.Total++
		if tf.Status == StatusNetNew {
			out.Summary.NetNew++
		} else {
			out.Summary.AlreadyFlagged++
		}
		sev := tf.Severity
		if sev == "" {
			sev = "unspecified"
		}
		out.Summary.BySeverity[sev]++
	}
	return out
}

// severityRank orders high < medium < low < anything-else so the headline
// findings sort first.
func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

// ParseJudgeOutput tolerantly extracts the JudgeOutput from a judge's stdout:
// the judge may wrap the JSON in prose or a ```json fence, so we slice from the
// first '{' to the last '}' before unmarshaling.
func ParseJudgeOutput(raw []byte) (JudgeOutput, error) {
	var j JudgeOutput
	s := string(raw)
	i, k := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || k < i {
		return j, fmt.Errorf("no JSON object found in judge output")
	}
	if err := json.Unmarshal([]byte(s[i:k+1]), &j); err != nil {
		return j, fmt.Errorf("parse judge output: %w", err)
	}
	return j, nil
}
