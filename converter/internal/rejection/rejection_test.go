package rejection_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
)

func TestCollector_AddRecordsCodeAndMessage(t *testing.T) {
	c := rejection.New()
	c.Add(failure.UnsupportedExecuteProcess, "no probe match")
	c.AddWithContext(failure.UnresolvedLinkDep, "Boost::system not in manifest", "myexe", "")
	got := c.Items()
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].Code != failure.UnsupportedExecuteProcess || got[0].Message != "no probe match" {
		t.Errorf("got[0]=%+v", got[0])
	}
	if got[1].Target != "myexe" || got[1].Code != failure.UnresolvedLinkDep {
		t.Errorf("got[1]=%+v", got[1])
	}
}

func TestCollector_NilSafe(t *testing.T) {
	var c *rejection.Collector
	c.Add(failure.UnsupportedExecuteProcess, "should not panic")
	c.AddWithContext(failure.UnsupportedExecuteProcess, "msg", "t", "s")
	if c.AddError(failure.New(failure.UnresolvedLinkDep, "x")) {
		t.Error("AddError(nil collector) should return false")
	}
	if c.Len() != 0 {
		t.Errorf("nil Len=%d, want 0", c.Len())
	}
	if c.Items() != nil {
		t.Errorf("nil Items=%v, want nil", c.Items())
	}
}

func TestCollector_AddErrorReturnsTrueWhenRecorded(t *testing.T) {
	c := rejection.New()
	err := failure.New(failure.UnsupportedCustomCommand, "weird shape")
	if !c.AddError(err) {
		t.Fatal("AddError returned false; want true")
	}
	if c.Len() != 1 {
		t.Fatalf("Len=%d, want 1", c.Len())
	}
	if items := c.Items(); items[0].Code != failure.UnsupportedCustomCommand {
		t.Errorf("items[0].Code=%q, want %q", items[0].Code, failure.UnsupportedCustomCommand)
	}
}

func TestCollector_ItemsReturnsCopy(t *testing.T) {
	c := rejection.New()
	c.Add(failure.UnsupportedExecuteProcess, "one")
	snapshot := c.Items()
	c.Add(failure.UnresolvedLinkDep, "two")
	if len(snapshot) != 1 {
		t.Errorf("Items() snapshot should be insulated from later adds; got len=%d", len(snapshot))
	}
}
