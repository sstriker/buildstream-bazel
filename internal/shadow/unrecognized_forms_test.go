package shadow

import "testing"

func TestAuditUnrecognizedCommandForms(t *testing.T) {
	const sr = "/src"
	trace := `{"args":["OUTPUT","gen.c","COMMAND","tool"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":1}
{"args":["TARGET","foo","PRE_LINK","COMMAND","cp","a","b","BYPRODUCTS","/src/build/x.h"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":2}
{"args":["COMMAND","echo","hi"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":3}
{"args":["COPY","a.h","DESTINATION","/src/build/inc"],"cmd":"file","file":"/src/CMakeLists.txt","line":4}
{"args":["COPY","hdrs","DESTINATION","/src/build/inc","FILES_MATCHING","PATTERN","*.h"],"cmd":"file","file":"/src/CMakeLists.txt","line":5}
{"args":["READ","/src/x","var"],"cmd":"file","file":"/src/CMakeLists.txt","line":6}
{"args":["COMMAND","onlycmd"],"cmd":"add_custom_command","file":"/elsewhere/CMakeLists.txt","line":7}
`
	got := AuditUnrecognizedCommandForms([]byte(trace), sr)

	// Expect exactly two flags: the no-OUTPUT/no-TARGET add_custom_command (line 3)
	// and the PATTERN-filtered file(COPY) (line 5). NOT flagged: OUTPUT-form (1),
	// TARGET-event form (2, closed by #722), plain file(COPY) (4), file(READ) (6,
	// not output-producing), and the out-of-tree event (7).
	if len(got) != 2 {
		t.Fatalf("got %d unrecognized forms, want 2: %+v", len(got), got)
	}
	byLine := map[int]UnrecognizedForm{}
	for _, uf := range got {
		byLine[uf.Line] = uf
	}
	if uf, ok := byLine[3]; !ok || uf.Cmd != "add_custom_command" {
		t.Errorf("expected add_custom_command (no OUTPUT/TARGET) flagged at line 3; got %+v", got)
	}
	if uf, ok := byLine[5]; !ok || uf.Form != "file(COPY)" {
		t.Errorf("expected file(COPY) PATTERN flagged at line 5; got %+v", got)
	}
	if _, ok := byLine[2]; ok {
		t.Error("TARGET-event form must NOT be flagged (closed by #722)")
	}
	if _, ok := byLine[4]; ok {
		t.Error("plain file(COPY) must NOT be flagged")
	}
}
