package lower_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
)

// When the Rejections collector is non-nil, refusal sites must
// record the rejection and skip the offending construct instead of
// aborting the whole walk. The output IR is allowed to be empty /
// partial; what we lock here is "no error returned, and the
// collector saw the refusal."

func TestRejections_UnsupportedTargetType_SkippedNotErrored(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "obj", Id: "obj::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"obj::@1": {Name: "obj", Type: "GLOBAL_TARGET"},
		},
	}
	c := rejection.New()
	pkg, err := lower.ToIR(r, nil, lower.Options{Rejections: c})
	if err != nil {
		t.Fatalf("ToIR returned error in diagnostic mode: %v", err)
	}
	if pkg == nil {
		t.Fatal("ToIR returned nil package")
	}
	if c.Len() != 1 {
		t.Fatalf("collector recorded %d rejections, want 1; items=%+v", c.Len(), c.Items())
	}
	got := c.Items()[0]
	if got.Code != failure.UnsupportedTargetType {
		t.Errorf("code=%q, want %q", got.Code, failure.UnsupportedTargetType)
	}
	if got.Target != "obj" {
		t.Errorf("target=%q, want %q", got.Target, "obj")
	}
}

func TestRejections_FileAPIMalformed_DanglingRef_Skipped(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "ghost", Id: "ghost::@nonexistent"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{},
	}
	c := rejection.New()
	pkg, err := lower.ToIR(r, nil, lower.Options{Rejections: c})
	if err != nil {
		t.Fatalf("ToIR returned error in diagnostic mode: %v", err)
	}
	if pkg == nil {
		t.Fatal("ToIR returned nil package")
	}
	if c.Len() != 1 {
		t.Fatalf("collector recorded %d rejections, want 1", c.Len())
	}
	if got := c.Items()[0]; got.Code != failure.FileAPIMalformed || got.Target != "ghost" {
		t.Errorf("rejection=%+v, want code=%q target=%q", got, failure.FileAPIMalformed, "ghost")
	}
}

func TestRejections_UnsupportedCustomCommand_GeneratedSource_Skipped(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {
				Name: "lib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{
					Path:        "generated.c",
					IsGenerated: true,
				}},
			},
		},
	}
	c := rejection.New()
	pkg, err := lower.ToIR(r, nil, lower.Options{Rejections: c})
	if err != nil {
		t.Fatalf("ToIR returned error in diagnostic mode: %v", err)
	}
	if pkg == nil {
		t.Fatal("ToIR returned nil package")
	}
	if c.Len() == 0 {
		t.Fatalf("collector recorded 0 rejections, want >=1")
	}
	found := false
	for _, item := range c.Items() {
		if item.Code == failure.UnsupportedCustomCommand {
			found = true
			if item.Target != "lib" {
				t.Errorf("rejection target=%q, want %q", item.Target, "lib")
			}
		}
	}
	if !found {
		t.Errorf("no rejection with code %q found; items=%+v",
			failure.UnsupportedCustomCommand, c.Items())
	}
}

// When the collector is nil (default), refusal sites continue to
// return typed failure.Error. This re-states the strict-mode
// contract that production callers depend on.
func TestRejections_NilCollector_PreservesStrictErrors(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "obj", Id: "obj::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"obj::@1": {Name: "obj", Type: "GLOBAL_TARGET"},
		},
	}
	_, err := lower.ToIR(r, nil, lower.Options{})
	if err == nil {
		t.Fatal("ToIR with nil collector returned no error on unsupported target type; want failure.Error")
	}
}

// TestRejections_MultiConfig_FoldedNotRejected pins the Phase-5 multi-config
// behavior: a multi-config codemodel whose configurations share the same
// target set is folded (per-config flag/src/dep deltas become select() arms)
// rather than blanket-rejected — so diagnostic mode records NO
// unsupported-target-type. Only a target that exists solely in a non-primary
// configuration (which the first-config-primary fold can't recover) is
// flagged, and precisely.
func TestRejections_MultiConfig_FoldedNotRejected(t *testing.T) {
	// Two configs, identical target set (the common case: same targets,
	// flags differ per config). The fold handles it → no rejection.
	sameSet := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{
				{Name: "Release", Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}}},
				{Name: "Debug", Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}}},
			},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {Name: "lib", Type: "STATIC_LIBRARY"},
		},
	}
	c := rejection.New()
	if _, err := lower.ToIR(sameSet, nil, lower.Options{Rejections: c}); err != nil {
		t.Fatalf("ToIR errored in diagnostic mode: %v", err)
	}
	for _, it := range c.Items() {
		if it.Code == failure.UnsupportedTargetType {
			t.Errorf("multi-config with a shared target set should not record unsupported-target-type; got %+v", it)
		}
	}

	// Add a Debug-only target. The first-config-primary fold can't recover
	// it, so it (and only it) is flagged.
	configOnly := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{
				{Name: "Release", Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}}},
				{Name: "Debug", Targets: []fileapi.ConfigTargetRef{
					{Name: "lib", Id: "lib::@1"},
					{Name: "debug_only", Id: "debug_only::@2"},
				}},
			},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {Name: "lib", Type: "STATIC_LIBRARY"},
		},
	}
	c2 := rejection.New()
	if _, err := lower.ToIR(configOnly, nil, lower.Options{Rejections: c2}); err != nil {
		t.Fatalf("ToIR errored in diagnostic mode: %v", err)
	}
	var flagged []string
	for _, it := range c2.Items() {
		if it.Code == failure.UnsupportedTargetType {
			flagged = append(flagged, it.Target)
		}
	}
	if len(flagged) != 1 || flagged[0] != "debug_only" {
		t.Errorf("expected exactly the config-only target 'debug_only' flagged, got %v", flagged)
	}
}
