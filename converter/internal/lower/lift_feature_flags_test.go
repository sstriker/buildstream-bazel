package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestLiftRawFeatureFlags_BasicRewrite locks the core contract under the
// DEFAULT vocabulary (backed=nil): toolchain-backed raw flags (-fPIC,
// -fsanitize=address) move from Copts/LinkOpts to Features; unrelated flags
// keep their slot + order; and flags whose feature the default vocabulary
// doesn't back (the visibility presets) stay raw rather than being lifted onto
// a no-op feature that would silently drop them (see RewriteFeature). With a
// toolchain that declares them, --toolchain-features-from re-enables lifting —
// see TestLiftRawFeatureFlags_OperatorVocabulary.
func TestLiftRawFeatureFlags_BasicRewrite(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:     "lib",
		Kind:     ir.KindCCLibrary,
		Copts:    []string{"-O3", "-fPIC", "-fvisibility=hidden", "-Wall", "-fvisibility-inlines-hidden"},
		LinkOpts: []string{"-fsanitize=address", "-Wl,-rpath,/foo"},
	}}}
	liftRawFeatureFlags(pkg, nil)
	tgt := pkg.Targets[0]
	// -fPIC lifted to a feature; the visibility presets stay raw (the default
	// vocabulary doesn't define a visibility feature, so lifting would drop them).
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
	liftRawFeatureFlags(pkg, nil)
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
	liftRawFeatureFlags(pkg, nil)
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
	liftRawFeatureFlags(pkg, nil)
	if got := pkg.Targets[0].Features; !reflect.DeepEqual(got, []string{"-pic"}) {
		t.Errorf("Features = %v, want [-pic]", got)
	}
}

// TestLiftRawFeatureFlags_EmptyPackageNoOp pins the nil-safe path.
func TestLiftRawFeatureFlags_EmptyPackageNoOp(t *testing.T) {
	liftRawFeatureFlags(nil, nil) // must not panic
	liftRawFeatureFlags(&ir.Package{}, nil)
}

// TestLiftRawFeatureFlags_OperatorVocabulary: when the operator passes their
// real toolchain's feature vocabulary, the lift gates on THAT instead of the
// generated default — inverting both directions. A feature the generated
// default doesn't back (visibility_hidden) IS lifted when the operator's
// toolchain declares it; and a flag whose feature the operator's toolchain
// lacks (asan, omitted here) stays a raw copt.
func TestLiftRawFeatureFlags_OperatorVocabulary(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:  "lib",
		Kind:  ir.KindCCLibrary,
		Copts: []string{"-fvisibility=hidden", "-fsanitize=address", "-O2"},
	}}}
	// Operator toolchain declares visibility_hidden but NOT asan.
	liftRawFeatureFlags(pkg, []string{"visibility_hidden"})
	tgt := pkg.Targets[0]
	if got, want := tgt.Copts, []string{"-fsanitize=address", "-O2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Copts = %v, want %v", got, want)
	}
	if got, want := tgt.Features, []string{"visibility_hidden"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Features = %v, want %v", got, want)
	}
}

// TestLiftRawFeatureFlags_EmptyVocabularyIsConservativeNotDefault locks the
// nil-vs-empty contract: a non-nil but EMPTY backed list (operator pointed at
// a toolchain whose features the parser couldn't read) lifts only the built-in
// pic — it must NOT fall back to the generated default and lift asan onto a
// toolchain that may not define it. nil (no operator toolchain) keeps default.
func TestLiftRawFeatureFlags_EmptyVocabularyIsConservativeNotDefault(t *testing.T) {
	mk := func() *ir.Package {
		return &ir.Package{Targets: []ir.Target{{
			Name:  "lib",
			Kind:  ir.KindCCLibrary,
			Copts: []string{"-fPIC", "-fsanitize=address", "-O2"},
		}}}
	}
	empty := mk()
	liftRawFeatureFlags(empty, []string{}) // non-nil, empty → only pic
	if got, want := empty.Targets[0].Features, []string{"pic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("empty-vocab Features = %v, want %v (asan must stay raw)", got, want)
	}
	if got, want := empty.Targets[0].Copts, []string{"-fsanitize=address", "-O2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("empty-vocab Copts = %v, want %v", got, want)
	}
	def := mk()
	liftRawFeatureFlags(def, nil) // nil → generated default: asan lifts too
	if got, want := def.Targets[0].Features, []string{"asan", "pic"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nil(default) Features = %v, want %v", got, want)
	}
}
