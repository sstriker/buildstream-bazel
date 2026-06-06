// Package todos records "no-mechanical-form" cmake constructs — ones
// that have a perfectly good *Bazel* form but no faithful mechanical
// translation, so an author (human or AI post-pass) must re-express
// them. The canonical case is `add_test(NAME … COMMAND cmake -P
// <runner>)` integration harnesses: the idiomatic target is an
// sh_test / bazel_skylib diff_test driving the built artifact, but
// reaching it means re-authoring the cmake-script harness, not
// translating an AST.
//
// The converter already breadcrumbs these to stderr warnings (the
// add_test-not-converted audit, the cmake-internal-drop audit, the
// install(SCRIPT)/install(CODE) surface) so a human can read them. The
// Collector promotes the same breadcrumb to a structured, deterministic
// conversion-todos.json an AI post-pass can consume — the stderr
// warnings are retained alongside it.
//
// The Collector is opt-in (plumbed through lower.Options and written
// only when --conversion-todos-report=<path> is set) and mirrors the
// rejection / coverage collectors. With a nil collector every producer
// site is a no-op; with a non-nil one each site Adds one grouped entry
// alongside its (retained) stderr warning.
//
// Scope: this is the deterministic PRODUCER plus the consumer CONTRACT
// (idempotency via the stable id + a file-ownership split; the trust
// boundary). The non-deterministic AI post-pass that consumes the
// report is out of scope — see the "Agent-actionable prompts for
// no-mechanical-form constructs" item in ROADMAP.md.
package todos

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
)

// SchemaVersion fences future readers of conversion-todos.json.
const SchemaVersion = 1

// Anchor is one source site folded into a todo unit. A todo always
// carries at least one anchor (an empty list is a producer bug). Line
// is informational payload — the stable id deliberately excludes it so
// an unrelated edit above the construct can't churn the id.
type Anchor struct {
	File      string `json:"file,omitempty"`
	Line      int    `json:"line,omitempty"`
	Construct string `json:"construct"`
}

// Todo is one grouped unit of no-mechanical-form work: the shared unit
// (e.g. a single cmake -P runner shared by N add_test calls) produces
// ONE todo with N anchors.
type Todo struct {
	// ID is the idempotency key: hash(Kind, GroupKey). Set by the
	// Collector at report-build time; deliberately excludes anchor
	// line numbers so the id is stable across unrelated source edits.
	ID string `json:"id"`
	// Kind is the producer category: "cmake-p-test" |
	// "cmake-internal-drop" | "install-script" | "install-code".
	Kind string `json:"kind"`
	// GroupKey is the shared unit's stable identity — the producer's
	// grouping key (a runner path, a drop kind, an install site). Always
	// set and unique per unit, so it doubles as the sort key.
	GroupKey string `json:"group_key"`
	// Anchors are the ≥1 source sites folded into this unit.
	Anchors []Anchor `json:"anchors"`
	// Evidence carries kind-specific recovered facts (the runner, the
	// exe target, the invocations, the verification contract, …). JSON
	// marshaling sorts map keys, so this stays deterministic.
	Evidence map[string]any `json:"evidence,omitempty"`
	// SuggestedShape names the idiomatic Bazel form the post-pass
	// should author toward.
	SuggestedShape string `json:"suggested_shape,omitempty"`
	// Prompt is the per-unit instruction the post-pass works from.
	Prompt string `json:"prompt,omitempty"`
}

// Preamble is the standing guidance the post-pass reads before working
// the list. The built-in default populates Intent/Rules/Example; an
// operator override (--conversion-todos-preamble=<file>) replaces the
// whole block with the file's text via Text. JSON consumers read
// whichever fields are present.
type Preamble struct {
	Intent  string `json:"intent,omitempty"`
	Rules   string `json:"rules,omitempty"`
	Example string `json:"example,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Report is the on-disk conversion-todos.json shape.
type Report struct {
	Version     int      `json:"version"`
	ToolVersion string   `json:"tool_version,omitempty"`
	Preamble    Preamble `json:"preamble"`
	Todos       []Todo   `json:"todos"`
}

// Collector accumulates todos concurrent-safely. The zero value is
// ready; nil is a no-op sink so callers can pass it unconditionally.
type Collector struct {
	mu    sync.Mutex
	items []Todo
}

// New returns a fresh Collector.
func New() *Collector { return &Collector{} }

// Add records one todo (no-op on a nil collector). The ID is computed
// at report-build time, so callers leave it empty.
func (c *Collector) Add(t Todo) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = append(c.items, t)
	c.mu.Unlock()
}

// Len returns the number of recorded todos.
func (c *Collector) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Reset drops all recorded todos. Callers that run the producers more
// than once against the same collector (e.g. the converter's two-pass
// genex / stamp-recovery re-lowers) Reset before the final pass so the
// report reflects only that pass rather than accumulating duplicates.
func (c *Collector) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = nil
	c.mu.Unlock()
}

// Report assembles the deterministic on-disk report: each todo gets its
// stable id, anchors are sorted by (file, line), and the todo slice is
// sorted by (kind, group_key). Same input → byte-identical report.
func (c *Collector) Report(pre Preamble, toolVersion string) Report {
	var raw []Todo
	if c != nil {
		c.mu.Lock()
		raw = make([]Todo, len(c.items))
		copy(raw, c.items)
		c.mu.Unlock()
	}
	items := make([]Todo, len(raw))
	for i, it := range raw {
		it.ID = ID(it.Kind, it.GroupKey)
		// Deep-copy the Anchors slice before sorting: the shallow copy
		// above shares each Todo's anchor backing array with the
		// Collector's internal state, so sorting in place would mutate it
		// (and race with a concurrent Report). Report must be a pure
		// assembler with no side effects on the Collector.
		anchors := append([]Anchor(nil), it.Anchors...)
		sort.SliceStable(anchors, func(a, b int) bool {
			if anchors[a].File != anchors[b].File {
				return anchors[a].File < anchors[b].File
			}
			if anchors[a].Line != anchors[b].Line {
				return anchors[a].Line < anchors[b].Line
			}
			return anchors[a].Construct < anchors[b].Construct
		})
		it.Anchors = anchors
		items[i] = it
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Kind != items[b].Kind {
			return items[a].Kind < items[b].Kind
		}
		return items[a].GroupKey < items[b].GroupKey
	})
	// items is make()'d above (never nil), so it marshals as `[]` when
	// empty — consumers can iterate the array unconditionally.
	return Report{
		Version:     SchemaVersion,
		ToolVersion: toolVersion,
		Preamble:    pre,
		Todos:       items,
	}
}

// ID is the stable idempotency key for a unit: a content hash of
// (kind, group_key). Deterministic and line-free.
func ID(kind, groupKey string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + groupKey))
	return "todo-" + hex.EncodeToString(sum[:])[:16]
}
