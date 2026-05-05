package main

import (
	"strings"
	"testing"
)

// TestEmitBuild_DeterministicGivenSameInput verifies the
// converter is deterministic — the same trace events, imports,
// and make-db produce byte-identical BUILD.bazel.out across
// runs. This is the precondition for the stability story:
// "changes to a dependency that have no bearing on the build
// graph should keep project B BUILD files stable."
//
// If the underlying build's compile/link command shape stays
// the same (because a comment-only edit doesn't change the
// build graph), the trace's recorded events are the same, and
// the converter's output is the same. Bazel's action cache +
// remote cache then deliver the unchanged BUILD.bazel.out to
// every consumer.
func TestEmitBuild_DeterministicGivenSameInput(t *testing.T) {
	events := []Event{
		{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}, Copts: []string{"-fstack-protector-strong"}},
		{Kind: EventCompile, Out: "bar.o", Srcs: []string{"bar.c"}, Copts: []string{"-fstack-protector-strong"}},
		{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o", "bar.o"}},
		{Kind: EventLink, Out: "myapp", Srcs: []string{"myapp.c"}, Libs: []string{"foo"}, Copts: []string{"-fstack-protector-strong"}},
	}
	first := emitBuild(correlate(events), nil, nil, nil)
	second := emitBuild(correlate(events), nil, nil, nil)
	if first != second {
		t.Errorf("emitBuild non-deterministic across runs:\n--first--\n%s\n--second--\n%s", first, second)
	}
}

// TestEmitBuild_StableUnderTraceNoise covers a related case:
// the trace artifact bytes can vary across runs (PIDs differ
// between processes; strace's pointer addresses + `/* N vars */`
// annotations differ when env counts change) but the recovered
// events should be identical. emitBuild's output stability
// depends on event-level identity, not trace-byte identity.
//
// Here we synthesize two event lists that are equal except for
// trace-level cosmetic noise (an extra PID-different event
// re-ordering); the converter's output should be identical
// because correlate() de-arrives by event kind and target.
func TestEmitBuild_StableUnderTraceNoise(t *testing.T) {
	base := []Event{
		{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}},
		{Kind: EventCompile, Out: "bar.o", Srcs: []string{"bar.c"}},
		{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o", "bar.o"}},
	}
	// Re-order compile events; archive still references both
	// .o by name. Output must match.
	reordered := []Event{
		{Kind: EventCompile, Out: "bar.o", Srcs: []string{"bar.c"}},
		{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}},
		{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o", "bar.o"}},
	}
	first := emitBuild(correlate(base), nil, nil, nil)
	second := emitBuild(correlate(reordered), nil, nil, nil)
	if first != second {
		t.Errorf("emitBuild diverged on event reorder:\n--first--\n%s\n--second--\n%s", first, second)
	}
	// Sanity: the output isn't trivially empty.
	if !strings.Contains(first, `cc_library(`) {
		t.Fatalf("expected a cc_library in the output:\n%s", first)
	}
}
