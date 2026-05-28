package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestLowerAliasTargets_EmitsAliasForInTreeUnderlying(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{
				File: "/src/CMakeLists.txt", Line: 12,
				Name: "foo_alias", Type: "ALIAS",
				Aliases: []string{"foo"},
			},
			{
				File: "/src/CMakeLists.txt", Line: 14,
				Name: "bar_alias", Type: "ALIAS",
				Aliases: []string{"bar"},
			},
		},
	}
	known := map[string]bool{"foo": true, "bar": true}
	got := lowerAliasTargets(decoded, known, "/src")
	if len(got) != 2 {
		t.Fatalf("want 2 aliases; got %d (%v)", len(got), got)
	}
	// Sorted alphabetically by alias name.
	if got[0].Name != "bar_alias" || got[1].Name != "foo_alias" {
		t.Errorf("alias order: got %s, %s; want bar_alias, foo_alias",
			got[0].Name, got[1].Name)
	}
	if got[0].Kind != ir.KindAlias {
		t.Errorf("Kind: %v; want KindAlias", got[0].Kind)
	}
	if got[0].AliasActual != ":bar" {
		t.Errorf("AliasActual: %q; want :bar", got[0].AliasActual)
	}
	if got[0].Provenance.File != "CMakeLists.txt" || got[0].Provenance.Command != "add_library" {
		t.Errorf("Provenance: %+v; want CMakeLists.txt (reanchored from /src)+line+add_library", got[0].Provenance)
	}
}

func TestLowerAliasTargets_SkipsNamespacedAliases(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{
				Name: "Foo::Bar", Type: "ALIAS",
				Aliases: []string{"foo_bar"},
			},
		},
	}
	known := map[string]bool{"foo_bar": true}
	got := lowerAliasTargets(decoded, known, "/src")
	if len(got) != 0 {
		t.Errorf("namespaced alias should be skipped; got %v", got)
	}
}

func TestLowerAliasTargets_SkipsAliasesPointingAtUnknownTarget(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{
				Name: "dangling_alias", Type: "ALIAS",
				Aliases: []string{"never_defined"},
			},
		},
	}
	known := map[string]bool{}
	got := lowerAliasTargets(decoded, known, "/src")
	if len(got) != 0 {
		t.Errorf("alias to unknown target should be skipped; got %v", got)
	}
}

func TestLowerAliasTargets_SkipsAliasShadowingCodemodelTarget(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{
				Name: "foo", Type: "ALIAS",
				Aliases: []string{"foo_impl"},
			},
		},
	}
	// "foo" is both an alias name AND a codemodel-emitted target.
	known := map[string]bool{"foo": true, "foo_impl": true}
	got := lowerAliasTargets(decoded, known, "/src")
	if len(got) != 0 {
		t.Errorf("alias shadowing codemodel target should be skipped; got %v", got)
	}
}

func TestLowerAliasTargets_DedupsRepeatedDeclarations(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "foo_alias", Type: "ALIAS", Aliases: []string{"foo"}},
			{Name: "foo_alias", Type: "ALIAS", Aliases: []string{"foo"}}, // dup
		},
	}
	known := map[string]bool{"foo": true}
	got := lowerAliasTargets(decoded, known, "/src")
	if len(got) != 1 {
		t.Errorf("dedup expected; got %d: %v", len(got), got)
	}
}

func TestLowerAliasTargets_SkipsNonAliasCalls(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "static_lib", Type: "STATIC"},
			{Name: "iface_lib", Type: "INTERFACE"},
			{Name: "shared_lib", Type: "SHARED"},
		},
	}
	known := map[string]bool{}
	got := lowerAliasTargets(decoded, known, "/src")
	if len(got) != 0 {
		t.Errorf("non-ALIAS calls should be ignored; got %v", got)
	}
}

func TestLowerAliasTargets_NilDecoded(t *testing.T) {
	got := lowerAliasTargets(nil, map[string]bool{}, "")
	if got != nil {
		t.Errorf("nil decoded should return nil; got %v", got)
	}
}
