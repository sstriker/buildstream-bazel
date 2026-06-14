package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

func TestRewriteToolFromTarget(t *testing.T) {
	artifacts := map[string]string{
		"bin/llvm-min-tblgen":     "llvm-min-tblgen",
		"bin/clang-tblgen":        "clang-tblgen",
		"lib/libfoo.so":           "foo",
		"obj/CustomGen/CustomGen": "CustomGen",
	}
	execArtifacts := map[string]bool{
		"bin/llvm-min-tblgen":     true,
		"bin/clang-tblgen":        true,
		"obj/CustomGen/CustomGen": true,
		// lib/libfoo.so is intentionally NOT an executable.
	}

	cases := []struct {
		name      string
		in        string
		wantCmd   string
		wantTools []string
	}{
		{
			name:      "passthrough — no artifact ref",
			in:        "echo hello",
			wantCmd:   "echo hello",
			wantTools: nil,
		},
		{
			name:      "leading tblgen ref",
			in:        "bin/llvm-min-tblgen -gen-attrs include/llvm/IR/Attributes.td -o out.inc",
			wantCmd:   "$(location :llvm-min-tblgen) -gen-attrs include/llvm/IR/Attributes.td -o out.inc",
			wantTools: []string{":llvm-min-tblgen"},
		},
		{
			name:      "two distinct tool refs",
			in:        "bin/llvm-min-tblgen -gen-foo && bin/clang-tblgen -gen-bar",
			wantCmd:   "$(location :llvm-min-tblgen) -gen-foo && $(location :clang-tblgen) -gen-bar",
			wantTools: []string{":llvm-min-tblgen", ":clang-tblgen"},
		},
		{
			name:      "repeated tool dedups",
			in:        "bin/llvm-min-tblgen a && bin/llvm-min-tblgen b",
			wantCmd:   "$(location :llvm-min-tblgen) a && $(location :llvm-min-tblgen) b",
			wantTools: []string{":llvm-min-tblgen"},
		},
		{
			// VAR=<executable-artifact> form: a custom command passes a
			// built tool as a cmake -D arg (VTK's
			// -DEXE_SQLITE3=bin/Debug/sqlitebin-9.4). The embedded path is
			// lifted, keeping the `VAR=` prefix.
			name:      "executable inside VAR= arg lifts",
			in:        "echo --tool=bin/llvm-min-tblgen",
			wantCmd:   "echo --tool=$(location :llvm-min-tblgen)",
			wantTools: []string{":llvm-min-tblgen"},
		},
		{
			// A LIBRARY artifact embedded in an arg must NOT be lifted — it's
			// not a runnable tool (a linker flag / data path), so the gate
			// keys on execArtifacts.
			name:      "library inside VAR= arg does not lift",
			in:        "echo -DFOO=lib/libfoo.so",
			wantCmd:   "echo -DFOO=lib/libfoo.so",
			wantTools: nil,
		},
		{
			name:      "no map → passthrough",
			in:        "bin/llvm-min-tblgen -gen",
			wantCmd:   "bin/llvm-min-tblgen -gen",
			wantTools: nil,
		},
		{
			name:      "leading ./ rewrites — cmake-Ninja shape",
			in:        "python3 ./bin/llvm-min-tblgen -gen-attrs",
			wantCmd:   "python3 $(location :llvm-min-tblgen) -gen-attrs",
			wantTools: []string{":llvm-min-tblgen"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := artifacts
			e := execArtifacts
			if tc.name == "no map → passthrough" {
				m = nil
				e = nil
			}
			gotCmd, gotTools := rewriteToolFromTarget(tc.in, m, e, nil, "")
			if gotCmd != tc.wantCmd {
				t.Errorf("cmd:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, gotCmd, tc.wantCmd)
			}
			if !reflect.DeepEqual(gotTools, tc.wantTools) {
				t.Errorf("tools: got %v; want %v", gotTools, tc.wantTools)
			}
		})
	}
}

// TestRewriteToolFromTarget_ImportsManifest covers the MANIFEST-provided
// tool lift: an absolute token matching an export's recorded
// IMPORTED_LOCATION (LookupLinkPath) rewrites to `$(execpath <label>)`
// with the full label in tools — without it, a genrule driving an
// imported tool keeps cmake's configure-time host-absolute path
// verbatim (non-hermetic; invisible under sandboxed /tmp). Both the
// driver-position and `VAR=<path>` forms lift; non-matching absolutes
// and relative tokens stay verbatim.
func TestRewriteToolFromTarget_ImportsManifest(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::gen",
				BazelLabel:  "//elements/foo:gen",
				LinkPaths:   []string{"/opt/foo/bin/gen"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd, tools := rewriteToolFromTarget(
		"/opt/foo/bin/gen --out x.c && other -DGEN=/opt/foo/bin/gen -DOTHER=/usr/bin/m4 sub/gen",
		nil, nil, res, "")
	want := "$(execpath //elements/foo:gen) --out x.c && other -DGEN=$(execpath //elements/foo:gen) -DOTHER=/usr/bin/m4 sub/gen"
	if cmd != want {
		t.Errorf("cmd = %q\nwant %q", cmd, want)
	}
	if len(tools) != 1 || tools[0] != "//elements/foo:gen" {
		t.Errorf("tools = %v, want exactly the manifest label once", tools)
	}

	// In-tree lookup wins over (and coexists with) the manifest path.
	cmd, tools = rewriteToolFromTarget(
		"bin/intree /opt/foo/bin/gen",
		map[string]string{"bin/intree": "intree"}, map[string]bool{"bin/intree": true}, res, "")
	if cmd != "$(location :intree) $(execpath //elements/foo:gen)" {
		t.Errorf("mixed cmd = %q", cmd)
	}
	if len(tools) != 2 {
		t.Errorf("mixed tools = %v", tools)
	}

	// Nil resolver: unchanged behavior (and no rewrite without in-tree map).
	cmd, tools = rewriteToolFromTarget("/opt/foo/bin/gen --out x.c", nil, nil, nil, "")
	if cmd != "/opt/foo/bin/gen --out x.c" || tools != nil {
		t.Errorf("nil-resolver = (%q, %v), want verbatim", cmd, tools)
	}
}

// TestRewriteToolFromTarget_ImportsAnchoredPrefix is the ORCHESTRATED
// flow (the #596 review finding): orchestrator-emitted manifests key
// link_paths in the virtual ManifestPrefixAnchor form, while cmake
// resolved the tool against the REAL synth-prefix dir — so the raw cmd
// token only matches after the hostPrefix→anchor remap (the same
// pre-lookup rewrite the link-fragment channel applies). Without the
// remap the lift is inert in exactly its target scenario.
func TestRewriteToolFromTarget_ImportsAnchoredPrefix(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::gen",
				BazelLabel:  "//elements/foo:gen",
				LinkPaths:   []string{ManifestPrefixAnchor + "bin/gen"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hostPrefix := "/tmp/synth-prefix"
	cmd, tools := rewriteToolFromTarget(
		hostPrefix+"/bin/gen --out x.c -DGEN="+hostPrefix+"/bin/gen",
		nil, nil, res, hostPrefix)
	want := "$(execpath //elements/foo:gen) --out x.c -DGEN=$(execpath //elements/foo:gen)"
	if cmd != want {
		t.Errorf("cmd = %q\nwant %q", cmd, want)
	}
	if len(tools) != 1 || tools[0] != "//elements/foo:gen" {
		t.Errorf("tools = %v", tools)
	}
	// Without the hostPrefix the anchored key can't match: verbatim.
	cmd, _ = rewriteToolFromTarget(hostPrefix+"/bin/gen --out x.c", nil, nil, res, "")
	if cmd != hostPrefix+"/bin/gen --out x.c" {
		t.Errorf("no-prefix cmd = %q, want verbatim", cmd)
	}
}

// TestRewriteToolFromTarget_ToolsMap covers the manifest `tools` section: a
// host codegen tool with no native rule, matched by driver basename or
// absolute path, is rewritten to $(execpath <label>) with the label in tools.
// This is the channel that hermeticizes a basename-driven host tool (flatc,
// python3, perl) — neither genrule path could do that through LinkPaths alone.
func TestRewriteToolFromTarget_ToolsMap(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Tools: []*manifest.Tool{
			{Match: "flatc", Label: "@flatbuffers//:flatc"},
			{Match: "/opt/host/bin/gen.py", Label: "//tools:gen"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Basename driver (PATH-resolved) + an absolute-path script arg; a
	// same-basenamed relative output (build/gen.py) is left untouched.
	cmd, tools := rewriteToolFromTarget(
		"flatc --cpp foo.fbs && python /opt/host/bin/gen.py -o build/gen.py",
		nil, nil, res, "")
	want := "$(execpath @flatbuffers//:flatc) --cpp foo.fbs && python $(execpath //tools:gen) -o build/gen.py"
	if cmd != want {
		t.Errorf("cmd = %q\nwant %q", cmd, want)
	}
	if len(tools) != 2 {
		t.Errorf("tools = %v, want both labels", tools)
	}

	// Absolute path whose basename matches a basename entry also lifts.
	cmd, _ = rewriteToolFromTarget("/usr/bin/flatc x.fbs", nil, nil, res, "")
	if cmd != "$(execpath @flatbuffers//:flatc) x.fbs" {
		t.Errorf("abs-basename cmd = %q", cmd)
	}

	// VAR=<tool> form lifts the value, keeping the VAR= prefix.
	cmd, _ = rewriteToolFromTarget("cmake -DFLATC=flatc .", nil, nil, res, "")
	if cmd != "cmake -DFLATC=$(execpath @flatbuffers//:flatc) ." {
		t.Errorf("VAR= cmd = %q", cmd)
	}

	// A tools-only manifest (no exports → Empty()==true) still drives the
	// swap: the fast-path proceeds on HasTools().
	cmd, _ = rewriteToolFromTarget("flatc x.fbs", nil, nil, res, "")
	if cmd != "$(execpath @flatbuffers//:flatc) x.fbs" {
		t.Errorf("tools-only cmd = %q", cmd)
	}
}
