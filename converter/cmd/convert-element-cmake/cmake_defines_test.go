package main

import (
	"reflect"
	"testing"
)

// TestCmakeDefinesToMap covers the --cmake-define KEY=VALUE -> ExtraCacheVars
// parsing: only the first '=' splits (so a value may itself contain '='), a
// bare KEY maps to an empty value (cmake reads -DKEY as KEY=""), and an empty
// slice yields a nil map (no -D args).
func TestCmakeDefinesToMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want map[string]string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"single", []string{"CMAKE_CXX_FLAGS=-w"}, map[string]string{"CMAKE_CXX_FLAGS": "-w"}},
		{"value-with-equals", []string{"K=a=b"}, map[string]string{"K": "a=b"}},
		{"bare-key", []string{"BARE"}, map[string]string{"BARE": ""}},
		{
			"multiple",
			[]string{"A=1", "B=ON"},
			map[string]string{"A": "1", "B": "ON"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cmakeDefinesToMap(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cmakeDefinesToMap(%v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}
