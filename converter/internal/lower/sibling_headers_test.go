package lower_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// TestToIR_StagesPrivateSiblingHeader pins the fix for the build-lens
// "missing private header" class (brotli c/common/platform.h, libxml2
// libxml.h, fmt test/gtest-extra.h): a header that lives in the target's own
// SOURCE directory — included by a sibling source via `#include "x.h"` — and
// is NOT under any declared include dir must still be staged as a declared
// hdr, or the quote-include misses in Bazel's sandbox. cmake finds it
// implicitly (it searches the including file's own directory); Bazel needs the
// input declared.
//
// It also pins the other half of the contract: the source dir is NOT added to
// the emitted include path (irt.Includes) — a sibling quote-include resolves
// relative to the including file, so the staged hdr alone suffices and we must
// not leak a spurious -I.
func TestToIR_StagesPrivateSiblingHeader(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostSrc, "c/common"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A source that quote-includes a private sibling header, plus the header —
	// both in c/common, which is NOT declared as an include dir.
	if err := os.WriteFile(filepath.Join(hostSrc, "c/common/transform.c"),
		[]byte("#include \"platform.h\"\nint t(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, "c/common/platform.h"),
		[]byte("#define PLAT 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: hostSrc,
				Build:  "/tmp/convert-element-build-sibling",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "common", Id: "common::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"common::@1": {
				Name: "common",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "c/common/transform.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					// No IncludeDirectories: the header is reachable only as a
					// source-dir sibling.
				}},
			},
		},
	}

	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: hostSrc})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]

	if !contains(tgt.Hdrs, "c/common/platform.h") {
		t.Errorf("Hdrs = %v, want it to stage the private sibling c/common/platform.h", tgt.Hdrs)
	}
	if contains(tgt.Includes, "c/common") {
		t.Errorf("Includes = %v, must NOT add the source dir c/common (sibling quote-include needs no -I)", tgt.Includes)
	}
}
