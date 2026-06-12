package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestGenruleOuts_ImplicitOutsIncluded pins that genruleOuts carries
// the edge's implicit (BYPRODUCTS) outputs — BuildFor indexes them,
// so a consumer can reference one — while dropping cmake's
// `${cmake_ninja_workdir}<name>` ninja-var shadows and duplicates.
func TestGenruleOuts_ImplicitOutsIncluded(t *testing.T) {
	b := &ninja.Build{
		Outputs:      []string{"gen/a.h", "/bld/gen/a.h"},
		ImplicitOuts: []string{"gen/b.cxx", "${cmake_ninja_workdir}gen/a.h"},
	}
	got := genruleOuts(b, "/bld")
	want := []string{"gen/a.h", "gen/b.cxx"}
	if len(got) != len(want) {
		t.Fatalf("genruleOuts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("genruleOuts = %v, want %v", got, want)
		}
	}
}
