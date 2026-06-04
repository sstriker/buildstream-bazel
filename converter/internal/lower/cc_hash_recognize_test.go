package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// recognizeCcHashCase drives recognizeCcHash with a vtkHashSource -P command
// carrying the given -D args, against a build statement that writes a single
// .h output. Returns whether the recognizer fired and (when it did) the spec
// it produced.
func recognizeCcHashCase(t *testing.T, dArgs string, outs []string) (bool, *ir.CCHashSpec) {
	t.Helper()
	cc := newCodegenContext()
	cc.LiftCCHash = true
	b := &ninja.Build{Outputs: outs}
	cmd := `/usr/bin/cmake ` + dArgs + ` -D_vtk_hash_source_run=ON -P /src/CMake/vtkHashSource.cmake`
	name, ok := recognizeCcHash(cc, b, cmd, "/src/CMake/vtkHashSource.cmake", "/src", "/build")
	if !ok {
		return false, nil
	}
	// The recognizer appends the cc_hash target to cc.Genrules; pull its spec.
	for _, g := range cc.Genrules {
		if g.Name == name && g.CCHash != nil {
			return true, g.CCHash
		}
	}
	t.Fatalf("recognizer fired (name=%q) but no CCHash spec landed in cc.Genrules", name)
	return false, nil
}

// the full, well-formed -D arg set vtkHashSource emits for one site.
const ccHashGoodArgs = `"-Dinput_file=/src/Parallel/Core/vtkSocketCommunicator.cxx" "-Doutput_file=/build/Parallel/Core/vtkSocketCommunicatorHash.h" "-Doutput_name=vtkSocketCommunicatorHash" "-Dalgorithm=SHA256"`

func TestRecognizeCcHash_FiresOnWellFormedSite(t *testing.T) {
	ok, spec := recognizeCcHashCase(t, ccHashGoodArgs, []string{"/build/Parallel/Core/vtkSocketCommunicatorHash.h"})
	if !ok {
		t.Fatal("well-formed vtkHashSource site should be recognized")
	}
	if spec.Src != "Parallel/Core/vtkSocketCommunicator.cxx" {
		t.Errorf("src = %q", spec.Src)
	}
	if spec.Name != "vtkSocketCommunicatorHash" {
		t.Errorf("define_name = %q", spec.Name)
	}
	if spec.Algorithm != "SHA256" {
		t.Errorf("algorithm = %q", spec.Algorithm)
	}
	if spec.OutHeader != "Parallel/Core/vtkSocketCommunicatorHash.h" {
		t.Errorf("out_header = %q", spec.OutHeader)
	}
}

// TestRecognizeCcHash_AlgorithmCaseInsensitive pins the Copilot-review fix: a
// lowercase `-Dalgorithm=sha256` (a script sharing vtkHashSource's -D
// interface) is recognized AND normalized to the canonical uppercase spelling
// the cc_hash rule's `values` attr accepts — not declined to the fallback.
func TestRecognizeCcHash_AlgorithmCaseInsensitive(t *testing.T) {
	args := `"-Dinput_file=/src/x.cxx" "-Doutput_name=xHash" "-Dalgorithm=sha256"`
	ok, spec := recognizeCcHashCase(t, args, []string{"/build/xHash.h"})
	if !ok {
		t.Fatal("lowercase algorithm should still be recognized")
	}
	if spec.Algorithm != "SHA256" {
		t.Errorf("algorithm = %q, want normalized SHA256", spec.Algorithm)
	}
}

// TestRecognizeCcHash_DefaultsToMD5 pins cmake's vtk_hash_source default: a
// missing -Dalgorithm= is treated as MD5 (cmake's default), not declined.
func TestRecognizeCcHash_DefaultsToMD5(t *testing.T) {
	args := `"-Dinput_file=/src/x.cxx" "-Doutput_name=xHash"`
	ok, spec := recognizeCcHashCase(t, args, []string{"/build/xHash.h"})
	if !ok {
		t.Fatal("missing algorithm should default to MD5, not decline")
	}
	if spec.Algorithm != "MD5" {
		t.Errorf("algorithm = %q, want MD5", spec.Algorithm)
	}
}

// TestRecognizeCcHash_Declines covers the fall-through-to-runner/bake cases.
func TestRecognizeCcHash_Declines(t *testing.T) {
	cases := []struct {
		name, dArgs string
		outs        []string
	}{
		{
			name:  "missing input_file",
			dArgs: `"-Doutput_name=xHash" "-Dalgorithm=SHA256"`,
			outs:  []string{"/build/xHash.h"},
		},
		{
			name:  "missing output_name",
			dArgs: `"-Dinput_file=/src/x.cxx" "-Dalgorithm=SHA256"`,
			outs:  []string{"/build/xHash.h"},
		},
		{
			name:  "unsupported algorithm",
			dArgs: `"-Dinput_file=/src/x.cxx" "-Doutput_name=xHash" "-Dalgorithm=CRC32"`,
			outs:  []string{"/build/xHash.h"},
		},
		{
			name:  "input outside source tree",
			dArgs: `"-Dinput_file=/elsewhere/x.cxx" "-Doutput_name=xHash" "-Dalgorithm=SHA256"`,
			outs:  []string{"/build/xHash.h"},
		},
		{
			name:  "no header output",
			dArgs: ccHashGoodArgs,
			outs:  []string{"/build/some.txt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok, _ := recognizeCcHashCase(t, tc.dArgs, tc.outs); ok {
				t.Errorf("%s: recognizer should have declined (fall through to runner/bake)", tc.name)
			}
		})
	}
}

// TestRecognizeCcHash_OffByDefault pins that without --lift-cc-hash the
// recognizer never fires, even on a well-formed site.
func TestRecognizeCcHash_OffByDefault(t *testing.T) {
	cc := newCodegenContext() // LiftCCHash defaults false
	b := &ninja.Build{Outputs: []string{"/build/xHash.h"}}
	cmd := `/usr/bin/cmake "-Dinput_file=/src/x.cxx" "-Doutput_name=xHash" "-Dalgorithm=SHA256" -P /src/CMake/vtkHashSource.cmake`
	if _, ok := recognizeCcHash(cc, b, cmd, "/src/CMake/vtkHashSource.cmake", "/src", "/build"); ok {
		t.Error("recognizer must not fire with --lift-cc-hash off")
	}
}
