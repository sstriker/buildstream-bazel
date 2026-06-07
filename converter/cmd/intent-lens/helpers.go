package main

import (
	"flag"
	"io"
)

// newFlagSet returns a flag set that prints usage to stderr and exits on a
// parse error (the standard CLI behavior).
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

// readAll drains r fully.
func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
