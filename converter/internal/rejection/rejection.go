// Package rejection records Tier-1 refusals that the converter would
// otherwise raise, so the operator can survey every refusal site in
// one run instead of fix-one-rerun-fix-one.
//
// The Collector is opt-in: it's plumbed through lower.Options (and the
// equivalent in other converters) only when --ignore-rejections-for-
// diagnostics is set. With nil collector, every refusal site behaves
// exactly as before — returning a typed failure.Error up the stack.
// With a non-nil collector, each refusal site Adds a Rejection record
// and falls through to a sensible local skip (drop the bad source,
// skip the dep, skip the target, etc.) so the lower can keep going
// and surface every refusal in the same pass.
//
// The resulting BUILD.bazel is NOT guaranteed to build — refused
// constructs are silently elided. The flag exists for diagnostic
// surveys (running the converter against a large real-world project
// to enumerate the refusal surface), not production conversion.
package rejection

import (
	"sync"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
)

// Rejection is one recorded refusal — the same information a typed
// failure.Error would have carried, plus optional Target / Source
// location hints for filterable post-processing.
type Rejection struct {
	// Code is the failure.Code that would have been raised. Stable
	// identifier; usable as an allowlist / filter key.
	Code failure.Code `json:"code"`
	// Message is the human-readable refusal description, identical
	// to what failure.Error would have carried.
	Message string `json:"message"`
	// Target, when set, names the cmake target whose lowering hit
	// the refusal — provides location context that bare failure.Error
	// doesn't expose because Tier-1 abort lost the call stack.
	Target string `json:"target,omitempty"`
	// Source, when set, names the source file (or input path) the
	// refusal fired on — same purpose as Target for source-path /
	// custom-command refusals.
	Source string `json:"source,omitempty"`
}

// Collector accumulates Rejections concurrent-safely. The zero value
// is ready to use; pass *Collector through Options to opt a refusal
// site into collect-mode.
type Collector struct {
	mu    sync.Mutex
	items []Rejection
}

// New returns a fresh Collector.
func New() *Collector { return &Collector{} }

// Add records one rejection. Safe for concurrent callers.
func (c *Collector) Add(code failure.Code, message string) {
	c.AddWithContext(code, message, "", "")
}

// AddWithContext records a rejection with optional target / source
// location hints.
func (c *Collector) AddWithContext(code failure.Code, message, target, source string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = append(c.items, Rejection{
		Code:    code,
		Message: message,
		Target:  target,
		Source:  source,
	})
	c.mu.Unlock()
}

// AddError is a convenience for the common pattern of recording a
// pre-constructed *failure.Error. Returns true if the error was
// recorded (collector non-nil), false otherwise — call sites use
// this to skip the legacy error-return path.
func (c *Collector) AddError(err *failure.Error) bool {
	if c == nil || err == nil {
		return false
	}
	c.Add(err.Code, err.Message)
	return true
}

// Reset clears the recorded rejections. ToIR can run more than once
// against the same collector (two-pass genex / stamp / nested-cmake
// recovery), re-emitting the same refusals each pass; callers Reset
// before the final pass so the report reflects only that pass's result
// rather than accumulating duplicates. Mirrors todos.Collector.Reset.
func (c *Collector) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = nil
	c.mu.Unlock()
}

// Items returns a copy of the recorded rejections in insertion order.
func (c *Collector) Items() []Rejection {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Rejection, len(c.items))
	copy(out, c.items)
	return out
}

// Len returns the number of recorded rejections.
func (c *Collector) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
