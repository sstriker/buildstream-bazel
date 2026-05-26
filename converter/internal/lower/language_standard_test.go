package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestPrependLanguageStandardCopt_CXX(t *testing.T) {
	std := &fileapi.LanguageStandard{Standard: "17"}
	got := prependLanguageStandardCopt("CXX", std, []string{"-O2", "-Wall"})
	want := []string{"-std=c++17", "-O2", "-Wall"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestPrependLanguageStandardCopt_C(t *testing.T) {
	std := &fileapi.LanguageStandard{Standard: "11"}
	got := prependLanguageStandardCopt("C", std, []string{"-O2"})
	want := []string{"-std=c11", "-O2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestPrependLanguageStandardCopt_NilStandard(t *testing.T) {
	got := prependLanguageStandardCopt("CXX", nil, []string{"-O2"})
	if !reflect.DeepEqual(got, []string{"-O2"}) {
		t.Errorf("nil standard should be no-op; got %v", got)
	}
}

func TestPrependLanguageStandardCopt_AlreadyHasStdFlag(t *testing.T) {
	// cmake's generator commonly inlines -std=gnu++17 directly
	// into CompileCommandFragments. The prepend's idempotency
	// guard prevents the duplicate.
	std := &fileapi.LanguageStandard{Standard: "17"}
	got := prependLanguageStandardCopt("CXX", std, []string{"-std=gnu++17", "-O2"})
	want := []string{"-std=gnu++17", "-O2"} // unchanged
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected idempotent skip; got %v want %v", got, want)
	}
}

func TestPrependLanguageStandardCopt_UnrecognizedLanguage(t *testing.T) {
	// Fortran / ASM / etc.: no -std flag mapping.
	std := &fileapi.LanguageStandard{Standard: "2008"}
	got := prependLanguageStandardCopt("Fortran", std, []string{"-O2"})
	if !reflect.DeepEqual(got, []string{"-O2"}) {
		t.Errorf("non-cc language should be no-op; got %v", got)
	}
}

func TestPrependLanguageStandardCopt_OBJCXX(t *testing.T) {
	std := &fileapi.LanguageStandard{Standard: "20"}
	got := prependLanguageStandardCopt("OBJCXX", std, nil)
	want := []string{"-std=c++20"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OBJCXX standard: got %v want %v", got, want)
	}
}

func TestStdFlagFor(t *testing.T) {
	cases := []struct {
		lang, version, want string
	}{
		{"CXX", "17", "-std=c++17"},
		{"cxx", "20", "-std=c++20"}, // case-insensitive
		{"C", "11", "-std=c11"},
		{"OBJC", "11", "-std=c11"},
		{"OBJCXX", "17", "-std=c++17"},
		{"Fortran", "2008", ""},
		{"ASM", "", ""},
		{"", "17", ""},
	}
	for _, c := range cases {
		if got := stdFlagFor(c.lang, c.version); got != c.want {
			t.Errorf("stdFlagFor(%q, %q) = %q want %q", c.lang, c.version, got, c.want)
		}
	}
}
