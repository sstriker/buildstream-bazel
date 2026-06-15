package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestNestedCmakeScriptCall pins the argv-level `cmake -P` wrapper detector that
// drives the P3 recursion: a harvested execute_process that is itself a
// `cmake [-D…] -P <script>` invocation is recognized so expandScriptCalls
// descends into the inner script, and its -D cache args are carried through so
// the inner trace expands the same way the real nested invocation would.
func TestNestedCmakeScriptCall(t *testing.T) {
	mk := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}
	tests := []struct {
		name       string
		call       shadow.ExecuteProcessCall
		wantOK     bool
		wantScript string
		wantDArgs  []string
	}{
		{
			name:       "resolved cmake path with combined -D",
			call:       mk("/usr/local/bin/cmake", "-DBIN_DIR=/b", "-DSRC_DIR=/s", "-P", "/s/inner.cmake"),
			wantOK:     true,
			wantScript: "/s/inner.cmake",
			wantDArgs:  []string{"-DBIN_DIR=/b", "-DSRC_DIR=/s"},
		},
		{
			name:       "unsubstituted CMAKE_COMMAND literal with separate -D pair",
			call:       mk("${CMAKE_COMMAND}", "-D", "PROTOC=protoc", "-P", "gen.cmake"),
			wantOK:     true,
			wantScript: "gen.cmake",
			wantDArgs:  []string{"-D", "PROTOC=protoc"},
		},
		{
			name:       "cmake.exe driver, no -D args",
			call:       mk("C:\\tools\\cmake.exe", "-P", "C:\\proj\\inner.cmake"),
			wantOK:     true,
			wantScript: "C:\\proj\\inner.cmake",
		},
		{
			name:   "real tool (protoc) is a leaf, not a wrapper",
			call:   mk("/usr/bin/protoc", "--cpp_out=/b", "-I", "/s", "/s/foo.proto"),
			wantOK: false,
		},
		{
			name:   "cmake without -P (a cmake -E call) is not a script wrapper",
			call:   mk("/usr/bin/cmake", "-E", "copy", "a", "b"),
			wantOK: false,
		},
		{
			name:   "empty argv",
			call:   shadow.ExecuteProcessCall{Commands: [][]string{{}}},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, dArgs, ok := nestedCmakeScriptCall(tt.call)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if script != tt.wantScript {
				t.Errorf("script = %q, want %q", script, tt.wantScript)
			}
			if !reflect.DeepEqual(dArgs, tt.wantDArgs) {
				t.Errorf("dArgs = %#v, want %#v", dArgs, tt.wantDArgs)
			}
		})
	}
}
