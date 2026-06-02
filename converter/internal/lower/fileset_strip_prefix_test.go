package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestLiftCompiledLibFileSetStripIncludePrefix(t *testing.T) {
	const cmakeSrc = "/src"
	run := func(irt ir.Target, baseDirs ...string) ir.Target {
		tgt := &fileapi.Target{
			Paths:    fileapi.TargetPaths{Source: cmakeSrc},
			FileSets: []fileapi.TargetFileSet{{Name: "HEADERS", Type: "HEADERS", BaseDirectories: baseDirs}},
		}
		liftCompiledLibFileSetStripIncludePrefix(&irt, tgt, cmakeSrc)
		return irt
	}

	t.Run("compiled lib lifts FILE_SET base dir, keeps other includes", func(t *testing.T) {
		got := run(ir.Target{
			Kind: ir.KindCCLibrary, Srcs: []string{"src/a.cc"},
			Hdrs: []string{"include/fscl/a.hpp"}, Includes: []string{"include", "src"},
		}, "/src/include")
		if got.StripIncludePrefix != "include" {
			t.Errorf("StripIncludePrefix = %q; want include", got.StripIncludePrefix)
		}
		if !reflect.DeepEqual(got.Includes, []string{"src"}) {
			t.Errorf("Includes = %v; want [src] (FILE_SET dir lifted, other -I kept)", got.Includes)
		}
	})

	t.Run("FILE_SET base dir not in includes: not lifted", func(t *testing.T) {
		got := run(ir.Target{Kind: ir.KindCCLibrary, Srcs: []string{"a.cc"}, Hdrs: []string{"include/a.h"}, Includes: []string{"other"}}, "/src/include")
		if got.StripIncludePrefix != "" {
			t.Errorf("StripIncludePrefix = %q; want empty", got.StripIncludePrefix)
		}
	})

	t.Run("header outside base dir: not lifted", func(t *testing.T) {
		got := run(ir.Target{Kind: ir.KindCCLibrary, Srcs: []string{"a.cc"}, Hdrs: []string{"include/a.h", "x/b.h"}, Includes: []string{"include"}}, "/src/include")
		if got.StripIncludePrefix != "" {
			t.Errorf("StripIncludePrefix = %q; want empty (header outside prefix)", got.StripIncludePrefix)
		}
	})

	t.Run("two FILE_SET base dirs: not lifted", func(t *testing.T) {
		got := run(ir.Target{Kind: ir.KindCCLibrary, Srcs: []string{"a.cc"}, Hdrs: []string{"include/a.h"}, Includes: []string{"include"}}, "/src/include", "/src/api")
		if got.StripIncludePrefix != "" {
			t.Errorf("StripIncludePrefix = %q; want empty (multiple base dirs)", got.StripIncludePrefix)
		}
	})

	t.Run("header-only (no srcs): left for the IR pass", func(t *testing.T) {
		got := run(ir.Target{Kind: ir.KindCCLibrary, Hdrs: []string{"include/a.h"}, Includes: []string{"include"}}, "/src/include")
		if got.StripIncludePrefix != "" {
			t.Errorf("StripIncludePrefix = %q; want empty (no srcs → header-only IR pass owns it)", got.StripIncludePrefix)
		}
	})
}

func TestShapeHeaderOnlyStripIncludePrefix(t *testing.T) {
	const traceTag = "cmake-codegen-interface-library-from-trace"
	cases := []struct {
		name      string
		in        ir.Target
		wantStrip string
		wantInc   []string
	}{
		{
			name:      "codemodel interface lib lifts",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"include/foo/bar.h"}, Includes: []string{"include"}},
			wantStrip: "include", wantInc: nil,
		},
		{
			name:      "trace-synth interface lib (cc_library + tag) lifts",
			in:        ir.Target{Kind: ir.KindCCLibrary, Tags: []string{traceTag}, Hdrs: []string{"include/iflib/iflib.hpp"}, Includes: []string{"include"}},
			wantStrip: "include", wantInc: nil,
		},
		{
			// A compiled lib whose generated sources were elided is a
			// cc_library with no srcs but compile-time includes — must NOT
			// be re-rooted (the elided-foo false-positive the tag gate fixes).
			name:      "elided compiled cc_library (no interface tag) not lifted",
			in:        ir.Target{Kind: ir.KindCCLibrary, Tags: []string{"cmake-elided-build-dir-source"}, Hdrs: []string{"include/foo/foo.hpp"}, Includes: []string{"include"}},
			wantStrip: "", wantInc: []string{"include"},
		},
		{
			// Trace-derived variants ("./include", trailing "/") normalize
			// to "include" and still lift.
			name:      "leading ./ and trailing slash normalize and lift",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"include/foo/bar.h"}, Includes: []string{"./include/"}},
			wantStrip: "include", wantInc: nil,
		},
		{
			// cmake allows target_sources on INTERFACE targets; an
			// interface lib carrying compiled srcs is not header-only, so
			// its includes (compile-time -I) must not be re-rooted.
			name:      "interface lib with srcs not lifted",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"include/a.h"}, Includes: []string{"include"}, Srcs: []string{"a.c"}},
			wantStrip: "", wantInc: []string{"include"},
		},
		{
			name:      "multiple include dirs not lifted",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"include/a.h"}, Includes: []string{"include", "api"}},
			wantStrip: "", wantInc: []string{"include", "api"},
		},
		{
			name:      "header outside the include dir not lifted",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"include/a.h", "other/b.h"}, Includes: []string{"include"}},
			wantStrip: "", wantInc: []string{"include"},
		},
		{
			name:      "package-root include not lifted",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"a.h"}, Includes: []string{"."}},
			wantStrip: "", wantInc: []string{"."},
		},
		{
			name:      "no headers not lifted",
			in:        ir.Target{Kind: ir.KindCCInterface, Includes: []string{"include"}},
			wantStrip: "", wantInc: []string{"include"},
		},
		{
			name:      "existing strip_include_prefix left alone",
			in:        ir.Target{Kind: ir.KindCCInterface, Hdrs: []string{"include/a.h"}, Includes: []string{"include"}, StripIncludePrefix: "preset"},
			wantStrip: "preset", wantInc: []string{"include"},
		},
		{
			name:      "plain cc_binary not lifted",
			in:        ir.Target{Kind: ir.KindCCBinary, Hdrs: []string{"include/a.h"}, Includes: []string{"include"}, Srcs: []string{"main.c"}},
			wantStrip: "", wantInc: []string{"include"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkg := &ir.Package{Targets: []ir.Target{tc.in}}
			shapeHeaderOnlyStripIncludePrefix(pkg)
			got := pkg.Targets[0]
			if got.StripIncludePrefix != tc.wantStrip {
				t.Errorf("StripIncludePrefix = %q; want %q", got.StripIncludePrefix, tc.wantStrip)
			}
			if !reflect.DeepEqual(got.Includes, tc.wantInc) {
				t.Errorf("Includes = %v; want %v", got.Includes, tc.wantInc)
			}
		})
	}
}
