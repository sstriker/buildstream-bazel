package bazel

import (
	"reflect"
	"testing"
)

// TestOverGrantedIncludeRoots: an include-root whose dir is a strict descendant
// of another include-root is "over-granted" (its bare -I is re-exported via the
// ancestor's header-lib forwarding). The element root "" is an ancestor of
// everything, so when it's an include-root it forwards them all.
func TestOverGrantedIncludeRoots(t *testing.T) {
	cases := []struct {
		name       string
		headerLibs map[string]string
		want       []string
	}{
		{
			// OpenBLAS shape: element-root "" header lib forwards every nested root.
			name: "element-root forwards all",
			headerLibs: map[string]string{
				"":                              "root_headers",
				"lapack-netlib/LAPACKE/include": "lapack_netlib_LAPACKE_include_headers",
				"lapack-netlib/include":         "lapack_netlib_include_headers",
			},
			want: []string{"lapack-netlib/LAPACKE/include", "lapack-netlib/include"},
		},
		{
			// Nested roots without an element-root: deepest is forwarded by its
			// ancestor.
			name: "nested only",
			headerLibs: map[string]string{
				"a":         "a_headers",
				"a/b":       "a_b_headers",
				"a/b/c":     "a_b_c_headers",
				"unrelated": "unrelated_headers",
			},
			want: []string{"a/b", "a/b/c"},
		},
		{
			// Flat, disjoint roots: none nests under another → no over-grant.
			name: "flat disjoint",
			headerLibs: map[string]string{
				"include": "include_headers",
				"src":     "src_headers",
			},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &splitPlan{headerLibs: c.headerLibs}
			got := p.overGrantedIncludeRoots()
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("overGrantedIncludeRoots() = %v; want %v", got, c.want)
			}
		})
	}
}
