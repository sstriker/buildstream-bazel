package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// classifyAndAttach is the single source-classification chokepoint the
// file(GENERATE) + execute_process consumer-attribution blocks share. These
// tests pin its routing so the consolidation stays behavior-preserving.
func TestClassifyAndAttach_Routing(t *testing.T) {
	irt := &ir.Target{}
	seen := map[string]bool{}

	// header → hdrs
	if !classifyAndAttach(irt, "gen/foo.h", seen, false) {
		t.Fatal("header should attach")
	}
	// cc source → srcs
	if !classifyAndAttach(irt, "gen/foo.cc", seen, false) {
		t.Fatal("cc source should attach")
	}
	// non-cc artifact, dropNonCc=false → data
	if !classifyAndAttach(irt, "mod-hierarchy.Debug.args", seen, false) {
		t.Fatal("non-cc (data) should attach")
	}
	// linkable object → srcs
	if !classifyAndAttach(irt, "gen/foo.o", seen, false) {
		t.Fatal("object should attach")
	}

	if want := []string{"gen/foo.h"}; !reflect.DeepEqual(irt.Hdrs, want) {
		t.Errorf("Hdrs = %v; want %v", irt.Hdrs, want)
	}
	if want := []string{"gen/foo.cc", "gen/foo.o"}; !reflect.DeepEqual(irt.Srcs, want) {
		t.Errorf("Srcs = %v; want %v", irt.Srcs, want)
	}
	if want := []string{"mod-hierarchy.Debug.args"}; !reflect.DeepEqual(irt.Data, want) {
		t.Errorf("Data = %v; want %v", irt.Data, want)
	}
}

// dropNonCc=true (the CompileGroup site's cross-package-safe disposition) drops
// a non-cc output instead of routing it to data; cc inputs and headers still
// attach.
func TestClassifyAndAttach_DropNonCc(t *testing.T) {
	irt := &ir.Target{}
	seen := map[string]bool{}

	if classifyAndAttach(irt, "weird.data", seen, true) {
		t.Error("non-cc with dropNonCc=true should NOT attach")
	}
	if !classifyAndAttach(irt, "k.h", seen, true) {
		t.Error("header should attach even with dropNonCc=true")
	}
	if len(irt.Data) != 0 {
		t.Errorf("Data = %v; want empty (dropped)", irt.Data)
	}
	if want := []string{"k.h"}; !reflect.DeepEqual(irt.Hdrs, want) {
		t.Errorf("Hdrs = %v; want %v", irt.Hdrs, want)
	}
}

// seen dedups within a pass: a repeated path attaches once and reports false on
// the repeat (so the caller's has-cmake-codegen tag isn't double-counted).
func TestClassifyAndAttach_Dedup(t *testing.T) {
	irt := &ir.Target{}
	seen := map[string]bool{}

	if !classifyAndAttach(irt, "gen/a.cc", seen, false) {
		t.Fatal("first attach should report true")
	}
	if classifyAndAttach(irt, "gen/a.cc", seen, false) {
		t.Error("repeat should report false")
	}
	if want := []string{"gen/a.cc"}; !reflect.DeepEqual(irt.Srcs, want) {
		t.Errorf("Srcs = %v; want %v (deduped)", irt.Srcs, want)
	}
}

// attachGeneratedSource is the sister chokepoint for the per-source IsGenerated /
// on-disk-generated / OutToGenrule branches: compile-group source→srcs (+ a
// cc_embed sibling header), header→hdrs, else→srcs, with an optional non-cc drop.
func TestAttachGeneratedSource(t *testing.T) {
	// compile-group source → srcs, and its cc_embed sibling header → hdrs.
	irt := &ir.Target{}
	attachGeneratedSource(irt, "gen/x.cxx", true, false, "gen/x.h")
	if want := []string{"gen/x.cxx"}; !reflect.DeepEqual(irt.Srcs, want) {
		t.Errorf("CG src: Srcs = %v; want %v", irt.Srcs, want)
	}
	if want := []string{"gen/x.h"}; !reflect.DeepEqual(irt.Hdrs, want) {
		t.Errorf("CG src: Hdrs = %v; want %v (cc_embed pairing)", irt.Hdrs, want)
	}

	// header → hdrs (no embed).
	irt = &ir.Target{}
	attachGeneratedSource(irt, "gen/foo.h", false, false, "")
	if want := []string{"gen/foo.h"}; !reflect.DeepEqual(irt.Hdrs, want) {
		t.Errorf("header: Hdrs = %v; want %v", irt.Hdrs, want)
	}

	// non-cc, dropNonCc=true → dropped (the IsGenerated branch's behavior).
	irt = &ir.Target{}
	attachGeneratedSource(irt, "mod-hierarchy.Debug.args", false, true, "")
	if len(irt.Srcs)+len(irt.Hdrs)+len(irt.Data) != 0 {
		t.Errorf("non-cc dropNonCc: attached %v/%v/%v; want all empty", irt.Srcs, irt.Hdrs, irt.Data)
	}

	// non-cc, dropNonCc=false → srcs (the on-disk / OutToGenrule branches keep it).
	irt = &ir.Target{}
	attachGeneratedSource(irt, "weird.xyz", false, false, "")
	if want := []string{"weird.xyz"}; !reflect.DeepEqual(irt.Srcs, want) {
		t.Errorf("non-cc keep: Srcs = %v; want %v", irt.Srcs, want)
	}

	// compilable source not in a compile group → srcs.
	irt = &ir.Target{}
	attachGeneratedSource(irt, "gen/y.cc", false, true, "")
	if want := []string{"gen/y.cc"}; !reflect.DeepEqual(irt.Srcs, want) {
		t.Errorf("non-CG cc: Srcs = %v; want %v", irt.Srcs, want)
	}
}
