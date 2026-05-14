package regression_test

import "os"

// openTrunc is a small file-open helper kept in a shared test file so
// registry_test.go and any future fixture-writing tests don't each
// reimplement it.
func openTrunc(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}
