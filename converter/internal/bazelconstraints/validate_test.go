package bazelconstraints

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string // substring; empty means no error expected
	}{
		{name: "happy path lowercase", in: "hello", wantErr: ""},
		{name: "happy path with underscore", in: "gen_config_h", wantErr: ""},
		{name: "happy path with digits", in: "v2_lib", wantErr: ""},
		{name: "happy path with dot", in: "foo.bar", wantErr: ""},
		{name: "happy path with plus and hyphen", in: "lib+v2-stub", wantErr: ""},
		{name: "empty rejected", in: "", wantErr: "empty"},
		{name: "whitespace rejected", in: "has space", wantErr: "valid Bazel identifier"},
		{name: "tab rejected", in: "has\ttab", wantErr: "valid Bazel identifier"},
		{name: "leading hyphen rejected", in: "-prefix", wantErr: "valid Bazel identifier"},
		{name: "leading dot rejected", in: ".hidden", wantErr: "valid Bazel identifier"},
		{name: "slash rejected", in: "sub/name", wantErr: "valid Bazel identifier"},
		{name: "colon rejected", in: "ns:name", wantErr: "valid Bazel identifier"},
		{name: "newline rejected", in: "line1\nline2", wantErr: "valid Bazel identifier"},
		{name: "unicode rejected", in: "café", wantErr: "valid Bazel identifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateName(tc.in)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("validateName(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateName(%q) = nil, want error containing %q", tc.in, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateGenruleCmd(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "happy path", in: "echo hi > $@", wantErr: false},
		{name: "empty string", in: "", wantErr: true},
		{name: "spaces only", in: "   ", wantErr: true},
		{name: "tabs only", in: "\t\t", wantErr: true},
		{name: "newlines only", in: "\n\n", wantErr: true},
		{name: "mixed whitespace", in: " \t\n ", wantErr: true},
		// Leading/trailing whitespace around a real command is
		// fine — buildtools formats it however it formats it.
		{name: "real command with surrounding whitespace", in: "  echo hi  ", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGenruleCmd(tc.in)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateGenruleCmd(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateGenruleCmd(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestValidateNoDuplicates(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		attr     string
		wantErr  bool
		wantSubs string // substring expected in err message when wantErr
	}{
		{name: "nil slice", in: nil, attr: "deps", wantErr: false},
		{name: "empty slice", in: []string{}, attr: "deps", wantErr: false},
		{name: "single entry", in: []string{":foo"}, attr: "deps", wantErr: false},
		{name: "two distinct entries", in: []string{":foo", ":bar"}, attr: "deps", wantErr: false},
		{
			name:     "issue #194 shape: two-of-the-same in deps",
			in:       []string{":foo", ":bar", ":foo"},
			attr:     "deps",
			wantErr:  true,
			wantSubs: `"deps"`,
		},
		{
			name:     "duplicate in implementation_deps surfaces the right attr name",
			in:       []string{":foo", ":foo"},
			attr:     "implementation_deps",
			wantErr:  true,
			wantSubs: `"implementation_deps"`,
		},
		{
			name:     "three copies still flagged once-per-extra",
			in:       []string{":a", ":a", ":a"},
			attr:     "deps",
			wantErr:  true,
			wantSubs: ":a",
		},
		{
			name:     "two pairs of duplicates surface both",
			in:       []string{":a", ":b", ":a", ":b"},
			attr:     "deps",
			wantErr:  true,
			wantSubs: ":b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNoDuplicates(tc.in, tc.attr)
			if tc.wantErr && err == nil {
				t.Fatalf("validateNoDuplicates(%v, %q) = nil, want error", tc.in, tc.attr)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateNoDuplicates(%v, %q) = %v, want nil", tc.in, tc.attr, err)
			}
			if tc.wantErr && tc.wantSubs != "" && !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestValidatePackage_HappyPath drives the top-level entry on a
// realistic two-target package: a cc_library with deps and a
// genrule. Both pass every constraint; ValidatePackage returns
// nil.
func TestValidatePackage_HappyPath(t *testing.T) {
	pkg := &ir.Package{
		Name: "hello",
		Targets: []ir.Target{
			{
				Name: "hello",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"hello.c"},
				Hdrs: []string{"hello.h"},
				Deps: []string{":helpers"},
			},
			{
				Name:        "gen_config_h",
				Kind:        ir.KindGenrule,
				GenruleCmd:  "echo '#define VER \"1.0\"' > $@",
				GenruleOuts: []string{"config.h"},
			},
		},
	}
	if err := ValidatePackage(pkg); err != nil {
		t.Errorf("ValidatePackage(happy) = %v, want nil", err)
	}
}

// TestValidatePackage_NilSafe locks the nil-package contract.
// Callers (e.g. test fixtures) sometimes pass nil; we don't
// want a panic there.
func TestValidatePackage_NilSafe(t *testing.T) {
	if err := ValidatePackage(nil); err != nil {
		t.Errorf("ValidatePackage(nil) = %v, want nil", err)
	}
}

// TestValidatePackage_AggregatesViolations exercises the
// errors.Join wrap: a single package with multiple violations
// across multiple targets must surface every violation, not
// short-circuit at the first.
func TestValidatePackage_AggregatesViolations(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name: "bad name", // whitespace — invalid name
				Kind: ir.KindCCLibrary,
			},
			{
				Name:        "gen_empty",
				Kind:        ir.KindGenrule,
				GenruleCmd:  "", // empty cmd — issue #193 shape
				GenruleOuts: []string{"out.h"},
			},
			{
				Name: "dup_deps",
				Kind: ir.KindCCLibrary,
				Deps: []string{":foo", ":foo"}, // duplicate — issue #194 shape
			},
		},
	}
	err := ValidatePackage(pkg)
	if err == nil {
		t.Fatal("ValidatePackage on violation-laden pkg returned nil; want error")
	}
	// Every violation must appear in the joined message — pin
	// that errors.Join didn't drop or collapse them.
	msg := err.Error()
	for _, want := range []string{
		`"bad name"`, // bad-name target's name in the wrap prefix
		"valid Bazel identifier",
		`"gen_empty"`,
		"empty or whitespace-only",
		`"dup_deps"`,
		`"deps"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error %q missing substring %q", msg, want)
		}
	}
}

// TestValidatePackage_DuplicateTargetNames catches the case
// where two targets in the same package share a name. Bazel
// rejects this at load time with "duplicate rule" — pre-emit
// detection means the broken BUILD never lands.
func TestValidatePackage_DuplicateTargetNames(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{Name: "foo", Kind: ir.KindCCLibrary},
			{Name: "bar", Kind: ir.KindCCLibrary},
			{Name: "foo", Kind: ir.KindGenrule, GenruleCmd: "true", GenruleOuts: []string{"x"}},
		},
	}
	err := ValidatePackage(pkg)
	if err == nil {
		t.Fatal("ValidatePackage with duplicate names returned nil; want error")
	}
	if !strings.Contains(err.Error(), "duplicate target name") {
		t.Errorf("err = %q, want substring 'duplicate target name'", err.Error())
	}
	if !strings.Contains(err.Error(), `"foo"`) {
		t.Errorf("err = %q, want the duplicated name in the message", err.Error())
	}
}

// TestValidateTarget_GenruleCmdOnlyCheckedForGenrules pins that
// the empty-cmd check fires only on KindGenrule. A KindCCLibrary
// with no GenruleCmd (the normal case) must NOT trip the
// validator.
func TestValidateTarget_GenruleCmdOnlyCheckedForGenrules(t *testing.T) {
	lib := ir.Target{
		Name: "hello",
		Kind: ir.KindCCLibrary,
		// GenruleCmd intentionally empty — not relevant for cc_library
	}
	if err := validateTarget(lib); err != nil {
		t.Errorf("validateTarget(cc_library w/ empty GenruleCmd) = %v, want nil", err)
	}

	bad := ir.Target{
		Name:        "gen_empty",
		Kind:        ir.KindGenrule,
		GenruleCmd:  "",
		GenruleOuts: []string{"out.h"},
	}
	if err := validateTarget(bad); err == nil {
		t.Error("validateTarget(genrule w/ empty cmd) = nil, want error")
	}
}
