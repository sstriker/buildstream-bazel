package lower

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLooksLikeCxxHeader(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// eigen-style extensionless module header
	if !looksLikeCxxHeader(write("Dense", "// license\n#include \"Core\"\n")) {
		t.Error("eigen module header should sniff as a C++ header")
	}
	if !looksLikeCxxHeader(write("Core", "#ifndef EIGEN_CORE_H\n#define EIGEN_CORE_H\n")) {
		t.Error("#ifndef header should sniff true")
	}
	// non-headers in an include dir
	if looksLikeCxxHeader(write("LICENSE", "Apache License 2.0\nTerms and conditions...\n")) {
		t.Error("LICENSE should not sniff as a header")
	}
	if looksLikeCxxHeader(write("README", "# Eigen\nA C++ template library.\n")) {
		t.Error("README markdown should not sniff as a header")
	}
}
