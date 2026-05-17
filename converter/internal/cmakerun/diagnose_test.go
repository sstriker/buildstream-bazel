package cmakerun

import (
	"errors"
	"strings"
	"testing"
)

func TestBoundedBuffer_TrimsToLimit(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if got := string(b.Bytes()); got != "hello" {
		t.Errorf("after first Write Bytes = %q, want hello", got)
	}
	// Push past the limit; the oldest bytes should drop.
	if _, err := b.Write([]byte(" world!")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if got := string(b.Bytes()); got != "o world!" {
		t.Errorf("after second Write Bytes = %q, want %q", got, "o world!")
	}
	if got := len(b.Bytes()); got != 8 {
		t.Errorf("Bytes len = %d, want 8", got)
	}
}

func TestBoundedBuffer_WriteLargerThanLimit(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("abcdefghij")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := string(b.Bytes()); got != "ghij" {
		t.Errorf("Bytes = %q, want ghij", got)
	}
}

func TestAnnotateConfigureFailure_CMP0026(t *testing.T) {
	base := errors.New("exit status 1")
	stderr := []byte(`CMake Error in CMakeLists.txt:
  The LOCATION property may not be read from target "thirdparty_foo".
  Use the target name directly with add_custom_command, or use the
  generator expression $<TARGET_FILE>, as appropriate.

  See CMake policy CMP0026 for more information.
`)
	got := annotateConfigureFailure(base, stderr)
	if got == nil {
		t.Fatal("annotateConfigureFailure returned nil")
	}
	msg := got.Error()
	if !strings.Contains(msg, "cmakerun: cmake failed:") {
		t.Errorf("error message lost the base prefix: %q", msg)
	}
	if !strings.Contains(msg, "[hint]") {
		t.Errorf("error message missing the [hint] annotation: %q", msg)
	}
	if !strings.Contains(msg, "CMP0026") {
		t.Errorf("hint should name CMP0026 so operators can grep for it: %q", msg)
	}
	if !strings.Contains(msg, "patch_cmds") {
		t.Errorf("hint should point at the Bazel patch_cmds workaround: %q", msg)
	}
	if !strings.Contains(msg, "--cmp0026-shim") {
		t.Errorf("hint should mention the --cmp0026-shim opt-in flag (#208): %q", msg)
	}
	if !errors.Is(got, base) {
		t.Errorf("annotated error should wrap the original; errors.Is(err, base) = false")
	}
}

func TestAnnotateConfigureFailure_UnrelatedFailureNoHint(t *testing.T) {
	base := errors.New("exit status 1")
	stderr := []byte("CMake Error: missing argument to project()\n")
	got := annotateConfigureFailure(base, stderr)
	if got == nil {
		t.Fatal("annotateConfigureFailure returned nil")
	}
	if strings.Contains(got.Error(), "[hint]") {
		t.Errorf("unrelated failure should not pick up the CMP0026 hint: %q", got.Error())
	}
	if !errors.Is(got, base) {
		t.Errorf("annotated error should still wrap the original")
	}
}

func TestMatchCMP0026(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "canonical cmake 4.x error",
			body: `The LOCATION property may not be read from target "foo".
See CMake policy CMP0026 for more information.`,
			want: true,
		},
		{
			name: "LOCATION + add_custom_command hint (no CMP0026 text)",
			body: `The LOCATION property may not be read from target "foo".
Use the target name directly with add_custom_command, or use the
generator expression $<TARGET_FILE>, as appropriate.`,
			want: true,
		},
		{
			name: "LOCATION + get_target_property hint",
			body: `The LOCATION property may not be read from target "foo".
Use get_target_property with $<TARGET_FILE> instead.`,
			want: true,
		},
		{
			name: "unrelated CMP0026 reference (interrogation, not a failure)",
			body: "Policy CMP0026 is set to NEW",
			want: false,
		},
		{
			name: "unrelated cmake failure",
			body: "CMake Error: cmake_minimum_required not called",
			want: false,
		},
		{
			name: "empty stderr",
			body: "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchCMP0026([]byte(tc.body))
			if got != tc.want {
				t.Errorf("matchCMP0026 = %v, want %v", got, tc.want)
			}
		})
	}
}
