package shadow

import (
	"strings"
	"testing"
)

// Trace-event fixtures are constructed inline so the test
// asserts the parser's behaviour independent of cmake's
// version-specific wire format. Each line is a real
// JSON-v1 trace event — one fewer integration with cmake's
// recording at test time.

const traceMixed = `{"args":["t","PUBLIC","inc","PRIVATE","inc/priv"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":4}
{"args":["cmTC_xxx","libfake"],"cmd":"target_link_libraries","file":"/build/CMakeFiles/CMakeScratch/TryCompile-1/CMakeLists.txt","line":1}
{"args":["t","PUBLIC","ZLIB::ZLIB","PRIVATE","privatedep"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":6}
{"args":["t2","libfoo","libbar"],"cmd":"target_link_libraries","file":"/src/sub/CMakeLists.txt","line":3}
{"args":["in.h.in","out.h","@ONLY"],"cmd":"configure_file","file":"/src/CMakeLists.txt","line":7}
{"args":["/usr/share/cmake-3.28/Modules/CMakeSystem.cmake.in","/build/CMakeFiles/3.28.3/CMakeSystem.cmake","@ONLY"],"cmd":"configure_file","file":"/usr/share/cmake-3.28/Modules/CMakeDetermineSystem.cmake","line":246}
`

func TestExtractTargetIncludes(t *testing.T) {
	got := ExtractTargetIncludes([]byte(traceMixed), "/src", nil)
	if len(got) != 1 {
		t.Fatalf("want 1 user call, got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Target != "t" {
		t.Errorf("target: %q want t", c.Target)
	}
	if len(c.Groups) != 2 {
		t.Fatalf("groups: %+v want 2", c.Groups)
	}
	if c.Groups[0].Visibility != "PUBLIC" || c.Groups[0].Dirs[0] != "inc" {
		t.Errorf("group 0: %+v", c.Groups[0])
	}
	if c.Groups[1].Visibility != "PRIVATE" || c.Groups[1].Dirs[0] != "inc/priv" {
		t.Errorf("group 1: %+v", c.Groups[1])
	}
}

func TestExtractTargetLinks_PublicPrivate(t *testing.T) {
	got := ExtractTargetLinks([]byte(traceMixed), "/src", nil)
	// 2 user calls: t (with PUBLIC/PRIVATE), t2 (legacy
	// positional). The cmTC_xxx scratch-target call is filtered
	// out (file is in build dir, not source).
	if len(got) != 2 {
		t.Fatalf("want 2 calls; got %d (%+v)", len(got), got)
	}
	tCall := got[0]
	if tCall.Target != "t" {
		t.Errorf("target 0: %q want t", tCall.Target)
	}
	if len(tCall.Groups) != 2 {
		t.Fatalf("t groups: %+v", tCall.Groups)
	}
	if tCall.Groups[0].Visibility != "PUBLIC" || tCall.Groups[0].Libs[0] != "ZLIB::ZLIB" {
		t.Errorf("t public: %+v", tCall.Groups[0])
	}
	if tCall.Groups[1].Visibility != "PRIVATE" || tCall.Groups[1].Libs[0] != "privatedep" {
		t.Errorf("t private: %+v", tCall.Groups[1])
	}

	t2Call := got[1]
	if t2Call.Target != "t2" {
		t.Errorf("target 1: %q want t2", t2Call.Target)
	}
	if len(t2Call.Groups) != 1 || t2Call.Groups[0].Visibility != "" {
		t.Errorf("t2 should be one unkeyed group; got %+v", t2Call.Groups)
	}
	if len(t2Call.Groups[0].Libs) != 2 ||
		t2Call.Groups[0].Libs[0] != "libfoo" || t2Call.Groups[0].Libs[1] != "libbar" {
		t.Errorf("t2 libs: %+v", t2Call.Groups[0].Libs)
	}
}

// TestExtractTargetLinks_ListExpansion pins the --trace-expand
// list-variable case: a `target_link_libraries(t PUBLIC ${VAR})` where
// VAR holds a cmake list records the whole list as ONE semicolon-joined
// argument (this is exactly how protobuf's
// `target_link_libraries(libprotobuf-lite PUBLIC ${protobuf_ABSL_USED_TARGETS})`
// arrives). The classifier must split it back into individual libs so
// downstream find_package attribution can match each absl::X — otherwise
// the whole blob is one un-matchable "lib" and a static archive's absl
// deps go unwired. An empty expansion (e.g. `${CMAKE_THREAD_LIBS_INIT}`
// → "") contributes nothing.
func TestExtractTargetLinks_ListExpansion(t *testing.T) {
	trace := `{"args":["lite","PRIVATE",""],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":1}
{"args":["lite","PUBLIC","absl::strings;absl::base;absl::log"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":2}
`
	got := ExtractTargetLinks([]byte(trace), "/src", nil)
	if len(got) != 2 {
		t.Fatalf("want 2 calls; got %d (%+v)", len(got), got)
	}
	// The empty-expansion PRIVATE arm yields a group with no libs.
	if len(got[0].Groups) != 1 || len(got[0].Groups[0].Libs) != 0 {
		t.Errorf("empty expansion should yield no libs; got %+v", got[0].Groups)
	}
	pub := got[1].Groups[0]
	if pub.Visibility != "PUBLIC" {
		t.Fatalf("want PUBLIC arm; got %+v", pub)
	}
	want := []string{"absl::strings", "absl::base", "absl::log"}
	if len(pub.Libs) != len(want) {
		t.Fatalf("list not split: got %+v want %v", pub.Libs, want)
	}
	for i := range want {
		if pub.Libs[i] != want[i] {
			t.Errorf("lib %d: %q want %q", i, pub.Libs[i], want[i])
		}
	}
}

// TestExtractTargetCompile_Definitions and _Options pin the
// new TARGET_PROPERTY INTERFACE_* aggregation extractor — same
// keyword shape as target_link_libraries, but for
// target_compile_definitions / target_compile_options.
func TestExtractTargetCompile_Definitions(t *testing.T) {
	trace := `{"args":["t","PUBLIC","FOO=1","INTERFACE","BAR","PRIVATE","BAZ=quoted"],"cmd":"target_compile_definitions","file":"/src/CMakeLists.txt","line":2}
{"args":["other","UNUSED"],"cmd":"target_compile_options","file":"/src/CMakeLists.txt","line":3}
`
	got := ExtractTargetCompile([]byte(trace), "/src", nil, "target_compile_definitions")
	if len(got) != 1 {
		t.Fatalf("want 1 call; got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Cmd != "target_compile_definitions" {
		t.Errorf("Cmd: %q", c.Cmd)
	}
	if c.Target != "t" {
		t.Errorf("Target: %q", c.Target)
	}
	if len(c.Groups) != 3 {
		t.Fatalf("Groups: %+v", c.Groups)
	}
	if c.Groups[0].Visibility != "PUBLIC" || c.Groups[0].Items[0] != "FOO=1" {
		t.Errorf("PUBLIC group: %+v", c.Groups[0])
	}
	if c.Groups[1].Visibility != "INTERFACE" || c.Groups[1].Items[0] != "BAR" {
		t.Errorf("INTERFACE group: %+v", c.Groups[1])
	}
	if c.Groups[2].Visibility != "PRIVATE" || c.Groups[2].Items[0] != "BAZ=quoted" {
		t.Errorf("PRIVATE group: %+v", c.Groups[2])
	}
}

func TestExtractTargetCompile_Options(t *testing.T) {
	trace := `{"args":["t","INTERFACE","-Wall","-Wextra"],"cmd":"target_compile_options","file":"/src/CMakeLists.txt","line":5}
`
	got := ExtractTargetCompile([]byte(trace), "/src", nil, "target_compile_options")
	if len(got) != 1 || got[0].Target != "t" || len(got[0].Groups) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0].Groups[0]
	if g.Visibility != "INTERFACE" {
		t.Errorf("Visibility: %q", g.Visibility)
	}
	if len(g.Items) != 2 || g.Items[0] != "-Wall" || g.Items[1] != "-Wextra" {
		t.Errorf("Items: %+v", g.Items)
	}
}

// TestExtractTargetCompile_LegacyPositional pins the empty-
// keyword fallback for the legacy positional shape
// `target_compile_definitions(t FOO=1)`. cmake treats those as
// PRIVATE-equivalent (no propagation to consumers); we record
// them under Visibility="" so callers can distinguish.
func TestExtractTargetCompile_LegacyPositional(t *testing.T) {
	trace := `{"args":["t","FOO=1","BAR=2"],"cmd":"target_compile_definitions","file":"/src/CMakeLists.txt","line":7}
`
	got := ExtractTargetCompile([]byte(trace), "/src", nil, "target_compile_definitions")
	if len(got) != 1 || len(got[0].Groups) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0].Groups[0]
	if g.Visibility != "" {
		t.Errorf("Visibility for legacy positional: %q want \"\"", g.Visibility)
	}
	if len(g.Items) != 2 {
		t.Errorf("Items: %+v", g.Items)
	}
}

func TestExtractConfigureFiles_FiltersCmakeInternal(t *testing.T) {
	got := ExtractConfigureFiles([]byte(traceMixed), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 user configure_file (cmake-internal one filtered); got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Input != "in.h.in" || c.Output != "out.h" {
		t.Errorf("input/output: %+v", c)
	}
	if len(c.Options) != 1 || c.Options[0] != "@ONLY" {
		t.Errorf("options: %+v", c.Options)
	}
}

// traceExportHeader holds one generate_export_header configure_file call: its
// call SITE is cmake's own GenerateExportHeader.cmake (outside the source
// tree), but its template is exportheader.cmake.in and its output is a
// per-target compile header consumers #include — so it must be recovered, not
// filtered like other cmake-internal configure_file calls.
const traceExportHeader = `{"args":["in.h.in","out.h","@ONLY"],"cmd":"configure_file","file":"/src/CMakeLists.txt","line":7}
{"args":["/usr/share/cmake-3.28/Modules/exportheader.cmake.in","/build/mylib/mylib_export.h","@ONLY"],"cmd":"configure_file","file":"/usr/share/cmake-3.28/Modules/GenerateExportHeader.cmake","line":406}
{"args":["/usr/share/cmake-3.28/Modules/CMakeSystem.cmake.in","/build/CMakeFiles/3.28.3/CMakeSystem.cmake","@ONLY"],"cmd":"configure_file","file":"/usr/share/cmake-3.28/Modules/CMakeDetermineSystem.cmake","line":246}
`

func TestExtractConfigureFiles_GenerateExportHeader(t *testing.T) {
	got := ExtractConfigureFiles([]byte(traceExportHeader), "/src")
	// The in-tree user call AND the export-header call are kept; the
	// CMakeSystem cmake-internal call is filtered.
	if len(got) != 2 {
		t.Fatalf("want 2 (in-tree user + generate_export_header; CMakeSystem filtered); got %d (%+v)", len(got), got)
	}
	var sawExport bool
	for _, c := range got {
		if c.Output == "/build/mylib/mylib_export.h" {
			sawExport = true
			if !strings.HasSuffix(c.Input, "/exportheader.cmake.in") {
				t.Errorf("export-header call input: %q", c.Input)
			}
		}
	}
	if !sawExport {
		t.Errorf("generate_export_header configure_file was filtered (call-site outside source tree); want recovered: %+v", got)
	}
}

func TestExtractTargetIncludes_SystemAndOrder(t *testing.T) {
	// SYSTEM + BEFORE + visibility — the order keywords prefix
	// the visibility group.
	trace := `{"args":["t","SYSTEM","BEFORE","INTERFACE","sys/inc"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":1}
`
	got := ExtractTargetIncludes([]byte(trace), "/src", nil)
	if len(got) != 1 || len(got[0].Groups) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0].Groups[0]
	if g.Visibility != "INTERFACE" || !g.System || g.Order != "BEFORE" {
		t.Errorf("group: %+v want SYSTEM BEFORE INTERFACE", g)
	}
}

func TestExtractTargetIncludes_PositionalDirs(t *testing.T) {
	// Legacy pre-3.0 shape: bare positional dirs without a
	// visibility keyword — group as PRIVATE per cmake's
	// historical default.
	trace := `{"args":["t","inc1","inc2"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":1}
`
	got := ExtractTargetIncludes([]byte(trace), "/src", nil)
	if len(got) != 1 || len(got[0].Groups) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0].Groups[0]
	if g.Visibility != "PRIVATE" || len(g.Dirs) != 2 {
		t.Errorf("group: %+v", g)
	}
}

// TestExtractTargetLinks_KnownTargetsRescue covers the
// macro-from-import case: a producer element's .cmake module
// (outside the consumer source root) calls
// target_link_libraries on a consumer-defined target. The
// strict file-path filter would drop that call; the
// knownTargets second arm keeps it.
func TestExtractTargetLinks_KnownTargetsRescue(t *testing.T) {
	trace := `{"args":["consumer_target","PUBLIC","ZLIB::ZLIB"],"cmd":"target_link_libraries","file":"/opt/producer-modules/Helpers.cmake","line":3}
{"args":["producer_internal","libfoo"],"cmd":"target_link_libraries","file":"/opt/producer-modules/Helpers.cmake","line":7}
`
	if got := ExtractTargetLinks([]byte(trace), "/src", nil); len(got) != 0 {
		t.Errorf("nil knownTargets: want 0 calls, got %d (%+v)", len(got), got)
	}
	known := map[string]bool{"consumer_target": true}
	got := ExtractTargetLinks([]byte(trace), "/src", known)
	if len(got) != 1 || got[0].Target != "consumer_target" {
		t.Fatalf("with knownTargets: want 1 call on consumer_target, got %+v", got)
	}
	if len(got[0].Groups) != 1 || got[0].Groups[0].Libs[0] != "ZLIB::ZLIB" {
		t.Errorf("rescued call libs: %+v", got[0].Groups[0])
	}
}

func TestInSourceTree(t *testing.T) {
	cases := []struct {
		file, root string
		want       bool
	}{
		{"/src/CMakeLists.txt", "/src", true},
		{"/src/sub/CMakeLists.txt", "/src", true},
		{"/usr/share/cmake-3.28/Modules/Foo.cmake", "/src", false},
		{"/build/CMakeFiles/scratch/CMakeLists.txt", "/src", false},
		{"/src", "/src", true},        // edge: source root itself
		{"/src-other", "/src", false}, // edge: prefix that's not a directory boundary
		{"", "/src", false},
		{"/src/CMakeLists.txt", "", false},
	}
	for _, c := range cases {
		t.Run(c.file+"::"+c.root, func(t *testing.T) {
			if got := inSourceTree(c.file, c.root); got != c.want {
				t.Errorf("inSourceTree(%q, %q) = %v, want %v", c.file, c.root, got, c.want)
			}
		})
	}
}

// TestExtractExecuteProcess_FiltersCmakeInternal asserts that
// only execute_process calls inside the source tree are
// surfaced. cmake-internal calls (CMakeDetermineCompilerId
// etc., living under /usr/share/cmake-*) are filtered out by
// the inSourceTree gate — the converter isn't trying to "lift"
// cmake's own toolchain probes.
func TestExtractExecuteProcess_FiltersCmakeInternal(t *testing.T) {
	trace := `{"args":["COMMAND","git","rev-parse","HEAD","OUTPUT_VARIABLE","GIT_SHA","OUTPUT_STRIP_TRAILING_WHITESPACE"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":12}
{"args":["COMMAND","/usr/bin/cmake","-E","capabilities","RESULT_VARIABLE","r"],"cmd":"execute_process","file":"/usr/share/cmake-3.28/Modules/CMakeDetermineSystem.cmake","line":18}
`
	got := ExtractExecuteProcess([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 user execute_process (cmake-internal one filtered); got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.File != "/src/CMakeLists.txt" || c.Line != 12 {
		t.Errorf("file/line: %+v want /src/CMakeLists.txt:12", c)
	}
	if len(c.Commands) != 1 ||
		len(c.Commands[0]) != 3 ||
		c.Commands[0][0] != "git" || c.Commands[0][2] != "HEAD" {
		t.Errorf("commands: %+v want [[git rev-parse HEAD]]", c.Commands)
	}
	if c.OutputVariable != "GIT_SHA" {
		t.Errorf("OutputVariable: %q want GIT_SHA", c.OutputVariable)
	}
}

// TestExtractExecuteProcess_MultiCommandPipeline asserts that
// each `COMMAND` clause becomes its own argv list in
// Commands[]. Multiple-COMMAND form is cmake's pipeline syntax
// (concurrent stages with stdout chaining) and the bucket
// classifier needs the per-stage argv to decide whether the
// pipeline is liftable.
func TestExtractExecuteProcess_MultiCommandPipeline(t *testing.T) {
	trace := `{"args":["COMMAND","grep","foo","input.txt","COMMAND","wc","-l","OUTPUT_VARIABLE","LINES","WORKING_DIRECTORY","/src/sub"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":1}
`
	got := ExtractExecuteProcess([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	c := got[0]
	if len(c.Commands) != 2 {
		t.Fatalf("commands: want 2 stages; got %+v", c.Commands)
	}
	if got, want := strings.Join(c.Commands[0], " "), "grep foo input.txt"; got != want {
		t.Errorf("stage 0: %q want %q", got, want)
	}
	if got, want := strings.Join(c.Commands[1], " "), "wc -l"; got != want {
		t.Errorf("stage 1: %q want %q", got, want)
	}
	if c.OutputVariable != "LINES" {
		t.Errorf("OutputVariable: %q want LINES", c.OutputVariable)
	}
	if c.WorkingDirectory != "/src/sub" {
		t.Errorf("WorkingDirectory: %q want /src/sub", c.WorkingDirectory)
	}
}

// TestExtractExecuteProcess_OutputFileAndEnvironment exercises
// OUTPUT_FILE (the file-producing bucket's signal) and
// ENVIRONMENT (variadic — consumes KEY=value tokens until the
// next keyword closes the list).
func TestExtractExecuteProcess_OutputFileAndEnvironment(t *testing.T) {
	trace := `{"args":["COMMAND","python3","gen.py","--in","spec.txt","OUTPUT_FILE","generated.h","ENVIRONMENT","PATH=/usr/bin","LANG=C","TIMEOUT","30"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":4}
`
	got := ExtractExecuteProcess([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	c := got[0]
	if len(c.Commands) != 1 {
		t.Fatalf("want 1 command stage, got %+v", c.Commands)
	}
	if got, want := strings.Join(c.Commands[0], " "), "python3 gen.py --in spec.txt"; got != want {
		t.Errorf("argv: %q want %q", got, want)
	}
	if c.OutputFile != "generated.h" {
		t.Errorf("OutputFile: %q want generated.h", c.OutputFile)
	}
	if len(c.Environment) != 2 ||
		c.Environment[0] != "PATH=/usr/bin" || c.Environment[1] != "LANG=C" {
		t.Errorf("Environment: %+v", c.Environment)
	}
	if c.Timeout != "30" {
		t.Errorf("Timeout: %q want 30", c.Timeout)
	}
}

// TestExtractExecuteProcess_NoCommandClauseDropped asserts that
// a malformed event with no COMMAND clause is dropped silently
// rather than surfacing as a zero-Commands call. cmake itself
// rejects this shape; defensive parser behaviour keeps the
// classifier from having to special-case empty-pipeline records.
func TestExtractExecuteProcess_NoCommandClauseDropped(t *testing.T) {
	trace := `{"args":["OUTPUT_VARIABLE","x"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":1}
`
	if got := ExtractExecuteProcess([]byte(trace), "/src"); len(got) != 0 {
		t.Errorf("want 0 calls (no COMMAND clause); got %+v", got)
	}
}

// TestExtractExecuteProcess_DecodeIntegration confirms that the
// combined-pass Decode dispatches execute_process events into
// d.ExecuteProcesses alongside the other extractor outputs —
// the Lower converter's expected access path.
func TestExtractExecuteProcess_DecodeIntegration(t *testing.T) {
	trace := `{"args":["t","PUBLIC","inc"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":4}
{"args":["COMMAND","git","describe","--tags","OUTPUT_VARIABLE","V"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}
`
	d := Decode([]byte(trace), "/src", nil)
	if len(d.Includes) != 1 {
		t.Errorf("Includes: want 1, got %d", len(d.Includes))
	}
	if len(d.ExecuteProcesses) != 1 {
		t.Fatalf("ExecuteProcesses: want 1, got %d", len(d.ExecuteProcesses))
	}
	if d.ExecuteProcesses[0].OutputVariable != "V" {
		t.Errorf("OutputVariable: %q want V", d.ExecuteProcesses[0].OutputVariable)
	}
}

// TestDecode_OutOfTreeExecuteProcess confirms Decode splits
// execute_process events by source-tree membership: in-tree calls land in
// d.ExecuteProcesses (the lift path), out-of-tree calls land in
// d.OutOfTreeExecuteProcesses (the surfacing path) — never both, never
// silently dropped at extraction.
func TestDecode_OutOfTreeExecuteProcess(t *testing.T) {
	trace := strings.Join([]string{
		// In-tree: the project's own call.
		`{"args":["COMMAND","git","describe","OUTPUT_VARIABLE","V"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}`,
		// Out-of-tree: a build-dir subproject's CMakeLists.
		`{"args":["COMMAND","python3","gen.py","input"],"cmd":"execute_process","file":"/build/_deps/sub-src/CMakeLists.txt","line":3}`,
		// Out-of-tree: a cmake bundled-module probe.
		`{"args":["COMMAND","/usr/bin/cc","-dumpversion"],"cmd":"execute_process","file":"/usr/share/cmake-4.0/Modules/CMakeDetermineCompilerId.cmake","line":9}`,
	}, "\n") + "\n"
	d := Decode([]byte(trace), "/src", nil)
	if len(d.ExecuteProcesses) != 1 || d.ExecuteProcesses[0].File != "/src/CMakeLists.txt" {
		t.Fatalf("ExecuteProcesses: want 1 in-tree call, got %+v", d.ExecuteProcesses)
	}
	if len(d.OutOfTreeExecuteProcesses) != 2 {
		t.Fatalf("OutOfTreeExecuteProcesses: want 2, got %d (%+v)", len(d.OutOfTreeExecuteProcesses), d.OutOfTreeExecuteProcesses)
	}
	files := []string{d.OutOfTreeExecuteProcesses[0].File, d.OutOfTreeExecuteProcesses[1].File}
	wantFiles := []string{"/build/_deps/sub-src/CMakeLists.txt", "/usr/share/cmake-4.0/Modules/CMakeDetermineCompilerId.cmake"}
	for i, w := range wantFiles {
		if files[i] != w {
			t.Errorf("OutOfTreeExecuteProcesses[%d].File = %q, want %q", i, files[i], w)
		}
	}
	// argv parses identically through the shared parser.
	if got := strings.Join(d.OutOfTreeExecuteProcesses[0].Commands[0], " "); got != "python3 gen.py input" {
		t.Errorf("out-of-tree argv: %q want %q", got, "python3 gen.py input")
	}
}

// TestExtract_RealCmakeTrace walks a hand-curated subset of
// real cmake-3.28 trace lines (captured from the trace-test
// fixture) to make sure the parser handles cmake's actual
// wire format — extra fields (frame / global_frame / time /
// line_end), arg encoding, etc.
func TestExtract_RealCmakeTrace(t *testing.T) {
	real := strings.Join([]string{
		`{"args":["t","PUBLIC","inc","PRIVATE","inc/priv"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","frame":1,"global_frame":1,"line":4,"time":1777633549.355098}`,
		`{"args":["t","PUBLIC","ZLIB::ZLIB"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","frame":1,"global_frame":1,"line":6,"time":1777633549.3724971}`,
		`{"args":["in.h.in","out.h","@ONLY"],"cmd":"configure_file","file":"/src/CMakeLists.txt","frame":1,"global_frame":1,"line":7,"line_end":7,"time":1777633549.3725619}`,
	}, "\n") + "\n"
	if len(ExtractTargetIncludes([]byte(real), "/src", nil)) != 1 {
		t.Errorf("missed target_include_directories with extra fields")
	}
	if len(ExtractTargetLinks([]byte(real), "/src", nil)) != 1 {
		t.Errorf("missed target_link_libraries with extra fields")
	}
	if len(ExtractConfigureFiles([]byte(real), "/src")) != 1 {
		t.Errorf("missed configure_file with extra fields")
	}
}

// TestDecode_InstallExportNamespace recovers the NAMESPACE the
// codemodel drops. The install(TARGETS ... EXPORT <name> ...) form
// (which carries no NAMESPACE) must be ignored; only the
// install(EXPORT <name> ... NAMESPACE <ns> ...) form is recorded.
func TestDecode_InstallExportNamespace(t *testing.T) {
	trace := strings.Join([]string{
		// Associates the target with an export — no namespace; must NOT match.
		`{"args":["TARGETS","usepkg","EXPORT","usepkgTargets","ARCHIVE","DESTINATION","lib"],"cmd":"install","file":"/src/CMakeLists.txt","line":17}`,
		// The real namespace-bearing form.
		`{"args":["EXPORT","usepkgTargets","FILE","usepkgTargets.cmake","NAMESPACE","usepkg::","DESTINATION","lib/cmake/usepkg"],"cmd":"install","file":"/src/CMakeLists.txt","line":21}`,
	}, "\n") + "\n"
	d := Decode([]byte(trace), "/src", nil)
	if len(d.InstallExports) != 1 {
		t.Fatalf("want 1 install(EXPORT) call, got %d (%+v)", len(d.InstallExports), d.InstallExports)
	}
	c := d.InstallExports[0]
	if c.ExportName != "usepkgTargets" {
		t.Errorf("ExportName = %q, want usepkgTargets", c.ExportName)
	}
	if c.Namespace != "usepkg::" {
		t.Errorf("Namespace = %q, want usepkg::", c.Namespace)
	}
	if c.File != "usepkgTargets.cmake" {
		t.Errorf("File = %q, want usepkgTargets.cmake", c.File)
	}
	if c.Destination != "lib/cmake/usepkg" {
		t.Errorf("Destination = %q, want lib/cmake/usepkg", c.Destination)
	}
}

// TestDecode_InstallExportNoNamespace covers install(EXPORT) without
// a NAMESPACE keyword (legal cmake — the targets export under their
// bare names). The call is still recorded; Namespace is empty.
func TestDecode_InstallExportNoNamespace(t *testing.T) {
	trace := `{"args":["EXPORT","fooTargets","DESTINATION","lib/cmake/foo"],"cmd":"install","file":"/src/CMakeLists.txt","line":9}` + "\n"
	d := Decode([]byte(trace), "/src", nil)
	if len(d.InstallExports) != 1 {
		t.Fatalf("want 1 call, got %d", len(d.InstallExports))
	}
	if d.InstallExports[0].Namespace != "" {
		t.Errorf("Namespace = %q, want empty", d.InstallExports[0].Namespace)
	}
}

// TestExtractFileGenerate_InputForm walks the INPUT shape:
// file(GENERATE OUTPUT <out> INPUT <in> CONDITION <c> NEWLINE_STYLE UNIX).
// CONDITION is recorded verbatim — evaluation happens at
// generate-time, not in the extractor.
func TestExtractFileGenerate_InputForm(t *testing.T) {
	trace := `{"args":["GENERATE","OUTPUT","gen.h","INPUT","gen.h.in","CONDITION","$<CONFIG:Release>","NEWLINE_STYLE","UNIX"],"cmd":"file","file":"/src/CMakeLists.txt","line":11}
`
	got := ExtractFileGenerate([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call, got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.File != "/src/CMakeLists.txt" || c.Line != 11 {
		t.Errorf("file/line: %+v want /src/CMakeLists.txt:11", c)
	}
	if c.Output != "gen.h" {
		t.Errorf("Output: %q want gen.h", c.Output)
	}
	if c.Input != "gen.h.in" || !c.HasInput {
		t.Errorf("Input: %q HasInput: %v want (gen.h.in, true)", c.Input, c.HasInput)
	}
	if c.Content != "" || c.HasContent {
		t.Errorf("Content: %q HasContent: %v want (empty, false) for INPUT form", c.Content, c.HasContent)
	}
	if c.Condition != "$<CONFIG:Release>" {
		t.Errorf("Condition: %q", c.Condition)
	}
	if c.NewlineStyle != "UNIX" {
		t.Errorf("NewlineStyle: %q want UNIX", c.NewlineStyle)
	}
}

// TestExtractFileGenerate_ContentForm covers the CONTENT shape
// where the substitution body lives inline as a string argument
// rather than as an INPUT file path.
func TestExtractFileGenerate_ContentForm(t *testing.T) {
	trace := `{"args":["GENERATE","OUTPUT","banner.h","CONTENT","#define BANNER \"hi\"\n","TARGET","mytarget"],"cmd":"file","file":"/src/CMakeLists.txt","line":3}
`
	got := ExtractFileGenerate([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call, got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Input != "" || c.HasInput {
		t.Errorf("Input: %q HasInput: %v want (empty, false) for CONTENT form", c.Input, c.HasInput)
	}
	if c.Content != "#define BANNER \"hi\"\n" || !c.HasContent {
		t.Errorf("Content: %q HasContent: %v", c.Content, c.HasContent)
	}
	if c.Target != "mytarget" {
		t.Errorf("Target: %q want mytarget", c.Target)
	}
}

// TestExtractFileGenerate_FiltersNonGenerateAndOutOfTree
// confirms two filter axes: (a) non-GENERATE file()
// subcommands (READ / WRITE / COPY / ...) are ignored; (b)
// file(GENERATE) calls from outside the source tree (e.g.
// cmake-internal modules) are filtered like configure_file
// and execute_process.
func TestExtractFileGenerate_FiltersNonGenerateAndOutOfTree(t *testing.T) {
	trace := `{"args":["READ","/src/foo.txt","V"],"cmd":"file","file":"/src/CMakeLists.txt","line":1}
{"args":["WRITE","/src/out.txt","hi"],"cmd":"file","file":"/src/CMakeLists.txt","line":2}
{"args":["GENERATE","OUTPUT","/build/internal.h","CONTENT","internal\n"],"cmd":"file","file":"/usr/share/cmake-3.28/Modules/CMakeSomething.cmake","line":42}
{"args":["GENERATE","OUTPUT","ok.h","CONTENT","ok\n"],"cmd":"file","file":"/src/CMakeLists.txt","line":12}
`
	got := ExtractFileGenerate([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 user file(GENERATE); got %d (%+v)", len(got), got)
	}
	if got[0].Output != "ok.h" {
		t.Errorf("output: %q want ok.h", got[0].Output)
	}
}

// TestExtractFileGenerate_EmptyContentPreserved covers the
// `file(GENERATE OUTPUT ... CONTENT "")` shape: cmake accepts
// this and writes an empty output file. The extractor must
// preserve the call (HasContent=true, Content="") rather than
// drop it as malformed — the lifter routes it through the
// CONTENT-form lift so the empty-template invariant rides into
// the genrule shape.
func TestExtractFileGenerate_EmptyContentPreserved(t *testing.T) {
	trace := `{"args":["GENERATE","OUTPUT","empty.txt","CONTENT",""],"cmd":"file","file":"/src/CMakeLists.txt","line":5}
`
	got := ExtractFileGenerate([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("CONTENT \"\" should be preserved; got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Output != "empty.txt" {
		t.Errorf("Output: %q want empty.txt", c.Output)
	}
	if c.Content != "" {
		t.Errorf("Content: %q want empty string", c.Content)
	}
	if !c.HasContent {
		t.Errorf("HasContent must be true even when Content == \"\"")
	}
	if c.HasInput {
		t.Errorf("HasInput must be false for CONTENT form")
	}
}

// TestExtractFileGenerate_BothInputAndContentDropped covers
// the case where cmake records a malformed call carrying both
// INPUT and CONTENT keywords. cmake itself rejects this shape;
// the extractor mirrors that so the lifter can rely on
// HasInput XOR HasContent after a successful classify.
func TestExtractFileGenerate_BothInputAndContentDropped(t *testing.T) {
	trace := `{"args":["GENERATE","OUTPUT","x.h","INPUT","x.in","CONTENT","fallback\n"],"cmd":"file","file":"/src/CMakeLists.txt","line":1}
`
	if got := ExtractFileGenerate([]byte(trace), "/src"); len(got) != 0 {
		t.Errorf("both-keywords-set should be rejected; got %+v", got)
	}
}

// TestExtractFileGenerate_MalformedDropped covers cmake's
// own well-formedness rules: a call with no OUTPUT, or with
// neither INPUT nor CONTENT, is rejected by cmake itself; the
// extractor drops these defensively rather than surfacing
// FileGenerateCall records the lifter can't act on.
func TestExtractFileGenerate_MalformedDropped(t *testing.T) {
	trace := `{"args":["GENERATE","INPUT","x.in"],"cmd":"file","file":"/src/CMakeLists.txt","line":1}
{"args":["GENERATE","OUTPUT","y.h"],"cmd":"file","file":"/src/CMakeLists.txt","line":2}
{"args":["GENERATE"],"cmd":"file","file":"/src/CMakeLists.txt","line":3}
`
	if got := ExtractFileGenerate([]byte(trace), "/src"); len(got) != 0 {
		t.Errorf("want 0 (all malformed); got %+v", got)
	}
}

// TestExtractFileGenerate_PermissionsConsumed asserts that
// the variadic PERMISSIONS / FILE_PERMISSIONS lists are
// consumed up to the next keyword, so subsequent keywords
// (e.g. CONDITION) are still recognized. Permission tokens
// themselves are dropped — the lifter doesn't care about mode
// bits.
func TestExtractFileGenerate_PermissionsConsumed(t *testing.T) {
	trace := `{"args":["GENERATE","OUTPUT","p.h","CONTENT","p\n","FILE_PERMISSIONS","OWNER_READ","OWNER_WRITE","GROUP_READ","CONDITION","TRUE"],"cmd":"file","file":"/src/CMakeLists.txt","line":1}
`
	got := ExtractFileGenerate([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	c := got[0]
	if c.Output != "p.h" {
		t.Errorf("Output: %q want p.h", c.Output)
	}
	if c.Condition != "TRUE" {
		t.Errorf("Condition: %q want TRUE (permissions list should have been bounded)", c.Condition)
	}
}

// TestExtractFileGenerate_DecodeIntegration confirms Decode's
// combined-pass dispatch routes file(GENERATE) events into
// d.FileGenerates alongside the other extractor outputs.
func TestExtractFileGenerate_DecodeIntegration(t *testing.T) {
	trace := `{"args":["t","PUBLIC","inc"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":4}
{"args":["GENERATE","OUTPUT","gen.h","CONTENT","hi\n"],"cmd":"file","file":"/src/CMakeLists.txt","line":7}
{"args":["in.h.in","out.h"],"cmd":"configure_file","file":"/src/CMakeLists.txt","line":9}
`
	d := Decode([]byte(trace), "/src", nil)
	if len(d.Includes) != 1 {
		t.Errorf("Includes: want 1, got %d", len(d.Includes))
	}
	if len(d.ConfigFiles) != 1 {
		t.Errorf("ConfigFiles: want 1, got %d", len(d.ConfigFiles))
	}
	if len(d.FileGenerates) != 1 {
		t.Fatalf("FileGenerates: want 1, got %d", len(d.FileGenerates))
	}
	if d.FileGenerates[0].Output != "gen.h" || d.FileGenerates[0].Content != "hi\n" {
		t.Errorf("FileGenerate[0]: %+v", d.FileGenerates[0])
	}
}

// TestExtractSourceFileProperties_Basic covers the common shape:
// files followed by PROPERTIES then (name, value) pairs.
func TestExtractSourceFileProperties_Basic(t *testing.T) {
	trace := `{"args":["foo.c","bar.c","PROPERTIES","COMPILE_DEFINITIONS","FOO=1","COMPILE_OPTIONS","-Wall"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":4}
`
	got := ExtractSourceFileProperties([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call; got %d (%+v)", len(got), got)
	}
	call := got[0]
	if call.File != "/src/CMakeLists.txt" || call.Line != 4 {
		t.Errorf("call site: %s:%d", call.File, call.Line)
	}
	wantFiles := []string{"foo.c", "bar.c"}
	if !sliceEq(call.Files, wantFiles) {
		t.Errorf("Files: got %v want %v", call.Files, wantFiles)
	}
	if len(call.Properties) != 2 {
		t.Fatalf("Properties len: %d", len(call.Properties))
	}
	if call.Properties[0].Name != "COMPILE_DEFINITIONS" || call.Properties[0].Value != "FOO=1" {
		t.Errorf("Properties[0]: %+v", call.Properties[0])
	}
	if call.Properties[1].Name != "COMPILE_OPTIONS" || call.Properties[1].Value != "-Wall" {
		t.Errorf("Properties[1]: %+v", call.Properties[1])
	}
}

// TestExtractSourceFileProperties_DirectoryArm covers the
// DIRECTORY <dir> arm before PROPERTIES.
func TestExtractSourceFileProperties_DirectoryArm(t *testing.T) {
	trace := `{"args":["lib.c","DIRECTORY","subdir","PROPERTIES","LANGUAGE","CXX"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":7}
`
	got := ExtractSourceFileProperties([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call; got %d", len(got))
	}
	call := got[0]
	if !sliceEq(call.Files, []string{"lib.c"}) {
		t.Errorf("Files: %v", call.Files)
	}
	if !sliceEq(call.Directories, []string{"subdir"}) {
		t.Errorf("Directories: %v", call.Directories)
	}
	if len(call.Properties) != 1 || call.Properties[0].Name != "LANGUAGE" || call.Properties[0].Value != "CXX" {
		t.Errorf("Properties: %+v", call.Properties)
	}
}

// TestExtractSourceFileProperties_TargetDirectoryArm covers the
// TARGET_DIRECTORY <tgt> arm.
func TestExtractSourceFileProperties_TargetDirectoryArm(t *testing.T) {
	trace := `{"args":["gen.c","TARGET_DIRECTORY","foo","PROPERTIES","GENERATED","TRUE"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":10}
`
	got := ExtractSourceFileProperties([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call; got %d", len(got))
	}
	if !sliceEq(got[0].TargetDirectories, []string{"foo"}) {
		t.Errorf("TargetDirectories: %v", got[0].TargetDirectories)
	}
}

// TestExtractSourceFileProperties_FiltersOutOfTree drops calls
// whose trace event fires outside the source tree (cmake-internal
// modules invoking set_source_files_properties).
func TestExtractSourceFileProperties_FiltersOutOfTree(t *testing.T) {
	trace := `{"args":["fake.c","PROPERTIES","LANGUAGE","C"],"cmd":"set_source_files_properties","file":"/usr/share/cmake-3.28/Modules/some.cmake","line":3}
{"args":["user.c","PROPERTIES","LANGUAGE","C"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":5}
`
	got := ExtractSourceFileProperties([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 user call (cmake-internal filtered); got %d (%+v)", len(got), got)
	}
	if got[0].Files[0] != "user.c" {
		t.Errorf("kept wrong call: %+v", got[0])
	}
}

// TestExtractSourceFileProperties_MalformedDropped drops calls
// with no files, no properties, or PROPERTIES with no pairs.
func TestExtractSourceFileProperties_MalformedDropped(t *testing.T) {
	trace := `{"args":["PROPERTIES","LANGUAGE","C"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":1}
{"args":["foo.c"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":2}
{"args":["foo.c","PROPERTIES"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":3}
`
	got := ExtractSourceFileProperties([]byte(trace), "/src")
	if len(got) != 0 {
		t.Errorf("want 0 calls (all malformed); got %d (%+v)", len(got), got)
	}
}

// TestExtractSourceFileProperties_DecodeIntegration confirms the
// combined-pass Decode dispatches set_source_files_properties events
// into Decoded.SourceFileProperties.
func TestExtractSourceFileProperties_DecodeIntegration(t *testing.T) {
	trace := `{"args":["foo.c","PROPERTIES","COMPILE_DEFINITIONS","BAR=2"],"cmd":"set_source_files_properties","file":"/src/CMakeLists.txt","line":4}
`
	d := Decode([]byte(trace), "/src", map[string]bool{})
	if len(d.SourceFileProperties) != 1 {
		t.Fatalf("Decode: want 1 source-file-property call, got %d", len(d.SourceFileProperties))
	}
	if d.SourceFileProperties[0].Properties[0].Value != "BAR=2" {
		t.Errorf("SourceFileProperties[0]: %+v", d.SourceFileProperties[0])
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExtractAddCustomCommand covers the OUTPUT-form
// add_custom_command shape used by the standalone-genrule
// cross-reference. Args are split into OUTPUT / COMMAND / DEPENDS /
// BYPRODUCTS / WORKING_DIRECTORY sections; multi-COMMAND forms
// produce multiple argv lists in Commands.
func TestExtractAddCustomCommand_OutputForm(t *testing.T) {
	trace := `{"args":["OUTPUT","gen/version.h","gen/build.h","COMMAND","python3","gen.py","--out","gen/version.h","COMMAND","touch","gen/build.h","DEPENDS","gen.py","BYPRODUCTS","gen.log","WORKING_DIRECTORY","/src","COMMENT","generating version+build headers","VERBATIM"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":12}
`
	got := ExtractAddCustomCommands([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call, got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.File != "/src/CMakeLists.txt" || c.Line != 12 {
		t.Errorf("file/line: %s:%d", c.File, c.Line)
	}
	if !sliceEq(c.Outputs, []string{"gen/version.h", "gen/build.h"}) {
		t.Errorf("Outputs: %v", c.Outputs)
	}
	if len(c.Commands) != 2 {
		t.Fatalf("Commands: want 2, got %d (%+v)", len(c.Commands), c.Commands)
	}
	if !sliceEq(c.Commands[0], []string{"python3", "gen.py", "--out", "gen/version.h"}) {
		t.Errorf("Commands[0]: %v", c.Commands[0])
	}
	if !sliceEq(c.Commands[1], []string{"touch", "gen/build.h"}) {
		t.Errorf("Commands[1]: %v", c.Commands[1])
	}
	if !sliceEq(c.Depends, []string{"gen.py"}) {
		t.Errorf("Depends: %v", c.Depends)
	}
	if !sliceEq(c.ByProducts, []string{"gen.log"}) {
		t.Errorf("ByProducts: %v", c.ByProducts)
	}
	if c.WorkingDirectory != "/src" {
		t.Errorf("WorkingDirectory: %q", c.WorkingDirectory)
	}
	if c.Comment != "generating version+build headers" {
		t.Errorf("Comment: %q", c.Comment)
	}
}

// TestExtractAddCustomCommand_TargetFormFiltered confirms the
// add_custom_command(TARGET ... PRE_BUILD|POST_BUILD|PRE_LINK ...)
// shape doesn't surface as an OUTPUT-form call (it attaches a hook
// to an existing target, doesn't declare a standalone genrule).
func TestExtractAddCustomCommand_TargetFormFiltered(t *testing.T) {
	trace := `{"args":["TARGET","mylib","POST_BUILD","COMMAND","echo","done"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":3}
`
	got := ExtractAddCustomCommands([]byte(trace), "/src")
	if len(got) != 0 {
		t.Errorf("TARGET-form should be filtered; got %+v", got)
	}
}

// TestExtractAddCustomTarget covers a typical
// add_custom_target(name ALL COMMAND ... DEPENDS ... BYPRODUCTS
// ... SOURCES ... COMMENT ...).
func TestExtractAddCustomTarget(t *testing.T) {
	trace := `{"args":["mygen","ALL","COMMAND","python3","gen.py","DEPENDS","gen/version.h","BYPRODUCTS","gen/out.txt","SOURCES","gen.py","COMMENT","run mygen","VERBATIM"],"cmd":"add_custom_target","file":"/src/CMakeLists.txt","line":17}
`
	got := ExtractAddCustomTargets([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call, got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Name != "mygen" {
		t.Errorf("Name: %q", c.Name)
	}
	if !c.All {
		t.Errorf("All: want true")
	}
	if len(c.Commands) != 1 || !sliceEq(c.Commands[0], []string{"python3", "gen.py"}) {
		t.Errorf("Commands: %v", c.Commands)
	}
	if !sliceEq(c.Depends, []string{"gen/version.h"}) {
		t.Errorf("Depends: %v", c.Depends)
	}
	if !sliceEq(c.ByProducts, []string{"gen/out.txt"}) {
		t.Errorf("ByProducts: %v", c.ByProducts)
	}
	if !sliceEq(c.Sources, []string{"gen.py"}) {
		t.Errorf("Sources: %v", c.Sources)
	}
	if c.Comment != "run mygen" {
		t.Errorf("Comment: %q", c.Comment)
	}
}

// TestExtractAddCustomTarget_NoArgsRejected makes sure a malformed
// add_custom_target with no name is dropped.
func TestExtractAddCustomTarget_NoArgsRejected(t *testing.T) {
	trace := `{"args":[],"cmd":"add_custom_target","file":"/src/CMakeLists.txt","line":1}
`
	got := ExtractAddCustomTargets([]byte(trace), "/src")
	if len(got) != 0 {
		t.Errorf("empty add_custom_target args should be dropped; got %+v", got)
	}
}

// TestExtractAddCustomCommand_FiltersCmakeInternal asserts the
// source-tree gate drops add_custom_command events fired from
// cmake-bundled scripts or scratch CMakeLists.
func TestExtractAddCustomCommand_FiltersCmakeInternal(t *testing.T) {
	trace := `{"args":["OUTPUT","internal.txt","COMMAND","cmake","-E","touch","internal.txt"],"cmd":"add_custom_command","file":"/usr/share/cmake-3.28/Modules/foo.cmake","line":1}
{"args":["OUTPUT","user.txt","COMMAND","cmake","-E","touch","user.txt"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":1}
`
	got := ExtractAddCustomCommands([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 user call (cmake-internal filtered); got %d (%+v)", len(got), got)
	}
	if !sliceEq(got[0].Outputs, []string{"user.txt"}) {
		t.Errorf("Outputs: %v", got[0].Outputs)
	}
}

// TestExtractAddDependencies covers the
// add_dependencies(<target> <dep> ...) shape used by the
// cross-reference to detect downstream consumers of a custom-
// command output.
func TestExtractAddDependencies(t *testing.T) {
	trace := `{"args":["mylib","mygen","other_gen"],"cmd":"add_dependencies","file":"/src/CMakeLists.txt","line":21}
`
	got := ExtractAddDependencies([]byte(trace), "/src")
	if len(got) != 1 {
		t.Fatalf("want 1 call, got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Target != "mylib" {
		t.Errorf("Target: %q", c.Target)
	}
	if !sliceEq(c.Depends, []string{"mygen", "other_gen"}) {
		t.Errorf("Depends: %v", c.Depends)
	}
}

// TestDecode_CustomCommandsAndTargets confirms the combined
// Decode pass dispatches add_custom_command / add_custom_target /
// add_dependencies events into their respective fields on the
// Decoded result.
func TestDecode_CustomCommandsAndTargets(t *testing.T) {
	trace := `{"args":["OUTPUT","gen/version.h","COMMAND","python3","gen.py"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":4}
{"args":["mygen","DEPENDS","gen/version.h"],"cmd":"add_custom_target","file":"/src/CMakeLists.txt","line":5}
{"args":["mylib","mygen"],"cmd":"add_dependencies","file":"/src/CMakeLists.txt","line":6}
`
	d := Decode([]byte(trace), "/src", nil)
	if len(d.AddCustomCommands) != 1 {
		t.Errorf("AddCustomCommands: want 1, got %d", len(d.AddCustomCommands))
	}
	if len(d.AddCustomTargets) != 1 {
		t.Errorf("AddCustomTargets: want 1, got %d", len(d.AddCustomTargets))
	}
	if len(d.AddDependencies) != 1 {
		t.Errorf("AddDependencies: want 1, got %d", len(d.AddDependencies))
	}
	if d.AddCustomTargets[0].Name != "mygen" {
		t.Errorf("AddCustomTargets[0].Name: %q", d.AddCustomTargets[0].Name)
	}
}

// TestExtractFileRename_AsConfigureCopyonly: file(RENAME tmp dest) called
// from the source tree is recovered as a synthetic COPYONLY configure_file
// (OpenBLAS's config.h cross-compile shape); a cmake-internal rename and a
// non-RENAME file() subcommand are both ignored. Decode routes RENAME into
// ConfigFiles alongside real configure_file calls.
func TestExtractFileRename_AsConfigureCopyonly(t *testing.T) {
	trace := `{"args":["RENAME","/build/config.h.tmp","/build/config.h"],"cmd":"file","file":"/src/cmake/prebuild.cmake","line":1374}
{"args":["RENAME","/x/a","/x/b"],"cmd":"file","file":"/usr/share/cmake/Modules/Internal.cmake","line":9}
{"args":["GLOB","V","/src/*.c"],"cmd":"file","file":"/src/CMakeLists.txt","line":3}
`
	got := Decode([]byte(trace), "/src", nil).ConfigFiles
	if len(got) != 1 {
		t.Fatalf("want 1 recovered RENAME (cmake-internal + GLOB ignored); got %d (%+v)", len(got), got)
	}
	c := got[0]
	if c.Input != "/build/config.h.tmp" || c.Output != "/build/config.h" {
		t.Errorf("input/output: %+v", c)
	}
	if len(c.Options) != 1 || c.Options[0] != "COPYONLY" {
		t.Errorf("options: %+v want [COPYONLY]", c.Options)
	}
}

// TestExecuteProcess_ListValuedCommandSplits: a COMMAND built from an unquoted
// ${command} list variable arrives as one ;-joined token; the parser must
// split it so argv[0] is the real driver (cc), not basename(/dev/null)=null.
// Escaped \; is a literal semicolon; empty elements are dropped.
func TestExecuteProcess_ListValuedCommandSplits(t *testing.T) {
	trace := `{"args":["COMMAND","/usr/bin/cc;-Wl,--version;-o;/dev/null","OUTPUT_VARIABLE","stdout"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}
`
	got := Decode([]byte(trace), "/src", nil).ExecuteProcesses
	if len(got) != 1 || len(got[0].Commands) != 1 {
		t.Fatalf("got %+v", got)
	}
	argv := got[0].Commands[0]
	want := []string{"/usr/bin/cc", "-Wl,--version", "-o", "/dev/null"}
	if len(argv) != 4 || argv[0] != want[0] || argv[3] != want[3] {
		t.Errorf("argv = %v, want %v", argv, want)
	}
}

// TestExtractAddLibrary_DeclFileFromFrameStack pins the frame-stack recovery of
// a function-wrapped add_library's declaring scope — the abseil shape, where
// absl_cc_library (a function in CMake/AbseilHelpers.cmake) wraps
// add_library(<name> INTERFACE). The add_library event's own File is the helper
// module; DeclFile must resolve to the enclosing frame-1 CMakeLists.txt that
// called the function.
func TestExtractAddLibrary_DeclFileFromFrameStack(t *testing.T) {
	trace := []byte(
		// frame-1 call site in the declaring CMakeLists, then the function body's
		// add_library at frame 2 in the helper module.
		`{"args":["mylib","INTERFACE"],"cmd":"my_cc_library","file":"/src/absl/base/CMakeLists.txt","frame":1,"line":20}` + "\n" +
			`{"args":["mylib","INTERFACE"],"cmd":"add_library","file":"/src/CMake/AbseilHelpers.cmake","frame":2,"line":321}` + "\n" +
			// A second, top-level add_library written directly in a CMakeLists: DeclFile == File.
			`{"args":["toplib","INTERFACE"],"cmd":"add_library","file":"/src/other/CMakeLists.txt","frame":1,"line":5}` + "\n",
	)
	calls := ExtractAddLibrary(trace, "/src")
	got := map[string]string{}
	for _, c := range calls {
		got[c.Name] = c.DeclFile
	}
	if got["mylib"] != "/src/absl/base/CMakeLists.txt" {
		t.Errorf("mylib DeclFile = %q, want /src/absl/base/CMakeLists.txt (enclosing frame-1 scope, not the helper module)", got["mylib"])
	}
	if got["toplib"] != "/src/other/CMakeLists.txt" {
		t.Errorf("toplib DeclFile = %q, want its own CMakeLists.txt (top-level call)", got["toplib"])
	}
}

// TestExtractAddLibrary_InvocationCallSiteFromFrameStack pins the frame-stack
// recovery of the user-level invocation (CallFile/CallLine/CallCmd): a
// function-wrapped add_library recovers the caller's file:line+command; a
// direct top-level add_library and one at the top level of an include()d file
// recover NOTHING — inclusion frames are scope changes, not invocations, so
// the include() line must not be misattributed as a call site.
func TestExtractAddLibrary_InvocationCallSiteFromFrameStack(t *testing.T) {
	trace := []byte(
		// include() pushes a frame: the included file's top-level add_library
		// runs at frame 2 with the include() event as its frame-1 parent.
		`{"args":["cmake/extra.cmake"],"cmd":"include","file":"/src/CMakeLists.txt","frame":1,"line":3}` + "\n" +
			`{"args":["inclib","INTERFACE"],"cmd":"add_library","file":"/src/cmake/extra.cmake","frame":2,"line":2}` + "\n" +
			// Function-wrapped (abseil shape): invocation at frame 1, body at frame 2.
			`{"args":["mylib"],"cmd":"my_cc_library","file":"/src/absl/base/CMakeLists.txt","frame":1,"line":20}` + "\n" +
			`{"args":["mylib","INTERFACE"],"cmd":"add_library","file":"/src/CMake/AbseilHelpers.cmake","frame":2,"line":321}` + "\n" +
			// Direct top-level call: no caller frame.
			`{"args":["toplib","INTERFACE"],"cmd":"add_library","file":"/src/other/CMakeLists.txt","frame":1,"line":5}` + "\n",
	)
	calls := ExtractAddLibrary(trace, "/src")
	got := map[string]AddLibraryCall{}
	for _, c := range calls {
		got[c.Name] = c
	}
	if c := got["mylib"]; c.CallFile != "/src/absl/base/CMakeLists.txt" || c.CallLine != 20 || c.CallCmd != "my_cc_library" {
		t.Errorf("mylib call site = %q:%d (%q), want /src/absl/base/CMakeLists.txt:20 (my_cc_library)",
			c.CallFile, c.CallLine, c.CallCmd)
	}
	if c := got["inclib"]; c.CallFile != "" || c.CallLine != 0 {
		t.Errorf("inclib call site = %q:%d, want none — include() is not an invocation", c.CallFile, c.CallLine)
	}
	if c := got["toplib"]; c.CallFile != "" || c.CallLine != 0 {
		t.Errorf("toplib call site = %q:%d, want none for a direct declaration", c.CallFile, c.CallLine)
	}
}

// TestDecode_CodegenCallSiteFromFrameStack pins the frame-stack recovery of
// the user-level invocation for codegen calls: a macro-wrapped
// add_custom_command / add_custom_target / execute_process records the
// wrapper invocation's file:line+command, while one at the top level of an
// include()d file records nothing (inclusions are scope changes).
func TestDecode_CodegenCallSiteFromFrameStack(t *testing.T) {
	trace := []byte(
		// Macro-wrapped pair: invocation at frame 1, bodies at frame 2.
		`{"args":["lut"],"cmd":"gen_lut","file":"/src/CMakeLists.txt","frame":1,"line":9}` + "\n" +
			`{"args":["OUTPUT","/b/lut.h","COMMAND","gen"],"cmd":"add_custom_command","file":"/src/cmake/codegen.cmake","frame":2,"line":3}` + "\n" +
			`{"args":["gen_lut_tgt","DEPENDS","/b/lut.h"],"cmd":"add_custom_target","file":"/src/cmake/codegen.cmake","frame":2,"line":7}` + "\n" +
			`{"args":["COMMAND","tool","OUTPUT_FILE","/b/probe.txt"],"cmd":"execute_process","file":"/src/cmake/codegen.cmake","frame":2,"line":11}` + "\n" +
			// include()d-file top-level codegen: no call site.
			`{"args":["cmake/tables.cmake"],"cmd":"include","file":"/src/CMakeLists.txt","frame":1,"line":12}` + "\n" +
			`{"args":["OUTPUT","/b/t2.h","COMMAND","gen"],"cmd":"add_custom_command","file":"/src/cmake/tables.cmake","frame":2,"line":2}` + "\n",
	)
	d := Decode(trace, "/src", nil)
	if len(d.AddCustomCommands) != 2 || len(d.AddCustomTargets) != 1 || len(d.ExecuteProcesses) != 1 {
		t.Fatalf("decoded counts = %d cmds / %d tgts / %d procs; want 2/1/1",
			len(d.AddCustomCommands), len(d.AddCustomTargets), len(d.ExecuteProcesses))
	}
	if c := d.AddCustomCommands[0]; c.CallFile != "/src/CMakeLists.txt" || c.CallLine != 9 || c.CallCmd != "gen_lut" {
		t.Errorf("macro-wrapped add_custom_command call site = %q:%d (%q); want /src/CMakeLists.txt:9 (gen_lut)",
			c.CallFile, c.CallLine, c.CallCmd)
	}
	if c := d.AddCustomTargets[0]; c.CallFile != "/src/CMakeLists.txt" || c.CallLine != 9 || c.CallCmd != "gen_lut" {
		t.Errorf("macro-wrapped add_custom_target call site = %q:%d (%q); want /src/CMakeLists.txt:9 (gen_lut)",
			c.CallFile, c.CallLine, c.CallCmd)
	}
	if c := d.ExecuteProcesses[0]; c.CallFile != "/src/CMakeLists.txt" || c.CallLine != 9 || c.CallCmd != "gen_lut" {
		t.Errorf("macro-wrapped execute_process call site = %q:%d (%q); want /src/CMakeLists.txt:9 (gen_lut)",
			c.CallFile, c.CallLine, c.CallCmd)
	}
	if c := d.AddCustomCommands[1]; c.CallFile != "" || c.CallLine != 0 {
		t.Errorf("include()d-file add_custom_command call site = %q:%d; want none", c.CallFile, c.CallLine)
	}
}

// TestDecode_IncludeDirectories records directory-scoped include_directories()
// calls (the private-include signal), dropping the AFTER/BEFORE/SYSTEM keywords
// and skipping calls outside the source tree.
func TestDecode_IncludeDirectories(t *testing.T) {
	trace := strings.Join([]string{
		`{"args":["include","/build/gen/include"],"cmd":"include_directories","file":"/src/lapacke/CMakeLists.txt","line":19}`,
		`{"args":["SYSTEM","AFTER","thirdparty/inc"],"cmd":"include_directories","file":"/src/CMakeLists.txt","line":3}`,
		// Outside the source tree — must be ignored.
		`{"args":["x"],"cmd":"include_directories","file":"/usr/share/cmake/Modules/Foo.cmake","line":5}`,
	}, "\n") + "\n"
	d := Decode([]byte(trace), "/src", nil)
	if len(d.IncludeDirectories) != 2 {
		t.Fatalf("want 2 include_directories calls, got %d (%+v)", len(d.IncludeDirectories), d.IncludeDirectories)
	}
	if got := d.IncludeDirectories[0]; got.File != "/src/lapacke/CMakeLists.txt" ||
		len(got.Dirs) != 2 || got.Dirs[0] != "include" || got.Dirs[1] != "/build/gen/include" {
		t.Errorf("call[0] = %+v; want File=/src/lapacke/CMakeLists.txt Dirs=[include /build/gen/include]", got)
	}
	if got := d.IncludeDirectories[1]; len(got.Dirs) != 1 || got.Dirs[0] != "thirdparty/inc" {
		t.Errorf("call[1] Dirs = %v; want [thirdparty/inc] (AFTER/SYSTEM dropped)", got.Dirs)
	}
}

// traceDeferDirectory holds a cmake_language(DEFER DIRECTORY <dir> CALL
// configure_file …) registration plus its deferred EXECUTION event: cmake
// re-reports the registration's file/line on the execution event and marks
// it with a defer id. A sibling plain-DEFER (no DIRECTORY) pair confirms
// DeferDir stays empty when the call executes in its own directory's scope.
const traceDeferDirectory = `{"args":["DEFER","DIRECTORY","/src","CALL","configure_file","/src/sub/cfg.h.in","cfg.h"],"cmd":"cmake_language","file":"/src/sub/CMakeLists.txt","line":4,"frame":1}
{"args":["/src/sub/cfg.h.in","cfg.h"],"cmd":"configure_file","file":"/src/sub/CMakeLists.txt","line":4,"frame":1,"defer":"__0"}
{"args":["DEFER","CALL","configure_file","/src/sub/own.h.in","own.h"],"cmd":"cmake_language","file":"/src/sub/CMakeLists.txt","line":9,"frame":1}
{"args":["/src/sub/own.h.in","own.h"],"cmd":"configure_file","file":"/src/sub/CMakeLists.txt","line":9,"frame":1,"defer":"__1"}
`

func TestExtractConfigureFiles_DeferDirectory(t *testing.T) {
	got := ExtractConfigureFiles([]byte(traceDeferDirectory), "/src")
	if len(got) != 2 {
		t.Fatalf("want 2 configure_file calls; got %d (%+v)", len(got), got)
	}
	byOut := map[string]ConfigureFileCall{}
	for _, c := range got {
		byOut[c.Output] = c
	}
	if c := byOut["cfg.h"]; c.DeferDir != "/src" {
		t.Errorf("DEFER DIRECTORY call: DeferDir = %q, want /src (%+v)", c.DeferDir, c)
	}
	if c := byOut["own.h"]; c.DeferDir != "" {
		t.Errorf("plain DEFER call: DeferDir = %q, want empty (own-scope execution anchors normally)", c.DeferDir)
	}
}

func TestDecode_DeferDirectoryOnConfigFiles(t *testing.T) {
	d := Decode([]byte(traceDeferDirectory), "/src", nil)
	if len(d.ConfigFiles) != 2 {
		t.Fatalf("want 2 ConfigFiles; got %d (%+v)", len(d.ConfigFiles), d.ConfigFiles)
	}
	var sawDeferred bool
	for _, c := range d.ConfigFiles {
		if c.Output == "cfg.h" {
			sawDeferred = true
			if c.DeferDir != "/src" {
				t.Errorf("Decode ConfigFiles DeferDir = %q, want /src", c.DeferDir)
			}
		}
	}
	if !sawDeferred {
		t.Errorf("deferred configure_file missing from Decode output: %+v", d.ConfigFiles)
	}
}

// traceBuildTypeRead: a project-side if(CMAKE_BUILD_TYPE STREQUAL ...) vs a
// cmake-internal module consulting the same variable.
const traceBuildTypeRead = `{"args":["CMAKE_BUILD_TYPE","STREQUAL","Debug"],"cmd":"if","file":"/src/CMakeLists.txt","line":6,"frame":1}
`
const traceBuildTypeInternalOnly = `{"args":["CMAKE_BUILD_TYPE"],"cmd":"if","file":"/usr/share/cmake-4.3/Modules/Foo.cmake","line":10,"frame":2}
`

func TestReadsBuildType(t *testing.T) {
	if !ReadsBuildType([]byte(traceBuildTypeRead), "/src") {
		t.Errorf("project-side CMAKE_BUILD_TYPE read should be detected")
	}
	if ReadsBuildType([]byte(traceBuildTypeInternalOnly), "/src") {
		t.Errorf("cmake-internal CMAKE_BUILD_TYPE read must NOT trigger the per-config passes")
	}
	if ReadsBuildType(nil, "/src") {
		t.Errorf("empty trace must not trigger")
	}
}

// traceDeferExecuteProcess: a cmake_language(DEFER DIRECTORY <dir> CALL
// execute_process …) registration + its deferred execution, plus an
// ordinary execute_process — the field-stamping parity check for
// ExecuteProcessCall.DeferDir (the lifts deliberately don't consume it; see
// the field's doc).
const traceDeferExecuteProcess = `{"args":["DEFER","DIRECTORY","/src","CALL","execute_process","COMMAND","/usr/bin/cmake","-E","copy","/src/a","/b/a"],"cmd":"cmake_language","file":"/src/sub/CMakeLists.txt","line":3,"frame":1}
{"args":["COMMAND","/usr/bin/cmake","-E","copy","/src/a","/b/a"],"cmd":"execute_process","file":"/src/sub/CMakeLists.txt","line":3,"frame":1,"defer":"__0"}
{"args":["COMMAND","/usr/bin/cmake","-E","copy","/src/b","/b/b"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":9,"frame":1}
`

func TestExtractExecuteProcess_DeferDirectory(t *testing.T) {
	got := ExtractExecuteProcess([]byte(traceDeferExecuteProcess), "/src")
	if len(got) != 2 {
		t.Fatalf("want 2 execute_process calls; got %d (%+v)", len(got), got)
	}
	byLine := map[int]ExecuteProcessCall{}
	for _, c := range got {
		byLine[c.Line] = c
	}
	if c := byLine[3]; c.DeferDir != "/src" {
		t.Errorf("deferred call DeferDir = %q, want /src", c.DeferDir)
	}
	if c := byLine[9]; c.DeferDir != "" {
		t.Errorf("ordinary call DeferDir = %q, want empty", c.DeferDir)
	}
}

func TestDecode_DeferDirectoryOnExecuteProcesses(t *testing.T) {
	d := Decode([]byte(traceDeferExecuteProcess), "/src", nil)
	if len(d.ExecuteProcesses) != 2 {
		t.Fatalf("want 2 ExecuteProcesses; got %d", len(d.ExecuteProcesses))
	}
	var sawDeferred bool
	for _, c := range d.ExecuteProcesses {
		if c.Line == 3 {
			sawDeferred = true
			if c.DeferDir != "/src" {
				t.Errorf("Decode ExecuteProcesses DeferDir = %q, want /src", c.DeferDir)
			}
		}
	}
	if !sawDeferred {
		t.Errorf("deferred execute_process missing from Decode output")
	}
}
