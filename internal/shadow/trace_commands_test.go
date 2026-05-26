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
