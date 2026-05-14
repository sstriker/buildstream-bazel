package element_test

import "os"

// writeBytes writes a small file. Lives in its own helper file so yaml_test
// stays focused on parser behavior and graph_test on graph behavior.
func writeBytes(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}
