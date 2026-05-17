package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestStripIDHash covers the codemodel id parser used as the
// lookup key into trace-derived link-scope maps and the imports
// manifest. The id shape is `<name>::@<hash>`; the hash is a
// per-codemodel-run signature and must be stripped before any
// cross-source lookup. Pins behavior for namespaced names that
// already carry `::` (Foo::bar::@hash collapses to Foo::bar) —
// the splitter must use the `::@` triple, not the `::` pair.
func TestStripIDHash(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "empty", id: "", want: ""},
		{name: "no hash suffix returns unchanged", id: "mylib", want: "mylib"},
		{
			name: "bare name with hash",
			id:   "mylib::@1a2b3c4d",
			want: "mylib",
		},
		{
			name: "namespaced name with hash",
			id:   "Foo::bar::@deadbeef",
			want: "Foo::bar",
		},
		{
			name: "double-colon without @ is preserved (not a hash separator)",
			id:   "Foo::bar",
			want: "Foo::bar",
		},
		{
			name: "empty name with hash suffix returns empty",
			id:   "::@x",
			want: "",
		},
		{
			name: "trailing ::@ with no hash still strips",
			id:   "mylib::@",
			want: "mylib",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripIDHash(tc.id)
			if got != tc.want {
				t.Errorf("stripIDHash(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// TestDepScopeIsPrivate covers the per-dep PRIVATE-scope check
// that drives target_link_libraries narrowing. The function
// must (a) bail out when no trace data is available, (b) try
// the namespaced cmake name (stripIDHash output) first — that's
// what imports-manifest deps carry — and (c) fall back to the
// per-codemodel target-name registry for in-codebase deps,
// where the trace records the bare target name rather than the
// codemodel id form.
//
// Adjacent to issue #194; locks the lookup-priority shape so a
// future refactor can't silently flip from "PRIVATE" to default.
func TestDepScopeIsPrivate(t *testing.T) {
	const depID = "uses_hello::@deadbeef"

	cases := []struct {
		name     string
		trace    map[string]string
		dep      fileapi.TargetDependency
		idToName map[string]string
		want     bool
	}{
		{
			name:     "empty trace bails out false",
			trace:    nil,
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: map[string]string{depID: "uses_hello"},
			want:     false,
		},
		{
			name:     "namespaced cmake name matches PRIVATE",
			trace:    map[string]string{"uses_hello": "PRIVATE"},
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: nil,
			want:     true,
		},
		{
			name:     "namespaced cmake name matches PUBLIC -> false",
			trace:    map[string]string{"uses_hello": "PUBLIC"},
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: nil,
			want:     false,
		},
		{
			name: "cmakeName miss falls through to idToName lookup",
			// trace recorded the bare target name (in-codebase shape);
			// the dep id namespaced version doesn't match directly.
			trace:    map[string]string{"uses_hello_alt": "PRIVATE"},
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: map[string]string{depID: "uses_hello_alt"},
			want:     true,
		},
		{
			name: "namespace import shape matches imports-manifest form",
			// `Foo::bar::@hash` -> `Foo::bar`; matches trace directly.
			trace:    map[string]string{"Foo::bar": "PRIVATE"},
			dep:      fileapi.TargetDependency{Id: "Foo::bar::@hash"},
			idToName: nil,
			want:     true,
		},
		{
			name:     "no match in either map returns false",
			trace:    map[string]string{"other": "PRIVATE"},
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: map[string]string{depID: "uses_hello"},
			want:     false,
		},
		{
			name:     "idToName entry without trace entry returns false",
			trace:    map[string]string{"unrelated": "PRIVATE"},
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: map[string]string{depID: "uses_hello"},
			want:     false,
		},
		{
			name:     "INTERFACE scope is not PRIVATE",
			trace:    map[string]string{"uses_hello": "INTERFACE"},
			dep:      fileapi.TargetDependency{Id: depID},
			idToName: nil,
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := depScopeIsPrivate(tc.trace, tc.dep, tc.idToName)
			if got != tc.want {
				t.Errorf("depScopeIsPrivate(...) = %v, want %v (case %q)", got, tc.want, tc.name)
			}
		})
	}
}
