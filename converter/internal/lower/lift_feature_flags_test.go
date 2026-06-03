package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestLiftRawFeatureFlags_BasicRewrite locks the core contract:
// toolchain-backed raw flags (-fPIC, -fsanitize=address) move from
// Copts/LinkOpts to Features; unrelated flags keep their slot + order;
// and flags whose feature no toolchain backs (the visibility presets)
// stay raw rather than being lifted onto a no-op feature that would
// silently drop them (see toolchainfeature.RewriteFeature).
func TestLiftRawFeatureFlags_BasicRewrite(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:     "lib",
		Kind:     ir.KindCCLibrary,
		Copts:    []string{"-O3", "-fPIC", "-fvisibility=hidden", "-Wall", "-fvisibility-inlines-hidden"},
		LinkOpts: []string{"-fsanitize=address", "-Wl,-rpath,/foo"},
	}}}
	liftRawFeatureFlags(pkg)
	tgt := pkg.Targets[0]
	// -fPIC lifted to a feature; the visibility presets stay raw (no
	// toolchain defines a visibility feature, so lifting would drop them).
	if got, want := tgt.Copts, []string{"-O3", "-fvisibility=hidden", "-Wall", "-fvisibility-inlines-hidden"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Copts = %v, want %v", got, want)
	}
	if got, want := tgt.LinkOpts, []string{"-Wl,-rpath,/foo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LinkOpts = %v, want %v", got, want)
	}
	if got, want := tgt.Features, []string{"asan", "pic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Features = %v, want %v", got, want)
	}
}

// TestLiftRawFeatureFlags_PreservesExistingFeatures merges with
// any Features the upstream lifters already populated (e.g.
// applyProbeGenexProperties adds "pic" for
// POSITION_INDEPENDENT_CODE=TRUE). Dedup keeps the list stable.
func TestLiftRawFeatureFlags_PreservesExistingFeatures(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:     "lib",
		Kind:     ir.KindCCLibrary,
		Copts:    []string{"-fPIC", "-O2"},
		Features: []string{"pic", "lto"}, // pre-existing
	}}}
	liftRawFeatureFlags(pkg)
	tgt := pkg.Targets[0]
	if got, want := tgt.Copts, []string{"-O2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Copts = %v, want %v", got, want)
	}
	if got, want := tgt.Features, []string{"lto", "pic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Features = %v, want %v", got, want)
	}
}

// TestLiftRawFeatureFlags_NoLiftableLeavesAlone keeps the slices
// untouched when no flag is liftable — preserves test-fixture
// byte-stability on the targets that don't carry feature-flags.
func TestLiftRawFeatureFlags_NoLiftableLeavesAlone(t *testing.T) {
	original := []string{"-O3", "-Wall", "-DFOO=1"}
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:  "lib",
		Copts: original,
	}}}
	liftRawFeatureFlags(pkg)
	if got := pkg.Targets[0].Copts; !reflect.DeepEqual(got, original) {
		t.Errorf("Copts mutated; got %v, want %v", got, original)
	}
	if got := pkg.Targets[0].Features; len(got) != 0 {
		t.Errorf("Features = %v, want empty", got)
	}
}

// TestLiftRawFeatureFlags_NegationFromProbeGenexPreserved
// covers the "-pic" (force-off) shape applyProbeGenexProperties
// emits when POSITION_INDEPENDENT_CODE=FALSE. The lift must not
// reinstate "pic" via -fPIC if both appear (the convert-time
// lifter shouldn't have produced this combination but the
// post-pass is defensive).
func TestLiftRawFeatureFlags_NegationFromProbeGenexPreserved(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:     "lib",
		Features: []string{"-pic"},
	}}}
	liftRawFeatureFlags(pkg)
	if got := pkg.Targets[0].Features; !reflect.DeepEqual(got, []string{"-pic"}) {
		t.Errorf("Features = %v, want [-pic]", got)
	}
}

// TestLiftRawFeatureFlags_EmptyPackageNoOp pins the nil-safe path.
func TestLiftRawFeatureFlags_EmptyPackageNoOp(t *testing.T) {
	liftRawFeatureFlags(nil) // must not panic
	liftRawFeatureFlags(&ir.Package{})
}
